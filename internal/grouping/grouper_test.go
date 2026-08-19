package grouping

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/imobhub/api/internal/ai"
	"github.com/imobhub/api/internal/db"
)

// fakeStore substitui o acesso a properties. Os contadores existem para provar
// as invariantes de custo do fluxo: um caminho que deveria sair cedo não pode
// ter tocado no banco.
type fakeStore struct {
	candidates []db.Property
	findErr    error
	createErr  error
	linkErr    error

	findCalls   int
	createCalls int
	linkCalls   int

	createdProperty  db.Property
	linkedPropertyID string
	linkedListingID  int64

	// findArgs guarda os parâmetros da última busca por raio.
	lastLat, lastLng float64
	lastRadius       int
}

func (s *fakeStore) FindPropertiesByCoordinates(_ context.Context, lat, lng float64, radiusMeters int) ([]db.Property, error) {
	s.findCalls++
	s.lastLat, s.lastLng, s.lastRadius = lat, lng, radiusMeters
	if s.findErr != nil {
		return nil, s.findErr
	}
	return s.candidates, nil
}

func (s *fakeStore) CreateProperty(_ context.Context, property db.Property) (db.Property, error) {
	s.createCalls++
	s.createdProperty = property
	if s.createErr != nil {
		return db.Property{}, s.createErr
	}
	created := property
	created.ID = "created-property-id"
	return created, nil
}

func (s *fakeStore) LinkListingToProperty(_ context.Context, propertyID string, listingID int64) error {
	s.linkCalls++
	s.linkedPropertyID, s.linkedListingID = propertyID, listingID
	return s.linkErr
}

// fakeMatcher substitui a chamada paga à Anthropic.
type fakeMatcher struct {
	match ai.PropertyMatch
	err   error

	calls          int
	lastListing    ai.MatchListing
	lastCandidates []db.Property
}

func (m *fakeMatcher) MatchProperty(_ context.Context, listing ai.MatchListing, candidates []db.Property) (ai.PropertyMatch, error) {
	m.calls++
	m.lastListing = listing
	m.lastCandidates = candidates
	return m.match, m.err
}

func testConfig() Config {
	return Config{ConfidenceThreshold: 0.85, RadiusMeters: 100, MaxCandidates: 5}
}

func newTestGrouper(t *testing.T, store *fakeStore, matcher *fakeMatcher) *PropertyGrouper {
	t.Helper()
	grouper, err := NewPropertyGrouper(store, matcher, testConfig())
	if err != nil {
		t.Fatalf("NewPropertyGrouper() error = %v, want nil", err)
	}
	return grouper
}

func ptr[T any](value T) *T { return &value }

func geocodedListing() Listing {
	return Listing{
		ID:                     42,
		SourceDomain:           "exemplo.com.br",
		ListingURL:             "https://exemplo.com.br/imoveis/1",
		AddressRaw:             "Rua das Flores, 100",
		NormalizedNeighborhood: "Centro",
		Lat:                    ptr(-23.55),
		Lng:                    ptr(-46.63),
		BedroomCount:           ptr(2),
		AreaRaw:                "72 m²",
		Amenities:              []string{"piscina"},
		ImageURLs:              []string{"https://exemplo.com.br/foto1.jpg"},
		TitleRaw:               "Apartamento 2 quartos no Centro",
		DescriptionRaw:         "Apartamento reformado com piscina no condomínio.",
	}
}

func candidate(id string) db.Property {
	return db.Property{
		ID:               id,
		CanonicalAddress: ptr("Rua das Flores, 100"),
		Neighborhood:     ptr("Centro"),
		Lat:              ptr(-23.55),
		Lng:              ptr(-46.63),
	}
}

