package grouping

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/imobhub/api/internal/db"
)

// storedProperty é o canônico como ele vem do banco: com todos os campos que o
// merge **não** calcula preenchidos, para que qualquer apagão acidental apareça.
func storedProperty() db.Property {
	return db.Property{
		ID:                 "prop-1",
		CanonicalAddress:   ptr("Rua das Flores, 100"),
		Neighborhood:       ptr("Centro"),
		City:               ptr("São Paulo"),
		State:              ptr("SP"),
		Lat:                ptr(-23.55),
		Lng:                ptr(-46.63),
		BathroomCount:      ptr(2),
		ParkingSpots:       ptr(1),
		AreaSqm:            ptr(72.5),
		TransactionType:    ptr("venda"),
		PropertyType:       ptr("apartamento"),
		ActiveListingCount: 3,
		Amenities:          []string{"piscina"},
		Photos:             []string{"https://cdn/antiga.jpg"},
	}
}

func mergeStore(listings ...db.Listing) *fakeStore {
	property := storedProperty()
	return &fakeStore{property: &property, listings: listings}
}

func newMerger(t *testing.T, store *fakeStore) *PropertyGrouper {
	t.Helper()
	return newTestGrouper(t, store, &fakeMatcher{})
}

func TestMergePropertyDataRequiresPropertyID(t *testing.T) {
	for _, id := range []string{"", "   "} {
		store := mergeStore()
		grouper := newMerger(t, store)

		err := grouper.MergePropertyData(context.Background(), id)
		if !errors.Is(err, ErrMissingPropertyID) {
			t.Fatalf("MergePropertyData(%q) error = %v, want ErrMissingPropertyID", id, err)
		}
		if store.getCalls != 0 || store.listCalls != 0 || store.updateCalls != 0 {
			t.Errorf("store was touched: get=%d list=%d update=%d", store.getCalls, store.listCalls, store.updateCalls)
		}
	}
}

// Imóvel apagado (correção de deduplicação) é erro sentinela, não um update
// perdido em silêncio.
func TestMergePropertyDataReportsMissingProperty(t *testing.T) {
	store := &fakeStore{}
	grouper := newMerger(t, store)

	err := grouper.MergePropertyData(context.Background(), "prop-1")
	if !errors.Is(err, ErrPropertyNotFound) {
		t.Fatalf("error = %v, want ErrPropertyNotFound", err)
	}
	if store.updateCalls != 0 {
		t.Errorf("updateCalls = %d, want 0", store.updateCalls)
	}
}

// Imóvel sem anúncios é no-op: o vínculo pode ter acabado de ser desfeito, e
// apagar as fotos por causa disso seria perda de dado.
func TestMergePropertyDataIsANoOpWithoutListings(t *testing.T) {
	store := mergeStore()
	grouper := newMerger(t, store)

	if err := grouper.MergePropertyData(context.Background(), "prop-1"); err != nil {
		t.Fatalf("MergePropertyData() error = %v, want nil", err)
	}
	if store.updateCalls != 0 {
		t.Errorf("updateCalls = %d, want 0 for a property without listings", store.updateCalls)
	}
}

func TestMergePropertyDataTrimsThePropertyID(t *testing.T) {
	store := mergeStore(db.Listing{ID: 1, ImageURLs: []string{"https://cdn/a.jpg"}})
	grouper := newMerger(t, store)

	if err := grouper.MergePropertyData(context.Background(), "  prop-1  "); err != nil {
		t.Fatalf("MergePropertyData() error = %v, want nil", err)
	}
	if store.lastGetID != "prop-1" || store.lastListID != "prop-1" {
		t.Errorf("store received (%q, %q), want the trimmed id in both", store.lastGetID, store.lastListID)
	}
}

