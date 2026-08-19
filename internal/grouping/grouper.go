// Package grouping decide, com apoio do Claude, se um anúncio já normalizado e
// geocodificado se refere ao mesmo imóvel físico de algum registro canônico em
// properties — vinculando-o ao existente ou criando um novo.
//
// O pacote é o par de internal/selectors do lado do enriquecimento: orquestra
// internal/ai e internal/db. Ele **não** vive em internal/enrichment de
// propósito: aquele pacote não importa db nem ai (ver internal/CLAUDE.md).
package grouping

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/imobhub/api/internal/ai"
	"github.com/imobhub/api/internal/db"
)

// Erros de construção e de uso do serviço. São sentinelas (errors.Is) para que
// a fila de enriquecimento distinga "este anúncio ainda não pode ser agrupado"
// (ErrListingNotGeocoded, pular e tentar depois da geocodificação) de "o
// serviço foi montado errado" (bug de wiring, derruba o boot).
var (
	// ErrMissingStore indica que o serviço foi construído sem acesso a
	// properties.
	ErrMissingStore = errors.New("grouping: property store is required")
	// ErrMissingMatcher indica que o serviço foi construído sem o comparador de
	// imóveis.
	ErrMissingMatcher = errors.New("grouping: property matcher is required")
	// ErrInvalidThreshold indica ConfidenceThreshold fora de [0,1].
	ErrInvalidThreshold = errors.New("grouping: confidence threshold must be between 0 and 1")
	// ErrInvalidRadius indica RadiusMeters não positivo.
	ErrInvalidRadius = errors.New("grouping: radius in meters must be greater than 0")
	// ErrInvalidMaxCandidates indica MaxCandidates não positivo.
	ErrInvalidMaxCandidates = errors.New("grouping: max candidates must be greater than 0")

	// ErrMissingListingID indica chamada sem o id do anúncio. Sem ele não há o
	// que vincular.
	ErrMissingListingID = errors.New("grouping: listing id is required")
	// ErrListingNotGeocoded indica anúncio sem lat/lng. Ele **não** é agrupado e
	// **não** vira property: um canônico sem coordenadas nunca mais aparece numa
	// busca por raio e, portanto, nunca mais casa com nada.
	ErrListingNotGeocoded = errors.New("grouping: listing has no coordinates")
)

// Listing é o anúncio já enriquecido, na forma que o agrupamento consome.
// Espelha as colunas de listings (inclusive as de enriquecimento); os campos
// opcionais são ponteiros porque a ausência é informação — nil é "não
// consolidado", 0 seria "zero quartos".
type Listing struct {
	// ID é listings.id. Obrigatório.
	ID int64
	// SourceDomain e ListingURL identificam o anúncio na origem; entram apenas
	// nos logs.
	SourceDomain string
	ListingURL   string

	AddressRaw             string
	NormalizedNeighborhood string

	// Lat e Lng vêm da geocodificação. Sem os dois não há agrupamento.
	Lat *float64
	Lng *float64

	BedroomCount *int
	AreaRaw      string
	Amenities    []string
	ImageURLs    []string

	TitleRaw       string
	DescriptionRaw string

	// PropertyID é o vínculo já existente (listings.property_id). Quando
	// presente, o agrupamento é um no-op: nenhuma chamada paga é feita.
	PropertyID *string
}

// PropertyStore é o acesso a dados de que o pacote precisa, em três blocos: o
// agrupamento (GroupListing), a consolidação do canônico (MergePropertyData) e
// a remoção de um anúncio do grupo (HandleListingRemoval).
//
// A interface é declarada **aqui, no consumidor**: internal/db expõe funções
// livres de propósito (ver internal/db/CLAUDE.md) e não ganha struct nem
// interface de repositório. É isso que permite testar este fluxo sem
// PostgreSQL e sem gastar token.
type PropertyStore interface {
	FindPropertiesByCoordinates(ctx context.Context, lat, lng float64, radiusMeters int) ([]db.Property, error)
	CreateProperty(ctx context.Context, property db.Property) (db.Property, error)
	LinkListingToProperty(ctx context.Context, propertyID string, listingID int64) error

	// GetPropertyByID devolve (nil, nil) quando o id não existe — mesmo
	// contrato de db.GetPropertyByID.
	GetPropertyByID(ctx context.Context, propertyID string) (*db.Property, error)
	// ListListingsByPropertyID devolve os anúncios do imóvel **ordenados por
	// id**; a ordem é o que sustenta a idempotência do merge.
	ListListingsByPropertyID(ctx context.Context, propertyID string) ([]db.Listing, error)
	UpdateProperty(ctx context.Context, property db.Property) error

	// UnlinkListingFromProperty devolve o imóvel de que o anúncio saiu, ou
	// string vazia quando não havia vínculo (inclusive anúncio inexistente).
	UnlinkListingFromProperty(ctx context.Context, listingID int64) (string, error)
	GetActiveListingCount(ctx context.Context, propertyID string) (int, error)
	DeleteProperty(ctx context.Context, propertyID string) error
}

