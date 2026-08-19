package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/imobhub/api/internal/db"
)

// propertySearcher é declarado no consumidor (mesmo padrão de
// grouping.PropertyStore) para que o handler seja testável sem PostgreSQL.
type propertySearcher func(ctx context.Context, params db.PropertySearchParams) ([]db.Property, int64, error)

// propertyListItem embute propertyResponse — o DTO do imóvel é um só para toda
// rota que devolve imóvel — e acrescenta o contador de anúncios.
//
// O contador fica **aqui** e não em propertyResponse porque o detalhe o omite de
// propósito: lá ele apareceria ao lado de "listings", e um contador denormalizado
// divergente viraria uma contradição visível na tela. Na busca não há lista de
// anúncios com que contradizer, e "3 anúncios" é a informação que faz o usuário
// abrir o imóvel.
type propertyListItem struct {
	propertyResponse
	ActiveListingCount int `json:"active_listing_count"`
}

type paginationResponse struct {
	Page       int   `json:"page"`
	PageSize   int   `json:"page_size"`
	Total      int64 `json:"total"`
	TotalPages int64 `json:"total_pages"`
}

type propertyListResponse struct {
	Data       []propertyListItem `json:"data"`
	Pagination paginationResponse `json:"pagination"`
}

const (
	headerCacheStatus = "X-Cache"
	cacheStatusHit    = "HIT"
	cacheStatusMiss   = "MISS"
)

// handleSearchProperties grava no cache só no caminho 200: um 400 ou um 500
// cacheados serviriam o erro por até o TTL inteiro. cache nulo (sem Redis
// configurado) é caminho válido e significa "sempre miss".
func handleSearchProperties(search propertySearcher, cache *propertyCache, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		params, err := parsePropertySearchParams(r.URL.Query())
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}

		key := propertySearchCacheKey(params)

		if cached, ok := cache.get(r.Context(), key, logger); ok {
			w.Header().Set(headerCacheStatus, cacheStatusHit)
			writeJSONBytes(w, http.StatusOK, cached)
			return
		}

		properties, total, err := search(r.Context(), params)
		if err != nil {
			logger.Error("api: search properties failed", "error", err, "path", r.URL.Path)
			writeError(w, http.StatusInternalServerError, msgInternalError)
			return
		}

		body, err := json.Marshal(newPropertyListResponse(properties, total, params))
		if err != nil {
			logger.Error("api: failed to encode properties response", "error", err)
			writeError(w, http.StatusInternalServerError, msgInternalError)
			return
		}

		cache.set(r.Context(), key, body, logger)

		w.Header().Set(headerCacheStatus, cacheStatusMiss)
		writeJSONBytes(w, http.StatusOK, body)
	}
}

// newPropertyListResponse ecoa a paginação **efetiva**, não a crua: devolver
// `page=0` porque o cliente pediu 0 faria a UI montar a paginação em cima de um
// número que o banco nunca usou.
func newPropertyListResponse(properties []db.Property, total int64, params db.PropertySearchParams) propertyListResponse {
	page, pageSize := db.EffectivePropertyPagination(params.Page, params.PageSize)

	data := make([]propertyListItem, 0, len(properties))
	for _, property := range properties {
		data = append(data, propertyListItem{
			propertyResponse:   newPropertyResponse(property),
			ActiveListingCount: property.ActiveListingCount,
		})
	}

	return propertyListResponse{
		Data: data,
		Pagination: paginationResponse{
			Page:       page,
			PageSize:   pageSize,
			Total:      total,
			TotalPages: totalPages(total, pageSize),
		},
	}
}

func totalPages(total int64, pageSize int) int64 {
	if total <= 0 || pageSize <= 0 {
		return 0
	}
	return (total + int64(pageSize) - 1) / int64(pageSize)
}

// Preço é rejeitado explicitamente porque properties não tem coluna de preço
// (ver internal/db/CLAUDE.md). Ignorá-lo em silêncio devolveria o catálogo
// inteiro parecendo filtrado — o pior resultado possível para o front.
var unsupportedQueryParams = []string{"min_price", "max_price"}

const msgPriceFilterUnsupported = "price filter is not supported yet"

// parsePropertySearchParams valida **formato** (é número? é negativo?); a
// **faixa** de page/page_size é normalizada no repositório. Parâmetros
// desconhecidos são ignorados.
func parsePropertySearchParams(query url.Values) (db.PropertySearchParams, error) {
	for _, name := range unsupportedQueryParams {
		if query.Has(name) {
			return db.PropertySearchParams{}, fmt.Errorf("%s (%q)", msgPriceFilterUnsupported, name)
		}
	}

	params := db.PropertySearchParams{
		TransactionType: strings.TrimSpace(query.Get("transaction_type")),
		PropertyType:    strings.TrimSpace(query.Get("property_type")),
		City:            strings.TrimSpace(query.Get("city")),
		Neighborhood:    strings.TrimSpace(query.Get("neighborhood")),
		Amenities:       parseAmenities(query["amenities"]),
	}

	intFields := []struct {
		name   string
		target *int
	}{
		{"min_bedrooms", &params.MinBedrooms},
		{"min_bathrooms", &params.MinBathrooms},
		{"min_parking_spots", &params.MinParkingSpots},
	}
	for _, field := range intFields {
		value, err := parseIntParam(query, field.name, true)
		if err != nil {
			return db.PropertySearchParams{}, err
		}
		*field.target = value
	}

	minArea, err := parseFloatParam(query, "min_area")
	if err != nil {
		return db.PropertySearchParams{}, err
	}
	params.MinArea = minArea

	// page/page_size aceitam negativo: a faixa é responsabilidade do
	// repositório, que normaliza em vez de rejeitar (decisão registrada em
	// internal/db/CLAUDE.md). Só o formato é validado aqui.
	if params.Page, err = parseIntParam(query, "page", false); err != nil {
		return db.PropertySearchParams{}, err
	}
	if params.PageSize, err = parseIntParam(query, "page_size", false); err != nil {
		return db.PropertySearchParams{}, err
	}

	return params, nil
}

// parseAmenities aceita repetição (?amenities=a&amenities=b) e CSV
// (?amenities=a,b), inclusive misturadas. Itens em branco são descartados: um
// elemento vazio no TEXT[] zeraria o resultado sem explicação visível.
func parseAmenities(values []string) []string {
	amenities := make([]string, 0, len(values))
	for _, value := range values {
		for _, item := range strings.Split(value, ",") {
			if item = strings.TrimSpace(item); item != "" {
				amenities = append(amenities, item)
			}
		}
	}
	if len(amenities) == 0 {
		return nil
	}
	return amenities
}

func parseIntParam(query url.Values, name string, rejectNegative bool) (int, error) {
	raw := strings.TrimSpace(query.Get(name))
	if raw == "" {
		return 0, nil
	}

	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid value for %q: must be an integer", name)
	}
	if rejectNegative && value < 0 {
		return 0, fmt.Errorf("invalid value for %q: must not be negative", name)
	}

	return value, nil
}

func parseFloatParam(query url.Values, name string) (float64, error) {
	raw := strings.TrimSpace(query.Get(name))
	if raw == "" {
		return 0, nil
	}

	// NaN e ±Inf passam pelo ParseFloat mas quebrariam a chave de cache, e
	// nenhuma comparação `>= NaN` no SQL faria sentido.
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, fmt.Errorf("invalid value for %q: must be a number", name)
	}
	if value < 0 {
		return 0, fmt.Errorf("invalid value for %q: must not be negative", name)
	}

	return value, nil
}
