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
