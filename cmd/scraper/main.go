// Command scraper é o ponto de entrada do coletor de imóveis do ImobHub.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/imobhub/api/internal/config"
	"github.com/imobhub/api/internal/db"
	"github.com/imobhub/api/internal/ratelimit"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	if err := run(); err != nil {
		// Erros de inicialização vão para o logger, não para panic: a mensagem
		// estruturada é o que o operador vê nos logs do container.
		slog.Error("scraper failed to start", "error", err)
		os.Exit(1)
	}
}

// run concentra a inicialização para que os defers rodem antes do os.Exit em
// main — um os.Exit direto os ignoraria, deixando o pool de conexões aberto.
func run() error {
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

	// Uma única instância compartilhada por todo o pipeline: o espaçamento por
	// domínio só funciona se todas as requisições passarem pelo mesmo limiter.
	limiter := ratelimit.NewDomainLimiter(int(cfg.ScraperRateLimit.Milliseconds()))

	slog.Info("imobhub scraper started",
		"sources_file", cfg.SourcesFile,
		"user_agent", cfg.ScraperUserAgent,
		"rate_limit", limiter.Interval().String(),
	)

	return nil
}
