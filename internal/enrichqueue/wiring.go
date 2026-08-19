package enrichqueue

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/imobhub/api/internal/ai"
	"github.com/imobhub/api/internal/config"
	"github.com/imobhub/api/internal/db"
	"github.com/imobhub/api/internal/enrichment"
	"github.com/imobhub/api/internal/grouping"
)

// RunEnrichment monta o pipeline com o wiring de produção e drena a fila de
// enriquecimento.
//
// A montagem mora aqui, e não em cmd/scraper, pelo mesmo motivo de
// scraper.RunPipeline: a assinatura (ctx, cfg, pool) não tem por onde receber
// módulos prontos, e cmd/scraper/CLAUDE.md determina que nenhuma regra de
// negócio viva no main.
//
// O erro retornado é reservado a falhas fatais (wiring incompleto, leitura da
// fila impossível, contexto cancelado). Anúncio que falha **não** vira erro do
// run: ele é logado, contado no resumo e a fila segue para o próximo.
func RunEnrichment(ctx context.Context, cfg *config.Config, pool *pgxpool.Pool) error {
	pipeline, err := NewPipeline(cfg, pool)
	if err != nil {
		return err
	}

	// O resumo é descartado de propósito: ele já foi logado por Run, e a
	// assinatura exigida pelo binário é só "deu certo ou não". Quem precisar dos
	// números em memória (um teste, um futuro endpoint) usa Run direto.
	_, err = pipeline.Run(ctx)
	return err
}

// NewPipeline monta o pipeline com as dependências de produção.
//
// **Cada colaborador nasce uma única vez e é compartilhado por todos os
// workers**, através dos closures de Deps. Isso é obrigatório, não estilo:
//
//   - o Geocoder carrega o ratelimit.DomainLimiter (a política de 1 req/s do
//     Nominatim) e o cache com negative caching. N geocoders seriam N relógios
//     independentes — N req/s — e o User-Agent seria bloqueado; é a mesma
//     invariante do "um DomainLimiter por run" de scraper.NewPipeline;
//   - o TermExtractor lê o vocabulário de comodidades do disco no construtor,
//     e reconstruí-lo por anúncio seria um open/parse por anúncio;
//   - o NeighborhoodNormalizer compila a tabela de aliases embutida uma vez
//     (o struct resultante é imutável e seguro para uso concorrente);
//   - o PropertyGrouper é stateless mas carrega o cliente da Anthropic, cujo
//     http.Client reusa conexões — um por worker desperdiçaria o pool TLS.
func NewPipeline(cfg *config.Config, pool *pgxpool.Pool) (*Pipeline, error) {
	if cfg == nil {
		return nil, ErrMissingConfig
	}
	if pool == nil {
		return nil, ErrMissingPool
	}

	normalizer, err := enrichment.NewNeighborhoodNormalizer()
	if err != nil {
		return nil, err
	}

	extractor, err := enrichment.NewTermExtractor(cfg.AmenitiesFile)
	if err != nil {
		return nil, err
	}

	geocoder, err := enrichment.NewGeocoder(
		cfg.GeocodingProvider,
		cfg.GeocodingAPIKey,
		cfg.ScraperUserAgent,
		int(cfg.GeocodingRateLimit.Milliseconds()),
	)
	if err != nil {
		return nil, err
	}

	store, err := grouping.NewPoolStore(pool)
	if err != nil {
		return nil, err
	}

	matcher, err := ai.New(cfg.AnthropicAPIKey)
	if err != nil {
		return nil, err
	}

	grouper, err := grouping.NewPropertyGrouper(store, matcher, grouping.Config{
		ConfidenceThreshold: cfg.GroupingConfidenceThreshold,
		RadiusMeters:        cfg.GroupingRadiusMeters,
		MaxCandidates:       cfg.GroupingMaxCandidates,
	})
	if err != nil {
		return nil, err
	}

	return New(Deps{
		ListPending: func(ctx context.Context, afterID int64, limit int) ([]db.PendingListing, error) {
			return db.ListPendingListings(ctx, pool, afterID, limit)
		},
		NormalizeNeighborhood: normalizer.Normalize,
		ExtractTerms:          extractor.Extract,
		Geocode:               geocoder.Geocode,
		SaveEnrichment: func(ctx context.Context, enriched db.ListingEnrichment) error {
			return db.UpdateListingEnrichment(ctx, pool, enriched)
		},
		GroupListing:  grouper.GroupListing,
		MergeProperty: grouper.MergePropertyData,
		MarkEnriched: func(ctx context.Context, listingID int64) error {
			return db.MarkListingEnriched(ctx, pool, listingID)
		},

		Workers:   cfg.EnrichmentWorkers,
		BatchSize: DefaultBatchSize,
	})
}
