package config

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	// DefaultPort é a porta usada quando PORT não está definida.
	DefaultPort = 8080

	// minPort/maxPort delimitam a faixa válida de portas TCP. Fora dela o bind
	// falharia mais tarde, com uma mensagem do sistema operacional que não cita
	// a variável de ambiente responsável.
	minPort = 1
	maxPort = 65535

	envPort        = "PORT"
	envCORSOrigins = "CORS_ORIGINS"
	corsOriginsSep = ","
)

// APIConfig agrupa a configuração de runtime do servidor HTTP (cmd/api).
type APIConfig struct {
	// DatabaseURL é a connection string PostgreSQL (obrigatória).
	DatabaseURL string
	// RedisURL é a connection string do Redis (obrigatória).
	RedisURL string
	// Port é a porta TCP em que o servidor escuta, já validada em [1, 65535].
	Port int
	// CORSOrigins é a allowlist de origens do navegador. Vazia (nil) desliga o
	// CORS por completo.
	CORSOrigins []string
}

// LoadAPI lê a configuração do servidor HTTP das variáveis de ambiente.
//
// É deliberadamente separada de Load: a API exige apenas DATABASE_URL e
// REDIS_URL. Afrouxar Load para tornar ANTHROPIC_API_KEY opcional resolveria o
// boot da API ao custo de deixar o scraper subir sem chave e quebrar só na
// primeira chamada de IA — uma regressão silenciosa no processo que paga por ela.
func LoadAPI() (*APIConfig, error) {
	var missing []string

	databaseURL := lookup(envDatabaseURL, "")
	if databaseURL == "" {
		missing = append(missing, envDatabaseURL)
	}

	redisURL := lookup(envRedisURL, "")
	if redisURL == "" {
		missing = append(missing, envRedisURL)
	}

	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrMissingRequired, strings.Join(missing, ", "))
	}

	port, err := parsePort(envPort, DefaultPort)
	if err != nil {
		return nil, err
	}

	return &APIConfig{
		DatabaseURL: databaseURL,
		RedisURL:    redisURL,
		Port:        port,
		CORSOrigins: parseCORSOrigins(lookup(envCORSOrigins, "")),
	}, nil
}

// parsePort lê uma porta TCP. Valor fora de [1, 65535] é erro de boot e não
// fallback silencioso: um PORT=8O80 (letra no lugar do zero) faria o servidor
// subir na 8080 e o front continuaria batendo na porta errada sem uma linha de
// log explicando o desencontro.
func parsePort(envName string, fallback int) (int, error) {
	raw := lookup(envName, "")
	if raw == "" {
		return fallback, nil
	}

	parsed, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("config: %s must be an integer between %d and %d, got %q: %w",
			envName, minPort, maxPort, raw, err)
	}
	if parsed < minPort || parsed > maxPort {
		return 0, fmt.Errorf("config: %s must be between %d and %d, got %d",
			envName, minPort, maxPort, parsed)
	}

	return parsed, nil
}

// parseCORSOrigins quebra a lista separada por vírgula em origens. Entradas
// vazias são descartadas para que uma vírgula sobrando ("http://a.com,") não
// vire uma origem "" que casaria com requisições sem header Origin.
func parseCORSOrigins(raw string) []string {
	if raw == "" {
		return nil
	}

	var origins []string
	for _, part := range strings.Split(raw, corsOriginsSep) {
		if origin := strings.TrimSpace(part); origin != "" {
			origins = append(origins, origin)
		}
	}

	return origins
}
