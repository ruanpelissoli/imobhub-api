package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func decodeError(t *testing.T, body []byte) string {
	t.Helper()

	var payload errorBody
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("resposta não é o envelope JSON de erro (%q): %v", body, err)
	}
	return payload.Error
}

func TestHealthReturnsOK(t *testing.T) {
	rec := httptest.NewRecorder()
	NewRouter(Deps{Logger: discardLogger()}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get(contentTypeHeader); got != contentTypeJSON {
		t.Errorf("Content-Type = %q, want %q", got, contentTypeJSON)
	}

	var payload map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("corpo inválido (%q): %v", rec.Body.String(), err)
	}
	if payload["status"] != "ok" {
		t.Errorf(`corpo = %v, want {"status":"ok"}`, payload)
	}
}

func TestUnknownRouteReturnsJSONNotFound(t *testing.T) {
	for _, path := range []string{"/api/v1/inexistente", "/nao-existe"} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			NewRouter(Deps{Logger: discardLogger()}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
			}
			if got := rec.Header().Get(contentTypeHeader); got != contentTypeJSON {
				t.Errorf("Content-Type = %q, want %q", got, contentTypeJSON)
			}
			if got := decodeError(t, rec.Body.Bytes()); got != msgNotFound {
				t.Errorf("error = %q, want %q", got, msgNotFound)
			}
		})
	}
}

func TestWrongMethodReturnsJSONMethodNotAllowed(t *testing.T) {
	rec := httptest.NewRecorder()
	NewRouter(Deps{Logger: discardLogger()}).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/health", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
	if got := rec.Header().Get(contentTypeHeader); got != contentTypeJSON {
		t.Errorf("Content-Type = %q, want %q", got, contentTypeJSON)
	}
	// O Allow do ServeMux é o que diz ao cliente qual método usar; a troca do
	// corpo por JSON não pode descartá-lo.
	if got := rec.Header().Get("Allow"); got == "" {
		t.Error("header Allow foi perdido na conversão para JSON")
	}
	if got := decodeError(t, rec.Body.Bytes()); got != msgNotAllowed {
		t.Errorf("error = %q, want %q", got, msgNotAllowed)
	}
}

func TestAPIV1PrefixIsRegistered(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+apiV1Prefix+"/exemplo", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
	})

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/exemplo", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d — apiV1Prefix não casa com /api/v1", rec.Code, http.StatusOK)
	}
}