// PropertyMatcher é a comparação por IA. Satisfeita por *ai.Client.
type PropertyMatcher interface {
	MatchProperty(ctx context.Context, listing ai.MatchListing, candidates []db.Property) (ai.PropertyMatch, error)
}

// Config são os parâmetros de custo e de rigor do agrupamento, vindos de
// internal/config.
type Config struct {
	// ConfidenceThreshold é a confiança mínima, em [0,1], para aceitar um match
	// do modelo. Abaixo dela o anúncio vira um imóvel novo.
	ConfidenceThreshold float64
	// RadiusMeters é o raio da busca por candidatos. Junto com MaxCandidates, é
	// o principal controle de custo do fluxo.
	RadiusMeters int
	// MaxCandidates limita quantos imóveis vão para o prompt.
	MaxCandidates int
}

// PropertyGrouper agrupa anúncios em imóveis canônicos.
//
// O ponto todo do serviço é que a comparação por IA é paga **por anúncio**: as
// saídas baratas (anúncio já vinculado, anúncio sem geo, nenhum candidato por
// perto) acontecem antes de qualquer requisição.
type PropertyGrouper struct {
	store   PropertyStore
	matcher PropertyMatcher
	cfg     Config
}

// NewPropertyGrouper monta o serviço. Todas as dependências e os limites são
// validados aqui para que um wiring incompleto derrube o boot, em vez de
// falhar no primeiro anúncio da fila.
func NewPropertyGrouper(store PropertyStore, matcher PropertyMatcher, cfg Config) (*PropertyGrouper, error) {
	if store == nil {
		return nil, ErrMissingStore
	}
	if matcher == nil {
		return nil, ErrMissingMatcher
	}
	if cfg.ConfidenceThreshold < 0 || cfg.ConfidenceThreshold > 1 {
		return nil, fmt.Errorf("%w, got %v", ErrInvalidThreshold, cfg.ConfidenceThreshold)
	}
	if cfg.RadiusMeters <= 0 {
		return nil, fmt.Errorf("%w, got %d", ErrInvalidRadius, cfg.RadiusMeters)
	}
	if cfg.MaxCandidates <= 0 {
		return nil, fmt.Errorf("%w, got %d", ErrInvalidMaxCandidates, cfg.MaxCandidates)
	}

	return &PropertyGrouper{store: store, matcher: matcher, cfg: cfg}, nil
}

