// Package config carrega a configuração da aplicação a partir de variáveis de
// ambiente. É a única fonte de configuração do projeto: nenhum outro pacote deve
// ler os.Getenv diretamente.
package config

import (
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

// Defaults aplicados quando a variável de ambiente correspondente não está
// definida ou está vazia.
const (
	DefaultSourcesFile   = "sources.txt"
	DefaultUserAgent     = "ImobHubBot/1.0"
	DefaultRateLimitMS   = 2000
	DefaultMigrationsDir = "migrations"
	// DefaultGeocodingProvider é o provider de geocodificação usado quando
	// GEOCODING_PROVIDER não está definida. Nominatim é gratuito e não exige
	// chave; por isso é o default.
	DefaultGeocodingProvider = ProviderNominatim
	// DefaultGeocodingRateLimitMS é o intervalo mínimo entre requisições ao
	// provider de geocodificação. O Nominatim permite no máximo 1 req/s.
	DefaultGeocodingRateLimitMS = 1000
	DefaultAmenitiesFile        = "configs/amenities.yaml"

	// DefaultGroupingConfidenceThreshold é a confiança mínima para aceitar que
	// dois anúncios são o mesmo imóvel. 0.85 é deliberadamente alto: juntar
	// imóveis distintos é caro de desfazer, criar um canônico duplicado não.
	DefaultGroupingConfidenceThreshold = 0.85
	// DefaultGroupingRadiusMeters é o raio da busca por imóveis candidatos.
	// 100 m cobre imprecisão de geocodificação de endereço sem varrer o
	// quarteirão inteiro.
	DefaultGroupingRadiusMeters = 100
	// DefaultGroupingMaxCandidates limita quantos imóveis vão para o prompt da
	// comparação. Junto com o raio, é o controle de custo do agrupamento.
	DefaultGroupingMaxCandidates = 5

	// DefaultEnrichmentWorkers é a concorrência da fila de enriquecimento.
	// 4 é conservador de propósito: a geocodificação serializa em 1 req/s
	// global (o DomainLimiter é compartilhado), então subir esse número só
	// paraleliza as chamadas de IA do agrupamento — que são pagas por anúncio.
	DefaultEnrichmentWorkers = 4

	envDatabaseURL        = "DATABASE_URL"
	envRedisURL           = "REDIS_URL"
	envAnthropicAPIKey    = "ANTHROPIC_API_KEY"
	envSourcesFile        = "SOURCES_FILE"
	envScraperUserAgent   = "SCRAPER_USER_AGENT"
	envScraperRateLimit   = "SCRAPER_RATE_LIMIT_MS"
	envMigrationsDir      = "MIGRATIONS_DIR"
	envGeocodingProvider  = "GEOCODING_PROVIDER"
	envGeocodingAPIKey    = "GEOCODING_API_KEY"
	envGeocodingRateLimit = "GEOCODING_RATE_LIMIT_MS"
	envAmenitiesFile      = "AMENITIES_FILE"

	envGroupingConfidenceThreshold = "GROUPING_CONFIDENCE_THRESHOLD"
	envGroupingRadiusMeters        = "GROUPING_RADIUS_METERS"
	envGroupingMaxCandidates       = "GROUPING_MAX_CANDIDATES"

	envEnrichmentWorkers = "ENRICHMENT_WORKERS"
)

// Providers de geocodificação aceitos em GEOCODING_PROVIDER.
//
// As mesmas constantes existem em internal/enrichment. A duplicação é
// consciente: config é folha do grafo de importação (ver internal/CLAUDE.md) e
// não pode importar enrichment. config valida no boot — valor desconhecido é
// erro, nunca fallback silencioso — e enrichment.NewGeocoder valida de novo,
// devolvendo ErrProviderNotSupported. Defesa em profundidade.
const (
	ProviderNominatim  = "nominatim"
	ProviderGoogleMaps = "googlemaps"
)

// Config agrupa toda a configuração de runtime do scraper.
type Config struct {
	// DatabaseURL é a connection string PostgreSQL (obrigatória).
	DatabaseURL string
	// RedisURL é a connection string do Redis, no formato aceito por
	// redis.ParseURL (obrigatória). Não tem default: um localhost implícito
	// apontaria para um Redis que não é o do ambiente, sem uma linha de aviso.
	RedisURL string
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
	// MigrationsDir é o diretório com os arquivos .sql de migration aplicados
	// no startup. Relativo ao working directory do processo.
	MigrationsDir string
	// GeocodingProvider indica qual serviço converte endereço em coordenadas.
	// Já normalizado e validado: só ProviderNominatim ou ProviderGoogleMaps
	// chegam aqui.
	GeocodingProvider string
	// GeocodingAPIKey é a chave do provider de geocodificação. Vazia para
	// nominatim (que não exige chave); obrigatória para googlemaps.
	GeocodingAPIKey string
	// GeocodingRateLimit é o intervalo mínimo entre requisições ao provider de
	// geocodificação.
	GeocodingRateLimit time.Duration
	// AmenitiesFile é o caminho do arquivo YAML com o vocabulário de comodidades
	// usado pelo pacote enrichment. Relativo ao working directory do processo.
	AmenitiesFile string
	// GroupingConfidenceThreshold é a confiança mínima, em [0,1], para o
	// agrupamento aceitar que um anúncio é o mesmo imóvel de um candidato.
	GroupingConfidenceThreshold float64
	// GroupingRadiusMeters é o raio, em metros, da busca por imóveis candidatos.
	GroupingRadiusMeters int
	// GroupingMaxCandidates é o número máximo de imóveis enviados ao modelo em
	// cada comparação.
	GroupingMaxCandidates int
	// EnrichmentWorkers é quantos anúncios a fila de enriquecimento processa
	// simultaneamente (internal/enrichqueue).
	EnrichmentWorkers int
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

	redisURL := lookup(envRedisURL, "")
	if redisURL == "" {
		missing = append(missing, envRedisURL)
	}

	anthropicKey := lookup(envAnthropicAPIKey, "")
	if anthropicKey == "" {
		missing = append(missing, envAnthropicAPIKey)
	}

	// O provider é resolvido antes do relatório de faltantes porque decide se a
	// GEOCODING_API_KEY é obrigatória. Valor desconhecido é erro de boot, não
	// fallback silencioso: quem digitou "google" errado espera as cotas do
	// Google e receberia, sem aviso, as coordenadas do Nominatim.
	geocodingProvider := strings.ToLower(lookup(envGeocodingProvider, DefaultGeocodingProvider))
	switch geocodingProvider {
	case ProviderNominatim, ProviderGoogleMaps:
	default:
		return nil, fmt.Errorf("config: %s must be one of %q or %q, got %q",
			envGeocodingProvider, ProviderNominatim, ProviderGoogleMaps, geocodingProvider)
	}

	// A chave só é obrigatória para googlemaps; o Nominatim é gratuito e não
	// aceita chave nenhuma.
	geocodingAPIKey := lookup(envGeocodingAPIKey, "")
	if geocodingProvider == ProviderGoogleMaps && geocodingAPIKey == "" {
		missing = append(missing, envGeocodingAPIKey)
	}

	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrMissingRequired, strings.Join(missing, ", "))
	}

	rateLimitMS, err := parseRateLimitMS(envScraperRateLimit, DefaultRateLimitMS)
	if err != nil {
		return nil, err
	}

	geocodingRateLimitMS, err := parseRateLimitMS(envGeocodingRateLimit, DefaultGeocodingRateLimitMS)
	if err != nil {
		return nil, err
	}

	groupingThreshold, err := parseUnitInterval(envGroupingConfidenceThreshold, DefaultGroupingConfidenceThreshold)
	if err != nil {
		return nil, err
	}

	groupingRadius, err := parsePositiveInt(envGroupingRadiusMeters, DefaultGroupingRadiusMeters)
	if err != nil {
		return nil, err
	}

	groupingMaxCandidates, err := parsePositiveInt(envGroupingMaxCandidates, DefaultGroupingMaxCandidates)
	if err != nil {
		return nil, err
	}

	enrichmentWorkers, err := parsePositiveInt(envEnrichmentWorkers, DefaultEnrichmentWorkers)
	if err != nil {
		return nil, err
	}

	return &Config{
		DatabaseURL:        databaseURL,
		RedisURL:           redisURL,
		AnthropicAPIKey:    anthropicKey,
		SourcesFile:        lookup(envSourcesFile, DefaultSourcesFile),
		ScraperUserAgent:   lookup(envScraperUserAgent, DefaultUserAgent),
		ScraperRateLimit:   time.Duration(rateLimitMS) * time.Millisecond,
		MigrationsDir:      lookup(envMigrationsDir, DefaultMigrationsDir),
		GeocodingProvider:  geocodingProvider,
		GeocodingAPIKey:    geocodingAPIKey,
		GeocodingRateLimit: time.Duration(geocodingRateLimitMS) * time.Millisecond,
		AmenitiesFile:      lookup(envAmenitiesFile, DefaultAmenitiesFile),

		GroupingConfidenceThreshold: groupingThreshold,
		GroupingRadiusMeters:        groupingRadius,
		GroupingMaxCandidates:       groupingMaxCandidates,

		EnrichmentWorkers: enrichmentWorkers,
	}, nil
}

