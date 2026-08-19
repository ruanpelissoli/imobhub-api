package api

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"
)

func TestNewServerSetsAllTimeouts(t *testing.T) {
	srv := NewServer(okHandler())

	timeouts := map[string]time.Duration{
		"ReadHeaderTimeout": srv.ReadHeaderTimeout,
		"ReadTimeout":       srv.ReadTimeout,
		"WriteTimeout":      srv.WriteTimeout,
		"IdleTimeout":       srv.IdleTimeout,
	}
	for name, value := range timeouts {
		if value <= 0 {
			t.Errorf("%s = %v, want > 0 (zero-value deixa a conexão aberta indefinidamente)", name, value)
		}
	}
	if srv.ReadHeaderTimeout > srv.ReadTimeout {
		t.Errorf("ReadHeaderTimeout (%v) não pode ser maior que ReadTimeout (%v)", srv.ReadHeaderTimeout, srv.ReadTimeout)
	}
}

func TestListenFailsOnPortInUse(t *testing.T) {
	first, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v, want nil", err)
	}
	defer first.Close()

	if _, err := Listen(first.Addr().String()); err == nil {
		t.Fatal("Listen() na mesma porta deveria falhar")
	}
}

func TestServeFinishesInFlightRequestBeforeShutdown(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})

	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	ln, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v, want nil", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	served := make(chan error, 1)
	go func() { served <- Serve(ctx, NewServer(handler), ln, discardLogger()) }()

	responses := make(chan *http.Response, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/qualquer")
		if err != nil {
			responses <- nil
			return
		}
		responses <- resp
	}()

	<-started
	// SIGTERM chega no meio de uma requisição: ela precisa terminar antes de o
	// processo sair.
	cancel()
	close(release)

	resp := <-responses
	if resp == nil {
		t.Fatal("a requisição em voo foi cortada pelo shutdown")
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d (corpo: %q)", resp.StatusCode, http.StatusOK, body)
	}

	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve() error = %v, want nil", err)
		}
	case <-time.After(shutdownTimeout + time.Second):
		t.Fatal("Serve() não retornou depois do cancelamento do contexto")
	}
}