// GroupListing devolve o id do imóvel canônico do anúncio e se ele acabou de
// ser criado.
//
// O fluxo, nesta ordem exata (as saídas baratas vêm antes de qualquer gasto):
//
//  1. anúncio já vinculado → devolve o vínculo, sem IA e sem banco;
//  2. sem lat/lng → ErrListingNotGeocoded, sem criar nada;
//  3. busca candidatos no raio configurado e corta em MaxCandidates;
//  4. nenhum candidato → cria e vincula, sem chamar a IA;
//  5. uma **única** chamada de IA comparando o anúncio com todos os candidatos;
//  6. same_property + confiança >= threshold + id pertencente à lista → vincula;
//  7. qualquer outro caso → cria um imóvel novo e vincula.
//
// Erro da IA é propagado sem criar nada: o anúncio continua pendente e a fila
// tenta de novo depois.
func (g *PropertyGrouper) GroupListing(ctx context.Context, listing Listing) (string, bool, error) {
	if listing.ID <= 0 {
		return "", false, ErrMissingListingID
	}

	if linked := strings.TrimSpace(derefString(listing.PropertyID)); linked != "" {
		return linked, false, nil
	}

	if listing.Lat == nil || listing.Lng == nil {
		return "", false, fmt.Errorf("grouping: listing %d: %w", listing.ID, ErrListingNotGeocoded)
	}

	candidates, err := g.store.FindPropertiesByCoordinates(ctx, *listing.Lat, *listing.Lng, g.cfg.RadiusMeters)
	if err != nil {
		return "", false, fmt.Errorf("grouping: finding candidates for listing %d: %w", listing.ID, err)
	}

	// A lista já vem ordenada do mais próximo ao mais distante, então o corte
	// descarta os piores candidatos — e é ele que segura o custo do prompt.
	if len(candidates) > g.cfg.MaxCandidates {
		candidates = candidates[:g.cfg.MaxCandidates]
	}

	if len(candidates) == 0 {
		return g.createAndLink(ctx, listing)
	}

	match, err := g.matcher.MatchProperty(ctx, matchListing(listing), candidates)
	if err != nil {
		return "", false, fmt.Errorf("grouping: matching listing %d against %d candidates: %w", listing.ID, len(candidates), err)
	}

	slog.DebugContext(ctx, "property match decided",
		"listing_id", listing.ID,
		"same_property", match.SameProperty,
		"confidence", match.Confidence,
		"property_id", match.PropertyID,
		"reason", match.Reason,
	)

	if match.SameProperty && match.Confidence >= g.cfg.ConfidenceThreshold {
		if propertyID, ok := candidateID(candidates, match.PropertyID); ok {
			if err := g.store.LinkListingToProperty(ctx, propertyID, listing.ID); err != nil {
				return "", false, fmt.Errorf("grouping: linking listing %d to property %q: %w", listing.ID, propertyID, err)
			}
			return propertyID, false, nil
		}

		// Um id fora da lista nunca pode ir ao banco: derrubaria na FK
		// listings_property_id_fkey, ou — pior — vincularia o anúncio a um
		// imóvel que o modelo nunca viu. Tratar como "sem match" é a saída
		// segura; o warning é o sinal de que o prompt precisa de ajuste.
		slog.WarnContext(ctx, "model returned a property id outside the candidate list",
			"listing_id", listing.ID,
			"property_id", match.PropertyID,
			"candidates", len(candidates),
			"source_domain", listing.SourceDomain,
			"listing_url", listing.ListingURL,
		)
	}

	return g.createAndLink(ctx, listing)
}

// HandleListingRemoval tira um anúncio do seu imóvel canônico e apaga o imóvel
// se ele tiver ficado sem nenhum anúncio. Devolve se o imóvel foi apagado.
//
// É o caminho **unitário**, para desfazer uma deduplicação errada (dois imóveis
// distintos que viraram um). A limpeza em lote do fim de cada coleta não passa
// por aqui: quem apaga os anúncios sumidos do site é db.DeleteStaleListings, e
// ela já decrementa o contador e remove os órfãos na mesma transação.
//
// Os três passos são transações **independentes** (cada função de db abre a
// sua), mesmo precedente de createAndLink. Isso é seguro porque a guarda vive no
// próprio DELETE: se um anúncio novo for vinculado entre a contagem e a
// remoção, o imóvel simplesmente não é apagado e o resultado é um warning.
//
// Anúncio sem vínculo — nunca agrupado ou já inexistente — é no-op sem erro:
// desvincular é limpeza, e falhar nela travaria o reprocessamento.
func (g *PropertyGrouper) HandleListingRemoval(ctx context.Context, listingID int64) (bool, error) {
	if listingID <= 0 {
		return false, ErrMissingListingID
	}

	propertyID, err := g.store.UnlinkListingFromProperty(ctx, listingID)
	if err != nil {
		return false, fmt.Errorf("grouping: unlinking listing %d: %w", listingID, err)
	}
	if strings.TrimSpace(propertyID) == "" {
		return false, nil
	}

	count, err := g.store.GetActiveListingCount(ctx, propertyID)
	if errors.Is(err, db.ErrPropertyNotFound) {
		slog.WarnContext(ctx, "property of the removed listing no longer exists",
			"listing_id", listingID,
			"property_id", propertyID,
		)
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("grouping: counting listings of property %q: %w", propertyID, err)
	}

	if count > 0 {
		return false, nil
	}

	err = g.store.DeleteProperty(ctx, propertyID)
	switch {
	case err == nil:
		slog.InfoContext(ctx, "canonical property deleted after losing its last listing",
			"listing_id", listingID,
			"property_id", propertyID,
		)
		return true, nil
	case errors.Is(err, db.ErrPropertyHasListings), errors.Is(err, db.ErrPropertyNotFound):
		// Outro anúncio foi vinculado entre a contagem e o DELETE, ou o imóvel
		// já tinha sido apagado. Nos dois casos o estado final é o correto.
		slog.WarnContext(ctx, "canonical property was not deleted after the listing removal",
			"listing_id", listingID,
			"property_id", propertyID,
			"error", err,
		)
		return false, nil
	default:
		return false, fmt.Errorf("grouping: deleting property %q of listing %d: %w", propertyID, listingID, err)
	}
}

