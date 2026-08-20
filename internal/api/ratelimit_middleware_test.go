package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// fakeRateLimitStore substitui o Redis sem rede: conta por chave em memória e
// devolve os erros programados pelo teste.
type fakeRateLimitStore struct {
	counters  map[string]int64
	incrErr   error
	expireErr error
	incrs     int
	expires   int
	lastKey   string
	lastTTL   time.Duration
}

func newFakeRateLimitStore() *fakeRateLimitStore {
	return &fakeRateLimitStore{counters: map[string]int64{}}
}

func (s *fakeRateLimitStore) Incr(_ context.Context, key string) *redis.IntCmd {
	s.incrs++
	s.lastKey = key
	if s.incrErr != nil {
		return redis.NewIntResult(0, s.incrErr)
	}
	s.counters[key]++
	return redis.NewIntResult(s.counters[key], nil)
}

func (s *fakeRateLimitStore) Expire(_ context.Context, key string, ttl time.Duration) *redis.BoolCmd {
	s.expires++
	s.lastKey = key
	s.lastTTL = ttl
	if s.expireErr != nil {
		return redis.NewBoolResult(false, s.expireErr)
	}
	return redis.NewBoolResult(true, nil)
}

func countingOKHandler(calls *int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*calls++
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
}

func doRateLimitedRequest(handler http.Handler, path, remoteAddr string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = remoteAddr

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestRateLimitAllowsRequestsWithinTheLimit(t *testing.T) {
	var calls int
	store := newFakeRateLimitStore()
	handler := rateLimit(countingOKHandler(&calls), store, 5, discardLogger())

	for i, wantRemaining := range []string{"4", "3", "2"} {
		rec := doRateLimitedRequest(handler, "/api/v1/properties", "192.0.2.1:1234")

		if rec.Code != http.StatusOK {
			t.Fatalf("requisição %d: status = %d, want %d", i+1, rec.Code, http.StatusOK)
		}
		if got := rec.Header().Get(headerRateLimitLimit); got != "5" {
			t.Errorf("requisição %d: %s = %q, want %q", i+1, headerRateLimitLimit, got, "5")
		}
		if got := rec.Header().Get(headerRateLimitRemaining); got != wantRemaining {
			t.Errorf("requisição %d: %s = %q, want %q", i+1, headerRateLimitRemaining, got, wantRemaining)
		}
		if got := rec.Header().Get(headerRetryAfter); got != "" {
			t.Errorf("requisição %d: %s = %q, want vazio numa resposta permitida", i+1, headerRetryAfter, got)
		}
	}

	if calls != 3 {
		t.Errorf("handler chamado %d vezes, want 3", calls)
	}
}

func TestRateLimitBlocksRequestsAboveTheLimit(t *testing.T) {
	var calls int
	store := newFakeRateLimitStore()
	handler := rateLimit(countingOKHandler(&calls), store, 2, discardLogger())

	for i := 0; i < 2; i++ {
		if rec := doRateLimitedRequest(handler, "/api/v1/properties", "192.0.2.1:1234"); rec.Code != http.StatusOK {
			t.Fatalf("requisição %d: status = %d, want %d", i+1, rec.Code, http.StatusOK)
		}
	}

	rec := doRateLimitedRequest(handler, "/api/v1/properties", "192.0.2.1:1234")

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusTooManyRequests)
	}
	if got := rec.Header().Get(contentTypeHeader); got != contentTypeJSON {
		t.Errorf("Content-Type = %q, want %q", got, contentTypeJSON)
	}
	if got := decodeError(t, rec.Body.Bytes()); got != msgTooManyRequests {
		t.Errorf("corpo = %q, want %q", got, msgTooManyRequests)
	}
	if got := rec.Header().Get(headerRateLimitRemaining); got != "0" {
		t.Errorf("%s = %q, want %q — nunca negativo", headerRateLimitRemaining, got, "0")
	}
	if got := rec.Header().Get(headerRateLimitLimit); got != "2" {
		t.Errorf("%s = %q, want %q", headerRateLimitLimit, got, "2")
	}

	retryAfter, err := strconv.Atoi(rec.Header().Get(headerRetryAfter))
	if err != nil {
		t.Fatalf("%s = %q, want um inteiro: %v", headerRetryAfter, rec.Header().Get(headerRetryAfter), err)
	}
	if retryAfter < 1 || retryAfter > 60 {
		t.Errorf("%s = %d, want entre 1 e 60", headerRetryAfter, retryAfter)
	}

	if calls != 2 {
		t.Errorf("handler chamado %d vezes, want 2 — a requisição bloqueada vazou para o downstream", calls)
	}
}

