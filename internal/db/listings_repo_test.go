package db

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
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

func TestListingEnrichmentArgsOrderAndNulls(t *testing.T) {
	neighborhood := "Jardim Botânico"
	bedrooms := 3
	lat, lng := -15.78, -47.93

	args := listingEnrichmentArgs(ListingEnrichment{
		ID:                     42,
		NormalizedNeighborhood: &neighborhood,
		BedroomCount:           &bedrooms,
		Amenities:              []string{"piscina", "elevador"},
		Lat:                    &lat,
		Lng:                    &lng,
	})

	// A ordem é o contrato com os placeholders de updateListingEnrichmentSQL.
	if got, want := len(args), 6; got != want {
		t.Fatalf("len(args) = %d, want %d", got, want)
	}
	if got, want := args[0], int64(42); got != want {
		t.Errorf("args[0] (id) = %v, want %v", got, want)
	}
	if got, want := args[1], &neighborhood; got != want {
		t.Errorf("args[1] (normalized_neighborhood) = %v, want %v", got, want)
	}
	if got, want := args[2], &bedrooms; got != want {
		t.Errorf("args[2] (bedroom_count) = %v, want %v", got, want)
	}
	if got, want := args[3], []string{"piscina", "elevador"}; !reflect.DeepEqual(got, want) {
		t.Errorf("args[3] (amenities) = %v, want %v", got, want)
	}
	if got, want := args[4], &lat; got != want {
		t.Errorf("args[4] (lat) = %v, want %v", got, want)
	}
	if got, want := args[5], &lng; got != want {
		t.Errorf("args[5] (lng) = %v, want %v", got, want)
	}
}

func TestListingEnrichmentArgsKeepsNilPointersAsNull(t *testing.T) {
	// nil precisa chegar ao banco como NULL: é a distinção que o struct existe
	// para preservar ("não reconhecido" != "reconhecido como vazio/zero").
	args := listingEnrichmentArgs(ListingEnrichment{ID: 7})

	for i, name := range map[int]string{1: "normalized_neighborhood", 2: "bedroom_count", 4: "lat", 5: "lng"} {
		if !reflect.ValueOf(args[i]).IsNil() {
			t.Errorf("args[%d] (%s) = %v, want nil", i, name, args[i])
		}
	}

	// amenities segue a regra oposta, a mesma de image_urls: nil vira array
	// vazio, nunca NULL.
	if got, want := args[3], []string{}; !reflect.DeepEqual(got, want) {
		t.Errorf("args[3] (amenities) = %v, want %v", got, want)
	}
}

func TestUpdateListingEnrichmentRejectsMissingID(t *testing.T) {
	// A guarda precisa retornar **antes** de tocar o pool — com pool nil,
	// qualquer ida ao banco seria um panic.
	if err := UpdateListingEnrichment(context.Background(), nil, ListingEnrichment{}); err == nil {
		t.Fatal("UpdateListingEnrichment() error = nil, want erro de id ausente")
	}
	if err := MarkListingEnriched(context.Background(), nil, 0); err == nil {
		t.Fatal("MarkListingEnriched() error = nil, want erro de id ausente")
	}
	if _, err := ListPendingListings(context.Background(), nil, 0, 0); err == nil {
		t.Fatal("ListPendingListings() com limit 0 error = nil, want erro")
	}
}

// Contrato em string, e não comportamento: os dois bugs mais caros desta fila
// são invisíveis em teste de unidade e só se manifestariam meses depois, como
// "o catálogo inteiro é reprocessado todo dia" ou "este anúncio nunca sai da
// fila". Um PostgreSQL real cobre o resto (ver o gotcha de testes deste pacote).
func TestEnrichmentSQLNeverTouchesUpdatedAt(t *testing.T) {
	// updated_at é metade do predicado da fila
	// (enriched_at IS NULL OR updated_at > enriched_at): setá-lo aqui deixaria
	// o anúncio pendente para sempre, um passe pago de IA por run.
	for name, sql := range map[string]string{
		"update enrichment": updateListingEnrichmentSQL,
		"mark enriched":     markListingEnrichedSQL,
	} {
		if strings.Contains(sql, "updated_at") {
			t.Errorf("%s menciona updated_at; o anúncio nunca sairia da fila:\n%s", name, sql)
		}
	}
}

