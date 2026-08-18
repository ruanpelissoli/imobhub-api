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
	t.Setenv(envAnthropicAPIKey, "sk-ant-test")
}

func TestLoadAppliesDefaults(t *testing.T) {
	setRequired(t)
	t.Setenv(envSourcesFile, "")
	t.Setenv(envScraperUserAgent, "")
	t.Setenv(envScraperRateLimit, "")

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
}

func TestLoadReadsOverrides(t *testing.T) {
	setRequired(t)
	t.Setenv(envSourcesFile, "  custom.txt  ") // espaços devem ser removidos
	t.Setenv(envScraperUserAgent, "CustomBot/2.0")
	t.Setenv(envScraperRateLimit, "500")

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
}

func TestLoadReportsAllMissingRequired(t *testing.T) {
	t.Setenv(envDatabaseURL, "")
	t.Setenv(envAnthropicAPIKey, "")

	_, err := Load()
	if !errors.Is(err, ErrMissingRequired) {
		t.Fatalf("Load() error = %v, want ErrMissingRequired", err)
	}
	// As duas faltantes devem aparecer para o operador corrigir de uma vez.
	msg := err.Error()
	for _, name := range []string{envDatabaseURL, envAnthropicAPIKey} {
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