func TestRateLimitCountsPerIP(t *testing.T) {
	var calls int
	store := newFakeRateLimitStore()
	handler := rateLimit(countingOKHandler(&calls), store, 1, discardLogger())

	if rec := doRateLimitedRequest(handler, "/api/v1/properties", "192.0.2.1:1234"); rec.Code != http.StatusOK {
		t.Fatalf("status do primeiro IP = %d, want %d", rec.Code, http.StatusOK)
	}
	// Outro IP tem cota própria: a cota é por cliente, não global.
	if rec := doRateLimitedRequest(handler, "/api/v1/properties", "198.51.100.7:5555"); rec.Code != http.StatusOK {
		t.Fatalf("status do segundo IP = %d, want %d — a cota vazou entre IPs", rec.Code, http.StatusOK)
	}
	if rec := doRateLimitedRequest(handler, "/api/v1/properties", "192.0.2.1:9999"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want %d — a porta de origem não pode criar uma cota nova", rec.Code, http.StatusTooManyRequests)
	}
}

func TestRateLimitFailsOpenWhenRedisIsUnavailable(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "redis indisponível", err: errors.New("dial tcp: connection refused")},
		{name: "timeout na operação", err: context.DeadlineExceeded},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			capture, logger := newCapture(nil)
			store := newFakeRateLimitStore()
			store.incrErr = tt.err

			var calls int
			handler := rateLimit(countingOKHandler(&calls), store, 1, logger)

			for i := 0; i < 5; i++ {
				rec := doRateLimitedRequest(handler, "/api/v1/properties", "192.0.2.1:1234")
				if rec.Code != http.StatusOK {
					t.Fatalf("requisição %d: status = %d, want %d — falha de Redis não pode bloquear nem virar 500",
						i+1, rec.Code, http.StatusOK)
				}
				if got := rec.Header().Get(headerRateLimitRemaining); got != "1" {
					t.Errorf("requisição %d: %s = %q, want %q", i+1, headerRateLimitRemaining, got, "1")
				}
			}

			if calls != 5 {
				t.Errorf("handler chamado %d vezes, want 5", calls)
			}
			if !hasWarning(capture.records) {
				t.Error("a falha do Redis precisa ser logada em nível warn")
			}
		})
	}
}

