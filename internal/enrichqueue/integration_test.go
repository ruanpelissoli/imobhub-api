package enrichqueue

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/imobhub/api/internal/ai"
	"github.com/imobhub/api/internal/db"
	"github.com/imobhub/api/internal/enrichment"
	"github.com/imobhub/api/internal/grouping"
)

// Teste de integração contra o PostgreSQL do docker-compose (serviço `db`).
//
// Não há testcontainers aqui de propósito: a ausência é decisão registrada em
// internal/db/CLAUDE.md, e a stack local já sobe um Postgres. O teste é pulado
// quando `-short` está ligado ou DATABASE_URL não está definida — ler o ambiente
// num arquivo _test.go é aceitável; a regra "os.Getenv só em internal/config"
// vale para o código de produção.
//
// A rede e a IA continuam fora: o geocoder é um stub de coordenadas fixas e o
// PropertyMatcher é um stub que sempre responde "mesmo imóvel". O que este teste
// exercita é o SQL de verdade — o predicado da fila, o UPDATE que não toca
// updated_at, o vínculo e o contador denormalizado.
const (
	// testSourceDomain isola as linhas deste teste; o cleanup apaga por ele.
	testSourceDomain = "enrichqueue-integration.test"
	// testLat e testLng são o ponto fixo devolvido pelo geocoder stub. Os dois
	// anúncios caem exatamente no mesmo lugar, que é a premissa do agrupamento.
	testLat = -15.7942
	testLng = -47.8822
)

func TestEnrichmentPipelineGroupsTwoListingsIntoOneProperty(t *testing.T) {
	pool := integrationPool(t)

	seedListings(t, pool)

	grouper, err := grouping.NewPropertyGrouper(mustStore(t, pool), alwaysSamePropertyMatcher{}, grouping.Config{
		ConfidenceThreshold: 0.85,
		RadiusMeters:        100,
		MaxCandidates:       5,
	})
	if err != nil {
		t.Fatalf("NewPropertyGrouper() error = %v, want nil", err)
	}

	normalizer, err := enrichment.NewNeighborhoodNormalizer()
	if err != nil {
		t.Fatalf("NewNeighborhoodNormalizer() error = %v, want nil", err)
	}

	pipeline, err := New(Deps{
		ListPending: func(ctx context.Context, afterID int64, limit int) ([]db.PendingListing, error) {
			return db.ListPendingListings(ctx, pool, afterID, limit)
		},
		NormalizeNeighborhood: normalizer.Normalize,
		ExtractTerms: func(string) enrichment.ExtractedData {
			bedrooms := 3
			return enrichment.ExtractedData{BedroomCount: &bedrooms, Amenities: []string{"piscina"}}
		},
		Geocode: func(context.Context, string, string, string) (float64, float64, error) {
			return testLat, testLng, nil
		},
		SaveEnrichment: func(ctx context.Context, enriched db.ListingEnrichment) error {
			return db.UpdateListingEnrichment(ctx, pool, enriched)
		},
		GroupListing:  grouper.GroupListing,
		MergeProperty: grouper.MergePropertyData,
		MarkEnriched: func(ctx context.Context, listingID int64) error {
			return db.MarkListingEnriched(ctx, pool, listingID)
		},
		// Um worker de propósito: com dois, ambos os anúncios buscariam
		// candidatos antes de qualquer property existir e cada um criaria o seu.
		// A corrida é conhecida e documentada — este teste verifica o
		// agrupamento, não a concorrência (que tem teste próprio com fakes).
		Workers:   1,
		BatchSize: 10,
	})
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	summary, err := pipeline.Run(ctx)
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if got, want := summary.Enriched, 2; got != want {
		t.Fatalf("summary.Enriched = %d, want %d (summary: %+v)", got, want, summary)
	}

	assertOneGroupedProperty(t, ctx, pool)

	// A fila precisa vir vazia num segundo passe: se o UPDATE de enriquecimento
	// tocasse updated_at, os mesmos anúncios voltariam aqui para sempre.
	pending, err := db.ListPendingListings(ctx, pool, 0, 10)
	if err != nil {
		t.Fatalf("ListPendingListings() error = %v, want nil", err)
	}
	for _, listing := range pending {
		if listing.SourceDomain == testSourceDomain {
			t.Fatalf("anúncio %d voltou à fila depois de enriquecido", listing.ID)
		}
	}
}

