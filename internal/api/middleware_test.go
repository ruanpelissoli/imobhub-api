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

// newCapture monta a captura com um nível explícito porque capturingHandler
// delega Enabled ao handler embutido: com o default (Info) um registro Debug é
// descartado antes de chegar ao Handle e o teste veria zero linhas.
func newCapture(level slog.Leveler) (*capturingHandler, *slog.Logger) {
	capture := &capturingHandler{
		Handler: slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: level}),
	}
	return capture, slog.New(capture)
}

func attrsOf(r slog.Record) map[string]slog.Value {
	attrs := map[string]slog.Value{}
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value
		return true
	})
	return attrs
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

func TestLoggingIncludesRequestIDAndRemoteAddr(t *testing.T) {
	capture, logger := newCapture(slog.LevelDebug)

	handler := requestID(logging(okHandler(), logger))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/exemplo", nil))

	if len(capture.records) != 1 {
		t.Fatalf("linhas de log = %d, want 1", len(capture.records))
	}

	attrs := attrsOf(capture.records[0])
	for _, key := range []string{"request_id", "method", "path", "status", "duration_ms", "bytes", "remote_addr"} {
		if _, ok := attrs[key]; !ok {
			t.Errorf("campo %q ausente na linha de log", key)
		}
	}
	if got := attrs["remote_addr"].String(); got != "192.0.2.1:1234" {
		t.Errorf("remote_addr = %q, want o RemoteAddr da requisição", got)
	}
	if got := attrs["request_id"].String(); got == "" {
		t.Error("request_id vazio na linha de log")
	}
}

func TestRequestIDPropagatesClientHeader(t *testing.T) {
	const sent = "abc-123_XYZ"

	capture, logger := newCapture(slog.LevelDebug)

	var fromContext string
	handler := requestID(logging(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fromContext = requestIDFromContext(r.Context())
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}), logger))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/exemplo", nil)
	req.Header.Set(headerRequestID, sent)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get(headerRequestID); got != sent {
		t.Errorf("%s da resposta = %q, want %q", headerRequestID, got, sent)
	}
	if fromContext != sent {
		t.Errorf("request_id no contexto = %q, want %q", fromContext, sent)
	}
	if len(capture.records) != 1 {
		t.Fatalf("linhas de log = %d, want 1", len(capture.records))
	}
	if got := attrsOf(capture.records[0])["request_id"].String(); got != sent {
		t.Errorf("request_id na linha de log = %q, want %q", got, sent)
	}
}

func TestRequestIDIsGeneratedWhenAbsent(t *testing.T) {
	capture, logger := newCapture(slog.LevelDebug)

	var fromContext string
	handler := requestID(logging(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fromContext = requestIDFromContext(r.Context())
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}), logger))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/exemplo", nil))

	fromHeader := rec.Header().Get(headerRequestID)
	if fromHeader == "" {
		t.Fatalf("%s da resposta vazio, want um id gerado", headerRequestID)
	}
	if !validRequestID(fromHeader) {
		t.Errorf("id gerado %q não passa na própria validação", fromHeader)
	}
	if fromContext != fromHeader {
		t.Errorf("request_id no contexto = %q, want %q", fromContext, fromHeader)
	}
	if len(capture.records) != 1 {
		t.Fatalf("linhas de log = %d, want 1", len(capture.records))
	}
	if got := attrsOf(capture.records[0])["request_id"].String(); got != fromHeader {
		t.Errorf("request_id na linha de log = %q, want %q", got, fromHeader)
	}
}

func TestRequestIDRejectsInvalidClientHeader(t *testing.T) {
	tests := []struct {
		name string
		sent string
	}{
		{name: "longo demais", sent: strings.Repeat("a", requestIDMaxLen+1)},
		{name: "caractere fora da lista", sent: "abc/def"},
		{name: "com espaço", sent: "abc def"},
		{name: "com quebra de linha", sent: "abc\ndef"},
		{name: "vazio", sent: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := requestID(okHandler())

			req := httptest.NewRequest(http.MethodGet, "/api/v1/exemplo", nil)
			req.Header[headerRequestID] = []string{tt.sent}

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			got := rec.Header().Get(headerRequestID)
			if got == "" {
				t.Fatalf("%s da resposta vazio, want um id gerado", headerRequestID)
			}
			if got == tt.sent {
				t.Errorf("%s = %q, want um id gerado no lugar do valor inválido", headerRequestID, got)
			}
			if !validRequestID(got) {
				t.Errorf("id gerado %q não passa na própria validação", got)
			}
			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want %d (valor inválido é descartado em silêncio)", rec.Code, http.StatusOK)
			}
		})
	}
}

func TestQuietPathsLogAtDebugLevel(t *testing.T) {
	tests := []struct {
		name      string
		path      string
		wantLevel slog.Level
	}{
		{name: "liveness é ruído de infraestrutura", path: "/health", wantLevel: slog.LevelDebug},
		{name: "rota de negócio", path: "/api/v1/inexistente", wantLevel: slog.LevelInfo},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			capture, logger := newCapture(slog.LevelDebug)
			router := NewRouter(Deps{Logger: logger})

			router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, tt.path, nil))

			if len(capture.records) != 1 {
				t.Fatalf("linhas de log = %d, want 1", len(capture.records))
			}
			if got := capture.records[0].Level; got != tt.wantLevel {
				t.Errorf("nível = %v, want %v", got, tt.wantLevel)
			}
			if got := attrsOf(capture.records[0])["request_id"].String(); got == "" {
				t.Error("request_id vazio na linha de log emitida pelo roteador")
			}
		})
	}
}

func TestRecoveryLogIncludesRequestID(t *testing.T) {
	capture, logger := newCapture(slog.LevelDebug)

	handler := recovery(requestID(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})), logger)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/exemplo", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if got := decodeError(t, rec.Body.Bytes()); got != msgInternalError {
		t.Errorf("error = %q, want %q", got, msgInternalError)
	}

	fromHeader := rec.Header().Get(headerRequestID)
	if fromHeader == "" {
		t.Fatalf("%s da resposta vazio mesmo com panic", headerRequestID)
	}
	if len(capture.records) != 1 {
		t.Fatalf("linhas de log = %d, want 1", len(capture.records))
	}
	if got := attrsOf(capture.records[0])["request_id"].String(); got != fromHeader {
		t.Errorf("request_id do log de panic = %q, want %q", got, fromHeader)
	}
}

func TestRequestIDIsExposedByCORS(t *testing.T) {
	if !strings.Contains(corsExposeHeaders, headerRequestID) {
		t.Errorf("corsExposeHeaders = %q, want conter %q", corsExposeHeaders, headerRequestID)
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
