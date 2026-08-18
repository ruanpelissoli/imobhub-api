// Package config carrega a configuração da aplicação a partir de variáveis de
// ambiente. É a única fonte de configuração do projeto: nenhum outro pacote deve
// ler os.Getenv diretamente.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Defaults aplicados quando a variável de ambiente correspondente não está
// definida ou está vazia.
const (
	DefaultSourcesFile  = "sources.txt"
	DefaultUserAgent    = "ImobHubBot/1.0"
	DefaultRateLimitMS  = 2000
	envDatabaseURL      = "DATABASE_URL"
	envAnthropicAPIKey  = "ANTHROPIC_API_KEY"
	envSourcesFile      = "SOURCES_FILE"
	envScraperUserAgent = "SCRAPER_USER_AGENT"
	envScraperRateLimit = "SCRAPER_RATE_LIMIT_MS"
)

// Config agrupa toda a configuração de runtime do scraper.
type Config struct {
	// DatabaseURL é a connection string PostgreSQL (obrigatória).
	DatabaseURL string
	// AnthropicAPIKey é a chave da API da Anthropic usada pelo pacote ai
	// (obrigatória).
	AnthropicAPIKey string
	// SourcesFile é o caminho do arquivo com a lista de fontes a serem raspadas.
	SourcesFile string
	// ScraperUserAgent é o User-Agent enviado em todas as requisições HTTP e
	// usado na avaliação do robots.txt.
	ScraperUserAgent string
	// ScraperRateLimit é o intervalo mínimo entre requisições para o mesmo host.
	ScraperRateLimit time.Duration
}

// ErrMissingRequired indica que uma ou mais variáveis obrigatórias não foram
// definidas. Use errors.Is para detectá-lo.
var ErrMissingRequired = errors.New("config: required environment variable is not set")

// Load lê a configuração das variáveis de ambiente, aplicando os defaults
// documentados em .env.example. Todos os erros de validação são acumulados para
// que o operador veja de uma vez tudo o que falta configurar.
func Load() (*Config, error) {
	var missing []string

	databaseURL := lookup(envDatabaseURL, "")
	if databaseURL == "" {
		missing = append(missing, envDatabaseURL)
	}

	anthropicKey := lookup(envAnthropicAPIKey, "")
	if anthropicKey == "" {
		missing = append(missing, envAnthropicAPIKey)
	}

	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrMissingRequired, strings.Join(missing, ", "))
	}

	rateLimitMS := DefaultRateLimitMS
	if raw := lookup(envScraperRateLimit, ""); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return nil, fmt.Errorf("config: %s must be an integer number of milliseconds, got %q: %w", envScraperRateLimit, raw, err)
		}
		if parsed < 0 {
			return nil, fmt.Errorf("config: %s must be zero or positive, got %d", envScraperRateLimit, parsed)
		}
		rateLimitMS = parsed
	}

	return &Config{
		DatabaseURL:      databaseURL,
		AnthropicAPIKey:  anthropicKey,
		SourcesFile:      lookup(envSourcesFile, DefaultSourcesFile),
		ScraperUserAgent: lookup(envScraperUserAgent, DefaultUserAgent),
		ScraperRateLimit: time.Duration(rateLimitMS) * time.Millisecond,
	}, nil
}

// lookup retorna o valor da variável de ambiente sem espaços nas bordas, ou
// fallback quando ela não existe ou está vazia. Tratar "" como ausente evita que
// uma variável exportada vazia (comum em docker-compose e CI) desative um default.
func lookup(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
