package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/imobhub/api/internal/db"
)

func mustParseQuery(t *testing.T, raw string) url.Values {
	t.Helper()

	query, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatalf("query string inválida no próprio teste (%q): %v", raw, err)
	}
	return query
}

func emptySearcher() propertySearcher {
	return func(context.Context, db.PropertySearchParams) ([]db.Property, int64, error) {
		return nil, 0, nil
	}
}

func doSearchRequest(t *testing.T, handler http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func ptr[T any](value T) *T {
	return &value
}

func TestParsePropertySearchParams(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  db.PropertySearchParams
	}{
		{
			name:  "query vazia não aplica filtro algum",
			query: "",
			want:  db.PropertySearchParams{},
		},
		{
			name: "todos os parâmetros suportados",
			query: "transaction_type=venda&property_type=apartamento&city=Curitiba&neighborhood=Batel" +
				"&min_bedrooms=2&min_bathrooms=1&min_parking_spots=3&min_area=72.5&page=4&page_size=10",
			want: db.PropertySearchParams{
				TransactionType: "venda",
				PropertyType:    "apartamento",
				City:            "Curitiba",
				Neighborhood:    "Batel",
				MinBedrooms:     2,
				MinBathrooms:    1,
				MinParkingSpots: 3,
				MinArea:         72.5,
				Page:            4,
				PageSize:        10,
			},
		},
		{
			name:  "amenities repetido",
			query: "amenities=piscina&amenities=churrasqueira",
			want:  db.PropertySearchParams{Amenities: []string{"piscina", "churrasqueira"}},
		},
		{
			name:  "amenities em CSV",
			query: "amenities=piscina,churrasqueira",
			want:  db.PropertySearchParams{Amenities: []string{"piscina", "churrasqueira"}},
		},
		{
			name:  "amenities misturando repetição e CSV",
			query: "amenities=piscina,churrasqueira&amenities=academia",
			want:  db.PropertySearchParams{Amenities: []string{"piscina", "churrasqueira", "academia"}},
		},
		{
			name:  "itens em branco de amenities são descartados",
			query: "amenities=piscina,,%20,churrasqueira",
			want:  db.PropertySearchParams{Amenities: []string{"piscina", "churrasqueira"}},
		},
		{
			name:  "amenities só com vazios não vira filtro",
			query: "amenities=,,",
			want:  db.PropertySearchParams{},
		},
		{
			name:  "strings são trimadas",
			query: "city=%20Curitiba%20&transaction_type=%20venda",
			want:  db.PropertySearchParams{City: "Curitiba", TransactionType: "venda"},
		},
		{
			name:  "parâmetros desconhecidos são ignorados",
			query: "sort=price&radius=10&foo=bar",
			want:  db.PropertySearchParams{},
		},
		{
			name:  "page e page_size fora da faixa chegam crus ao repositório",
			query: "page=0&page_size=999",
			want:  db.PropertySearchParams{Page: 0, PageSize: 999},
		},
		{
			name:  "page negativo é normalizado, não rejeitado",
			query: "page=-2&page_size=-5",
			want:  db.PropertySearchParams{Page: -2, PageSize: -5},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePropertySearchParams(mustParseQuery(t, tt.query))
			if err != nil {
				t.Fatalf("erro inesperado: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("params = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParsePropertySearchParamsRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name         string
		query        string
		wantContains string
	}{
		{name: "page não numérico", query: "page=abc", wantContains: "page"},
		{name: "page_size não numérico", query: "page_size=abc", wantContains: "page_size"},
		{name: "min_area não numérico", query: "min_area=xyz", wantContains: "min_area"},
		{name: "min_bedrooms não numérico", query: "min_bedrooms=dois", wantContains: "min_bedrooms"},
		{name: "min_bedrooms negativo", query: "min_bedrooms=-1", wantContains: "min_bedrooms"},
		{name: "min_bathrooms negativo", query: "min_bathrooms=-1", wantContains: "min_bathrooms"},
		{name: "min_parking_spots negativo", query: "min_parking_spots=-1", wantContains: "min_parking_spots"},
		{name: "min_area negativo", query: "min_area=-2.5", wantContains: "min_area"},
		{name: "min_area NaN", query: "min_area=NaN", wantContains: "min_area"},
		{name: "min_area infinito", query: "min_area=Inf", wantContains: "min_area"},
		{name: "min_price ainda não suportado", query: "min_price=1000", wantContains: "min_price"},
		{name: "max_price ainda não suportado", query: "max_price=1000", wantContains: "max_price"},
		{name: "min_price vazio também é rejeitado", query: "min_price=", wantContains: "min_price"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parsePropertySearchParams(mustParseQuery(t, tt.query))
			if err == nil {
				t.Fatalf("parse de %q deveria falhar", tt.query)
			}
			if !strings.Contains(err.Error(), tt.wantContains) {
				t.Errorf("mensagem = %q, want citando %q", err.Error(), tt.wantContains)
			}
		})
	}
}

func TestSearchPropertiesRejectsInvalidQueryWithErrorEnvelope(t *testing.T) {
	var searcherCalls int
	search := func(context.Context, db.PropertySearchParams) ([]db.Property, int64, error) {
		searcherCalls++
		return nil, 0, nil
	}
	handler := handleSearchProperties(search, nil, discardLogger())

	for _, query := range []string{"?page=abc", "?min_bedrooms=-1", "?min_price=1000", "?min_area=xyz"} {
		t.Run(query, func(t *testing.T) {
			rec := doSearchRequest(t, handler, "/api/v1/properties"+query)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
			}
			if got := rec.Header().Get(contentTypeHeader); got != contentTypeJSON {
				t.Errorf("Content-Type = %q, want %q", got, contentTypeJSON)
			}
			if got := decodeError(t, rec.Body.Bytes()); got == "" {
				t.Error("envelope de erro veio sem mensagem")
			}
		})
	}

	if searcherCalls != 0 {
		t.Errorf("searcher chamado %d vezes numa requisição inválida, want 0", searcherCalls)
	}
}

func TestSearchPropertiesNormalizesPaginationInsteadOfRejecting(t *testing.T) {
	tests := []struct {
		name         string
		query        string
		wantPage     int
		wantPageSize int
	}{
		{name: "page zero vira a primeira página", query: "?page=0", wantPage: 1, wantPageSize: 20},
		{name: "page_size acima do teto é cortado", query: "?page_size=999", wantPage: 1, wantPageSize: 50},
		{name: "negativos caem no default", query: "?page=-3&page_size=-1", wantPage: 1, wantPageSize: 20},
		{name: "query vazia usa o default", query: "", wantPage: 1, wantPageSize: 20},
		{name: "página absurda é cortada, não rejeitada", query: "?page=9223372036854775807", wantPage: 1_000_000, wantPageSize: 20},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := handleSearchProperties(emptySearcher(), nil, discardLogger())
			rec := doSearchRequest(t, handler, "/api/v1/properties"+tt.query)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
			}

			var payload propertyListResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
				t.Fatalf("corpo inválido (%q): %v", rec.Body.String(), err)
			}
			if payload.Pagination.Page != tt.wantPage {
				t.Errorf("page = %d, want %d", payload.Pagination.Page, tt.wantPage)
			}
			if payload.Pagination.PageSize != tt.wantPageSize {
				t.Errorf("page_size = %d, want %d", payload.Pagination.PageSize, tt.wantPageSize)
			}
		})
	}
}

