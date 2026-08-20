package api

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func scrapeMetrics(t *testing.T, handler http.Handler) string {
	t.Helper()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, metricsPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s status = %d, want %d", metricsPath, rec.Code, http.StatusOK)
	}
	return rec.Body.String()
}

// sampleValue devolve 0 para série ausente de propósito: o registry é global e
// compartilhado entre os testes do pacote, então toda asserção aqui é por delta
// (antes/depois) e nunca por valor absoluto.
func sampleValue(t *testing.T, output, sample string) float64 {
	t.Helper()

	for _, line := range strings.Split(output, "\n") {
		raw, found := strings.CutPrefix(line, sample+" ")
		if !found {
			continue
		}
		value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err != nil {
			t.Fatalf("valor ilegível em %q: %v", line, err)
		}
		return value
	}
	return 0
}

func requestsTotal(method, route, status string) float64 {
	return testutil.ToFloat64(httpRequestsTotal.WithLabelValues(method, route, status))
}

func TestRouteLabel(t *testing.T) {
	cases := map[string]string{
		"GET /api/v1/properties/{id}": "/api/v1/properties/{id}",
		"GET /health":                 "/health",
		"GET /metrics":                metricsPath,
		"/sem-metodo":                 "/sem-metodo",
		"":                            unmatchedRoute,
		" ":                           unmatchedRoute,
	}

	for pattern, want := range cases {
		if got := routeLabel(pattern); got != want {
			t.Errorf("routeLabel(%q) = %q, want %q", pattern, got, want)
		}
	}
}

func TestMetricsEndpointServesPrometheusText(t *testing.T) {
	router := NewRouter(Deps{Logger: discardLogger()})

	// Vetores sem nenhum filho não emitem linha alguma, e este é o primeiro
	// teste do pacote a rodar: sem uma requisição real antes, o output não teria
	// as séries de rota.
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/health", nil))

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, metricsPath, nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	// Asserção por substring: versões recentes do client_golang acrescentam
	// charset e escaping ao Content-Type, e igualdade exata quebraria no bump.
	contentType := rec.Header().Get(contentTypeHeader)
	for _, want := range []string{"text/plain", "version=0.0.4"} {
		if !strings.Contains(contentType, want) {
			t.Errorf("Content-Type = %q, want conter %q", contentType, want)
		}
	}

	body := rec.Body.String()
	for _, name := range []string{
		"http_requests_total",
		"http_request_duration_seconds",
		"http_requests_in_flight",
		"go_goroutines",
	} {
		if !strings.Contains(body, name) {
			t.Errorf("output não contém a métrica %q", name)
		}
	}
}

func TestMetricsCountsFinishedRequests(t *testing.T) {
	router := NewRouter(Deps{Logger: discardLogger()})

	before := requestsTotal(http.MethodGet, "/health", "200")

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	if got := requestsTotal(http.MethodGet, "/health", "200") - before; got != 1 {
		t.Errorf("delta do counter = %v, want 1", got)
	}

	output := scrapeMetrics(t, router)
	duration := `http_request_duration_seconds_count{method="GET",route="/health"}`
	if sampleValue(t, output, duration) < 1 {
		t.Errorf("histograma não observou a requisição: %s ausente ou zerado", duration)
	}
}

func TestMetricsRouteLabelIsThePatternNotThePath(t *testing.T) {
	const id = "3f2504e0-4f89-11d3-9a0c-0305e82c3301"
	const pattern = "/api/v1/properties/{id}"

	// Mux próprio com handler dummy: a rota real de detalhe entraria em panic
	// com o pool nil, e o que este teste pina é a origem do label.
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+pattern, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
	})
	handler := metrics(mux, mux)

	before := requestsTotal(http.MethodGet, pattern, "200")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/properties/"+id, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	if got := requestsTotal(http.MethodGet, pattern, "200") - before; got != 1 {
		t.Errorf("delta do counter para route=%q = %v, want 1", pattern, got)
	}

	if output := scrapeMetrics(t, NewRouter(Deps{Logger: discardLogger()})); strings.Contains(output, id) {
		t.Errorf("o id real vazou para o output das métricas — cardinalidade por imóvel")
	}
}

func TestMetricsUsesUnmatchedForRoutesWithoutPattern(t *testing.T) {
	router := NewRouter(Deps{Logger: discardLogger()})

	before := requestsTotal(http.MethodGet, unmatchedRoute, "404")

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/rota-que-nao-existe", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}

	if got := requestsTotal(http.MethodGet, unmatchedRoute, "404") - before; got != 1 {
		t.Errorf("delta do counter para route=%q = %v, want 1", unmatchedRoute, got)
	}

	if output := scrapeMetrics(t, router); strings.Contains(output, "/rota-que-nao-existe") {
		t.Error("o path recebido virou label — deveria ser " + unmatchedRoute)
	}
}

func TestMetricsUsesUnmatchedForPreflight(t *testing.T) {
	router := NewRouter(Deps{Logger: discardLogger(), CORSOrigins: []string{"https://app.imobhub.com.br"}})

	before := requestsTotal(http.MethodOptions, unmatchedRoute, "204")

	req := httptest.NewRequest(http.MethodOptions, "/api/v1/properties", nil)
	req.Header.Set(headerOrigin, "https://app.imobhub.com.br")
	req.Header.Set(headerRequestMethod, http.MethodGet)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}

	if got := requestsTotal(http.MethodOptions, unmatchedRoute, "204") - before; got != 1 {
		t.Errorf("delta do counter para o preflight = %v, want 1", got)
	}
}

func TestMetricsEndpointIsNotInstrumented(t *testing.T) {
	router := NewRouter(Deps{Logger: discardLogger()})

	scrapeMetrics(t, router)
	output := scrapeMetrics(t, router)

	if strings.Contains(output, `route="`+metricsPath+`"`) {
		t.Errorf("%s aparece nas próprias séries de rota", metricsPath)
	}
}

func TestMetricsCountsPanicAs500AndReleasesInFlight(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /panico", func(http.ResponseWriter, *http.Request) { panic("boom") })
	handler := metrics(recovery(mux, discardLogger()), mux)

	before := requestsTotal(http.MethodGet, "/panico", "500")
	inFlightBefore := testutil.ToFloat64(httpRequestsInFlight)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panico", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if got := requestsTotal(http.MethodGet, "/panico", "500") - before; got != 1 {
		t.Errorf("delta do counter com status_code=500 = %v, want 1", got)
	}
	if got := testutil.ToFloat64(httpRequestsInFlight); got != inFlightBefore {
		t.Errorf("in-flight = %v, want %v — o gauge não voltou depois do panic", got, inFlightBefore)
	}
}

func TestNewRouterCanBeCalledTwiceInTheSameProcess(t *testing.T) {
	NewRouter(Deps{Logger: discardLogger()})
	NewRouter(Deps{Logger: discardLogger()})
}

func TestMetricsDoesNotChangeExistingResponses(t *testing.T) {
	router := NewRouter(Deps{Logger: discardLogger()})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get(contentTypeHeader); got != contentTypeJSON {
		t.Errorf("Content-Type = %q, want %q", got, contentTypeJSON)
	}
	if rec.Header().Get(headerRequestID) == "" {
		t.Error("X-Request-Id sumiu da resposta")
	}
}
