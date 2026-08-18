package robots

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

// newRobotsServer sobe um servidor que responde ao /robots.txt com o handler
// informado e conta quantas vezes ele foi consultado.
func newRobotsServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *atomic.Int64) {
	t.Helper()

	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		hits.Add(1)
		handler(w, r)
	}))
	t.Cleanup(server.Close)

	return server, &hits
}

func serveRobots(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(body))
	}
}

func TestIsAllowedAppliesAgentSpecificRules(t *testing.T) {
	server, _ := newRobotsServer(t, serveRobots(sample))
	checker := NewChecker("ImobHubBot")
	ctx := context.Background()

	allowed, err := checker.IsAllowed(ctx, server.URL+"/imoveis/123")
	if err != nil {
		t.Fatalf("IsAllowed() error = %v, want nil", err)
	}
	if !allowed {
		t.Error("IsAllowed(/imoveis/123) = false, want true")
	}

	allowed, err = checker.IsAllowed(ctx, server.URL+"/admin/painel")
	if err != nil {
		t.Fatalf("IsAllowed() error = %v, want nil", err)
	}
	if allowed {
		t.Error("IsAllowed(/admin/painel) = true, want false")
	}

	// Outro agente cai no bloco "*", que proíbe tudo.
	other := NewChecker("OutroBot")
	allowed, err = other.IsAllowed(ctx, server.URL+"/imoveis/123")
	if err != nil {
		t.Fatalf("IsAllowed() error = %v, want nil", err)
	}
	if allowed {
		t.Error("IsAllowed(OutroBot, /imoveis/123) = true, want false")
	}
}

func TestIsAllowedFetchesRobotsOncePerHost(t *testing.T) {
	server, hits := newRobotsServer(t, serveRobots(sample))
	checker := NewChecker("ImobHubBot")
	ctx := context.Background()

	for _, path := range []string{"/imoveis/1", "/imoveis/2", "/admin/x"} {
		if _, err := checker.IsAllowed(ctx, server.URL+path); err != nil {
			t.Fatalf("IsAllowed(%q) error = %v, want nil", path, err)
		}
	}

	if got := hits.Load(); got != 1 {
		t.Errorf("robots.txt requests = %d, want 1", got)
	}
}

func TestIsAllowedCachesPerHost(t *testing.T) {
	blocked, _ := newRobotsServer(t, serveRobots("User-agent: *\nDisallow: /\n"))
	open, _ := newRobotsServer(t, serveRobots("User-agent: *\nDisallow:\n"))
	checker := NewChecker("ImobHubBot")
	ctx := context.Background()

	allowed, err := checker.IsAllowed(ctx, blocked.URL+"/imoveis/1")
	if err != nil {
		t.Fatalf("IsAllowed() error = %v, want nil", err)
	}
	if allowed {
		t.Error("IsAllowed(host bloqueado) = true, want false")
	}

	// O cache é por origem: o segundo host não pode herdar as regras do primeiro.
	allowed, err = checker.IsAllowed(ctx, open.URL+"/imoveis/1")
	if err != nil {
		t.Fatalf("IsAllowed() error = %v, want nil", err)
	}
	if !allowed {
		t.Error("IsAllowed(host liberado) = false, want true")
	}
}

func TestIsAllowedAssumesAllowedOnNotFound(t *testing.T) {
	server, hits := newRobotsServer(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})
	checker := NewChecker("ImobHubBot")
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		allowed, err := checker.IsAllowed(ctx, server.URL+"/imoveis/1")
		if err != nil {
			t.Fatalf("IsAllowed() error = %v, want nil", err)
		}
		if !allowed {
			t.Error("IsAllowed() com robots.txt 404 = false, want true")
		}
	}

	// A decisão permissiva também é cacheada: nada de repetir o 404.
	if got := hits.Load(); got != 1 {
		t.Errorf("robots.txt requests = %d, want 1", got)
	}
}

func TestIsAllowedAssumesAllowedOnServerError(t *testing.T) {
	server, _ := newRobotsServer(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	checker := NewChecker("ImobHubBot")

	allowed, err := checker.IsAllowed(context.Background(), server.URL+"/imoveis/1")
	if err != nil {
		t.Fatalf("IsAllowed() error = %v, want nil", err)
	}
	if !allowed {
		t.Error("IsAllowed() com robots.txt 500 = false, want true")
	}
}

func TestIsAllowedAssumesAllowedOnNetworkFailure(t *testing.T) {
	server, _ := newRobotsServer(t, serveRobots(sample))
	target := server.URL
	server.Close() // ninguém escutando: a busca falha na conexão.

	checker := NewChecker("ImobHubBot")

	// Mesmo um path que o robots.txt proibiria passa: sem regras avaliáveis, a
	// decisão do projeto é seguir em frente.
	allowed, err := checker.IsAllowed(context.Background(), target+"/admin/painel")
	if err != nil {
		t.Fatalf("IsAllowed() error = %v, want nil", err)
	}
	if !allowed {
		t.Error("IsAllowed() com host inacessível = false, want true")
	}
}

func TestIsAllowedRejectsInvalidURLs(t *testing.T) {
	checker := NewChecker("ImobHubBot")
	ctx := context.Background()

	cases := map[string]string{
		"sem host":        "/imoveis/123",
		"scheme inválido": "ftp://exemplo.com.br/imoveis",
		"url malformada":  "http://exemplo.com.br/%zz",
		"string vazia":    "",
	}

	for name, rawURL := range cases {
		t.Run(name, func(t *testing.T) {
			allowed, err := checker.IsAllowed(ctx, rawURL)
			if err == nil {
				t.Fatalf("IsAllowed(%q) error = nil, want error", rawURL)
			}
			if allowed {
				t.Errorf("IsAllowed(%q) = true, want false on error", rawURL)
			}
		})
	}
}

func TestIsAllowedIsSafeForConcurrentUse(t *testing.T) {
	server, _ := newRobotsServer(t, serveRobots(sample))
	checker := NewChecker("ImobHubBot")
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := checker.IsAllowed(ctx, server.URL+"/imoveis/1"); err != nil {
				t.Errorf("IsAllowed() error = %v, want nil", err)
			}
		}()
	}
	wg.Wait()
}