// parseUnitInterval lê uma probabilidade em [0,1]. Valor fora da faixa é erro
// de boot e não recorte silencioso para a borda: um threshold de 8.5 (vírgula
// digitada no lugar do ponto) viraria "nunca agrupa", e a coleta inteira
// produziria imóveis duplicados sem uma linha de log explicando por quê.
func parseUnitInterval(envName string, fallback float64) (float64, error) {
	raw := lookup(envName, "")
	if raw == "" {
		return fallback, nil
	}

	parsed, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("config: %s must be a number between 0 and 1, got %q: %w", envName, raw, err)
	}
	if math.IsNaN(parsed) || parsed < 0 || parsed > 1 {
		return 0, fmt.Errorf("config: %s must be between 0 and 1, got %v", envName, parsed)
	}

	return parsed, nil
}

// parsePositiveInt lê um inteiro estritamente positivo. Ao contrário de
// parseRateLimitMS, zero **não** é aceito: raio zero não encontra candidato
// nenhum, zero candidatos desliga o agrupamento e zero workers é uma fila que
// nunca processa nada — os três casos parecem "funcionando" nos logs enquanto
// criam um imóvel canônico por anúncio, ou nenhum.
func parsePositiveInt(envName string, fallback int) (int, error) {
	raw := lookup(envName, "")
	if raw == "" {
		return fallback, nil
	}

	parsed, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("config: %s must be a positive integer, got %q: %w", envName, raw, err)
	}
	if parsed <= 0 {
		return 0, fmt.Errorf("config: %s must be greater than 0, got %d", envName, parsed)
	}

	return parsed, nil
}

// parseRateLimitMS lê um intervalo em milissegundos com as regras comuns a
// todos os rate limits do projeto: inteiro, zero desativa o espaçamento
// (só para testes locais) e negativo é erro. Existe para que
// SCRAPER_RATE_LIMIT_MS e GEOCODING_RATE_LIMIT_MS não divirjam.
func parseRateLimitMS(envName string, fallback int) (int, error) {
	raw := lookup(envName, "")
	if raw == "" {
		return fallback, nil
	}

	parsed, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("config: %s must be an integer number of milliseconds, got %q: %w", envName, raw, err)
	}
	if parsed < 0 {
		return 0, fmt.Errorf("config: %s must be zero or positive, got %d", envName, parsed)
	}

	return parsed, nil
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
