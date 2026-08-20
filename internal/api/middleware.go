package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"
)

const (
	headerOrigin        = "Origin"
	headerVary          = "Vary"
	headerAllowOrigin   = "Access-Control-Allow-Origin"
	headerAllowMethods  = "Access-Control-Allow-Methods"
	headerAllowHeaders  = "Access-Control-Allow-Headers"
	headerMaxAge        = "Access-Control-Max-Age"
	headerRequestMethod = "Access-Control-Request-Method"
	headerExposeHeaders = "Access-Control-Expose-Headers"
	headerRequestID     = "X-Request-Id"

	corsAllowMethods = "GET, POST, PUT, PATCH, DELETE, OPTIONS"
	corsAllowHeaders = "Content-Type, Authorization"
	corsMaxAge       = "600"
	// corsExposeHeaders libera os headers de resposta que o browser esconde por
	// padrão. Sem X-Cache aqui, o front não consegue nem medir o hit rate; sem
	// X-Request-Id, não consegue citar o id da requisição num relato de erro —
	// que é a razão de devolvê-lo.
	corsExposeHeaders = headerCacheStatus + ", " + headerRequestID

	requestIDMaxLen = 64
)

// quietPaths são os paths de infraestrutura, logados em Debug para não afogar o
// log de produção com o liveness do orquestrador. /metrics já entra aqui mesmo
// sem existir no roteador ainda, para não exigir edição quando nascer.
var quietPaths = map[string]struct{}{
	"/health":  {},
	"/metrics": {},
}

type ctxKeyRequestID struct{}

func requestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(ctxKeyRequestID{}).(string)
	return id
}

// responseWriter observa o que a cadeia escreveu, para que o logging registre
// status e tamanho e o recovery saiba se ainda dá para trocar a resposta por um
// 500.
type responseWriter struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
}

// wrapWriter é idempotente de propósito: recovery cria o wrapper e logging
// reaproveita o mesmo, sem contar bytes duas vezes nem esconder o status real
// atrás de uma segunda camada.
func wrapWriter(w http.ResponseWriter) *responseWriter {
	if rw, ok := w.(*responseWriter); ok {
		return rw
	}
	return &responseWriter{ResponseWriter: w, status: http.StatusOK}
}

func (w *responseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.status = status
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// requestID garante uma identidade por requisição: herda o X-Request-Id do
// cliente quando ele é confiável, gera um UUID v4 caso contrário, devolve o
// valor no header de resposta e o propaga pelo contexto.
//
// É middleware próprio, e não parte do logging, porque produzir identidade é
// gerar estado — o logging continua só observando.
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(headerRequestID)
		if !validRequestID(id) {
			id = newRequestID()
		}

		// Setado antes do downstream para que o id exista na resposta mesmo se
		// o handler entrar em panic.
		w.Header().Set(headerRequestID, id)

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeyRequestID{}, id)))
	})
}

// validRequestID filtra input não confiável que termina num header de resposta e
// numa linha de log. Valor rejeitado é substituído em silêncio: nem 400 nem log
// de erro, que só dariam ao cliente um canal barato de flood.
func validRequestID(s string) bool {
	if s == "" || len(s) > requestIDMaxLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

func newRequestID() string {
	var b [16]byte
	// crypto/rand.Read não devolve erro utilizável: o pacote entra em pânico
	// internamente em falha de entropia, então não há fallback a inventar.
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80

	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:36], b[10:16])
	return string(out[:])
}

func recovery(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rw := wrapWriter(w)

		defer func() {
			rec := recover()
			if rec == nil {
				return
			}

			// O id vem do header já setado na resposta, não do contexto: o r
			// capturado por este defer é o de fora do requestID.
			logger.Error("api: recovered from panic in handler",
				"panic", rec,
				"request_id", w.Header().Get(headerRequestID),
				"method", r.Method,
				"path", r.URL.Path,
				"stack", string(debug.Stack()),
			)

			// Se o handler já começou a responder, o status foi para a rede e
			// trocar o corpo agora só produziria uma resposta corrompida.
			if rw.wroteHeader {
				return
			}
			writeError(rw, http.StatusInternalServerError, msgInternalError)
		}()

		next.ServeHTTP(rw, r)
	})
}