func TestMergePropertyDataWithASingleListing(t *testing.T) {
	store := mergeStore(db.Listing{
		ID:             7,
		DescriptionRaw: "  Apartamento reformado com vista.  ",
		BedroomCount:   ptr(3),
		Amenities:      []string{"piscina", "academia"},
		ImageURLs:      []string{"https://cdn/1.jpg", "https://cdn/2.jpg"},
	})
	grouper := newMerger(t, store)

	if err := grouper.MergePropertyData(context.Background(), "prop-1"); err != nil {
		t.Fatalf("MergePropertyData() error = %v, want nil", err)
	}
	if store.updateCalls != 1 {
		t.Fatalf("updateCalls = %d, want 1", store.updateCalls)
	}

	got := store.updatedProperty
	if want := []string{"https://cdn/1.jpg", "https://cdn/2.jpg"}; !reflect.DeepEqual(got.Photos, want) {
		t.Errorf("Photos = %v, want %v", got.Photos, want)
	}
	if want := []string{"piscina", "academia"}; !reflect.DeepEqual(got.Amenities, want) {
		t.Errorf("Amenities = %v, want %v", got.Amenities, want)
	}
	if got.Description == nil || *got.Description != "Apartamento reformado com vista." {
		t.Errorf("Description = %v, want the trimmed text", got.Description)
	}
	if got.BedroomCount == nil || *got.BedroomCount != 3 {
		t.Errorf("BedroomCount = %v, want 3", got.BedroomCount)
	}
}

// O merge reescreve apenas fotos, descrição, comodidades e quartos:
// db.UpdateProperty reescreve **todas** as colunas consolidadas, então perder
// qualquer um dos demais campos aqui apagaria o dado no banco.
func TestMergePropertyDataPreservesTheOtherFields(t *testing.T) {
	store := mergeStore(db.Listing{ID: 1, ImageURLs: []string{"https://cdn/1.jpg"}})
	grouper := newMerger(t, store)

	if err := grouper.MergePropertyData(context.Background(), "prop-1"); err != nil {
		t.Fatalf("MergePropertyData() error = %v, want nil", err)
	}

	original, got := storedProperty(), store.updatedProperty
	if got.ID != original.ID {
		t.Errorf("ID = %q, want %q", got.ID, original.ID)
	}
	for _, field := range []struct {
		name      string
		got, want *string
	}{
		{"CanonicalAddress", got.CanonicalAddress, original.CanonicalAddress},
		{"Neighborhood", got.Neighborhood, original.Neighborhood},
		{"City", got.City, original.City},
		{"State", got.State, original.State},
		{"TransactionType", got.TransactionType, original.TransactionType},
		{"PropertyType", got.PropertyType, original.PropertyType},
	} {
		if field.got == nil || *field.got != *field.want {
			t.Errorf("%s = %v, want %q", field.name, field.got, *field.want)
		}
	}
	if got.Lat == nil || *got.Lat != *original.Lat || got.Lng == nil || *got.Lng != *original.Lng {
		t.Errorf("coordinates = (%v, %v), want (%v, %v)", got.Lat, got.Lng, *original.Lat, *original.Lng)
	}
	if got.AreaSqm == nil || *got.AreaSqm != *original.AreaSqm {
		t.Errorf("AreaSqm = %v, want %v", got.AreaSqm, *original.AreaSqm)
	}
	if got.BathroomCount == nil || *got.BathroomCount != *original.BathroomCount {
		t.Errorf("BathroomCount = %v, want %v", got.BathroomCount, *original.BathroomCount)
	}
	if got.ParkingSpots == nil || *got.ParkingSpots != *original.ParkingSpots {
		t.Errorf("ParkingSpots = %v, want %v", got.ParkingSpots, *original.ParkingSpots)
	}
	if got.ActiveListingCount != original.ActiveListingCount {
		t.Errorf("ActiveListingCount = %d, want %d", got.ActiveListingCount, original.ActiveListingCount)
	}
}

// Três anúncios com fotos sobrepostas: dedup exato e ordem determinística
// (listings.id asc, ordem original dentro de cada array).
func TestMergePropertyDataDeduplicatesPhotosPreservingOrder(t *testing.T) {
	store := mergeStore(
		db.Listing{ID: 1, ImageURLs: []string{"https://cdn/a.jpg", "https://cdn/b.jpg"}},
		db.Listing{ID: 2, ImageURLs: []string{"https://cdn/b.jpg", "  ", "https://cdn/c.jpg"}},
		db.Listing{ID: 3, ImageURLs: []string{"https://cdn/c.jpg", "https://cdn/a.jpg", "https://cdn/d.jpg"}},
	)
	grouper := newMerger(t, store)

	if err := grouper.MergePropertyData(context.Background(), "prop-1"); err != nil {
		t.Fatalf("MergePropertyData() error = %v, want nil", err)
	}

	want := []string{"https://cdn/a.jpg", "https://cdn/b.jpg", "https://cdn/c.jpg", "https://cdn/d.jpg"}
	if got := store.updatedProperty.Photos; !reflect.DeepEqual(got, want) {
		t.Errorf("Photos = %v, want %v", got, want)
	}
}

