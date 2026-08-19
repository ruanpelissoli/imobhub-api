package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// setRequired define as variáveis obrigatórias para que os testes possam focar
// no comportamento das opcionais.
func setRequired(t *testing.T) {
	t.Helper()
	t.Setenv(envDatabaseURL, "postgres://user:pass@localhost:5432/imobhub")
	t.Setenv(envRedisURL, "redis://localhost:6379/0")
	t.Setenv(envAnthropicAPIKey, "sk-ant-test")
}

func TestLoadAppliesDefaults(t *testing.T) {
	setRequired(t)
	t.Setenv(envSourcesFile, "")
	t.Setenv(envScraperUserAgent, "")
	t.Setenv(envScraperRateLimit, "")
	t.Setenv(envAmenitiesFile, "")
	t.Setenv(envGroupingConfidenceThreshold, "")
	t.Setenv(envGroupingRadiusMeters, "")
	t.Setenv(envGroupingMaxCandidates, "")
	t.Setenv(envEnrichmentWorkers, "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}

	if cfg.SourcesFile != DefaultSourcesFile {
		t.Errorf("SourcesFile = %q, want %q", cfg.SourcesFile, DefaultSourcesFile)
	}
	if cfg.ScraperUserAgent != DefaultUserAgent {
		t.Errorf("ScraperUserAgent = %q, want %q", cfg.ScraperUserAgent, DefaultUserAgent)
	}
	if want := DefaultRateLimitMS * time.Millisecond; cfg.ScraperRateLimit != want {
		t.Errorf("ScraperRateLimit = %v, want %v", cfg.ScraperRateLimit, want)
	}
	if cfg.MigrationsDir != DefaultMigrationsDir {
		t.Errorf("MigrationsDir = %q, want %q", cfg.MigrationsDir, DefaultMigrationsDir)
	}
	if cfg.GeocodingProvider != DefaultGeocodingProvider {
		t.Errorf("GeocodingProvider = %q, want %q", cfg.GeocodingProvider, DefaultGeocodingProvider)
	}
	if cfg.GeocodingAPIKey != "" {
		t.Errorf("GeocodingAPIKey = %q, want %q", cfg.GeocodingAPIKey, "")
	}
	if want := DefaultGeocodingRateLimitMS * time.Millisecond; cfg.GeocodingRateLimit != want {
		t.Errorf("GeocodingRateLimit = %v, want %v", cfg.GeocodingRateLimit, want)
	}
	if cfg.AmenitiesFile != DefaultAmenitiesFile {
		t.Errorf("AmenitiesFile = %q, want %q", cfg.AmenitiesFile, DefaultAmenitiesFile)
	}
	if cfg.GroupingConfidenceThreshold != DefaultGroupingConfidenceThreshold {
		t.Errorf("GroupingConfidenceThreshold = %v, want %v", cfg.GroupingConfidenceThreshold, DefaultGroupingConfidenceThreshold)
	}
	if cfg.GroupingRadiusMeters != DefaultGroupingRadiusMeters {
		t.Errorf("GroupingRadiusMeters = %d, want %d", cfg.GroupingRadiusMeters, DefaultGroupingRadiusMeters)
	}
	if cfg.GroupingMaxCandidates != DefaultGroupingMaxCandidates {
		t.Errorf("GroupingMaxCandidates = %d, want %d", cfg.GroupingMaxCandidates, DefaultGroupingMaxCandidates)
	}
	if cfg.EnrichmentWorkers != DefaultEnrichmentWorkers {
		t.Errorf("EnrichmentWorkers = %d, want %d", cfg.EnrichmentWorkers, DefaultEnrichmentWorkers)
	}
}

func TestLoadReadsEnrichmentWorkers(t *testing.T) {
	setRequired(t)
	t.Setenv(envEnrichmentWorkers, "  8  ") // espaços devem ser removidos

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}

	if cfg.EnrichmentWorkers != 8 {
		t.Errorf("EnrichmentWorkers = %d, want 8", cfg.EnrichmentWorkers)
	}
}

// Zero worker é uma fila que nunca processa nada e que parece "funcionando" nos
// logs; por isso é erro de boot, como raio e candidatos do agrupamento.
func TestLoadRejectsInvalidEnrichmentWorkers(t *testing.T) {
	for _, raw := range []string{"0", "-2", "quatro"} {
		t.Run(raw, func(t *testing.T) {
			setRequired(t)
			t.Setenv(envEnrichmentWorkers, raw)

			_, err := Load()
			if err == nil {
				t.Fatalf("Load() with %s=%q error = nil, want error", envEnrichmentWorkers, raw)
			}
			if !strings.Contains(err.Error(), envEnrichmentWorkers) {
				t.Errorf("error %q does not mention %q", err, envEnrichmentWorkers)
			}
			if !strings.Contains(err.Error(), raw) {
				t.Errorf("error %q does not report the received value %q", err, raw)
			}
		})
	}
}

