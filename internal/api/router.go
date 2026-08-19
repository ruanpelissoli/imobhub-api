// Package api monta o servidor HTTP do ImobHub: roteador, middlewares base e o
// ciclo de vida do http.Server. Não conhece variáveis de ambiente — tudo chega
// por Deps, preenchida em cmd/api.
package api

import (
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
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
	Logger      *slog.Logger
}

func (d Deps) logger() *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.Default()
}

// NewRouter monta o roteador com a cadeia de middlewares.
//
// A ordem é de fora para dentro: recovery → logging → cors → jsonErrors → mux.
// recovery é o mais externo para que um panic em qualquer outro middleware ainda
// vire 500; logging vem logo abaixo para registrar o status que o recovery
// escreveu; cors precisa responder o preflight sem passar pelo mux (que
// devolveria 404 na rota inexistente do preflight); jsonErrors é o mais interno
// porque só existe para reescrever o que o próprio mux emite.
func NewRouter(deps Deps) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handleHealth)
	registerV1Routes(mux, deps)

	logger := deps.logger()

	var handler http.Handler = mux
	handler = jsonErrors(handler)
	handler = cors(handler, deps.CORSOrigins)
	handler = logging(handler, logger)
	handler = recovery(handler, logger)

	return handler
}

// registerV1Routes é o ponto de extensão do grupo /api/v1. As rotas de negócio
// entram aqui, com o padrão do ServeMux 1.22+ ("GET "+apiV1Prefix+"/properties"),
// para que nenhuma delas precise repetir a montagem dos middlewares.
func registerV1Routes(mux *http.ServeMux, deps Deps) {
	_ = mux
	_ = deps
}

// handleHealth é liveness, não readiness: responde 200 sem tocar em Postgres ou
// Redis. Um health que falha quando o banco pisca faz o orquestrador reiniciar
// uma API que estava perfeitamente viva. Fica fora do /api/v1 porque não é
// contrato de produto — é infraestrutura, e não deve ser versionada com ele.
func handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