// Anúncio já vinculado é o caminho mais barato de todos: nem banco, nem IA.
func TestGroupListingReturnsExistingLinkWithoutAnyCall(t *testing.T) {
	store := &fakeStore{}
	matcher := &fakeMatcher{}
	grouper := newTestGrouper(t, store, matcher)

	listing := geocodedListing()
	listing.PropertyID = ptr("  already-linked  ")

	propertyID, isNew, err := grouper.GroupListing(context.Background(), listing)
	if err != nil {
		t.Fatalf("GroupListing() error = %v, want nil", err)
	}
	if propertyID != "already-linked" {
		t.Errorf("propertyID = %q, want %q", propertyID, "already-linked")
	}
	if isNew {
		t.Error("isNew = true, want false for an already linked listing")
	}
	if matcher.calls != 0 {
		t.Errorf("matcher calls = %d, want 0", matcher.calls)
	}
	if store.findCalls != 0 || store.createCalls != 0 || store.linkCalls != 0 {
		t.Errorf("store was touched: find=%d create=%d link=%d", store.findCalls, store.createCalls, store.linkCalls)
	}
}

// Um property_id vazio dentro do ponteiro não conta como vínculo — o anúncio
// segue o fluxo normal.
func TestGroupListingIgnoresBlankExistingLink(t *testing.T) {
	store := &fakeStore{}
	matcher := &fakeMatcher{}
	grouper := newTestGrouper(t, store, matcher)

	listing := geocodedListing()
	listing.PropertyID = ptr("   ")

	_, isNew, err := grouper.GroupListing(context.Background(), listing)
	if err != nil {
		t.Fatalf("GroupListing() error = %v, want nil", err)
	}
	if !isNew {
		t.Error("isNew = false, want true")
	}
}

// Sem coordenadas o anúncio não é agrupado e **não** vira imóvel: um canônico
// sem geo nunca mais aparece numa busca por raio.
func TestGroupListingRequiresCoordinates(t *testing.T) {
	for _, tt := range []struct {
		name     string
		lat, lng *float64
	}{
		{name: "sem lat e sem lng"},
		{name: "só lat", lat: ptr(-23.55)},
		{name: "só lng", lng: ptr(-46.63)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeStore{}
			matcher := &fakeMatcher{}
			grouper := newTestGrouper(t, store, matcher)

			listing := geocodedListing()
			listing.Lat, listing.Lng = tt.lat, tt.lng

			_, _, err := grouper.GroupListing(context.Background(), listing)
			if !errors.Is(err, ErrListingNotGeocoded) {
				t.Fatalf("error = %v, want ErrListingNotGeocoded", err)
			}
			if store.createCalls != 0 {
				t.Errorf("createCalls = %d, want 0", store.createCalls)
			}
			if matcher.calls != 0 {
				t.Errorf("matcher calls = %d, want 0", matcher.calls)
			}
		})
	}
}

func TestGroupListingRequiresListingID(t *testing.T) {
	grouper := newTestGrouper(t, &fakeStore{}, &fakeMatcher{})

	listing := geocodedListing()
	listing.ID = 0

	if _, _, err := grouper.GroupListing(context.Background(), listing); !errors.Is(err, ErrMissingListingID) {
		t.Fatalf("error = %v, want ErrMissingListingID", err)
	}
}

// Sem candidatos por perto não há nada a comparar: criar direto é o que impede
// que a coleta pague uma chamada por anúncio numa cidade nova.
func TestGroupListingCreatesWithoutCandidatesAndWithoutAI(t *testing.T) {
	store := &fakeStore{}
	matcher := &fakeMatcher{}
	grouper := newTestGrouper(t, store, matcher)

	propertyID, isNew, err := grouper.GroupListing(context.Background(), geocodedListing())
	if err != nil {
		t.Fatalf("GroupListing() error = %v, want nil", err)
	}
	if propertyID != "created-property-id" || !isNew {
		t.Errorf("got (%q, %v), want (created-property-id, true)", propertyID, isNew)
	}
	if matcher.calls != 0 {
		t.Errorf("matcher calls = %d, want 0", matcher.calls)
	}
	if store.createCalls != 1 || store.linkCalls != 1 {
		t.Errorf("create=%d link=%d, want 1 and 1", store.createCalls, store.linkCalls)
	}
	if store.linkedPropertyID != "created-property-id" || store.linkedListingID != 42 {
		t.Errorf("linked (%q, %d), want (created-property-id, 42)", store.linkedPropertyID, store.linkedListingID)
	}
	if store.lastLat != -23.55 || store.lastLng != -46.63 || store.lastRadius != 100 {
		t.Errorf("search args = (%v, %v, %d), want (-23.55, -46.63, 100)", store.lastLat, store.lastLng, store.lastRadius)
	}
}