func TestLoadReadsGroupingOverrides(t *testing.T) {
	setRequired(t)
	t.Setenv(envGroupingConfidenceThreshold, "  0.7  ") // espaços devem ser removidos
	t.Setenv(envGroupingRadiusMeters, "250")
	t.Setenv(envGroupingMaxCandidates, "3")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}

	if cfg.GroupingConfidenceThreshold != 0.7 {
		t.Errorf("GroupingConfidenceThreshold = %v, want 0.7", cfg.GroupingConfidenceThreshold)
	}
	if cfg.GroupingRadiusMeters != 250 {
		t.Errorf("GroupingRadiusMeters = %d, want 250", cfg.GroupingRadiusMeters)
	}
	if cfg.GroupingMaxCandidates != 3 {
		t.Errorf("GroupingMaxCandidates = %d, want 3", cfg.GroupingMaxCandidates)
	}
}

// As bordas de [0,1] são valores válidos: 0 aceita qualquer match do modelo e
// 1 exige certeza. Ambos são configurações legítimas de calibração.
func TestLoadAcceptsThresholdAtTheBounds(t *testing.T) {
	for _, raw := range []string{"0", "1"} {
		t.Run(raw, func(t *testing.T) {
			setRequired(t)
			t.Setenv(envGroupingConfidenceThreshold, raw)

			if _, err := Load(); err != nil {
				t.Fatalf("Load() with %s=%q error = %v, want nil", envGroupingConfidenceThreshold, raw, err)
			}
		})
	}
}

// Valor inválido é erro de boot citando o valor recebido, nunca fallback
// silencioso: um threshold errado produz duplicatas (ou fusões) em silêncio
// pela coleta inteira.
func TestLoadRejectsInvalidGroupingValues(t *testing.T) {
	tests := []struct {
		envName string
		raw     string
	}{
		{envGroupingConfidenceThreshold, "-0.1"},
		{envGroupingConfidenceThreshold, "1.5"},
		{envGroupingConfidenceThreshold, "alto"},
		{envGroupingRadiusMeters, "0"},
		{envGroupingRadiusMeters, "-5"},
		{envGroupingRadiusMeters, "cem"},
		{envGroupingMaxCandidates, "0"},
		{envGroupingMaxCandidates, "-1"},
	}

	for _, tt := range tests {
		t.Run(tt.envName+"="+tt.raw, func(t *testing.T) {
			setRequired(t)
			t.Setenv(tt.envName, tt.raw)

			_, err := Load()
			if err == nil {
				t.Fatalf("Load() with %s=%q error = nil, want error", tt.envName, tt.raw)
			}
			if !strings.Contains(err.Error(), tt.envName) {
				t.Errorf("error %q does not mention %q", err, tt.envName)
			}
			if !strings.Contains(err.Error(), tt.raw) {
				t.Errorf("error %q does not report the received value %q", err, tt.raw)
			}
		})
	}
}

func TestLoadReadsOverrides(t *testing.T) {
	setRequired(t)
	t.Setenv(envSourcesFile, "  custom.txt  ") // espaços devem ser removidos
	t.Setenv(envScraperUserAgent, "CustomBot/2.0")
	t.Setenv(envScraperRateLimit, "500")
	t.Setenv(envMigrationsDir, "db/migrations")
	t.Setenv(envAmenitiesFile, "etc/amenities.yaml")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}

	if cfg.SourcesFile != "custom.txt" {
		t.Errorf("SourcesFile = %q, want %q", cfg.SourcesFile, "custom.txt")
	}
	if cfg.ScraperUserAgent != "CustomBot/2.0" {
		t.Errorf("ScraperUserAgent = %q, want %q", cfg.ScraperUserAgent, "CustomBot/2.0")
	}
	if want := 500 * time.Millisecond; cfg.ScraperRateLimit != want {
		t.Errorf("ScraperRateLimit = %v, want %v", cfg.ScraperRateLimit, want)
	}
	if cfg.MigrationsDir != "db/migrations" {
		t.Errorf("MigrationsDir = %q, want %q", cfg.MigrationsDir, "db/migrations")
	}
	if cfg.AmenitiesFile != "etc/amenities.yaml" {
		t.Errorf("AmenitiesFile = %q, want %q", cfg.AmenitiesFile, "etc/amenities.yaml")
	}
}

func TestLoadReadsGeocodingOverrides(t *testing.T) {
	setRequired(t)
	t.Setenv(envGeocodingProvider, "  GoogleMaps  ") // espaços e caixa toleradas
	t.Setenv(envGeocodingAPIKey, "gm-secret")
	t.Setenv(envGeocodingRateLimit, "250")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}

	if cfg.GeocodingProvider != ProviderGoogleMaps {
		t.Errorf("GeocodingProvider = %q, want %q", cfg.GeocodingProvider, ProviderGoogleMaps)
	}
	if cfg.GeocodingAPIKey != "gm-secret" {
		t.Errorf("GeocodingAPIKey = %q, want %q", cfg.GeocodingAPIKey, "gm-secret")
	}
	if want := 250 * time.Millisecond; cfg.GeocodingRateLimit != want {
		t.Errorf("GeocodingRateLimit = %v, want %v", cfg.GeocodingRateLimit, want)
	}
}