func logging(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rw := wrapWriter(w)
		start := time.Now()

		next.ServeHTTP(rw, r)

		level := slog.LevelInfo
		if _, quiet := quietPaths[r.URL.Path]; quiet {
			level = slog.LevelDebug
		}

		// remote_addr é o r.RemoteAddr cru: interpretar X-Forwarded-For sem um
		// proxy confiável configurado é spoofing de graça.
		logger.Log(r.Context(), level, "api: request",
			"request_id", requestIDFromContext(r.Context()),
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"bytes", rw.bytes,
			"remote_addr", r.RemoteAddr,
		)
	})
}

// cors aplica a allowlist de origens. Lista vazia devolve o handler intacto: um
// middleware que não faz nada ainda custaria uma alocação por requisição, e
// deixar claro que "sem CORS_ORIGINS = sem CORS" evita header meio-configurado.
//
// A origem é ecoada em vez de "*" porque a allowlist já é a política; "*"
// inviabilizaria cookies/credenciais depois, e trocar isso quando o front
// precisar autenticar seria uma mudança silenciosa de segurança.
func cors(next http.Handler, origins []string) http.Handler {
	if len(origins) == 0 {
		return next
	}

	allowed := make(map[string]struct{}, len(origins))
	for _, origin := range origins {
		allowed[origin] = struct{}{}
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get(headerOrigin)
		if _, ok := allowed[origin]; origin == "" || !ok {
			next.ServeHTTP(w, r)
			return
		}

		w.Header().Add(headerVary, headerOrigin)
		w.Header().Set(headerAllowOrigin, origin)
		// Expose-Headers vale para a resposta real, não para o preflight — por
		// isso é setado aqui, antes do desvio de OPTIONS.
		w.Header().Set(headerExposeHeaders, corsExposeHeaders)

		if r.Method == http.MethodOptions && r.Header.Get(headerRequestMethod) != "" {
			w.Header().Set(headerAllowMethods, corsAllowMethods)
			w.Header().Set(headerAllowHeaders, corsAllowHeaders)
			w.Header().Set(headerMaxAge, corsMaxAge)
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// jsonErrors converte os 404/405 do ServeMux, que saem em text/plain, para o
// envelope de erro do projeto. Sem isso o front receberia "404 page not found"
// numa rota errada e JSON em todas as outras falhas.
func jsonErrors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&jsonErrorWriter{ResponseWriter: w}, r)
	})
}

type jsonErrorWriter struct {
	http.ResponseWriter
	replaced bool
}

func (w *jsonErrorWriter) WriteHeader(status int) {
	message, replace := plainErrorMessage(w.Header(), status)
	if !replace {
		w.ResponseWriter.WriteHeader(status)
		return
	}

	// O header Allow que o mux setou no 405 é preservado: só o corpo muda.
	w.replaced = true
	w.Header().Set(contentTypeHeader, contentTypeJSON)
	w.ResponseWriter.WriteHeader(status)
	_ = json.NewEncoder(w.ResponseWriter).Encode(errorBody{Error: message})
}

func (w *jsonErrorWriter) Write(b []byte) (int, error) {
	if w.replaced {
		return len(b), nil
	}
	return w.ResponseWriter.Write(b)
}

// plainErrorMessage decide se o corpo deve ser substituído. O discriminador é o
// Content-Type: handlers do projeto sempre setam application/json antes do
// WriteHeader, então só o 404/405 cru do ServeMux chega aqui sem ele.
func plainErrorMessage(header http.Header, status int) (string, bool) {
	if header.Get(contentTypeHeader) == contentTypeJSON {
		return "", false
	}

	switch status {
	case http.StatusNotFound:
		return msgNotFound, true
	case http.StatusMethodNotAllowed:
		return msgNotAllowed, true
	default:
		return "", false
	}
}