// assertOneGroupedProperty verifica o estado final: um único imóvel canônico,
// com os dois anúncios vinculados e o contador denormalizado coerente.
func assertOneGroupedProperty(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()

	var (
		properties int
		propertyID string
		count      int
	)
	err := pool.QueryRow(ctx, `
		SELECT COUNT(DISTINCT property_id), MIN(property_id::text)
		FROM listings WHERE source_domain = $1`, testSourceDomain).Scan(&properties, &propertyID)
	if err != nil {
		t.Fatalf("contagem de properties: %v", err)
	}
	if properties != 1 {
		t.Fatalf("os anúncios ficaram em %d imóveis canônicos, want 1", properties)
	}

	if err := pool.QueryRow(ctx, `SELECT active_listing_count FROM properties WHERE id = $1::uuid`, propertyID).Scan(&count); err != nil {
		t.Fatalf("leitura do contador: %v", err)
	}
	if count != 2 {
		t.Errorf("active_listing_count = %d, want 2", count)
	}

	var unstamped int
	err = pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM listings
		WHERE source_domain = $1 AND enriched_at IS NULL`, testSourceDomain).Scan(&unstamped)
	if err != nil {
		t.Fatalf("contagem de enriched_at: %v", err)
	}
	if unstamped != 0 {
		t.Errorf("%d anúncios ficaram sem enriched_at, want 0", unstamped)
	}
}

// integrationPool conecta ao Postgres do compose, aplica as migrations e
// registra a limpeza. Pula o teste quando o ambiente não oferece o banco.
func integrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	if testing.Short() {
		t.Skip("teste de integração pulado em -short")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL não definida: suba o Postgres do docker-compose (`docker compose up -d db`)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := db.Connect(ctx, databaseURL)
	if err != nil {
		t.Skipf("Postgres indisponível na DATABASE_URL: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := db.RunMigrations(ctx, pool, "../../migrations"); err != nil {
		t.Fatalf("RunMigrations() error = %v, want nil", err)
	}

	cleanup(t, pool)
	t.Cleanup(func() { cleanup(t, pool) })

	return pool
}

// cleanup remove tudo que este teste cria: os anúncios do domínio de teste e os
// imóveis canônicos que ficarem sem nenhum anúncio depois disso.
func cleanup(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if _, err := pool.Exec(ctx, `
		DELETE FROM properties WHERE id IN (
			SELECT DISTINCT property_id FROM listings
			WHERE source_domain = $1 AND property_id IS NOT NULL
		)`, testSourceDomain); err != nil {
		t.Fatalf("limpeza das properties: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM listings WHERE source_domain = $1`, testSourceDomain); err != nil {
		t.Fatalf("limpeza dos listings: %v", err)
	}
}

// seedListings grava dois anúncios do mesmo imóvel, em portais diferentes — o
// cenário que o agrupamento existe para resolver.
func seedListings(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	listings := []db.RawListing{
		{
			SourceDomain:   testSourceDomain,
			ListingURL:     "https://portal-a.test/imovel/1",
			TitleRaw:       "Apartamento 3 quartos no Jardim Botânico",
			PriceRaw:       "R$ 850.000",
			AddressRaw:     "SQN 210, Bloco A, Asa Norte",
			DescriptionRaw: "Apartamento de 3 quartos com piscina e vista livre.",
			BedroomsRaw:    "3",
			AreaRaw:        "98 m²",
			ImageURLs:      []string{"https://cdn.portal-a.test/1.jpg"},
		},
		{
			SourceDomain:   testSourceDomain,
			ListingURL:     "https://portal-b.test/anuncio/99",
			TitleRaw:       "Apto 3 qtos Asa Norte",
			PriceRaw:       "R$ 849.000",
			AddressRaw:     "SQN 210 Bloco A - Asa Norte",
			DescriptionRaw: "Três quartos, piscina no condomínio, andar alto.",
			BedroomsRaw:    "3 quartos",
			AreaRaw:        "98m2",
			ImageURLs:      []string{"https://cdn.portal-b.test/99.jpg"},
		},
	}

	if err := db.UpsertListings(ctx, pool, listings); err != nil {
		t.Fatalf("UpsertListings() error = %v, want nil", err)
	}
}

// mustStore adapta as funções de db à interface do agrupamento.
func mustStore(t *testing.T, pool *pgxpool.Pool) grouping.PropertyStore {
	t.Helper()

	store, err := grouping.NewPoolStore(pool)
	if err != nil {
		t.Fatalf("NewPoolStore() error = %v, want nil", err)
	}
	return store
}

// alwaysSamePropertyMatcher substitui a chamada paga à Anthropic: aponta sempre
// o primeiro candidato com confiança máxima. O que este teste exercita é o SQL
// do agrupamento, não a qualidade da decisão do modelo.
type alwaysSamePropertyMatcher struct{}

func (alwaysSamePropertyMatcher) MatchProperty(_ context.Context, _ ai.MatchListing, candidates []db.Property) (ai.PropertyMatch, error) {
	if len(candidates) == 0 {
		return ai.PropertyMatch{}, nil
	}
	return ai.PropertyMatch{
		SameProperty: true,
		PropertyID:   candidates[0].ID,
		Confidence:   1,
		Reason:       "stub de integração",
	}, nil
}
