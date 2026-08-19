package db

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestListingArgsOrderAndTrimming(t *testing.T) {
	listing := RawListing{
		SourceDomain:   "  www.exemplo.com.br  ",
		ListingURL:     " https://www.exemplo.com.br/imovel/1 ",
		TitleRaw:       "Apartamento 2 quartos",
		PriceRaw:       "R$ 350.000",
		AddressRaw:     "Rua A, 100",
		DescriptionRaw: "Perto do centro",
		BedroomsRaw:    "2",
		AreaRaw:        "70 m²",
		ImageURLs:      []string{"https://cdn.exemplo.com.br/1.jpg"},
		ExtraData:      map[string]any{"condominio": "R$ 400"},
	}

	args, err := listingArgs(listing)
	if err != nil {
		t.Fatalf("listingArgs() error = %v, want nil", err)
	}

	// A ordem é o contrato com os placeholders de upsertListingSQL.
	if got, want := len(args), 10; got != want {
		t.Fatalf("len(args) = %d, want %d", got, want)
	}
	if got, want := args[0], "www.exemplo.com.br"; got != want {
		t.Errorf("args[0] (source_domain) = %v, want %q", got, want)
	}
	if got, want := args[1], "https://www.exemplo.com.br/imovel/1"; got != want {
		t.Errorf("args[1] (listing_url) = %v, want %q", got, want)
	}

	wantRaw := []any{
		"Apartamento 2 quartos", "R$ 350.000", "Rua A, 100",
		"Perto do centro", "2", "70 m²",
	}
	for i, want := range wantRaw {
		if got := args[i+2]; got != want {
			t.Errorf("args[%d] = %v, want %v", i+2, got, want)
		}
	}

	if got, want := args[8], []string{"https://cdn.exemplo.com.br/1.jpg"}; !reflect.DeepEqual(got, want) {
		t.Errorf("args[8] (image_urls) = %v, want %v", got, want)
	}

	extraData, ok := args[9].([]byte)
	if !ok {
		t.Fatalf("args[9] (extra_data) has type %T, want []byte", args[9])
	}
	var decoded map[string]any
	if err := json.Unmarshal(extraData, &decoded); err != nil {
		t.Fatalf("extra_data is not valid JSON: %v", err)
	}
	if got, want := decoded["condominio"], "R$ 400"; got != want {
		t.Errorf("extra_data[condominio] = %v, want %v", got, want)
	}
}

func TestListingArgsRejectsMissingIdentity(t *testing.T) {
	tests := []struct {
		name    string
		listing RawListing
		wantIn  string
	}{
		{
			name:    "sem domínio",
			listing: RawListing{ListingURL: "https://exemplo.com.br/1"},
			wantIn:  "source_domain",
		},
		{
			name:    "domínio só com espaços",
			listing: RawListing{SourceDomain: "   ", ListingURL: "https://exemplo.com.br/1"},
			wantIn:  "source_domain",
		},
		{
			name:    "sem URL",
			listing: RawListing{SourceDomain: "exemplo.com.br"},
			wantIn:  "listing_url",
		},
		{
			name:    "URL só com espaços",
			listing: RawListing{SourceDomain: "exemplo.com.br", ListingURL: "  "},
			wantIn:  "listing_url",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := listingArgs(tt.listing)
			if err == nil {
				t.Fatalf("listingArgs() error = nil, want error mentioning %q", tt.wantIn)
			}
			if !strings.Contains(err.Error(), tt.wantIn) {
				t.Errorf("listingArgs() error = %q, want it to mention %q", err, tt.wantIn)
			}
		})
	}
}

func TestListingArgsRejectsUnserializableExtraData(t *testing.T) {
	listing := RawListing{
		SourceDomain: "exemplo.com.br",
		ListingURL:   "https://exemplo.com.br/1",
		// NaN não tem representação em JSON: o erro precisa aparecer aqui e não
		// como uma falha opaca no meio do batch.
		ExtraData: map[string]any{"ratio": math.NaN()},
	}

	_, err := listingArgs(listing)
	if err == nil {
		t.Fatal("listingArgs() error = nil, want error about extra_data")
	}
	if !strings.Contains(err.Error(), "extra_data") {
		t.Errorf("listingArgs() error = %q, want it to mention %q", err, "extra_data")
	}
	if !strings.Contains(err.Error(), "https://exemplo.com.br/1") {
		t.Errorf("listingArgs() error = %q, want it to mention the listing URL", err)
	}
}

// A guarda de id vazio precisa valer antes de qualquer ida ao banco: com um
// pool nil, chegar à query seria panic — é assim que o teste prova que ela
// existe.
func TestListListingsByPropertyIDRejectsBlankPropertyID(t *testing.T) {
	for _, id := range []string{"", "   "} {
		_, err := ListListingsByPropertyID(context.Background(), nil, id)
		if err == nil {
			t.Fatalf("ListListingsByPropertyID(%q) error = nil, want an error", id)
		}
		if !strings.Contains(err.Error(), "property id is required") {
			t.Errorf("error = %q, want it to mention the missing property id", err)
		}
	}
}

