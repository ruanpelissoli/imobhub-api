// Command scraper é o ponto de entrada do coletor de imóveis do ImobHub.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/imobhub/api/internal/cache"
	"github.com/imobhub/api/internal/config"
	"github.com/imobhub/api/internal/db"
	"github.com/imobhub/api/internal/enrichqueue"
	"github.com/imobhub/api/internal/scraper"
)

// Etapas aceitas em -only. O default é stageAll: uma execução diária coleta e,
// em seguida, enriquece o que foi coletado. As outras duas existem para rodar
// uma etapa isolada (reprocessar a fila sem raspar nada, ou coletar sem gastar
// chamadas de IA no agrupamento).
const (
	stageAll     = "all"
	stageScrape  = "scrape"
	stageEnrich  = "enrich"
	onlyFlagName = "only"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	only := flag.String(onlyFlagName, stageAll,
		fmt.Sprintf("etapa a executar: %q (coleta), %q (fila de enriquecimento) ou %q", stageScrape, stageEnrich, stageAll))
	flag.Parse()

	if err := run(*only); err != nil {
		// Erros de inicialização vão para o logger, não para panic: a mensagem
		// estruturada é o que o operador vê nos logs do container.
		slog.Error("scraper failed to start", "error", err)
		os.Exit(1)
	}
}

// run concentra a inicialização para que os defers rodem antes do os.Exit em
// main — um os.Exit direto os ignoraria, deixando o pool de conexões aberto.
func run(only string) error {
	stage := strings.ToLower(strings.TrimSpace(only))
	switch stage {
	case stageAll, stageScrape, stageEnrich:
	default:
		// Valor desconhecido é erro de boot, não fallback silencioso: quem
		// digitou "-only=enrichment" esperava só o enriquecimento e receberia,
		// sem aviso, uma coleta completa.
		return fmt.Errorf("main: -%s must be one of %q, %q or %q, got %q",
			onlyFlagName, stageScrape, stageEnrich, stageAll, only)
	}

	// O contexto é cancelado em SIGINT/SIGTERM para que o encerramento do pool
	// e das coletas em andamento seja ordenado.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	// As migrations rodam antes de qualquer coleta: o pipeline pressupõe que as
	// tabelas site_selectors e listings existem no formato desta versão.
	if err := db.RunMigrations(ctx, pool, cfg.MigrationsDir); err != nil {
		return err
	}

	// O Redis é validado no boot, como o banco: um cache inalcançável
	// descoberto no meio da coleta é mais caro de diagnosticar do que uma falha
	// de inicialização. Ainda não há consumidor do client — a decisão de falhar
	// (em vez de degradar sem cache) está registrada em cmd/scraper/CLAUDE.md.
	redisClient, err := cache.New(ctx, cfg.RedisURL)
	if err != nil {
		return err
	}
	defer redisClient.Close()

	slog.Info("imobhub scraper started",
		"stage", stage,
		"sources_file", cfg.SourcesFile,
		"user_agent", cfg.ScraperUserAgent,
		"rate_limit", cfg.ScraperRateLimit.String(),
		"enrichment_workers", cfg.EnrichmentWorkers,
	)

	return runStage(ctx, stage, cfg, pool)
}

// runStage despacha a etapa escolhida. É dispatch de CLI, não regra de negócio:
// a montagem dos módulos de cada etapa mora em scraper.RunPipeline e
// enrichqueue.RunEnrichment.
//
// Em stageAll a ordem é fixa: coleta e, **só em caso de sucesso**, o
// enriquecimento. Enriquecer sobre uma coleta interrompida no meio processaria
// um catálogo que ainda vai mudar no mesmo dia, gastando IA duas vezes.
func runStage(ctx context.Context, stage string, cfg *config.Config, pool *pgxpool.Pool) error {
	if stage == stageEnrich {
		return enrichqueue.RunEnrichment(ctx, cfg, pool)
	}

	if err := scraper.RunPipeline(ctx, cfg, pool); err != nil {
		return err
	}
	if stage == stageScrape {
		return nil
	}

	if err := enrichqueue.RunEnrichment(ctx, cfg, pool); err != nil {
		// A coleta já foi persistida: o exit code 1 daqui **não** significa que
		// os anúncios se perderam, e sim que a fila de enriquecimento parou. Ela
		// é retomável — os anúncios sem enriched_at voltam no próximo run.
		slog.Error("a coleta foi persistida, mas o enriquecimento falhou; os anúncios pendentes serão reprocessados no próximo run",
			"error", err,
		)
		return err
	}

	return nil
}
