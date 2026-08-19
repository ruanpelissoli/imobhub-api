package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/imobhub/api/internal/db"
)

// fakeStore substitui o Redis sem rede: cada operação devolve o resultado
// programado pelo teste e conta as chamadas.
type fakeStore struct {
	value    string
	getErr   error
	setErr   error
	gets     int
	sets     int
	lastKey  string
	lastBody []byte
	lastTTL  time.Duration
}

func (s *fakeStore) Get(_ context.Context, key string) *redis.StringCmd {
	s.gets++
	s.lastKey = key
	if s.getErr != nil {
		return redis.NewStringResult("", s.getErr)
	}
	if s.value == "" {
		return redis.NewStringResult("", redis.Nil)
	}
	return redis.NewStringResult(s.value, nil)
}

func (s *fakeStore) Set(_ context.Context, key string, value any, ttl time.Duration) *redis.StatusCmd {
	s.sets++
	s.lastKey = key
	s.lastTTL = ttl
	if body, ok := value.([]byte); ok {
		s.lastBody = append([]byte(nil), body...)
	}
	if s.setErr != nil {
		return redis.NewStatusResult("", s.setErr)
	}
	return redis.NewStatusResult("OK", nil)
}

func countingSearcher(calls *int, properties []db.Property, total int64, err error) propertySearcher {
	return func(context.Context, db.PropertySearchParams) ([]db.Property, int64, error) {
		*calls++
		return properties, total, err
	}
}

func keyFor(t *testing.T, query string) string {
	t.Helper()

	params, err := parsePropertySearchParams(mustParseQuery(t, query))
	if err != nil {
		t.Fatalf("parse de %q falhou: %v", query, err)
	}
	return propertySearchCacheKey(params)
}

func TestPropertySearchCacheKeyIsStableAcrossEquivalentQueries(t *testing.T) {
	tests := []struct {
		name string
		a    string
		b    string
	}{
		{
			name: "ordem das amenities não importa",
			a:    "amenities=piscina,academia",
			b:    "amenities=academia,piscina",
		},
		{
			name: "repetição e CSV são a mesma busca",
			a:    "amenities=piscina&amenities=academia",
			b:    "amenities=piscina,academia",
		},
		{
			name: "amenities duplicadas são deduplicadas",
			a:    "amenities=piscina,piscina,academia",
			b:    "amenities=academia,piscina",
		},
		{
			name: "itens em branco não mudam a chave",
			a:    "amenities=piscina,,academia",
			b:    "amenities=piscina,academia",
		},
		{
			name: "paginação usa os valores efetivos",
			a:    "page=0&page_size=0",
			b:    "page=1&page_size=20",
		},
		{
			name: "page_size acima do teto colapsa no teto",
			a:    "page_size=999",
			b:    "page_size=50",
		},
		{
			name: "espaços em volta dos valores são irrelevantes",
			a:    "city=%20Curitiba%20",
			b:    "city=Curitiba",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, want := keyFor(t, tt.a), keyFor(t, tt.b); got != want {
				t.Errorf("chaves diferentes para buscas equivalentes:\n%q\n%q", got, want)
			}
		})
	}
}

func TestPropertySearchCacheKeyDiffersForDifferentSearches(t *testing.T) {
	queries := []string{
		"",
		"city=Curitiba",
		"city=Curitiba&neighborhood=Batel",
		"neighborhood=Curitiba",
		"transaction_type=venda",
		"property_type=venda",
		"min_bedrooms=2",
		"min_bathrooms=2",
		"min_parking_spots=2",
		"min_area=2",
		"amenities=piscina",
		"amenities=piscina,academia",
		"page=2",
		"page_size=30",
	}

	seen := make(map[string]string, len(queries))
	for _, query := range queries {
		key := keyFor(t, query)
		if previous, ok := seen[key]; ok {
			t.Errorf("colisão de chave entre %q e %q", previous, query)
		}
		seen[key] = query
	}
}

func TestPropertySearchCacheKeyUsesVersionedPrefix(t *testing.T) {
	key := keyFor(t, "city=Curitiba")

	if !strings.HasPrefix(key, "properties:search:v1:") {
		t.Fatalf("chave = %q, want prefixo %q", key, "properties:search:v1:")
	}
	// SHA-256 em hex: o tamanho fixo é o que garante que nenhum valor do usuário
	// vaze para dentro da chave.
	if got := len(strings.TrimPrefix(key, propertySearchCachePrefix)); got != 64 {
		t.Errorf("hash com %d caracteres, want 64", got)
	}
}