func TestGroupListingLinksToMatchAboveThreshold(t *testing.T) {
	store := &fakeStore{candidates: []db.Property{candidate("prop-a"), candidate("prop-b")}}
	matcher := &fakeMatcher{match: ai.PropertyMatch{SameProperty: true, PropertyID: "prop-b", Confidence: 0.9}}
	grouper := newTestGrouper(t, store, matcher)

	propertyID, isNew, err := grouper.GroupListing(context.Background(), geocodedListing())
	if err != nil {
		t.Fatalf("GroupListing() error = %v, want nil", err)
	}
	if propertyID != "prop-b" {
		t.Errorf("propertyID = %q, want prop-b", propertyID)
	}
	if isNew {
		t.Error("isNew = true, want false")
	}
	if matcher.calls != 1 {
		t.Errorf("matcher calls = %d, want exactly 1 (the API is paid per listing)", matcher.calls)
	}
	if store.createCalls != 0 {
		t.Errorf("createCalls = %d, want 0", store.createCalls)
	}
	if store.linkedPropertyID != "prop-b" || store.linkedListingID != 42 {
		t.Errorf("linked (%q, %d), want (prop-b, 42)", store.linkedPropertyID, store.linkedListingID)
	}
}

// A confiança exatamente no threshold é aceita: o critério é >=.
func TestGroupListingAcceptsConfidenceExactlyAtThreshold(t *testing.T) {
	store := &fakeStore{candidates: []db.Property{candidate("prop-a")}}
	matcher := &fakeMatcher{match: ai.PropertyMatch{SameProperty: true, PropertyID: "prop-a", Confidence: 0.85}}
	grouper := newTestGrouper(t, store, matcher)

	propertyID, isNew, err := grouper.GroupListing(context.Background(), geocodedListing())
	if err != nil {
		t.Fatalf("GroupListing() error = %v, want nil", err)
	}
	if propertyID != "prop-a" || isNew {
		t.Errorf("got (%q, %v), want (prop-a, false)", propertyID, isNew)
	}
}

// O modelo pode ecoar o UUID em outra caixa; o id gravado deve ser o do banco.
func TestGroupListingMatchesCandidateIDCaseInsensitively(t *testing.T) {
	store := &fakeStore{candidates: []db.Property{candidate("PROP-A")}}
	matcher := &fakeMatcher{match: ai.PropertyMatch{SameProperty: true, PropertyID: "  prop-a  ", Confidence: 0.99}}
	grouper := newTestGrouper(t, store, matcher)

	propertyID, isNew, err := grouper.GroupListing(context.Background(), geocodedListing())
	if err != nil {
		t.Fatalf("GroupListing() error = %v, want nil", err)
	}
	if propertyID != "PROP-A" || isNew {
		t.Errorf("got (%q, %v), want (PROP-A, false)", propertyID, isNew)
	}
	if store.linkedPropertyID != "PROP-A" {
		t.Errorf("linked property id = %q, want the id as stored in the database", store.linkedPropertyID)
	}
}

func TestGroupListingCreatesOnLowConfidence(t *testing.T) {
	store := &fakeStore{candidates: []db.Property{candidate("prop-a")}}
	matcher := &fakeMatcher{match: ai.PropertyMatch{SameProperty: true, PropertyID: "prop-a", Confidence: 0.84}}
	grouper := newTestGrouper(t, store, matcher)

	propertyID, isNew, err := grouper.GroupListing(context.Background(), geocodedListing())
	if err != nil {
		t.Fatalf("GroupListing() error = %v, want nil", err)
	}
	if propertyID != "created-property-id" || !isNew {
		t.Errorf("got (%q, %v), want (created-property-id, true)", propertyID, isNew)
	}
	if store.linkedPropertyID != "created-property-id" {
		t.Errorf("linked property id = %q, want the new property", store.linkedPropertyID)
	}
}

