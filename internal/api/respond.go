package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

const (
	contentTypeHeader = "Content-Type"
	contentTypeJSON   = "application/json"

	// msgInternalError é a mensagem pública de qualquer falha inesperada. O
	// detalhe (erro do pgx, stack trace, connection string) fica só no log:
	// DATABASE_URL e REDIS_URL carregam senha, e um stack trace no corpo entrega
	// a estrutura interna a quem chamar a API.
	msgInternalError = "internal server error"
	msgNotFound      = "not found"
	msgNotAllowed    = "method not allowed"
	msgInvalidID     = "id inválido"
)

// errorBody é o envelope de erro do projeto. Todo handler da API responde
// falhas nesse formato, e o front depende disso.
type errorBody struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set(contentTypeHeader, contentTypeJSON)
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(payload); err != nil {
		// O status já foi escrito: não há como transformar isso em resposta de
		// erro. Resta registrar — costuma ser cliente que desconectou no meio.
		slog.Error("api: failed to encode response body", "error", err)
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, errorBody{Error: message})
}

// writeJSONBytes escreve um corpo já serializado. Existe para o caminho em que
// os mesmos bytes vão para a resposta e para o cache: writeJSON usaria um
// json.Encoder, que acrescenta "\n", e o corpo de um cache hit deixaria de ser
// byte a byte igual ao de um miss.
//
// O Content-Type é setado antes do WriteHeader, como todo handler do projeto —
// é o discriminador de que jsonErrors depende.
func writeJSONBytes(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set(contentTypeHeader, contentTypeJSON)
	w.WriteHeader(status)

	if _, err := w.Write(body); err != nil {
		slog.Error("api: failed to write response body", "error", err)
	}
}