// A dedup é por igualdade exata: query string e barra final distinguem recortes
// diferentes da mesma imagem num CDN e **não** são normalizadas.
func TestMergePropertyDataDoesNotNormalizePhotoURLs(t *testing.T) {
	store := mergeStore(db.Listing{ID: 1, ImageURLs: []string{
		"https://cdn/a.jpg",
		"https://cdn/a.jpg?w=800",
		"https://cdn/a.jpg/",
	}})
	grouper := newMerger(t, store)

	if err := grouper.MergePropertyData(context.Background(), "prop-1"); err != nil {
		t.Fatalf("MergePropertyData() error = %v, want nil", err)
	}
	if got := len(store.updatedProperty.Photos); got != 3 {
		t.Errorf("len(Photos) = %d, want 3 (no URL normalization)", got)
	}
}

// O corte em 50 precisa ser previsível: sobrevivem as primeiras fotos na ordem
// determinística, e nada além delas.
func TestMergePropertyDataCapsPhotosAtFifty(t *testing.T) {
	first := make([]string, 0, 40)
	for i := range 40 {
		first = append(first, fmt.Sprintf("https://cdn/first-%02d.jpg", i))
	}
	second := make([]string, 0, 30)
	for i := range 30 {
		second = append(second, fmt.Sprintf("https://cdn/second-%02d.jpg", i))
	}

	store := mergeStore(
		db.Listing{ID: 1, ImageURLs: first},
		db.Listing{ID: 2, ImageURLs: second},
	)
	grouper := newMerger(t, store)

	if err := grouper.MergePropertyData(context.Background(), "prop-1"); err != nil {
		t.Fatalf("MergePropertyData() error = %v, want nil", err)
	}

	photos := store.updatedProperty.Photos
	if len(photos) != maxMergedPhotos {
		t.Fatalf("len(Photos) = %d, want %d", len(photos), maxMergedPhotos)
	}
	want := append(append([]string{}, first...), second[:10]...)
	if !reflect.DeepEqual(photos, want) {
		t.Errorf("Photos = %v, want the first %d in deterministic order", photos, maxMergedPhotos)
	}
}

func TestMergePropertyDataPicksTheLongestDescription(t *testing.T) {
	store := mergeStore(
		db.Listing{ID: 1, DescriptionRaw: "Curto."},
		db.Listing{ID: 2, DescriptionRaw: "Apartamento reformado, com piscina e academia no condomínio."},
		db.Listing{ID: 3, DescriptionRaw: "Médio, mas não o maior."},
	)
	grouper := newMerger(t, store)

	if err := grouper.MergePropertyData(context.Background(), "prop-1"); err != nil {
		t.Fatalf("MergePropertyData() error = %v, want nil", err)
	}

	want := "Apartamento reformado, com piscina e academia no condomínio."
	if got := store.updatedProperty.Description; got == nil || *got != want {
		t.Errorf("Description = %v, want %q", got, want)
	}
}

// A contagem é em runes: em bytes, o texto acentuado (mais curto) venceria.
func TestMergePropertyDataComparesDescriptionsInRunes(t *testing.T) {
	accented := "Área às três avós órfãs"    // 23 runes, 29 bytes
	plain := "Casa com quintal e garagem ok" // 29 runes, 29 bytes

	store := mergeStore(
		db.Listing{ID: 1, DescriptionRaw: accented},
		db.Listing{ID: 2, DescriptionRaw: plain},
	)
	grouper := newMerger(t, store)

	if err := grouper.MergePropertyData(context.Background(), "prop-1"); err != nil {
		t.Fatalf("MergePropertyData() error = %v, want nil", err)
	}
	if got := store.updatedProperty.Description; got == nil || *got != plain {
		t.Errorf("Description = %v, want %q (rune count, not bytes)", got, plain)
	}
}

