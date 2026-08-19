package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/imobhub/api/internal/db"
)

const (
	// propertySearchCachePrefix carrega a versão do **formato do payload**, não
	// da API. Mudar o DTO de resposta sem trocar o `v1` faria o Redis servir o
	// shape antigo por até um TTL inteiro, para clientes que já esperam o novo.
	propertySearchCachePrefix = "properties:search:v1:"

	// propertySearchCacheTTL é constante do pacote, não variável de ambiente:
	// internal/api não importa internal/config (ver CLAUDE.md).
	propertySearchCacheTTL = 5 * time.Minute

	// propertyCacheOpTimeout limita cada ida ao Redis. Sem ele, um Redis lento
	// consumiria o WriteTimeout de 30s do servidor e o cache passaria a piorar
	// exatamente a latência que existe para melhorar.
	propertyCacheOpTimeout = 200 * time.Millisecond
)

// cacheStore é declarado no consumidor para que os testes do pacote rodem sem
// Redis (mesma convenção de propertySearcher).
type cacheStore interface {
	Get(ctx context.Context, key string) *redis.StringCmd
	Set(ctx context.Context, key string, value any, expiration time.Duration) *redis.StatusCmd
}

type propertyCache struct {
	store cacheStore
}

// newPropertyCache checa o nil **antes** da atribuição à interface: guardar um
// *redis.Client nil em cacheStore produziria uma interface não-nil e um panic na
// primeira operação. Um *propertyCache nil é o caminho "sem cache".
func newPropertyCache(client *redis.Client) *propertyCache {
	if client == nil {
		return nil
	}
	return &propertyCache{store: client}
}

// get trata qualquer problema do Redis como miss: indisponibilidade, timeout ou
// valor ilegível degradam para o banco com um warning, nunca para 500.
func (c *propertyCache) get(ctx context.Context, key string, logger *slog.Logger) ([]byte, bool) {
	if c == nil || c.store == nil {
		return nil, false
	}

	ctx, cancel := context.WithTimeout(ctx, propertyCacheOpTimeout)
	defer cancel()

	value, err := c.store.Get(ctx, key).Bytes()
	switch {
	case errors.Is(err, redis.Nil):
		return nil, false
	case err != nil:
		logger.Warn("api: property cache read failed", "error", err, "key", key)
		return nil, false
	}

	// Um valor corrompido iria direto para o corpo da resposta com
	// Content-Type: application/json — o cliente receberia lixo com status 200.
	if !json.Valid(value) {
		logger.Warn("api: property cache value is not valid json", "key", key)
		return nil, false
	}

	return value, true
}

// set grava o corpo já serializado do 200. É chamado só no caminho de sucesso:
// cachear um 400 ou um 500 serviria o erro por um TTL inteiro.
func (c *propertyCache) set(ctx context.Context, key string, body []byte, logger *slog.Logger) {
	if c == nil || c.store == nil {
		return
	}

	ctx, cancel := context.WithTimeout(ctx, propertyCacheOpTimeout)
	defer cancel()

	if err := c.store.Set(ctx, key, body, propertySearchCacheTTL).Err(); err != nil {
		logger.Warn("api: property cache write failed", "error", err, "key", key)
	}
}

// propertySearchCacheKey normaliza antes de hashear — paginação efetiva e
// comodidades com trim, sem vazios, deduplicadas e ordenadas —, que é o que faz
// `?amenities=a,b` e `?amenities=b,a&amenities=b` compartilharem a entrada.
//
// Os campos vão direto para o hash com %q em vez de passar por json.Marshal: não
// existe caminho de erro para tratar e a delimitação por aspas impede que
// ("ab","") e ("a","b") colidam.
func propertySearchCacheKey(params db.PropertySearchParams) string {
	page, pageSize := db.EffectivePropertyPagination(params.Page, params.PageSize)

	sum := sha256.New()
	fmt.Fprintf(sum, "transaction_type=%q\n", strings.TrimSpace(params.TransactionType))
	fmt.Fprintf(sum, "property_type=%q\n", strings.TrimSpace(params.PropertyType))
	fmt.Fprintf(sum, "city=%q\n", strings.TrimSpace(params.City))
	fmt.Fprintf(sum, "neighborhood=%q\n", strings.TrimSpace(params.Neighborhood))
	fmt.Fprintf(sum, "min_bedrooms=%d\n", params.MinBedrooms)
	fmt.Fprintf(sum, "min_bathrooms=%d\n", params.MinBathrooms)
	fmt.Fprintf(sum, "min_parking_spots=%d\n", params.MinParkingSpots)
	fmt.Fprintf(sum, "min_area=%s\n", strconv.FormatFloat(params.MinArea, 'g', -1, 64))
	fmt.Fprintf(sum, "page=%d\npage_size=%d\n", page, pageSize)
	for _, amenity := range normalizeCacheAmenities(params.Amenities) {
		fmt.Fprintf(sum, "amenity=%q\n", amenity)
	}

	return propertySearchCachePrefix + hex.EncodeToString(sum.Sum(nil))
}

func normalizeCacheAmenities(amenities []string) []string {
	normalized := make([]string, 0, len(amenities))
	for _, amenity := range amenities {
		if amenity = strings.TrimSpace(amenity); amenity != "" {
			normalized = append(normalized, amenity)
		}
	}

	slices.Sort(normalized)
	return slices.Compact(normalized)
}