func TestRateLimitKeepsServingWhenExpireFails(t *testing.T) {
	capture, logger := newCapture(nil)
	store := newFakeRateLimitStore()
	store.expireErr = errors.New("READONLY")

	var calls int
	handler := rateLimit(countingOKHandler(&calls), store, 5, logger)

	if rec := doRateLimitedRequest(handler, "/api/v1/properties", "192.0.2.1:1234"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !hasWarning(capture.records) {
		t.Error("a falha do EXPIRE precisa ser logada em nível warn")
	}
}

func TestRateLimitSetsTTLOnlyOnTheFirstRequestOfTheWindow(t *testing.T) {
	store := newFakeRateLimitStore()
	var calls int
	handler := rateLimit(countingOKHandler(&calls), store, 10, discardLogger())

	for i := 0; i < 4; i++ {
		doRateLimitedRequest(handler, "/api/v1/properties", "192.0.2.1:1234")
	}

	if store.expires != 1 {
		t.Errorf("EXPIRE chamado %d vezes, want 1 — só a primeira da janela define o TTL", store.expires)
	}
	if want := rateLimitWindow + rateLimitKeyTTLSlack; store.lastTTL != want {
		t.Errorf("TTL = %v, want %v — a folga evita a chave expirar antes do fim da janela", store.lastTTL, want)
	}
}

func TestRateLimitExemptsInfrastructurePaths(t *testing.T) {
	store := newFakeRateLimitStore()
	var calls int
	handler := rateLimit(countingOKHandler(&calls), store, 1, discardLogger())

	for path := range rateLimitExemptPaths {
		for i := 0; i < 5; i++ {
			rec := doRateLimitedRequest(handler, path, "192.0.2.1:1234")
			if rec.Code != http.StatusOK {
				t.Fatalf("%s na requisição %d: status = %d, want %d", path, i+1, rec.Code, http.StatusOK)
			}
			if got := rec.Header().Get(headerRateLimitLimit); got != "" {
				t.Errorf("%s: %s = %q, want vazio num path isento", path, headerRateLimitLimit, got)
			}
		}
	}

	if store.incrs != 0 {
		t.Errorf("INCR chamado %d vezes, want 0 — path isento não pode tocar o Redis", store.incrs)
	}
}

func TestInfrastructureRoutesAreNotRateLimitedThroughTheRouter(t *testing.T) {
	router := NewRouter(Deps{Logger: discardLogger(), RateLimitRPM: 1})

	for _, path := range []string{"/health", metricsPath} {
		t.Run(path, func(t *testing.T) {
			for i := 0; i < 5; i++ {
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

				if rec.Code != http.StatusOK {
					t.Fatalf("requisição %d: status = %d, want %d", i+1, rec.Code, http.StatusOK)
				}
			}
		})
	}
}

// O conjunto de isenções precisa acompanhar as rotas de infraestrutura reais:
// uma rota nova registrada fora do /api/v1 e esquecida aqui passa a competir
// pela mesma cota do tráfego de produto.
func TestRateLimitExemptPathsCoverTheInfrastructureRoutes(t *testing.T) {
	for _, path := range []string{"/health", metricsPath} {
		if _, exempt := rateLimitExemptPaths[path]; !exempt {
			t.Errorf("%q não está em rateLimitExemptPaths", path)
		}
	}
	if len(rateLimitExemptPaths) != len(quietPaths) {
		t.Errorf("rateLimitExemptPaths tem %d paths e quietPaths %d — os dois conjuntos descrevem as mesmas rotas de infraestrutura",
			len(rateLimitExemptPaths), len(quietPaths))
	}
}

func TestRateLimitIsPassthroughWithoutStore(t *testing.T) {
	// newRateLimitStore(nil) precisa devolver nil, e não um *redis.Client nil
	// dentro da interface: o segundo entraria em panic na primeira operação.
	if store := newRateLimitStore(nil); store != nil {
		t.Fatalf("newRateLimitStore(nil) = %v, want nil", store)
	}

	var calls int
	next := countingOKHandler(&calls)

	if got := rateLimit(next, nil, 60, discardLogger()); got == nil {
		t.Fatal("rateLimit sem store devolveu nil")
	}
	if got := rateLimit(next, newFakeRateLimitStore(), 0, discardLogger()); got == nil {
		t.Fatal("rateLimit com limite 0 devolveu nil")
	}

	for _, handler := range []http.Handler{
		rateLimit(next, nil, 60, discardLogger()),
		rateLimit(next, newFakeRateLimitStore(), 0, discardLogger()),
	} {
		for i := 0; i < 3; i++ {
			rec := doRateLimitedRequest(handler, "/api/v1/properties", "192.0.2.1:1234")
			if rec.Code != http.StatusOK {
				t.Fatalf("requisição %d: status = %d, want %d", i+1, rec.Code, http.StatusOK)
			}
			if got := rec.Header().Get(headerRateLimitLimit); got != "" {
				t.Errorf("%s = %q, want vazio com o limiter desligado", headerRateLimitLimit, got)
			}
		}
	}
}

// Os testes do pacote montam Deps sem Redis: com o limiter ligado, o roteador
// precisa continuar respondendo normalmente em vez de 429 ou panic.
func TestRouterWithoutRedisIsNotRateLimited(t *testing.T) {
	router := NewRouter(Deps{Logger: discardLogger(), RateLimitRPM: 1})

	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/inexistente", nil))

		if rec.Code != http.StatusNotFound {
			t.Fatalf("requisição %d: status = %d, want %d", i+1, rec.Code, http.StatusNotFound)
		}
	}
}