// createAndLink cria o imóvel canônico a partir do anúncio e o vincula.
//
// Não há compensação transacional entre os dois passos: eles vivem em pacotes
// diferentes e o INSERT já foi commitado quando o vínculo é tentado. Se o Link
// falhar, a property recém-criada fica órfã com active_listing_count = 0 — o id
// vai no log e no erro justamente para que ela possa ser removida com
// db.DeleteProperty.
func (g *PropertyGrouper) createAndLink(ctx context.Context, listing Listing) (string, bool, error) {
	created, err := g.store.CreateProperty(ctx, propertyFrom(listing))
	if err != nil {
		return "", false, fmt.Errorf("grouping: creating property for listing %d: %w", listing.ID, err)
	}

	if err := g.store.LinkListingToProperty(ctx, created.ID, listing.ID); err != nil {
		slog.ErrorContext(ctx, "created property was left orphan: linking the listing failed",
			"listing_id", listing.ID,
			"property_id", created.ID,
			"error", err,
		)
		return "", false, fmt.Errorf("grouping: linking listing %d to the property %q just created: %w", listing.ID, created.ID, err)
	}

	return created.ID, true, nil
}

// candidateID devolve o id do candidato correspondente a returned, na grafia
// que veio do banco. A comparação é EqualFold porque o modelo pode ecoar o UUID
// em outra caixa; devolver o id do candidato (e não o do modelo) garante que o
// valor gravado seja exatamente o que o PostgreSQL emitiu.
func candidateID(candidates []db.Property, returned string) (string, bool) {
	returned = strings.TrimSpace(returned)
	if returned == "" {
		return "", false
	}
	for _, candidate := range candidates {
		if strings.EqualFold(strings.TrimSpace(candidate.ID), returned) {
			return candidate.ID, true
		}
	}
	return "", false
}

// matchListing recorta do anúncio só o que o modelo usa para decidir.
func matchListing(listing Listing) ai.MatchListing {
	return ai.MatchListing{
		AddressRaw:     listing.AddressRaw,
		Neighborhood:   listing.NormalizedNeighborhood,
		AreaRaw:        listing.AreaRaw,
		TitleRaw:       listing.TitleRaw,
		DescriptionRaw: listing.DescriptionRaw,
		BedroomCount:   listing.BedroomCount,
		ImageURLs:      listing.ImageURLs,
	}
}

// propertyFrom monta o imóvel canônico a partir do anúncio.
//
// Campo ausente vira nil, nunca "" nem 0: em db.Property a ausência é
// informação ("não consolidado ainda"), e gravar string vazia faria o próximo
// anúncio comparar-se com um endereço em branco.
//
// AreaSqm, City, State, TransactionType e PropertyType ficam nil: listings não
// tem esses dados. AreaRaw é texto livre do portal ("72,5 m² úteis") e este
// pacote não o parseia — chutar um número aqui contaminaria a comparação de
// todos os anúncios seguintes. Em particular, **a v1 não distingue venda de
// aluguel**: o mesmo imóvel anunciado para venda e para locação vira um único
// canônico.
func propertyFrom(listing Listing) db.Property {
	property := db.Property{
		CanonicalAddress: optionalString(listing.AddressRaw),
		Neighborhood:     optionalString(listing.NormalizedNeighborhood),
		BedroomCount:     listing.BedroomCount,
		Amenities:        listing.Amenities,
		Description:      optionalString(listing.DescriptionRaw),
		Photos:           listing.ImageURLs,
	}

	// Cópias dos valores, e não os ponteiros do chamador: o Listing pode ser
	// reaproveitado pela fila depois desta chamada.
	if listing.Lat != nil && listing.Lng != nil {
		lat, lng := *listing.Lat, *listing.Lng
		property.Lat = &lat
		property.Lng = &lng
	}
	if listing.BedroomCount != nil {
		bedrooms := *listing.BedroomCount
		property.BedroomCount = &bedrooms
	}

	return property
}

// optionalString devolve nil para texto vazio (ou só espaços), preservando a
// distinção entre "não informado" e "informado como vazio".
func optionalString(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