func TestSearchPropertiesReturnsEmptyArrayNeverNull(t *testing.T) {
	// A asserção é sobre o JSON cru: `data: null` desserializa para uma slice nil
	// sem erro, e o front quebraria com um `.map` em cima de null.
	handler := handleSearchProperties(emptySearcher(), nil, discardLogger())
	rec := doSearchRequest(t, handler, "/api/v1/properties")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"data":[]`) {
		t.Errorf("corpo = %q, want data como array vazio", body)
	}
}

func TestSearchPropertiesPageBeyondTheEndKeepsRealTotal(t *testing.T) {
	search := func(_ context.Context, params db.PropertySearchParams) ([]db.Property, int64, error) {
		if params.Page != 1000 {
			t.Errorf("page recebido pelo repositório = %d, want 1000", params.Page)
		}
		return nil, 137, nil
	}

	handler := handleSearchProperties(search, nil, discardLogger())
	rec := doSearchRequest(t, handler, "/api/v1/properties?page=1000")

	var payload propertyListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("corpo inválido (%q): %v", rec.Body.String(), err)
	}
	if len(payload.Data) != 0 {
		t.Errorf("data = %d itens, want 0", len(payload.Data))
	}
	if payload.Pagination.Total != 137 {
		t.Errorf("total = %d, want 137", payload.Pagination.Total)
	}
	if payload.Pagination.TotalPages != 7 {
		t.Errorf("total_pages = %d, want 7", payload.Pagination.TotalPages)
	}
}

func TestTotalPages(t *testing.T) {
	tests := []struct {
		total    int64
		pageSize int
		want     int64
	}{
		{total: 0, pageSize: 20, want: 0},
		{total: 1, pageSize: 20, want: 1},
		{total: 20, pageSize: 20, want: 1},
		{total: 21, pageSize: 20, want: 2},
		{total: 137, pageSize: 20, want: 7},
		{total: 137, pageSize: 50, want: 3},
		{total: 10, pageSize: 0, want: 0},
	}

	for _, tt := range tests {
		if got := totalPages(tt.total, tt.pageSize); got != tt.want {
			t.Errorf("totalPages(%d, %d) = %d, want %d", tt.total, tt.pageSize, got, tt.want)
		}
	}
}

func TestSearchPropertiesSerializesDTOInSnakeCase(t *testing.T) {
	created := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	property := db.Property{
		ID:                 "11111111-1111-1111-1111-111111111111",
		CanonicalAddress:   ptr("Rua Exemplo, 100"),
		Neighborhood:       ptr("Batel"),
		City:               ptr("Curitiba"),
		State:              ptr("PR"),
		Lat:                ptr(-25.44),
		Lng:                ptr(-49.27),
		BedroomCount:       ptr(3),
		BathroomCount:      ptr(2),
		ParkingSpots:       ptr(1),
		AreaSqm:            ptr(72.5),
		Amenities:          []string{"piscina"},
		Description:        ptr("Apartamento reformado"),
		Photos:             []string{"https://example.com/1.jpg"},
		TransactionType:    ptr("venda"),
		PropertyType:       ptr("apartamento"),
		ActiveListingCount: 2,
		CreatedAt:          created,
		UpdatedAt:          created,
	}

	search := func(context.Context, db.PropertySearchParams) ([]db.Property, int64, error) {
		return []db.Property{property}, 1, nil
	}

	rec := doSearchRequest(t, handleSearchProperties(search, nil, discardLogger()), "/api/v1/properties")

	var payload struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("corpo inválido (%q): %v", rec.Body.String(), err)
	}
	if len(payload.Data) != 1 {
		t.Fatalf("data = %d itens, want 1", len(payload.Data))
	}

	want := []string{
		"id", "canonical_address", "neighborhood", "city", "state", "lat", "lng",
		"bedroom_count", "bathroom_count", "parking_spots", "area_sqm", "amenities",
		"description", "photos", "transaction_type", "property_type",
		"active_listing_count", "created_at", "updated_at",
	}
	item := payload.Data[0]
	for _, key := range want {
		if _, ok := item[key]; !ok {
			t.Errorf("campo %q ausente no DTO", key)
		}
	}
	if len(item) != len(want) {
		t.Errorf("DTO com %d campos, want %d — algum campo interno vazou", len(item), len(want))
	}
	if item["canonical_address"] != "Rua Exemplo, 100" {
		t.Errorf("canonical_address = %v", item["canonical_address"])
	}
}

func TestSearchPropertiesSerializesNilsAsNullAndSlicesAsEmpty(t *testing.T) {
	search := func(context.Context, db.PropertySearchParams) ([]db.Property, int64, error) {
		return []db.Property{{ID: "vazio"}}, 1, nil
	}

	rec := doSearchRequest(t, handleSearchProperties(search, nil, discardLogger()), "/api/v1/properties")
	body := rec.Body.String()

	for _, fragment := range []string{`"canonical_address":null`, `"bedroom_count":null`, `"lat":null`} {
		if !strings.Contains(body, fragment) {
			t.Errorf("corpo = %q, want conter %q", body, fragment)
		}
	}
	for _, fragment := range []string{`"amenities":[]`, `"photos":[]`} {
		if !strings.Contains(body, fragment) {
			t.Errorf("corpo = %q, want conter %q — slice nil não pode virar null", body, fragment)
		}
	}
}

func TestSearchPropertiesMapsQueryStringToRepositoryParams(t *testing.T) {
	var got db.PropertySearchParams
	search := func(_ context.Context, params db.PropertySearchParams) ([]db.Property, int64, error) {
		got = params
		return nil, 0, nil
	}

	handler := handleSearchProperties(search, nil, discardLogger())
	doSearchRequest(t, handler,
		"/api/v1/properties?transaction_type=aluguel&property_type=casa&city=Curitiba"+
			"&neighborhood=Batel&min_bedrooms=2&min_bathrooms=1&min_parking_spots=1"+
			"&min_area=80&amenities=piscina,academia&page=2&page_size=10")

	want := db.PropertySearchParams{
		TransactionType: "aluguel",
		PropertyType:    "casa",
		City:            "Curitiba",
		Neighborhood:    "Batel",
		MinBedrooms:     2,
		MinBathrooms:    1,
		MinParkingSpots: 1,
		MinArea:         80,
		Amenities:       []string{"piscina", "academia"},
		Page:            2,
		PageSize:        10,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("params = %+v, want %+v", got, want)
	}
}

func TestSearchPropertiesHidesRepositoryFailureDetails(t *testing.T) {
	capture := &capturingHandler{Handler: slog.NewJSONHandler(io.Discard, nil)}

	search := func(context.Context, db.PropertySearchParams) ([]db.Property, int64, error) {
		return nil, 0, errors.New("pgx: dial tcp postgres://app:senha@localhost:5432 refused")
	}

	handler := handleSearchProperties(search, nil, slog.New(capture))
	rec := doSearchRequest(t, handler, "/api/v1/properties")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if got := decodeError(t, rec.Body.Bytes()); got != msgInternalError {
		t.Errorf("error = %q, want %q", got, msgInternalError)
	}
	if strings.Contains(rec.Body.String(), "senha") || strings.Contains(rec.Body.String(), "pgx") {
		t.Errorf("o corpo vazou detalhe interno: %q", rec.Body.String())
	}
	if len(capture.records) == 0 {
		t.Error("a falha do repositório precisa aparecer no log")
	}
}

func TestSearchPropertiesWithoutRedisAlwaysReportsMiss(t *testing.T) {
	// newPropertyCache(nil) é o caminho de um ambiente sem Redis configurado:
	// precisa responder 200 sem panic (a interface cacheStore nunca recebe um
	// *redis.Client nil).
	handler := handleSearchProperties(emptySearcher(), newPropertyCache(nil), discardLogger())

	rec := doSearchRequest(t, handler, "/api/v1/properties?page=1")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (corpo: %q)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := rec.Header().Get(headerCacheStatus); got != cacheStatusMiss {
		t.Errorf("%s = %q, want %q", headerCacheStatus, got, cacheStatusMiss)
	}
}

func TestPropertiesRouteIsRegisteredUnderV1(t *testing.T) {
	router := NewRouter(Deps{Logger: discardLogger()})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, apiV1Prefix+"/properties", nil))

	// 405 (e não 404) prova que o path existe e que só o método não bate.
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}