func TestNormalizeImageURLsNeverReturnsNil(t *testing.T) {
	// nil precisa virar array vazio: a coluna image_urls nunca guarda NULL.
	if got := normalizeImageURLs(nil); got == nil {
		t.Fatal("normalizeImageURLs(nil) = nil, want empty slice")
	} else if len(got) != 0 {
		t.Errorf("normalizeImageURLs(nil) = %v, want empty slice", got)
	}

	urls := []string{"a.jpg", "b.jpg"}
	if got := normalizeImageURLs(urls); !reflect.DeepEqual(got, urls) {
		t.Errorf("normalizeImageURLs(%v) = %v, want the same values", urls, got)
	}
}

func TestStaleCountsByPropertyAggregatesWithMultiplicity(t *testing.T) {
	// Um mesmo imóvel pode perder vários anúncios no mesmo run: o decremento tem
	// que ser pelo número real, não por um.
	ids := []*string{
		ptr("prop-b"), ptr("prop-a"), nil, ptr("prop-b"),
		ptr("prop-c"), nil, ptr("prop-b"),
	}

	got := staleCountsByProperty(ids)

	want := []propertyDelta{
		{propertyID: "prop-a", count: 1},
		{propertyID: "prop-b", count: 3},
		{propertyID: "prop-c", count: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("staleCountsByProperty() = %+v, want %+v", got, want)
	}
}

func TestStaleCountsByPropertyIgnoresListingsWithoutProperty(t *testing.T) {
	// Anúncio nunca agrupado (property_id NULL) é apagado normalmente e não
	// mexe em contador algum. Espaços em branco caem no mesmo caso.
	for _, tt := range []struct {
		name string
		ids  []*string
	}{
		{name: "entrada vazia", ids: nil},
		{name: "só NULL", ids: []*string{nil, nil}},
		{name: "só espaços", ids: []*string{ptr("   ")}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := staleCountsByProperty(tt.ids)
			if len(got) != 0 {
				t.Errorf("staleCountsByProperty(%v) = %+v, want vazio", tt.ids, got)
			}
		})
	}
}

func TestStaleCountsByPropertyIsDeterministicallyOrdered(t *testing.T) {
	// A ordem fixa é a ordem de aquisição de locks: dois runs concorrentes que
	// decrementassem o mesmo par de imóveis em ordens opostas se travariam.
	ids := []*string{ptr("zzz"), ptr("aaa"), ptr("mmm"), ptr("aaa")}

	for range 20 {
		got := staleCountsByProperty(ids)
		want := []string{"aaa", "mmm", "zzz"}
		for i, id := range want {
			if got[i].propertyID != id {
				t.Fatalf("ordem = %+v, want %v", got, want)
			}
		}
	}
}

func TestStaleCountsByPropertyTrimsIDs(t *testing.T) {
	got := staleCountsByProperty([]*string{ptr("  prop-a  "), ptr("prop-a")})

	want := []propertyDelta{{propertyID: "prop-a", count: 2}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("staleCountsByProperty() = %+v, want %+v", got, want)
	}
}

func TestDeleteStaleListingsRejectsInvalidArguments(t *testing.T) {
	// As guardas precisam retornar **antes** de abrir transação — com pool nil,
	// qualquer ida ao banco seria um panic.
	tests := []struct {
		name         string
		domain       string
		runStartedAt time.Time
		wantIn       string
	}{
		{name: "domínio vazio", domain: "  ", runStartedAt: time.Now(), wantIn: "domain is required"},
		{name: "sem início de run", domain: "exemplo.com.br", wantIn: "runStartedAt is required"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deleted, properties, err := DeleteStaleListings(context.Background(), nil, tt.domain, tt.runStartedAt)
			if err == nil {
				t.Fatalf("DeleteStaleListings() error = nil, want erro mencionando %q", tt.wantIn)
			}
			if !strings.Contains(err.Error(), tt.wantIn) {
				t.Errorf("DeleteStaleListings() error = %q, want it to mention %q", err, tt.wantIn)
			}
			if deleted != 0 || properties != 0 {
				t.Errorf("contagens = (%d, %d), want (0, 0)", deleted, properties)
			}
		})
	}
}

func TestEncodeExtraDataDefaultsToEmptyObject(t *testing.T) {
	// nil e mapa vazio caem no mesmo lugar: "{}", nunca NULL.
	for _, data := range []map[string]any{nil, {}} {
		encoded, err := encodeExtraData(data)
		if err != nil {
			t.Fatalf("encodeExtraData(%v) error = %v, want nil", data, err)
		}
		if got, want := string(encoded), "{}"; got != want {
			t.Errorf("encodeExtraData(%v) = %q, want %q", data, got, want)
		}
	}
}
