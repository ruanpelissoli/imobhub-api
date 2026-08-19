package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/imobhub/api/internal/db"
)

func ptr[T any](value T) *T { return &value }

func marshalDetail(t *testing.T, detail db.PropertyDetail) map[string]any {
	t.Helper()

	raw, err := json.Marshal(newPropertyDetailResponse(detail))
	if err != nil {
		t.Fatalf("marshal do DTO falhou: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("JSON inválido (%q): %v", raw, err)
	}
	return payload
}

func TestPropertyDetailResponseFieldNames(t *testing.T) {
	createdAt := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	lastSeenAt := time.Date(2026, 8, 19, 9, 0, 0, 0, time.UTC)

	payload := marshalDetail(t, db.PropertyDetail{
		Property: db.Property{
			ID:               "6f1a0d1e-0000-4000-8000-000000000001",
			CanonicalAddress: ptr("Rua A, 100"),
			Neighborhood:     ptr("Batel"),
			City:             ptr("Curitiba"),
			State:            ptr("PR"),
			Lat:              ptr(-25.4),
			Lng:              ptr(-49.2),
			BedroomCount:     ptr(3),
			BathroomCount:    ptr(2),
			ParkingSpots:     ptr(1),
			AreaSqm:          ptr(72.5),
			Amenities:        []string{"piscina"},
			Description:      ptr("Perto do centro"),
			Photos:           []string{"https://exemplo.com/1.jpg"},
			TransactionType:  ptr("venda"),
			PropertyType:     ptr("apartamento"),
			// Denormalizado e propositalmente ausente da resposta.
			ActiveListingCount: 9,
			CreatedAt:          createdAt,
		},
		Listings: []db.PropertyListing{{
			ID:           12,
			SourceDomain: "www.exemplo.com.br",
			ListingURL:   "https://www.exemplo.com.br/imovel/12",
			PriceRaw:     "R$ 450.000",
			LastSeenAt:   lastSeenAt,
		}},
	})

	want := map[string]any{
		"id":                "6f1a0d1e-0000-4000-8000-000000000001",
		"transaction_type":  "venda",
		"property_type":     "apartamento",
		"bedrooms":          float64(3),
		"bathrooms":         float64(2),
		"parking_spots":     float64(1),
		"area":              72.5,
		"canonical_address": "Rua A, 100",
		"city":              "Curitiba",
		"neighborhood":      "Batel",
		"state":             "PR",
		"description":       "Perto do centro",
		"latitude":          -25.4,
		"longitude":         -49.2,
		"created_at":        "2026-08-19T10:00:00Z",
	}

	for key, value := range want {
		if got := payload[key]; got != value {
			t.Errorf("%s = %v, want %v", key, got, value)
		}
	}

	if got := payload["amenities"]; !equalStrings(got, []string{"piscina"}) {
		t.Errorf("amenities = %v, want [piscina]", got)
	}
	if got := payload["photos"]; !equalStrings(got, []string{"https://exemplo.com/1.jpg"}) {
		t.Errorf("photos = %v", got)
	}

	// O contador denormalizado pode divergir de len(listings); a resposta
	// reflete as linhas realmente lidas.
	if _, exists := payload["active_listing_count"]; exists {
		t.Error("active_listing_count não deve ser exposto: contradiz len(listings) em bases antigas")
	}
	// Não há preço canônico no schema; o preço vive por anúncio.
	if _, exists := payload["price"]; exists {
		t.Error("property não tem preço canônico no schema")
	}

	listings, ok := payload["listings"].([]any)
	if !ok || len(listings) != 1 {
		t.Fatalf("listings = %v, want 1 item", payload["listings"])
	}
	listing, _ := listings[0].(map[string]any)

	wantListing := map[string]any{
		"id":            float64(12),
		"source_domain": "www.exemplo.com.br",
		"price_raw":     "R$ 450.000",
		"original_url":  "https://www.exemplo.com.br/imovel/12",
		"last_seen_at":  "2026-08-19T09:00:00Z",
	}
	for key, value := range wantListing {
		if got := listing[key]; got != value {
			t.Errorf("listings[0].%s = %v, want %v", key, got, value)
		}
	}
	if _, exists := listing["source_name"]; exists {
		t.Error("source_name não existe no schema; a identidade da fonte é source_domain")
	}
}

func TestPropertyDetailResponseMapsNullsAndEmptyArrays(t *testing.T) {
	// Imóvel que existe mas nunca foi consolidado: quase tudo NULL.
	payload := marshalDetail(t, db.PropertyDetail{
		Property: db.Property{ID: "6f1a0d1e-0000-4000-8000-000000000002"},
	})

	nullable := []string{
		"transaction_type", "property_type", "bedrooms", "bathrooms",
		"parking_spots", "area", "canonical_address", "city", "neighborhood",
		"state", "description", "latitude", "longitude",
	}
	for _, key := range nullable {
		value, exists := payload[key]
		if !exists {
			t.Errorf("%s ausente do JSON; a ausência é informação e precisa virar null", key)
			continue
		}
		if value != nil {
			t.Errorf("%s = %v, want null (0/\"\" apagariam a distinção)", key, value)
		}
	}

	for _, key := range []string{"amenities", "photos", "listings"} {
		value, ok := payload[key].([]any)
		if !ok {
			t.Errorf("%s = %v, want [] (nunca null)", key, payload[key])
			continue
		}
		if len(value) != 0 {
			t.Errorf("%s = %v, want []", key, value)
		}
	}
}

func TestPropertyDetailListingsPreserveRepositoryOrder(t *testing.T) {
	detail := db.PropertyDetail{
		Property: db.Property{ID: "6f1a0d1e-0000-4000-8000-000000000003"},
		Listings: []db.PropertyListing{
			{ID: 3, SourceDomain: "a.com.br"},
			{ID: 7, SourceDomain: "b.com.br"},
			{ID: 11, SourceDomain: "c.com.br"},
		},
	}

	got := newPropertyDetailResponse(detail).Listings
	want := []int64{3, 7, 11}
	if len(got) != len(want) {
		t.Fatalf("listings = %d itens, want %d", len(got), len(want))
	}
	for i, id := range want {
		if got[i].ID != id {
			t.Errorf("listings[%d].id = %d, want %d", i, got[i].ID, id)
		}
	}
}

func TestPropertyDetailTimestampsAreRFC3339UTC(t *testing.T) {
	// Timezone de conexão não pode vazar para o contrato: o mesmo instante tem
	// que sair escrito do mesmo jeito em qualquer ambiente.
	saoPaulo := time.FixedZone("-03", -3*60*60)
	detail := db.PropertyDetail{
		Property: db.Property{
			ID:        "6f1a0d1e-0000-4000-8000-000000000004",
			CreatedAt: time.Date(2026, 8, 19, 7, 0, 0, 0, saoPaulo),
		},
		Listings: []db.PropertyListing{{
			ID:         1,
			LastSeenAt: time.Date(2026, 8, 19, 6, 0, 0, 0, saoPaulo),
		}},
	}

	response := newPropertyDetailResponse(detail)
	if got, want := response.CreatedAt, "2026-08-19T10:00:00Z"; got != want {
		t.Errorf("created_at = %q, want %q", got, want)
	}
	if got, want := response.Listings[0].LastSeenAt, "2026-08-19T09:00:00Z"; got != want {
		t.Errorf("last_seen_at = %q, want %q", got, want)
	}
}

func TestGetPropertyRejectsBlankID(t *testing.T) {
	// Nenhum destes caminhos toca o pool (nil em Deps prova isso): o id é
	// rejeitado antes da ida ao banco.
	paths := []string{
		"/api/v1/properties/",
		"/api/v1/properties/%20%20",
	}

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			NewRouter(Deps{Logger: discardLogger()}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
			}
			if got := rec.Header().Get(contentTypeHeader); got != contentTypeJSON {
				t.Errorf("Content-Type = %q, want %q", got, contentTypeJSON)
			}
			if got := decodeError(t, rec.Body.Bytes()); got != msgInvalidID {
				t.Errorf("error = %q, want %q", got, msgInvalidID)
			}
		})
	}
}