func TestUpsertListingSQLBumpsUpdatedAtOnlyOnRealChanges(t *testing.T) {
	// Sem a guarda, todo anúncio visto em toda coleta voltaria à fila de
	// enriquecimento, mesmo sem nenhuma mudança.
	if !strings.Contains(upsertListingSQL, "IS DISTINCT FROM") {
		t.Fatalf("upsertListingSQL não compara os campos com IS DISTINCT FROM:\n%s", upsertListingSQL)
	}

	for _, column := range []string{
		"title_raw", "price_raw", "address_raw", "description_raw",
		"bedrooms_raw", "area_raw", "image_urls", "extra_data",
	} {
		guard := "listings." + column + " "
		if !strings.Contains(upsertListingSQL, guard) {
			t.Errorf("upsertListingSQL não compara %s com o valor gravado", column)
		}
	}

	// last_seen_at responde "o anúncio ainda está no site", não "mudou":
	// condicioná-lo faria DeleteStaleListings apagar tudo que não mudou.
	if !strings.Contains(upsertListingSQL, "last_seen_at    = NOW()") {
		t.Errorf("upsertListingSQL não renova last_seen_at incondicionalmente:\n%s", upsertListingSQL)
	}
}

// O índice da fila (migrations/006) é um índice **parcial** cujo predicado
// precisa ser idêntico ao WHERE de selectPendingListingsSQL: o PostgreSQL só usa
// um parcial quando consegue provar que o resultado da query satisfaz o
// predicado do índice, e com expressões iguais essa prova é trivial. Se um dos
// dois textos mudar sozinho, o índice para de ser usado **em silêncio** — a
// query continua correta e volta a varrer a tabela inteira, o pior tipo de
// regressão de performance. Este teste é a única coisa que segura isso sem um
// PostgreSQL na mão.
func TestEnrichmentQueueIndexPredicateMatchesTheQuery(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "migrations", "006_fix_enrichment_queue_index.sql"))
	if err != nil {
		t.Fatalf("leitura da migration: %v", err)
	}

	predicate := indexPredicateOf(t, string(raw))
	if predicate == "" {
		t.Fatal("não foi possível extrair o predicado do CREATE INDEX da migration 006")
	}

	// O predicado aparece entre parênteses na query, porque lá ele é combinado
	// com o cursor por AND.
	if want := "(" + predicate + ")"; !strings.Contains(collapseSpaces(selectPendingListingsSQL), want) {
		t.Errorf("o predicado do índice e o da query divergiram; o índice deixará de ser usado.\níndice: %s\nquery:  %s",
			predicate, collapseSpaces(selectPendingListingsSQL))
	}
}

// indexPredicateOf devolve o predicado do CREATE INDEX, já com espaços
// colapsados. As linhas de comentário são descartadas primeiro: elas citam o
// predicado antigo e casariam por engano.
func indexPredicateOf(t *testing.T, migration string) string {
	t.Helper()

	var statements []string
	for _, line := range strings.Split(migration, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		statements = append(statements, line)
	}

	sql := collapseSpaces(strings.Join(statements, " "))
	_, after, found := strings.Cut(sql, "CREATE INDEX")
	if !found {
		return ""
	}
	_, predicate, found := strings.Cut(after, " WHERE ")
	if !found {
		return ""
	}

	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(predicate), ";"))
}

// collapseSpaces reduz qualquer sequência de espaços em branco a um espaço só,
// para que a comparação não dependa da indentação de cada arquivo.
func collapseSpaces(sql string) string {
	return strings.Join(strings.Fields(sql), " ")
}
