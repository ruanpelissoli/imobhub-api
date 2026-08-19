package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// capturingHandler guarda os registros emitidos para que o teste inspecione os
// campos da linha de log sem depender do formato de saída.
type capturingHandler struct {
	slog.Handler
	records []slog.Record
}

func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.records = append(h.records, r)
	return nil
}

func TestRecoveryTurnsPanicIntoJSONAndKeepsServing(t *testing.T) {
	var calls int
	handler := recovery(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			panic("boom")
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}), discardLogger())

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/qualquer", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if got := rec.Header().Get(contentTypeHeader); got != contentTypeJSON {
		t.Errorf("Content-Type = %q, want %q", got, contentTypeJSON)
	}
	if got := decodeError(t, rec.Body.Bytes()); got != msgInternalError {
		t.Errorf("error = %q, want %q", got, msgInternalError)
	}
	if strings.Contains(rec.Body.String(), "boom") {
		t.Error("o corpo vazou o valor do panic")
	}

	// A requisição seguinte prova que o panic não derrubou o handler.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/qualquer", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status da segunda requisição = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestRecoveryKeepsStatusWhenResponseAlreadyStarted(t *testing.T) {
	handler := recovery(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"parcial": "1"})
		panic("depois de responder")
	}), discardLogger())

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/qualquer", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestLoggingEmitsOneLinePerRequest(t *testing.T) {
	capture := &capturingHandler{Handler: slog.NewJSONHandler(io.Discard, nil)}
	logger := slog.New(capture)

	handler := logging(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusTeapot, map[string]string{"status": "ok"})
	}), logger)

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/exemplo", nil))

	if len(capture.records) != 1 {
		t.Fatalf("linhas de log = %d, want 1", len(capture.records))
	}

	attrs := map[string]slog.Value{}
	capture.records[0].Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value
		return true
	})

	for _, key := range []string{"method", "path", "status", "duration_ms", "bytes"} {
		if _, ok := attrs[key]; !ok {
			t.Errorf("campo %q ausente na linha de log", key)
		}
	}
	if got := attrs["method"].String(); got != http.MethodPost {
		t.Errorf("method = %q, want %q", got, http.MethodPost)
	}
	if got := attrs["path"].String(); got != "/api/v1/exemplo" {
		t.Errorf("path = %q, want %q", got, "/api/v1/exemplo")
	}
	if got := attrs["status"].Int64(); got != int64(http.StatusTeapot) {
		t.Errorf("status = %d, want %d", got, http.StatusTeapot)
	}
	if got := attrs["bytes"].Int64(); got <= 0 {
		t.Errorf("bytes = %d, want > 0", got)
	}
}

func TestCORSAllowsListedOrigin(t *testing.T) {
	handler := cors(okHandler(), []string{"http://localhost:3000", "https://imobhub.com.br"})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/exemplo", nil)
	req.Header.Set(headerOrigin, "https://imobhub.com.br")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get(headerAllowOrigin); got != "https://imobhub.com.br" {
		t.Errorf("%s = %q, want a origem ecoada", headerAllowOrigin, got)
	}
	if got := rec.Header().Get(headerVary); got != headerOrigin {
		t.Errorf("%s = %q, want %q", headerVary, got, headerOrigin)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	// Sem Expose-Headers o browser esconde o X-Cache e o front não consegue nem
	// medir o hit rate da busca.
	if got := rec.Header().Get(headerExposeHeaders); got != corsExposeHeaders {
		t.Errorf("%s = %q, want %q", headerExposeHeaders, got, corsExposeHeaders)
	}
	if !strings.Contains(corsExposeHeaders, headerCacheStatus) {
		t.Errorf("corsExposeHeaders = %q, want conter %q", corsExposeHeaders, headerCacheStatus)
	}
}

func TestCORSIgnoresUnlistedOrigin(t *testing.T) {
	handler := cors(okHandler(), []string{"http://localhost:3000"})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/exemplo", nil)
	req.Header.Set(headerOrigin, "http://evil.example")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get(headerAllowOrigin); got != "" {
		t.Errorf("%s = %q, want vazio", headerAllowOrigin, got)
	}
	if got := rec.Header().Get(headerVary); got != "" {
		t.Errorf("%s = %q, want vazio", headerVary, got)
	}
	if got := rec.Header().Get(headerExposeHeaders); got != "" {
		t.Errorf("%s = %q, want vazio", headerExposeHeaders, got)
	}
}

func TestCORSPreflight(t *testing.T) {
	tests := []struct {
		name       string
		origins    []string
		origin     string
		wantStatus int
		wantOrigin string
	}{
		{
			name:       "origem permitida responde 204",
			origins:    []string{"http://localhost:3000"},
			origin:     "http://localhost:3000",
			wantStatus: http.StatusNoContent,
			wantOrigin: "http://localhost:3000",
		},
		{
			name:       "origem negada cai no handler sem headers CORS",
			origins:    []string{"http://localhost:3000"},
			origin:     "http://evil.example",
			wantStatus: http.StatusOK,
			wantOrigin: "",
		},
		{
			name:       "allowlist vazia desliga o CORS",
			origins:    nil,
			origin:     "http://localhost:3000",
			wantStatus: http.StatusOK,
			wantOrigin: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := cors(okHandler(), tt.origins)

			req := httptest.NewRequest(http.MethodOptions, "/api/v1/exemplo", nil)
			req.Header.Set(headerOrigin, tt.origin)
			req.Header.Set(headerRequestMethod, http.MethodGet)

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if got := rec.Header().Get(headerAllowOrigin); got != tt.wantOrigin {
				t.Errorf("%s = %q, want %q", headerAllowOrigin, got, tt.wantOrigin)
			}
			if tt.wantStatus != http.StatusNoContent {
				return
			}
			if got := rec.Header().Get(headerAllowMethods); got != corsAllowMethods {
				t.Errorf("%s = %q, want %q", headerAllowMethods, got, corsAllowMethods)
			}
			if got := rec.Header().Get(headerAllowHeaders); got != corsAllowHeaders {
				t.Errorf("%s = %q, want %q", headerAllowHeaders, got, corsAllowHeaders)
			}
		})
	}
}

func TestCORSPreflightThroughRouter(t *testing.T) {
	router := NewRouter(Deps{CORSOrigins: []string{"http://localhost:3000"}, Logger: discardLogger()})

	req := httptest.NewRequest(http.MethodOptions, "/api/v1/x", nil)
	req.Header.Set(headerOrigin, "http://localhost:3000")
	req.Header.Set(headerRequestMethod, http.MethodGet)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	// O preflight não pode chegar ao mux: /api/v1/x não existe e viraria 404.
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
}