func TestPropertiesWithoutTrailingSlashIsTheSearchRoute(t *testing.T) {
	// Substitui TestPropertiesWithoutTrailingSlashRedirects: o 301 implícito que
	// ".../properties/{$}" provocava era transitório e valia só enquanto a busca
	// não existisse. Com o padrão exato registrado, ele ganha do redirect — o que
	// importa pinar agora é que "/api/v1/properties" **não** redireciona mais
	// (navegadores cacheiam 301) e não cai no 404 do mux.
	rec := httptest.NewRecorder()
	NewRouter(Deps{Logger: discardLogger()}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/properties", nil))

	if rec.Code == http.StatusMovedPermanently || rec.Code == http.StatusNotFound {
		t.Fatalf("status = %d, want a busca respondendo (nem 301, nem 404)", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "" {
		t.Errorf("Location = %q, want vazio — a busca não redireciona", got)
	}
}

func TestNestedPropertyPathIsNotFound(t *testing.T) {
	// "{$}" casa só com o path terminado em barra: um path mais fundo continua
	// caindo no 404 do mux, no envelope JSON.
	rec := httptest.NewRecorder()
	NewRouter(Deps{Logger: discardLogger()}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/properties/abc/def", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	if got := decodeError(t, rec.Body.Bytes()); got != msgNotFound {
		t.Errorf("error = %q, want %q", got, msgNotFound)
	}
}

func TestGetPropertyRouteIsRegisteredUnderV1(t *testing.T) {
	// POST na rota só devolve 405 (com o envelope JSON do jsonErrors) se o
	// padrão foi registrado dentro de registerV1Routes, com a cadeia completa
	// de middlewares.
	rec := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/properties/6f1a0d1e-0000-4000-8000-000000000001", nil)
	NewRouter(Deps{Logger: discardLogger()}).ServeHTTP(rec, request)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
	if got := rec.Header().Get(contentTypeHeader); got != contentTypeJSON {
		t.Errorf("Content-Type = %q, want %q", got, contentTypeJSON)
	}
	if got := decodeError(t, rec.Body.Bytes()); got != msgNotAllowed {
		t.Errorf("error = %q, want %q", got, msgNotAllowed)
	}
}

func equalStrings(value any, want []string) bool {
	items, ok := value.([]any)
	if !ok || len(items) != len(want) {
		return false
	}
	for i, item := range items {
		if item != want[i] {
			return false
		}
	}
	return true
}
