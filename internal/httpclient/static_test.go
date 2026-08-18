package httpclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/text/encoding/charmap"
)

func newServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func TestFetchStaticReturnsBodyAndSendsUserAgent(t *testing.T) {
	var gotUA string
	server := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html><body>Imóvel</body></html>"))
	})

	html, finalURL, err := FetchStatic(context.Background(), server.URL+"/imoveis/1", "ImobHubBot/1.0")
	if err != nil {
		t.Fatalf("FetchStatic() error = %v, want nil", err)
	}
	if want := "<html><body>Imóvel</body></html>"; html != want {
		t.Errorf("html = %q, want %q", html, want)
	}
	if want := server.URL + "/imoveis/1"; finalURL != want {
		t.Errorf("finalURL = %q, want %q", finalURL, want)
	}
	if gotUA != "ImobHubBot/1.0" {
		t.Errorf("User-Agent = %q, want %q", gotUA, "ImobHubBot/1.0")
	}
}

// Nenhum header além do User-Agent deve ser enviado: cada header extra é mais
// um bit de fingerprint. Accept-Encoding é a exceção, setado pelo net/http.
func TestFetchStaticSendsOnlyUserAgentAmongFingerprintHeaders(t *testing.T) {
	var got http.Header
	server := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte("ok"))
	})

	if _, _, err := FetchStatic(context.Background(), server.URL, "ImobHubBot/1.0"); err != nil {
		t.Fatalf("FetchStatic() error = %v, want nil", err)
	}

	for _, header := range []string{"Accept-Language", "Accept", "DNT", "Referer", "Cookie"} {
		if v := got.Get(header); v != "" {
			t.Errorf("header %s = %q, want empty", header, v)
		}
	}
	if enc := got.Get("Accept-Encoding"); !strings.Contains(enc, "gzip") {
		t.Errorf("Accept-Encoding = %q, want it to advertise gzip", enc)
	}
}

func TestFetchStaticFollowsRedirectsAndReportsFinalURL(t *testing.T) {
	var server *httptest.Server
	server = newServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/velho":
			http.Redirect(w, r, server.URL+"/novo", http.StatusMovedPermanently)
		case "/novo":
			_, _ = w.Write([]byte("<html>novo</html>"))
		default:
			http.NotFound(w, r)
		}
	})

	html, finalURL, err := FetchStatic(context.Background(), server.URL+"/velho", "ImobHubBot/1.0")
	if err != nil {
		t.Fatalf("FetchStatic() error = %v, want nil", err)
	}
	if html != "<html>novo</html>" {
		t.Errorf("html = %q, want the redirected page", html)
	}
	if want := server.URL + "/novo"; finalURL != want {
		t.Errorf("finalURL = %q, want %q", finalURL, want)
	}
}

func TestFetchStaticStopsAfterTooManyRedirects(t *testing.T) {
	var server *httptest.Server
	server = newServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, fmt.Sprintf("%s/loop?n=%s", server.URL, r.URL.Query().Get("n")+"x"), http.StatusFound)
	})

	_, _, err := FetchStatic(context.Background(), server.URL+"/loop", "ImobHubBot/1.0")
	if err == nil {
		t.Fatal("FetchStatic() error = nil, want a redirect-limit error")
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("%d redirects", MaxRedirects)) {
		t.Errorf("error = %v, want it to mention the %d redirect limit", err, MaxRedirects)
	}
}

func TestFetchStaticReturnsStatusErrorFor4xxAnd5xx(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusForbidden, http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			server := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(code)
				_, _ = w.Write([]byte("<html>erro</html>"))
			})

			html, finalURL, err := FetchStatic(context.Background(), server.URL+"/imovel", "ImobHubBot/1.0")
			if err == nil {
				t.Fatalf("FetchStatic() error = nil, want an error for HTTP %d", code)
			}
			if html != "" {
				t.Errorf("html = %q, want empty on error", html)
			}
			if want := server.URL + "/imovel"; finalURL != want {
				t.Errorf("finalURL = %q, want %q even on error", finalURL, want)
			}

			var statusErr *StatusError
			if !errors.As(err, &statusErr) {
				t.Fatalf("error = %v, want a *StatusError", err)
			}
			if statusErr.StatusCode != code {
				t.Errorf("StatusCode = %d, want %d", statusErr.StatusCode, code)
			}
			if !strings.Contains(err.Error(), fmt.Sprint(code)) || !strings.Contains(err.Error(), server.URL+"/imovel") {
				t.Errorf("error = %q, want it to contain the status code and the URL", err)
			}
			if !IsStatus(err, code) {
				t.Errorf("IsStatus(err, %d) = false, want true", code)
			}
		})
	}
}

// Corpo vazio não é erro: a detecção de charset não tem bytes para inspecionar,
// mas a requisição em si foi bem-sucedida.
func TestFetchStaticAcceptsEmptyBody(t *testing.T) {
	server := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	html, _, err := FetchStatic(context.Background(), server.URL, "ImobHubBot/1.0")
	if err != nil {
		t.Fatalf("FetchStatic() error = %v, want nil for a 2xx", err)
	}
	if html != "" {
		t.Errorf("html = %q, want empty", html)
	}
}

// Portais antigos ainda servem ISO-8859-1; ler esses bytes como UTF-8
// corromperia justamente os acentos dos campos extraídos.
func TestFetchStaticDecodesISO88591(t *testing.T) {
	const utf8Body = `<html><head><title>Apartamento em São Paulo</title></head><body>Três dormitórios, área útil</body></html>`

	tests := []struct {
		name        string
		contentType string
		meta        string
	}{
		{name: "charset no Content-Type", contentType: "text/html; charset=ISO-8859-1"},
		{name: "charset na meta tag", contentType: "text/html", meta: `<meta charset="ISO-8859-1">`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := utf8Body
			if tt.meta != "" {
				body = strings.Replace(utf8Body, "<head>", "<head>"+tt.meta, 1)
			}
			encoded, err := charmap.ISO8859_1.NewEncoder().String(body)
			if err != nil {
				t.Fatalf("could not build the ISO-8859-1 fixture: %v", err)
			}

			server := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tt.contentType)
				_, _ = w.Write([]byte(encoded))
			})

			html, _, err := FetchStatic(context.Background(), server.URL, "ImobHubBot/1.0")
			if err != nil {
				t.Fatalf("FetchStatic() error = %v, want nil", err)
			}
			if html != body {
				t.Errorf("html = %q, want it decoded to UTF-8 as %q", html, body)
			}
		})
	}
}

func TestFetchStaticRespectsCanceledContext(t *testing.T) {
	server := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>ok</html>"))
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := FetchStatic(ctx, server.URL, "ImobHubBot/1.0")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("FetchStatic() error = %v, want context.Canceled", err)
	}
}

func TestFetchStaticRejectsInvalidURL(t *testing.T) {
	_, _, err := FetchStatic(context.Background(), "://sem-esquema", "ImobHubBot/1.0")
	if err == nil {
		t.Fatal("FetchStatic() error = nil, want an error for a malformed URL")
	}
	if !strings.Contains(err.Error(), "httpclient:") {
		t.Errorf("error = %q, want it prefixed with the package name", err)
	}
}