func TestGroupListingCreatesWhenModelSaysNoMatch(t *testing.T) {
	store := &fakeStore{candidates: []db.Property{candidate("prop-a")}}
	matcher := &fakeMatcher{match: ai.PropertyMatch{SameProperty: false, Confidence: 0.97}}
	grouper := newTestGrouper(t, store, matcher)

	propertyID, isNew, err := grouper.GroupListing(context.Background(), geocodedListing())
	if err != nil {
		t.Fatalf("GroupListing() error = %v, want nil", err)
	}
	if propertyID != "created-property-id" || !isNew {
		t.Errorf("got (%q, %v), want (created-property-id, true)", propertyID, isNew)
	}
}

// Um id que não está entre os candidatos derrubaria na FK; é tratado como
// "sem match" e nunca chega ao banco.
func TestGroupListingIgnoresPropertyIDOutsideTheCandidateList(t *testing.T) {
	store := &fakeStore{candidates: []db.Property{candidate("prop-a")}}
	matcher := &fakeMatcher{match: ai.PropertyMatch{SameProperty: true, PropertyID: "prop-fantasma", Confidence: 0.99}}
	grouper := newTestGrouper(t, store, matcher)

	propertyID, isNew, err := grouper.GroupListing(context.Background(), geocodedListing())
	if err != nil {
		t.Fatalf("GroupListing() error = %v, want nil", err)
	}
	if propertyID != "created-property-id" || !isNew {
		t.Errorf("got (%q, %v), want (created-property-id, true)", propertyID, isNew)
	}
	if store.linkedPropertyID == "prop-fantasma" {
		t.Error("the hallucinated property id reached the database")
	}
}

// Um property_id vazio com same_property=true também é "sem match".
func TestGroupListingIgnoresEmptyPropertyID(t *testing.T) {
	store := &fakeStore{candidates: []db.Property{candidate("prop-a")}}
	matcher := &fakeMatcher{match: ai.PropertyMatch{SameProperty: true, PropertyID: "  ", Confidence: 0.99}}
	grouper := newTestGrouper(t, store, matcher)

	propertyID, isNew, err := grouper.GroupListing(context.Background(), geocodedListing())
	if err != nil {
		t.Fatalf("GroupListing() error = %v, want nil", err)
	}
	if propertyID != "created-property-id" || !isNew {
		t.Errorf("got (%q, %v), want (created-property-id, true)", propertyID, isNew)
	}
}

// Erro da IA não pode criar imóvel: o anúncio continua pendente para a fila
// tentar de novo depois.
func TestGroupListingPropagatesMatcherErrorWithoutCreating(t *testing.T) {
	matcherErr := errors.New("anthropic: 503")
	store := &fakeStore{candidates: []db.Property{candidate("prop-a")}}
	matcher := &fakeMatcher{err: matcherErr}
	grouper := newTestGrouper(t, store, matcher)

	_, _, err := grouper.GroupListing(context.Background(), geocodedListing())
	if !errors.Is(err, matcherErr) {
		t.Fatalf("error = %v, want it to wrap the matcher error", err)
	}
	if store.createCalls != 0 || store.linkCalls != 0 {
		t.Errorf("create=%d link=%d, want 0 and 0", store.createCalls, store.linkCalls)
	}
}

func TestGroupListingPropagatesSearchError(t *testing.T) {
	findErr := errors.New("db: connection refused")
	store := &fakeStore{findErr: findErr}
	matcher := &fakeMatcher{}
	grouper := newTestGrouper(t, store, matcher)

	_, _, err := grouper.GroupListing(context.Background(), geocodedListing())
	if !errors.Is(err, findErr) {
		t.Fatalf("error = %v, want it to wrap the search error", err)
	}
	if matcher.calls != 0 || store.createCalls != 0 {
		t.Errorf("matcher=%d create=%d, want 0 and 0", matcher.calls, store.createCalls)
	}
}

