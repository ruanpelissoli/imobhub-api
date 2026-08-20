// Package api monta o servidor HTTP do ImobHub: roteador, middlewares base e o
// ciclo de vida do http.Server. Não conhece variáveis de ambiente — tudo chega
// por Deps, preenchida em cmd/api.
package api

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/imobhub/api/internal/db"
)

// apiV1Prefix é o prefixo de toda rota de negócio. Versionar desde a primeira
// rota evita ter que reescrever o front quando o contrato mudar.
const apiV1Prefix = "/api/v1"

// Deps reúne o que os handlers precisam. Injetada por parâmetro, nunca por
// variável global: é o que permite testar cada handler com um pool próprio.
type Deps struct {
	Pool        *pgxpool.Pool
	Redis       *redis.Client
	CORSOrigins []string
	// RateLimitRPM é o teto de requisições por minuto por IP. Zero (o
	// zero-value) desliga o rate limiting, assim como um Redis nulo.
	RateLimitRPM int
	Logger       *slog.Logger
}

func (d Deps) logger() *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.Default()
}

// NewRouter monta o roteador com a cadeia de middlewares.
//
// A ordem é de fora para dentro: recovery → requestID → logging → cors →
// rateLimit → jsonErrors → mux. recovery é o mais externo para que um panic em
// qualquer outro middleware ainda vire 500; requestID vem dentro dele (para que
// o panic continue virando 500) e fora do logging (para que toda linha de log já
// tenha o id); logging registra o status que o recovery escreveu; cors precisa
// responder o preflight sem passar pelo mux (que devolveria 404 na rota
// inexistente do preflight); rateLimit fica dentro do logging (para o 429 ser
// logado) e dentro do cors (para o preflight OPTIONS não consumir cota);
// jsonErrors é o mais interno porque só existe para reescrever o que o próprio
// mux emite.
func NewRouter(deps Deps) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handleHealth)
	registerV1Routes(mux, deps)

	logger := deps.logger()

	var handler http.Handler = mux
	handler = jsonErrors(handler)
	handler = rateLimit(handler, newRateLimitStore(deps.Redis), deps.RateLimitRPM, logger)
	handler = cors(handler, deps.CORSOrigins)
	handler = logging(handler, logger)
	handler = requestID(handler)
	handler = recovery(handler, logger)

	return handler
}

// registerV1Routes é o ponto de extensão do grupo /api/v1. As rotas de negócio
// entram aqui, com o padrão do ServeMux 1.22+ ("GET "+apiV1Prefix+"/properties"),
// para que nenhuma delas precise repetir a montagem dos middlewares.
//
// O padrão terminado em "/{$}" existe só para o id vazio: "/properties/" não
// casa com "{id}" (o wildcard exige um segmento não-vazio) e cairia no 404
// genérico do mux, quando o correto é 400 — o cliente mandou um id, ele é que
// está em branco. "{$}" casa exclusivamente com esse path, sem capturar
// "/properties/a/b".
//
// O padrão exato "/properties" (a busca) tem precedência sobre esses dois, então
// ele encerra o 301 que o mux emitia de "/properties" para "/properties/".
func registerV1Routes(mux *http.ServeMux, deps Deps) {
	search := func(ctx context.Context, params db.PropertySearchParams) ([]db.Property, int64, error) {
		return db.SearchProperties(ctx, deps.Pool, params)
	}
	mux.Handle("GET "+apiV1Prefix+"/properties",
		handleSearchProperties(search, newPropertyCache(deps.Redis), deps.logger()))

	getProperty := handleGetProperty(deps)
	mux.HandleFunc("GET "+apiV1Prefix+"/properties/{id}", getProperty)
	mux.HandleFunc("GET "+apiV1Prefix+"/properties/{$}", getProperty)
}

// handleHealth é liveness, não readiness: responde 200 sem tocar em Postgres ou
// Redis. Um health que falha quando o banco pisca faz o orquestrador reiniciar
// uma API que estava perfeitamente viva. Fica fora do /api/v1 porque não é
// contrato de produto — é infraestrutura, e não deve ser versionada com ele.
func handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