func TestSearchPropertiesCacheHitSkipsTheDatabase(t *testing.T) {
	var calls int
	store := &fakeStore{}
	cache := &propertyCache{store: store}
	handler := handleSearchProperties(
		countingSearcher(&calls, []db.Property{{ID: "abc"}}, 1, nil), cache, discardLogger())

	miss := doSearchRequest(t, handler, "/api/v1/properties?city=Curitiba")
	if miss.Code != http.StatusOK {
		t.Fatalf("status do miss = %d, want %d", miss.Code, http.StatusOK)
	}
	if got := miss.Header().Get(headerCacheStatus); got != cacheStatusMiss {
		t.Errorf("%s do miss = %q, want %q", headerCacheStatus, got, cacheStatusMiss)
	}
	if store.sets != 1 {
		t.Fatalf("gravações no cache = %d, want 1", store.sets)
	}
	if store.lastTTL != propertySearchCacheTTL {
		t.Errorf("TTL = %v, want %v", store.lastTTL, propertySearchCacheTTL)
	}
	if propertySearchCacheTTL != 5*time.Minute {
		t.Errorf("propertySearchCacheTTL = %v, want 5m", propertySearchCacheTTL)
	}

	store.value = string(store.lastBody)

	hit := doSearchRequest(t, handler, "/api/v1/properties?city=Curitiba")
	if hit.Code != http.StatusOK {
		t.Fatalf("status do hit = %d, want %d", hit.Code, http.StatusOK)
	}
	if got := hit.Header().Get(headerCacheStatus); got != cacheStatusHit {
		t.Errorf("%s do hit = %q, want %q", headerCacheStatus, got, cacheStatusHit)
	}
	if got := hit.Header().Get(contentTypeHeader); got != contentTypeJSON {
		t.Errorf("Content-Type do hit = %q, want %q", got, contentTypeJSON)
	}
	if hit.Body.String() != miss.Body.String() {
		t.Errorf("corpo do hit = %q, want idêntico ao do miss = %q", hit.Body.String(), miss.Body.String())
	}
	if calls != 1 {
		t.Errorf("searcher chamado %d vezes, want 1 — o hit tocou o banco", calls)
	}
	if store.sets != 1 {
		t.Errorf("gravações no cache = %d, want 1 — o hit regravou a entrada", store.sets)
	}
}

func TestSearchPropertiesFallsBackWhenRedisFails(t *testing.T) {
	tests := []struct {
		name  string
		store *fakeStore
	}{
		{name: "redis indisponível", store: &fakeStore{getErr: errors.New("dial tcp: connection refused")}},
		{name: "timeout na leitura", store: &fakeStore{getErr: context.DeadlineExceeded}},
		{name: "valor corrompido", store: &fakeStore{value: "{isso não é json"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			capture := &capturingHandler{Handler: slog.NewJSONHandler(io.Discard, nil)}
			var calls int
			handler := handleSearchProperties(
				countingSearcher(&calls, nil, 0, nil), &propertyCache{store: tt.store}, slog.New(capture))

			rec := doSearchRequest(t, handler, "/api/v1/properties")

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d — falha de Redis não pode virar erro", rec.Code, http.StatusOK)
			}
			if got := rec.Header().Get(headerCacheStatus); got != cacheStatusMiss {
				t.Errorf("%s = %q, want %q", headerCacheStatus, got, cacheStatusMiss)
			}
			if calls != 1 {
				t.Errorf("searcher chamado %d vezes, want 1 — o fallback não chegou ao banco", calls)
			}
			if !strings.Contains(rec.Body.String(), `"data":[]`) {
				t.Errorf("corpo = %q, want a resposta normal", rec.Body.String())
			}
			if !hasWarning(capture.records) {
				t.Error("a falha do Redis precisa ser logada em nível warn")
			}
		})
	}
}

func TestSearchPropertiesKeepsRespondingWhenCacheWriteFails(t *testing.T) {
	capture := &capturingHandler{Handler: slog.NewJSONHandler(io.Discard, nil)}
	store := &fakeStore{setErr: errors.New("READONLY")}
	handler := handleSearchProperties(emptySearcher(), &propertyCache{store: store}, slog.New(capture))

	rec := doSearchRequest(t, handler, "/api/v1/properties")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get(headerCacheStatus); got != cacheStatusMiss {
		t.Errorf("%s = %q, want %q", headerCacheStatus, got, cacheStatusMiss)
	}
	if !hasWarning(capture.records) {
		t.Error("a falha de gravação precisa ser logada em nível warn")
	}
}

func TestSearchPropertiesOnlyCachesSuccessfulResponses(t *testing.T) {
	tests := []struct {
		name       string
		query      string
		searchErr  error
		wantStatus int
	}{
		{name: "400 não é cacheado", query: "?page=abc", wantStatus: http.StatusBadRequest},
		{name: "400 de preço não é cacheado", query: "?min_price=1", wantStatus: http.StatusBadRequest},
		{
			name:       "500 não é cacheado",
			query:      "",
			searchErr:  errors.New("pgx: falhou"),
			wantStatus: http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls int
			store := &fakeStore{}
			handler := handleSearchProperties(
				countingSearcher(&calls, nil, 0, tt.searchErr), &propertyCache{store: store}, discardLogger())

			rec := doSearchRequest(t, handler, "/api/v1/properties"+tt.query)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if store.sets != 0 {
				t.Errorf("gravações no cache = %d, want 0", store.sets)
			}
		})
	}
}

func TestPropertyCacheWithoutStoreIsAlwaysAMiss(t *testing.T) {
	// newPropertyCache(nil) precisa devolver nil, e não um *propertyCache com um
	// *redis.Client nil dentro da interface: o segundo entraria em panic.
	cache := newPropertyCache(nil)
	if cache != nil {
		t.Fatalf("newPropertyCache(nil) = %v, want nil", cache)
	}

	if _, ok := cache.get(context.Background(), "chave", discardLogger()); ok {
		t.Error("cache nulo não pode reportar hit")
	}
	cache.set(context.Background(), "chave", []byte(`{}`), discardLogger())
}

func hasWarning(records []slog.Record) bool {
	for _, record := range records {
		if record.Level == slog.LevelWarn {
			return true
		}
	}
	return false
}