func TestClientIP(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		want       string
	}{
		{name: "ipv4 com porta", remoteAddr: "192.0.2.1:1234", want: "192.0.2.1"},
		{name: "ipv6 com porta", remoteAddr: "[::1]:54321", want: "::1"},
		{name: "ipv6 completo", remoteAddr: "[2001:db8::1]:443", want: "2001:db8::1"},
		{name: "sem porta usa a string crua", remoteAddr: "192.0.2.1", want: "192.0.2.1"},
		{name: "vazio não entra em pânico", remoteAddr: "", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := clientIP(tt.remoteAddr); got != tt.want {
				t.Errorf("clientIP(%q) = %q, want %q", tt.remoteAddr, got, tt.want)
			}
		})
	}
}

func TestRateLimitKeyChangesOnWindowRollover(t *testing.T) {
	base := time.Date(2026, 8, 19, 10, 30, 0, 0, time.UTC)

	start := rateLimitKey("192.0.2.1", base)
	last := rateLimitKey("192.0.2.1", base.Add(59*time.Second+999*time.Millisecond))
	next := rateLimitKey("192.0.2.1", base.Add(rateLimitWindow))

	if start != last {
		t.Errorf("chaves diferentes dentro da mesma janela: %q e %q", start, last)
	}
	if start == next {
		t.Errorf("chave = %q na virada da janela, want diferente de %q", next, start)
	}
	if !strings.HasPrefix(start, rateLimitKeyPrefix+"192.0.2.1:") {
		t.Errorf("chave = %q, want prefixo %q", start, rateLimitKeyPrefix+"192.0.2.1:")
	}
	if other := rateLimitKey("198.51.100.7", base); other == start {
		t.Errorf("IPs diferentes compartilham a chave %q", start)
	}
}

func TestRetryAfterSecondsIsNeverZero(t *testing.T) {
	base := time.Date(2026, 8, 19, 10, 30, 0, 0, time.UTC)

	tests := []struct {
		name string
		now  time.Time
		want int
	}{
		{name: "início da janela", now: base, want: 60},
		{name: "metade da janela", now: base.Add(30 * time.Second), want: 30},
		{name: "último segundo", now: base.Add(59 * time.Second), want: 1},
		{name: "fração final arredonda para cima", now: base.Add(59*time.Second + 900*time.Millisecond), want: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := retryAfterSeconds(tt.now)
			if got != tt.want {
				t.Errorf("retryAfterSeconds(%v) = %d, want %d", tt.now, got, tt.want)
			}
			if got < 1 {
				t.Errorf("retryAfterSeconds(%v) = %d — Retry-After 0 convida a repetir na hora", tt.now, got)
			}
		})
	}
}

func TestRateLimitHeadersAreExposedByCORS(t *testing.T) {
	for _, header := range []string{headerRetryAfter, headerRateLimitLimit, headerRateLimitRemaining} {
		if !strings.Contains(corsExposeHeaders, header) {
			t.Errorf("corsExposeHeaders = %q, want conter %q", corsExposeHeaders, header)
		}
	}
	// Os headers já expostos não podem sumir com a adição.
	for _, header := range []string{headerCacheStatus, headerRequestID} {
		if !strings.Contains(corsExposeHeaders, header) {
			t.Errorf("corsExposeHeaders = %q, want conter %q", corsExposeHeaders, header)
		}
	}
}