// Empate de tamanho fica com o anúncio de menor id — sem isso a escolha mudaria
// entre execuções.
func TestMergePropertyDataBreaksDescriptionTiesByListingID(t *testing.T) {
	store := mergeStore(
		db.Listing{ID: 1, DescriptionRaw: "Texto A do portal um"},
		db.Listing{ID: 2, DescriptionRaw: "Texto B do portal um"},
	)
	grouper := newMerger(t, store)

	if err := grouper.MergePropertyData(context.Background(), "prop-1"); err != nil {
		t.Fatalf("MergePropertyData() error = %v, want nil", err)
	}
	if got := store.updatedProperty.Description; got == nil || *got != "Texto A do portal um" {
		t.Errorf("Description = %v, want the text of the lowest listing id", got)
	}
}

// Nenhum anúncio com texto **não** apaga a descrição já consolidada: um texto
// antigo é melhor do que nenhum.
func TestMergePropertyDataKeepsTheExistingDescriptionWhenListingsHaveNone(t *testing.T) {
	store := mergeStore(
		db.Listing{ID: 1, DescriptionRaw: "   "},
		db.Listing{ID: 2},
	)
	store.property.Description = ptr("Descrição consolidada anteriormente.")
	grouper := newMerger(t, store)

	if err := grouper.MergePropertyData(context.Background(), "prop-1"); err != nil {
		t.Fatalf("MergePropertyData() error = %v, want nil", err)
	}

	got := store.updatedProperty.Description
	if got == nil || *got != "Descrição consolidada anteriormente." {
		t.Errorf("Description = %v, want the previously consolidated text", got)
	}
}

func TestMergePropertyDataConsolidatesBedroomCount(t *testing.T) {
	tests := []struct {
		name     string
		listings []db.Listing
		want     *int
	}{
		{
			name: "maioria vence",
			listings: []db.Listing{
				{ID: 1, BedroomCount: ptr(2)},
				{ID: 2, BedroomCount: ptr(3)},
				{ID: 3, BedroomCount: ptr(3)},
			},
			want: ptr(3),
		},
		{
			name: "empate fica com o menor id",
			listings: []db.Listing{
				{ID: 1, BedroomCount: ptr(2)},
				{ID: 2, BedroomCount: ptr(4)},
			},
			want: ptr(2),
		},
		{
			name: "anúncios sem o dado são ignorados",
			listings: []db.Listing{
				{ID: 1},
				{ID: 2, BedroomCount: ptr(1)},
			},
			want: ptr(1),
		},
		{
			name:     "ninguém informa: nil, nunca 0",
			listings: []db.Listing{{ID: 1}, {ID: 2}},
			want:     nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := mergeStore(tt.listings...)
			store.property.BedroomCount = ptr(9)
			grouper := newMerger(t, store)

			if err := grouper.MergePropertyData(context.Background(), "prop-1"); err != nil {
				t.Fatalf("MergePropertyData() error = %v, want nil", err)
			}

			got := store.updatedProperty.BedroomCount
			switch {
			case tt.want == nil && got != nil:
				t.Errorf("BedroomCount = %d, want nil", *got)
			case tt.want != nil && (got == nil || *got != *tt.want):
				t.Errorf("BedroomCount = %v, want %d", got, *tt.want)
			}
		})
	}
}

// O ponteiro devolvido não pode ser o do anúncio lido: o chamador reaproveita a
// slice depois da chamada.
func TestMergePropertyDataCopiesTheBedroomCountPointer(t *testing.T) {
	listing := db.Listing{ID: 1, BedroomCount: ptr(2)}
	store := mergeStore(listing)
	grouper := newMerger(t, store)

	if err := grouper.MergePropertyData(context.Background(), "prop-1"); err != nil {
		t.Fatalf("MergePropertyData() error = %v, want nil", err)
	}
	if store.updatedProperty.BedroomCount == store.listings[0].BedroomCount {
		t.Error("the property aliases the listing's bedroom count pointer")
	}
}

