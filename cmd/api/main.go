// Command api é o servidor HTTP que serve o catálogo consolidado do ImobHub.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/imobhub/api/internal/api"
	"github.com/imobhub/api/internal/cache"
	"github.com/imobhub/api/internal/config"
	"github.com/imobhub/api/internal/db"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		// Erros de inicialização vão para o logger, não para panic: a mensagem
		// estruturada é o que o operador vê nos logs do container.
		slog.Error("api failed to start", "error", err)
		os.Exit(1)
	}
}

// run concentra a inicialização para que os defers rodem antes do os.Exit em
// main — um os.Exit direto os ignoraria, deixando o pool de conexões aberto.
func run(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.LoadAPI()
	if err != nil {
		return err
	}

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	redisClient, err := cache.New(ctx, cfg.RedisURL)
	if err != nil {
		return err
	}
	defer redisClient.Close()

	// A API não aplica migrations: quem é dono do schema é o scraper. Uma API de
	// leitura que migra no boot mudaria o schema a cada deploy dela.
	router := api.NewRouter(api.Deps{
		Pool:         pool,
		Redis:        redisClient,
		CORSOrigins:  cfg.CORSOrigins,
		RateLimitRPM: cfg.RateLimitRPM,
		Logger:       logger,
	})

	ln, err := api.Listen(fmt.Sprintf(":%d", cfg.Port))
	if err != nil {
		return err
	}

	logger.Info("imobhub api started",
		"addr", ln.Addr().String(),
		"port", cfg.Port,
		"cors_origins", cfg.CORSOrigins,
		"rate_limit_rpm", cfg.RateLimitRPM,
	)

	// Bloqueia até SIGINT/SIGTERM; os defers acima fecham pool e Redis só depois
	// do Shutdown, com as requisições em voo já concluídas.
	return api.Serve(ctx, api.NewServer(router), ln, logger)
}