// Se o Link falhar depois do Create, a property fica órfã — o erro precisa
// carregar o id para que ela possa ser removida.
func TestGroupListingReportsTheOrphanPropertyIDWhenLinkFails(t *testing.T) {
	linkErr := errors.New("db: listing not found")
	store := &fakeStore{linkErr: linkErr}
	grouper := newTestGrouper(t, store, &fakeMatcher{})

	_, _, err := grouper.GroupListing(context.Background(), geocodedListing())
	if !errors.Is(err, linkErr) {
		t.Fatalf("error = %v, want it to wrap the link error", err)
	}
	if got := err.Error(); !strings.Contains(got, "created-property-id") {
		t.Errorf("error %q does not name the orphan property", got)
	}
}

// A lista vem ordenada do mais próximo ao mais distante; o corte descarta os
// piores candidatos e é o principal controle de custo do prompt.
func TestGroupListingTruncatesCandidatesPreservingOrder(t *testing.T) {
	store := &fakeStore{candidates: []db.Property{
		candidate("prop-1"), candidate("prop-2"), candidate("prop-3"),
		candidate("prop-4"), candidate("prop-5"), candidate("prop-6"),
	}}
	matcher := &fakeMatcher{match: ai.PropertyMatch{SameProperty: false}}
	grouper := newTestGrouper(t, store, matcher)

	if _, _, err := grouper.GroupListing(context.Background(), geocodedListing()); err != nil {
		t.Fatalf("GroupListing() error = %v, want nil", err)
	}

	if len(matcher.lastCandidates) != 5 {
		t.Fatalf("candidates sent = %d, want 5 (MaxCandidates)", len(matcher.lastCandidates))
	}
	for i, want := range []string{"prop-1", "prop-2", "prop-3", "prop-4", "prop-5"} {
		if matcher.lastCandidates[i].ID != want {
			t.Errorf("candidate[%d] = %q, want %q", i, matcher.lastCandidates[i].ID, want)
		}
	}
}

func TestGroupListingSendsTheListingFieldsToTheMatcher(t *testing.T) {
	store := &fakeStore{candidates: []db.Property{candidate("prop-a")}}
	matcher := &fakeMatcher{match: ai.PropertyMatch{SameProperty: false}}
	grouper := newTestGrouper(t, store, matcher)

	listing := geocodedListing()
	if _, _, err := grouper.GroupListing(context.Background(), listing); err != nil {
		t.Fatalf("GroupListing() error = %v, want nil", err)
	}

	want := ai.MatchListing{
		AddressRaw:     listing.AddressRaw,
		Neighborhood:   listing.NormalizedNeighborhood,
		AreaRaw:        listing.AreaRaw,
		TitleRaw:       listing.TitleRaw,
		DescriptionRaw: listing.DescriptionRaw,
		BedroomCount:   listing.BedroomCount,
		ImageURLs:      listing.ImageURLs,
	}
	if !reflect.DeepEqual(matcher.lastListing, want) {
		t.Errorf("match listing = %+v, want %+v", matcher.lastListing, want)
	}
}

func TestPropertyFromMapsTheListing(t *testing.T) {
	listing := geocodedListing()

	property := propertyFrom(listing)

	if property.CanonicalAddress == nil || *property.CanonicalAddress != listing.AddressRaw {
		t.Errorf("CanonicalAddress = %v, want %q", property.CanonicalAddress, listing.AddressRaw)
	}
	if property.Neighborhood == nil || *property.Neighborhood != listing.NormalizedNeighborhood {
		t.Errorf("Neighborhood = %v, want %q", property.Neighborhood, listing.NormalizedNeighborhood)
	}
	if property.Lat == nil || *property.Lat != *listing.Lat {
		t.Errorf("Lat = %v, want %v", property.Lat, *listing.Lat)
	}
	if property.Lng == nil || *property.Lng != *listing.Lng {
		t.Errorf("Lng = %v, want %v", property.Lng, *listing.Lng)
	}
	if property.BedroomCount == nil || *property.BedroomCount != 2 {
		t.Errorf("BedroomCount = %v, want 2", property.BedroomCount)
	}
	if !reflect.DeepEqual(property.Amenities, listing.Amenities) {
		t.Errorf("Amenities = %v, want %v", property.Amenities, listing.Amenities)
	}
	if !reflect.DeepEqual(property.Photos, listing.ImageURLs) {
		t.Errorf("Photos = %v, want %v", property.Photos, listing.ImageURLs)
	}
	if property.Description == nil || *property.Description != listing.DescriptionRaw {
		t.Errorf("Description = %v, want %q", property.Description, listing.DescriptionRaw)
	}

	// listings não carrega esses dados; chutá-los contaminaria a comparação de
	// todos os anúncios seguintes.
	if property.AreaSqm != nil || property.City != nil || property.State != nil ||
		property.TransactionType != nil || property.PropertyType != nil {
		t.Errorf("property invented data not present in listings: %+v", property)
	}
}