// Idempotência: os mesmos anúncios produzem exatamente o mesmo resultado, na
// mesma ordem. É o que permite à fila reprocessar um imóvel sem consequência.
func TestMergePropertyDataIsIdempotent(t *testing.T) {
	store := mergeStore(
		db.Listing{ID: 1, DescriptionRaw: "Primeiro texto", Amenities: []string{"piscina"}, ImageURLs: []string{"https://cdn/a.jpg", "https://cdn/b.jpg"}},
		db.Listing{ID: 2, DescriptionRaw: "Segundo texto, um pouco maior", Amenities: []string{"piscina", "churrasqueira"}, ImageURLs: []string{"https://cdn/b.jpg", "https://cdn/c.jpg"}},
	)
	grouper := newMerger(t, store)

	if err := grouper.MergePropertyData(context.Background(), "prop-1"); err != nil {
		t.Fatalf("first MergePropertyData() error = %v, want nil", err)
	}
	first := store.updatedProperty

	// O banco agora guarda o resultado do primeiro merge.
	*store.property = first

	if err := grouper.MergePropertyData(context.Background(), "prop-1"); err != nil {
		t.Fatalf("second MergePropertyData() error = %v, want nil", err)
	}
	second := store.updatedProperty

	if !reflect.DeepEqual(first.Photos, second.Photos) {
		t.Errorf("Photos changed between runs: %v then %v", first.Photos, second.Photos)
	}
	if !reflect.DeepEqual(first.Amenities, second.Amenities) {
		t.Errorf("Amenities changed between runs: %v then %v", first.Amenities, second.Amenities)
	}
	if !reflect.DeepEqual(first.Description, second.Description) {
		t.Errorf("Description changed between runs: %v then %v", first.Description, second.Description)
	}
	if !reflect.DeepEqual(first.BedroomCount, second.BedroomCount) {
		t.Errorf("BedroomCount changed between runs: %v then %v", first.BedroomCount, second.BedroomCount)
	}
}

func TestMergePropertyDataPropagatesStoreErrors(t *testing.T) {
	getErr := errors.New("db: connection refused")
	listErr := errors.New("db: query cancelled")
	updateErr := errors.New("db: property not found")

	t.Run("get", func(t *testing.T) {
		store := mergeStore()
		store.getErr = getErr
		grouper := newMerger(t, store)

		err := grouper.MergePropertyData(context.Background(), "prop-1")
		if !errors.Is(err, getErr) {
			t.Fatalf("error = %v, want it to wrap the get error", err)
		}
		if !strings.Contains(err.Error(), "grouping:") {
			t.Errorf("error = %q, want it to name the package", err)
		}
		if store.updateCalls != 0 {
			t.Errorf("updateCalls = %d, want 0", store.updateCalls)
		}
	})

	t.Run("list", func(t *testing.T) {
		store := mergeStore()
		store.listErr = listErr
		grouper := newMerger(t, store)

		err := grouper.MergePropertyData(context.Background(), "prop-1")
		if !errors.Is(err, listErr) {
			t.Fatalf("error = %v, want it to wrap the list error", err)
		}
		if store.updateCalls != 0 {
			t.Errorf("updateCalls = %d, want 0", store.updateCalls)
		}
	})

	t.Run("update", func(t *testing.T) {
		store := mergeStore(db.Listing{ID: 1, ImageURLs: []string{"https://cdn/a.jpg"}})
		store.updateErr = updateErr
		grouper := newMerger(t, store)

		err := grouper.MergePropertyData(context.Background(), "prop-1")
		if !errors.Is(err, updateErr) {
			t.Fatalf("error = %v, want it to wrap the update error", err)
		}
		if !strings.Contains(err.Error(), "prop-1") {
			t.Errorf("error = %q, want it to name the property", err)
		}
	})
}

// Comodidades vazias/em branco não entram, e a união segue a mesma ordem
// determinística das fotos.
func TestMergeAmenitiesUnionIsDeterministic(t *testing.T) {
	got := mergeAmenities([]db.Listing{
		{ID: 1, Amenities: []string{"piscina", "  ", "academia"}},
		{ID: 2, Amenities: []string{"academia", "churrasqueira"}},
		{ID: 3},
	})

	want := []string{"piscina", "academia", "churrasqueira"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mergeAmenities() = %v, want %v", got, want)
	}
}

// Fotos e comodidades nunca viram nil: as colunas TEXT[] não guardam NULL.
func TestMergedArraysAreNeverNil(t *testing.T) {
	listings := []db.Listing{{ID: 1}}

	if got := mergePhotos(listings); got == nil {
		t.Error("mergePhotos() = nil, want empty slice")
	}
	if got := mergeAmenities(listings); got == nil {
		t.Error("mergeAmenities() = nil, want empty slice")
	}
}