func TestLoadRejectsUnknownGeocodingProvider(t *testing.T) {
	setRequired(t)
	t.Setenv(envGeocodingProvider, "mapquest")

	// Provider desconhecido é erro de boot, nunca fallback silencioso para o
	// nominatim: as coordenadas sairiam de um serviço diferente do configurado.
	_, err := Load()
	if err == nil {
		t.Fatalf("Load() error = nil, want error")
	}
	if !strings.Contains(err.Error(), envGeocodingProvider) {
		t.Errorf("error %q does not mention %q", err.Error(), envGeocodingProvider)
	}
}

func TestLoadRequiresAPIKeyOnlyForGoogleMaps(t *testing.T) {
	t.Run("googlemaps sem chave", func(t *testing.T) {
		setRequired(t)
		t.Setenv(envGeocodingProvider, ProviderGoogleMaps)
		t.Setenv(envGeocodingAPIKey, "")

		_, err := Load()
		if !errors.Is(err, ErrMissingRequired) {
			t.Fatalf("Load() error = %v, want ErrMissingRequired", err)
		}
		if !strings.Contains(err.Error(), envGeocodingAPIKey) {
			t.Errorf("error %q does not mention %q", err.Error(), envGeocodingAPIKey)
		}
	})

	t.Run("nominatim sem chave", func(t *testing.T) {
		setRequired(t)
		t.Setenv(envGeocodingProvider, ProviderNominatim)
		t.Setenv(envGeocodingAPIKey, "")

		if _, err := Load(); err != nil {
			t.Fatalf("Load() error = %v, want nil (nominatim does not use an API key)", err)
		}
	})
}

func TestLoadRejectsInvalidGeocodingRateLimit(t *testing.T) {
	for _, raw := range []string{"abc", "-1"} {
		t.Run(raw, func(t *testing.T) {
			setRequired(t)
			t.Setenv(envGeocodingRateLimit, raw)

			if _, err := Load(); err == nil {
				t.Fatalf("Load() with %s=%q error = nil, want error", envGeocodingRateLimit, raw)
			}
		})
	}
}

func TestLoadAcceptsZeroGeocodingRateLimit(t *testing.T) {
	setRequired(t)
	t.Setenv(envGeocodingRateLimit, "0")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if cfg.GeocodingRateLimit != 0 {
		t.Errorf("GeocodingRateLimit = %v, want 0 (disabled)", cfg.GeocodingRateLimit)
	}
}

func TestLoadReadsRedisURL(t *testing.T) {
	setRequired(t)
	t.Setenv(envRedisURL, "  redis://cache.internal:6380/2  ") // espaços devem ser removidos

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}

	if want := "redis://cache.internal:6380/2"; cfg.RedisURL != want {
		t.Errorf("RedisURL = %q, want %q", cfg.RedisURL, want)
	}
}

// REDIS_URL não tem default: o projeto rejeita fallback silencioso, e um
// localhost:6379 implícito apontaria para um Redis que não é o do ambiente.
func TestLoadRequiresRedisURL(t *testing.T) {
	setRequired(t)
	t.Setenv(envRedisURL, "")

	_, err := Load()
	if !errors.Is(err, ErrMissingRequired) {
		t.Fatalf("Load() error = %v, want ErrMissingRequired", err)
	}
	if !strings.Contains(err.Error(), envRedisURL) {
		t.Errorf("error %q does not mention %q", err, envRedisURL)
	}
}

func TestLoadReportsAllMissingRequired(t *testing.T) {
	t.Setenv(envDatabaseURL, "")
	t.Setenv(envRedisURL, "")
	t.Setenv(envAnthropicAPIKey, "")

	_, err := Load()
	if !errors.Is(err, ErrMissingRequired) {
		t.Fatalf("Load() error = %v, want ErrMissingRequired", err)
	}
	// As três faltantes devem aparecer para o operador corrigir de uma vez.
	msg := err.Error()
	for _, name := range []string{envDatabaseURL, envRedisURL, envAnthropicAPIKey} {
		if !strings.Contains(msg, name) {
			t.Errorf("error %q does not mention %q", msg, name)
		}
	}
}

func TestLoadRejectsInvalidRateLimit(t *testing.T) {
	for _, raw := range []string{"abc", "-1"} {
		t.Run(raw, func(t *testing.T) {
			setRequired(t)
			t.Setenv(envScraperRateLimit, raw)

			if _, err := Load(); err == nil {
				t.Fatalf("Load() with %s=%q error = nil, want error", envScraperRateLimit, raw)
			}
		})
	}
}
