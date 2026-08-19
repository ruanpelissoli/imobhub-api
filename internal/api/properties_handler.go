package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/imobhub/api/internal/db"
)

// propertyResponse é **o** DTO do imóvel canônico da API. Toda rota que devolver
// um imóvel (detalhe, busca, listagem) monta este struct: dois mapeamentos JSON
// para a mesma entidade divergiriam no primeiro campo novo, e o front passaria a
// tratar "o mesmo imóvel" de dois jeitos.
//
// Os campos consolidados são ponteiros porque a coluna é nullable e a ausência é
// informação: NULL vira `null`, não `0`/`""`. Os nomes JSON são o contrato
// público e diferem das colunas de propósito (bedroom_count → bedrooms,
// area_sqm → area, lat/lng → latitude/longitude).
//
// **Não há campo de preço.** O schema não tem preço canônico: o único dado de
// preço é listings.price_raw, texto bruto, exposto por anúncio.
type propertyResponse struct {
	ID               string   `json:"id"`
	TransactionType  *string  `json:"transaction_type"`
	PropertyType     *string  `json:"property_type"`
	Bedrooms         *int     `json:"bedrooms"`
	Bathrooms        *int     `json:"bathrooms"`
	ParkingSpots     *int     `json:"parking_spots"`
	Area             *float64 `json:"area"`
	Amenities        []string `json:"amenities"`
	CanonicalAddress *string  `json:"canonical_address"`
	City             *string  `json:"city"`
	Neighborhood     *string  `json:"neighborhood"`
	State            *string  `json:"state"`
	Photos           []string `json:"photos"`
	Description      *string  `json:"description"`
	Latitude         *float64 `json:"latitude"`
	Longitude        *float64 `json:"longitude"`
	CreatedAt        string   `json:"created_at"`
	UpdatedAt        string   `json:"updated_at"`
}

// listingResponse é um anúncio na tela de comparação entre portais.
//
// source_domain é o host do portal, e não um nome comercial: a imobiliária por
// trás do anúncio não existe em lugar nenhum do schema. price_raw é o texto
// publicado pela fonte ("R$ 450.000", "450 mil") — o front não deve tratá-lo
// como número.
type listingResponse struct {
	ID           int64  `json:"id"`
	SourceDomain string `json:"source_domain"`
	PriceRaw     string `json:"price_raw"`
	OriginalURL  string `json:"original_url"`
	LastSeenAt   string `json:"last_seen_at"`
}

// propertyDetailResponse embute propertyResponse para que o JSON saia achatado:
// os campos do imóvel na raiz, os anúncios em "listings".
type propertyDetailResponse struct {
	propertyResponse
	Listings []listingResponse `json:"listings"`
}

// handleGetProperty responde GET /api/v1/properties/{id}.
//
// **Este endpoint não toca no Redis, nem para ler nem para escrever.** É a tela
// de comparação de preços entre portais: servir um preço obsoleto de cache é o
// pior defeito possível aqui, porque o usuário compara valores que já mudaram na
// origem sem ter como perceber. Duas queries por requisição é um custo aceitável
// para o volume de acessos a um detalhe.
func handleGetProperty(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")

		detail, err := db.GetPropertyWithListings(r.Context(), deps.Pool, id)
		switch {
		case errors.Is(err, db.ErrInvalidPropertyID):
			writeError(w, http.StatusBadRequest, msgInvalidID)
			return
		case errors.Is(err, context.Canceled):
			// Cliente desconectou no meio: não é falha da aplicação e não deve
			// poluir o nível de erro do log.
			deps.logger().Debug("api: get property canceled by client", "property_id", id)
			writeError(w, http.StatusInternalServerError, msgInternalError)
			return
		case err != nil:
			// O erro do pgx (que pode citar a query e a conexão) fica só aqui.
			deps.logger().Error("api: failed to load property detail", "property_id", id, "error", err)
			writeError(w, http.StatusInternalServerError, msgInternalError)
			return
		case detail == nil:
			writeError(w, http.StatusNotFound, msgNotFound)
			return
		}

		writeJSON(w, http.StatusOK, newPropertyDetailResponse(*detail))
	}
}

// newPropertyDetailResponse é pura de propósito: é ela que os testes cobrem sem
// banco, e é o único lugar onde o mapeamento coluna → campo JSON existe.
func newPropertyDetailResponse(detail db.PropertyDetail) propertyDetailResponse {
	listings := make([]listingResponse, 0, len(detail.Listings))
	for _, listing := range detail.Listings {
		listings = append(listings, listingResponse{
			ID:           listing.ID,
			SourceDomain: listing.SourceDomain,
			PriceRaw:     listing.PriceRaw,
			OriginalURL:  listing.ListingURL,
			LastSeenAt:   formatTimestamp(listing.LastSeenAt),
		})
	}

	return propertyDetailResponse{
		propertyResponse: newPropertyResponse(detail.Property),
		Listings:         listings,
	}
}

// newPropertyResponse converte o registro canônico no DTO público.
//
// active_listing_count fica **fora** da resposta: o contador é denormalizado e
// pode divergir de len(listings) em bases anteriores à IMO-22. Publicar os dois
// lado a lado colocaria uma contradição visível na tela de comparação, e a
// verdade é a lista de anúncios efetivamente lida.
func newPropertyResponse(property db.Property) propertyResponse {
	return propertyResponse{
		ID:               property.ID,
		TransactionType:  property.TransactionType,
		PropertyType:     property.PropertyType,
		Bedrooms:         property.BedroomCount,
		Bathrooms:        property.BathroomCount,
		ParkingSpots:     property.ParkingSpots,
		Area:             property.AreaSqm,
		Amenities:        emptyIfNil(property.Amenities),
		CanonicalAddress: property.CanonicalAddress,
		City:             property.City,
		Neighborhood:     property.Neighborhood,
		State:            property.State,
		Photos:           emptyIfNil(property.Photos),
		Description:      property.Description,
		Latitude:         property.Lat,
		Longitude:        property.Lng,
		CreatedAt:        formatTimestamp(property.CreatedAt),
		UpdatedAt:        formatTimestamp(property.UpdatedAt),
	}
}

// emptyIfNil garante `[]` no JSON. Uma slice nil serializa como `null`, e o
// front teria que distinguir "sem fotos" de "não sei" numa distinção que o banco
// não faz (ver normalizeTextArray em internal/db).
func emptyIfNil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

// formatTimestamp fixa RFC 3339 em UTC. Sem a conversão, o offset da timezone da
// conexão vazaria para o contrato e o mesmo instante apareceria escrito de
// formas diferentes conforme o ambiente.
func formatTimestamp(value time.Time) string {
	return value.UTC().Format(time.RFC3339)
}
