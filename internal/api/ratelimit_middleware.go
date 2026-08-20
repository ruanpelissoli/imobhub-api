package api

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	rateLimitKeyPrefix = "ratelimit:"

	// rateLimitWindow é a janela fixa do contador. Janela fixa, e não deslizante:
	// o esquema de chave `ratelimit:<ip>:<janela>` já é um contador por janela, e
	// um sliding window exigiria sorted set com limpeza por requisição — custo
	// desproporcional para uma defesa anti-abuso.
	rateLimitWindow = time.Minute

	// rateLimitKeyTTLSlack cobre o desencontro entre o relógio do processo e o do
	// Redis. Sem folga, a chave da janela corrente pode expirar antes do fim dela
	// e zerar a contagem no meio do minuto.
	rateLimitKeyTTLSlack = 10 * time.Second

	// rateLimitOpTimeout espelha propertyCacheOpTimeout: um Redis lento não pode
	// consumir o WriteTimeout de 30s do servidor.
	rateLimitOpTimeout = 200 * time.Millisecond

	headerRateLimitLimit     = "X-RateLimit-Limit"
	headerRateLimitRemaining = "X-RateLimit-Remaining"
	headerRetryAfter         = "Retry-After"
)

// rateLimitExemptPaths são os paths de infraestrutura que nunca são limitados —
// o liveness do orquestrador e o scrape do Prometheus não podem receber 429 por
// causa do tráfego de um vizinho no mesmo NAT. O casamento é por path exato,
// como em quietPaths.
var rateLimitExemptPaths = map[string]struct{}{
	"/health":  {},
	"/metrics": {},
}

// rateLimitStore é declarado no consumidor para que os testes do pacote rodem
// sem Redis (mesma convenção de cacheStore).
type rateLimitStore interface {
	Incr(ctx context.Context, key string) *redis.IntCmd
	Expire(ctx context.Context, key string, ttl time.Duration) *redis.BoolCmd
}

// newRateLimitStore checa o nil **antes** da atribuição à interface: guardar um
// *redis.Client nil em rateLimitStore produziria uma interface não-nil e um
// panic na primeira operação. Store nil é o caminho "sem rate limiting".
func newRateLimitStore(client *redis.Client) rateLimitStore {
	if client == nil {
		return nil
	}
	return client
}

// rateLimit limita requisições por IP com um contador de janela fixa no Redis.
//
// Fail-open por decisão: qualquer erro do Redis vira Warn e deixa a requisição
// passar. O limitador existe para conter abuso, não para ser um ponto único de
// falha capaz de derrubar a API inteira quando o cache pisca.
func rateLimit(next http.Handler, store rateLimitStore, limit int, logger *slog.Logger) http.Handler {
	if store == nil || limit <= 0 {
		return next
	}

	limitValue := strconv.Itoa(limit)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, exempt := rateLimitExemptPaths[r.URL.Path]; exempt {
			next.ServeHTTP(w, r)
			return
		}

		now := time.Now()
		ip := clientIP(r.RemoteAddr)
		key := rateLimitKey(ip, now)

		ctx, cancel := context.WithTimeout(r.Context(), rateLimitOpTimeout)
		defer cancel()

		count, err := store.Incr(ctx, key).Result()
		if err != nil {
			logger.Warn("api: rate limit counter failed", "error", err, "key", key)
			// Sem contagem não há consumo a reportar: o remaining cheio é a
			// verdade operacional do momento.
			w.Header().Set(headerRateLimitLimit, limitValue)
			w.Header().Set(headerRateLimitRemaining, limitValue)
			next.ServeHTTP(w, r)
			return
		}

		if count == 1 {
			if err := store.Expire(ctx, key, rateLimitWindow+rateLimitKeyTTLSlack).Err(); err != nil {
				logger.Warn("api: rate limit ttl failed", "error", err, "key", key)
			}
		}

		remaining := limit - int(count)
		if remaining < 0 {
			remaining = 0
		}
		w.Header().Set(headerRateLimitLimit, limitValue)
		w.Header().Set(headerRateLimitRemaining, strconv.Itoa(remaining))

		if count > int64(limit) {
			w.Header().Set(headerRetryAfter, strconv.Itoa(retryAfterSeconds(now)))
			writeError(w, http.StatusTooManyRequests, msgTooManyRequests)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// clientIP extrai só o host de RemoteAddr. X-Forwarded-For é ignorado de
// propósito: sem um proxy confiável na frente, aceitá-lo deixaria qualquer
// cliente escolher a própria chave e burlar o limite com um header. Quando
// houver proxy, isso vira uma config TRUSTED_PROXY.
//
// Endereço sem porta (ou malformado) devolve a string crua em vez de erro: uma
// chave estranha ainda limita, um panic derruba a requisição.
func clientIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}

func rateLimitKey(ip string, now time.Time) string {
	window := now.Unix() / int64(rateLimitWindow/time.Second)
	return rateLimitKeyPrefix + ip + ":" + strconv.FormatInt(window, 10)
}

// retryAfterSeconds devolve os segundos até o fim da janela corrente,
// arredondados para cima e com mínimo de 1: um Retry-After: 0 convida o cliente
// a repetir imediatamente, que é exatamente o comportamento a conter.
func retryAfterSeconds(now time.Time) int {
	windowSeconds := int64(rateLimitWindow / time.Second)
	start := time.Unix(now.Unix()/windowSeconds*windowSeconds, 0)

	remaining := start.Add(rateLimitWindow).Sub(now)
	seconds := int((remaining + time.Second - 1) / time.Second)
	if seconds < 1 {
		return 1
	}
	return seconds
}
