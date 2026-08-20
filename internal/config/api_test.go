package config

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// setAPIRequired define só o que a API exige — nenhuma variável do scraper.
func setAPIRequired(t *testing.T) {
	t.Helper()
	t.Setenv(envDatabaseURL, "postgres://user:pass@localhost:5432/imobhub")
	t.Setenv(envRedisURL, "redis://localhost:6379/0")
}

func TestLoadAPIAppliesDefaults(t *testing.T) {
	setAPIRequired(t)
	t.Setenv(envPort, "")
	t.Setenv(envCORSOrigins, "")

	cfg, err := LoadAPI()
	if err != nil {
		t.Fatalf("LoadAPI() error = %v, want nil", err)
	}

	if cfg.Port != DefaultPort {
		t.Errorf("Port = %d, want %d", cfg.Port, DefaultPort)
	}
	if cfg.CORSOrigins != nil {
		t.Errorf("CORSOrigins = %v, want nil (CORS desligado)", cfg.CORSOrigins)
	}
	if cfg.RateLimitRPM != DefaultRateLimitRPM {
		t.Errorf("RateLimitRPM = %d, want %d", cfg.RateLimitRPM, DefaultRateLimitRPM)
	}
}

func TestLoadAPIReadsRateLimitRPM(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want int
	}{
		{name: "valor com espaços", raw: "  120  ", want: 120},
		{name: "zero desliga o limiter", raw: "0", want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setAPIRequired(t)
			t.Setenv(envRateLimitRPM, tt.raw)

			cfg, err := LoadAPI()
			if err != nil {
				t.Fatalf("LoadAPI() error = %v, want nil", err)
			}
			if cfg.RateLimitRPM != tt.want {
				t.Errorf("RateLimitRPM = %d, want %d", cfg.RateLimitRPM, tt.want)
			}
		})
	}
}

func TestLoadAPIRejectsInvalidRateLimitRPM(t *testing.T) {
	for _, raw := range []string{"-1", "abc", "60.5"} {
		t.Run(raw, func(t *testing.T) {
			setAPIRequired(t)
			t.Setenv(envRateLimitRPM, raw)

			_, err := LoadAPI()
			if err == nil {
				t.Fatalf("LoadAPI() com %s=%q deveria falhar", envRateLimitRPM, raw)
			}
			if !strings.Contains(err.Error(), raw) {
				t.Errorf("mensagem %q não cita o valor recebido %q", err, raw)
			}
			if !strings.Contains(err.Error(), envRateLimitRPM) {
				t.Errorf("mensagem %q não cita %s", err, envRateLimitRPM)
			}
		})
	}
}

// A API não pode exigir as variáveis exclusivas do scraper — subir o servidor
// HTTP não depende de chave de IA nem de arquivo de fontes.
func TestLoadAPIDoesNotRequireScraperVariables(t *testing.T) {
	setAPIRequired(t)
	t.Setenv(envAnthropicAPIKey, "")
	t.Setenv(envSourcesFile, "")
	t.Setenv(envAmenitiesFile, "")

	if _, err := LoadAPI(); err != nil {
		t.Fatalf("LoadAPI() error = %v, want nil", err)
	}
}

func TestLoadAPIReportsMissingRequired(t *testing.T) {
	t.Setenv(envDatabaseURL, "")
	t.Setenv(envRedisURL, "")

	_, err := LoadAPI()
	if !errors.Is(err, ErrMissingRequired) {
		t.Fatalf("LoadAPI() error = %v, want ErrMissingRequired", err)
	}
	for _, name := range []string{envDatabaseURL, envRedisURL} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("mensagem %q não cita %s", err, name)
		}
	}
}

func TestLoadAPIReadsPort(t *testing.T) {
	setAPIRequired(t)
	t.Setenv(envPort, "  9090  ")

	cfg, err := LoadAPI()
	if err != nil {
		t.Fatalf("LoadAPI() error = %v, want nil", err)
	}
	if cfg.Port != 9090 {
		t.Errorf("Port = %d, want 9090", cfg.Port)
	}
}

func TestLoadAPIRejectsInvalidPort(t *testing.T) {
	for _, raw := range []string{"0", "abc", "99999", "-1", "8080.5"} {
		t.Run(raw, func(t *testing.T) {
			setAPIRequired(t)
			t.Setenv(envPort, raw)

			_, err := LoadAPI()
			if err == nil {
				t.Fatalf("LoadAPI() com PORT=%q deveria falhar", raw)
			}
			if !strings.Contains(err.Error(), raw) {
				t.Errorf("mensagem %q não cita o valor recebido %q", err, raw)
			}
			if !strings.Contains(err.Error(), envPort) {
				t.Errorf("mensagem %q não cita %s", err, envPort)
			}
		})
	}
}

func TestLoadAPIReadsCORSOrigins(t *testing.T) {
	setAPIRequired(t)
	t.Setenv(envCORSOrigins, "http://localhost:3000, https://imobhub.com.br")

	cfg, err := LoadAPI()
	if err != nil {
		t.Fatalf("LoadAPI() error = %v, want nil", err)
	}

	want := []string{"http://localhost:3000", "https://imobhub.com.br"}
	if !slices.Equal(cfg.CORSOrigins, want) {
		t.Errorf("CORSOrigins = %v, want %v", cfg.CORSOrigins, want)
	}
}

func TestParseCORSOrigins(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{name: "vazia", raw: "", want: nil},
		{name: "só vírgulas", raw: ",, ,", want: nil},
		{name: "uma origem", raw: "http://a.com", want: []string{"http://a.com"}},
		{name: "espaços em volta", raw: " http://a.com ,  http://b.com ", want: []string{"http://a.com", "http://b.com"}},
		{name: "entrada vazia no meio", raw: "http://a.com,,http://b.com", want: []string{"http://a.com", "http://b.com"}},
		{name: "vírgula sobrando no fim", raw: "http://a.com,", want: []string{"http://a.com"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseCORSOrigins(tt.raw); !slices.Equal(got, tt.want) {
				t.Errorf("parseCORSOrigins(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}