// Campo ausente vira nil, nunca "" nem 0: a ausência é informação em
// db.Property.
func TestPropertyFromLeavesAbsentFieldsNil(t *testing.T) {
	property := propertyFrom(Listing{ID: 1, AddressRaw: "   ", Lat: ptr(-23.0), Lng: ptr(-46.0)})

	if property.CanonicalAddress != nil {
		t.Errorf("CanonicalAddress = %v, want nil for a blank address", *property.CanonicalAddress)
	}
	if property.Neighborhood != nil {
		t.Errorf("Neighborhood = %v, want nil", *property.Neighborhood)
	}
	if property.Description != nil {
		t.Errorf("Description = %v, want nil", *property.Description)
	}
	if property.BedroomCount != nil {
		t.Errorf("BedroomCount = %v, want nil (0 would mean 'zero bedrooms')", *property.BedroomCount)
	}
}

// A property criada não pode compartilhar ponteiros com o Listing: a fila
// reaproveita o struct depois da chamada.
func TestPropertyFromCopiesCoordinatePointers(t *testing.T) {
	listing := geocodedListing()
	property := propertyFrom(listing)

	if property.Lat == listing.Lat || property.Lng == listing.Lng {
		t.Error("the property aliases the caller's coordinate pointers")
	}
	if property.BedroomCount == listing.BedroomCount {
		t.Error("the property aliases the caller's bedroom count pointer")
	}
}

func TestNewPropertyGrouperValidatesDependencies(t *testing.T) {
	store := &fakeStore{}
	matcher := &fakeMatcher{}

	tests := []struct {
		name    string
		store   PropertyStore
		matcher PropertyMatcher
		cfg     Config
		wantErr error
	}{
		{name: "sem store", matcher: matcher, cfg: testConfig(), wantErr: ErrMissingStore},
		{name: "sem matcher", store: store, cfg: testConfig(), wantErr: ErrMissingMatcher},
		{
			name: "threshold negativo", store: store, matcher: matcher,
			cfg:     Config{ConfidenceThreshold: -0.1, RadiusMeters: 100, MaxCandidates: 5},
			wantErr: ErrInvalidThreshold,
		},
		{
			name: "threshold acima de 1", store: store, matcher: matcher,
			cfg:     Config{ConfidenceThreshold: 1.5, RadiusMeters: 100, MaxCandidates: 5},
			wantErr: ErrInvalidThreshold,
		},
		{
			name: "raio zero", store: store, matcher: matcher,
			cfg:     Config{ConfidenceThreshold: 0.85, RadiusMeters: 0, MaxCandidates: 5},
			wantErr: ErrInvalidRadius,
		},
		{
			name: "candidatos zero", store: store, matcher: matcher,
			cfg:     Config{ConfidenceThreshold: 0.85, RadiusMeters: 100, MaxCandidates: 0},
			wantErr: ErrInvalidMaxCandidates,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewPropertyGrouper(tt.store, tt.matcher, tt.cfg)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestNewPoolStoreRequiresPool(t *testing.T) {
	if _, err := NewPoolStore(nil); !errors.Is(err, ErrMissingPool) {
		t.Fatalf("error = %v, want ErrMissingPool", err)
	}
}
