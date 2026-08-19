package api

import (
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

	corsAllowMethods = "GET, POST, PUT, PATCH, DELETE, OPTIONS"
	corsAllowHeaders = "Content-Type, Authorization"
	corsMaxAge       = "600"
)

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

func recovery(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rw := wrapWriter(w)

		defer func() {
			rec := recover()
			if rec == nil {
				return
			}

			logger.Error("api: recovered from panic in handler",
				"panic", rec,
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

		logger.Info("api: request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"bytes", rw.bytes,
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
