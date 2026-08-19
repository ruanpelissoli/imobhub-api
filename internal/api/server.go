package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// Timeouts do servidor. Todos explícitos: o zero-value do http.Server é "sem
// limite", e uma conexão que envia headers byte a byte seguraria um goroutine
// indefinidamente.
const (
	// ReadHeaderTimeout é o mais curto porque é a defesa contra slowloris.
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 15 * time.Second
	// WriteTimeout cobre a query mais lenta do catálogo. Revisar junto com a
	// paginação: uma listagem grande sem limite estoura esse valor.
	writeTimeout = 30 * time.Second
	idleTimeout  = 60 * time.Second

	// shutdownTimeout é quanto o Shutdown espera as requisições em voo antes de
	// desistir. Acima do WriteTimeout não ajudaria: a requisição já teria morrido.
	shutdownTimeout = 10 * time.Second
)

// Listen abre o socket antes de o servidor começar a atender, para que "porta
// já em uso" seja erro de boot com exit 1 em vez de uma falha assíncrona depois
// que o processo já se declarou pronto.
func Listen(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("api: could not listen on %q: %w", addr, err)
	}
	return ln, nil
}

func NewServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}
}

// Serve atende requisições até ctx ser cancelado (SIGINT/SIGTERM) e então
// encerra graciosamente. Bloqueia até o servidor parar.
func Serve(ctx context.Context, srv *http.Server, ln net.Listener, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}

	served := make(chan error, 1)
	go func() {
		err := srv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		served <- err
	}()

	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}

	logger.Info("api: shutting down", "timeout", shutdownTimeout.String())

	// context.Background() e não ctx: ctx já está cancelado neste ponto, e
	// passá-lo abortaria o Shutdown imediatamente — exatamente o oposto de
	// deixar as requisições em voo terminarem.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("api: graceful shutdown did not finish in time", "error", err)
		return fmt.Errorf("api: graceful shutdown failed: %w", err)
	}

	return <-served
}
