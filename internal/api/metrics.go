package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const (
	metricsPath = "/metrics"
	// unmatchedRoute é o label de toda requisição sem pattern casado (404 em
	// rota inexistente, preflight respondido pelo cors). Usar o path recebido
	// aqui deixaria qualquer cliente criar uma série nova por requisição.
	unmatchedRoute = "unmatched"
)

// Os coletores são variáveis de pacote porque o prometheus.DefaultRegisterer é
// global: registrar dentro de NewRouter faria a segunda chamada no mesmo
// processo entrar em panic por duplicate registration.
var (
	httpRequestsTotal = promauto.With(prometheus.DefaultRegisterer).NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total de requisições HTTP finalizadas.",
		},
		[]string{"method", "route", "status_code"},
	)

	httpRequestDuration = promauto.With(prometheus.DefaultRegisterer).NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "Duração das requisições HTTP em segundos.",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5},
		},
		[]string{"method", "route"},
	)

	httpRequestsInFlight = promauto.With(prometheus.DefaultRegisterer).NewGauge(
		prometheus.GaugeOpts{
			Name: "http_requests_in_flight",
			Help: "Requisições HTTP em processamento no momento.",
		},
	)
)

// metrics instrumenta a cadeia e é a camada mais externa dela: por dentro do
// recovery, o panic desfaria a pilha antes da contabilização e o 500 que o
// recovery escreve nunca apareceria no counter — a taxa de erro subestimaria as
// falhas reais.
//
// Recebe o *http.ServeMux, e não o http.Handler já embrulhado, porque só
// mux.Handler(r) resolve o pattern da rota sem executar o handler. Um middleware
// montado por fora do mux nunca enxerga r.Pattern: ele só é preenchido na cópia
// da request que o mux entrega ao handler.
func metrics(next http.Handler, mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pattern := mux.Handler(r)
		route := routeLabel(pattern)

		if route == metricsPath {
			next.ServeHTTP(w, r)
			return
		}

		rw := wrapWriter(w)
		start := time.Now()

		httpRequestsInFlight.Inc()
		defer func() {
			httpRequestsInFlight.Dec()
			httpRequestDuration.WithLabelValues(r.Method, route).Observe(time.Since(start).Seconds())
			httpRequestsTotal.WithLabelValues(r.Method, route, strconv.Itoa(rw.status)).Inc()
		}()

		next.ServeHTTP(rw, r)
	})
}

// routeLabel extrai o path do pattern do ServeMux, que vem com o método
// ("GET /api/v1/properties/{id}"). Sem o corte o label viraria "GET /metrics" e
// a exclusão do próprio endpoint deixaria de casar.
func routeLabel(pattern string) string {
	if pattern == "" {
		return unmatchedRoute
	}
	if i := strings.LastIndex(pattern, " "); i >= 0 {
		pattern = pattern[i+1:]
	}
	if pattern == "" {
		return unmatchedRoute
	}
	return pattern
}
