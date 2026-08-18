package selectors

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/imobhub/api/internal/db"
)

// fakeService monta um SelectorService com todas as costuras substituídas, para
// exercitar o fluxo sem PostgreSQL, sem rede e sem gastar tokens. O pool é nil
// de propósito: nenhuma das costuras o usa, e um pool não-nil aqui só esconderia
// um acesso real ao banco que passasse despercebido.
type fakeService struct {
	*SelectorService

	staticCalls   []string
	headlessCalls []string
	analyzeCalls  []string
	upserted      []db.SelectorConfig
}

// stubs descreve o que cada costura deve devolver no teste.
type stubs struct {
	stored     *db.SelectorConfig
	storedErr  error
	staticHTML string
	staticErr  error
	renderMode string
	analyzeErr error
	upsertErr  error
}

func newFakeService(t *testing.T, s stubs) *fakeService {
	t.Helper()

	fake := &fakeService{}
	renderMode := s.renderMode
	if renderMode == "" {
		renderMode = db.RenderModeStatic
	}

	fake.SelectorService = &SelectorService{
		apiKey: "test-key",
		static: func(_ context.Context, siteURL string) (string, error) {
			fake.staticCalls = append(fake.staticCalls, siteURL)
			return s.staticHTML, s.staticErr
		},
		headless: func(_ context.Context, siteURL string) (string, error) {
			fake.headlessCalls = append(fake.headlessCalls, siteURL)
			return "<html>headless</html>", nil
		},
		getSelectors: func(_ context.Context, _ *pgxpool.Pool, _ string) (*db.SelectorConfig, error) {
			return s.stored, s.storedErr
		},
		upsertSelectors: func(_ context.Context, _ *pgxpool.Pool, config db.SelectorConfig) error {
			fake.upserted = append(fake.upserted, config)
			return s.upsertErr
		},
		analyze: func(_ context.Context, _, pageHTML, _ string) (*db.SelectorFields, string, error) {
			fake.analyzeCalls = append(fake.analyzeCalls, pageHTML)
			if s.analyzeErr != nil {
				return nil, "", s.analyzeErr
			}
			return &db.SelectorFields{ListingContainer: ".card", Title: "h2"}, renderMode, nil
		},
	}

	return fake
}

func validStored() *db.SelectorConfig {
	validatedAt := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	return &db.SelectorConfig{
		Domain:          "www.exemplo.com.br",
		Selectors:       db.SelectorFields{ListingContainer: ".imovel", Title: ".titulo"},
		RenderMode:      db.RenderModeStatic,
		Status:          db.StatusValid,
		LastValidatedAt: &validatedAt,
	}
}

func TestEnsureSelectorsReusesValidRow(t *testing.T) {
	// O caminho normal de toda coleta: um SELECT e mais nada. Se este teste
	// quebrar, o scraper passou a pagar a Anthropic em cada execução.
	stored := validStored()
	fake := newFakeService(t, stubs{stored: stored})

	got, err := fake.EnsureSelectors(context.Background(), stored.Domain, "https://www.exemplo.com.br/imoveis")
	if err != nil {
		t.Fatalf("EnsureSelectors() error = %v, want nil", err)
	}
	if got != stored {
		t.Errorf("EnsureSelectors() = %+v, want the stored config", got)
	}
	if len(fake.analyzeCalls) != 0 || len(fake.staticCalls) != 0 || len(fake.headlessCalls) != 0 {
		t.Errorf("linha válida acionou rede/IA: static=%d headless=%d analyze=%d",
			len(fake.staticCalls), len(fake.headlessCalls), len(fake.analyzeCalls))
	}
	if len(fake.upserted) != 0 {
		t.Errorf("linha válida gravou no banco: %+v", fake.upserted)
	}
}

func TestEnsureSelectorsFirstVisitUsesStaticAndSavesRenderMode(t *testing.T) {
	// Primeira visita: sem linha no banco, busca estática e o render_mode é o
	// que o Claude devolveu — mesmo quando ele pede headless.
	fake := newFakeService(t, stubs{
		staticHTML: `<div id="__next"></div>`,
		renderMode: db.RenderModeHeadless,
	})

	const siteURL = "https://www.novo.com.br/imoveis"
	got, err := fake.EnsureSelectors(context.Background(), "www.novo.com.br", siteURL)
	if err != nil {
		t.Fatalf("EnsureSelectors() error = %v, want nil", err)
	}

	if len(fake.staticCalls) != 1 || fake.staticCalls[0] != siteURL {
		t.Errorf("staticCalls = %v, want [%q]", fake.staticCalls, siteURL)
	}
	if len(fake.headlessCalls) != 0 {
		t.Errorf("primeira visita usou headless: %v", fake.headlessCalls)
	}
	if len(fake.analyzeCalls) != 1 || fake.analyzeCalls[0] != `<div id="__next"></div>` {
		t.Errorf("analyzeCalls = %v, want o HTML buscado", fake.analyzeCalls)
	}

	if len(fake.upserted) != 1 {
		t.Fatalf("upserted = %d chamadas, want 1", len(fake.upserted))
	}
	saved := fake.upserted[0]
	if saved.Domain != "www.novo.com.br" {
		t.Errorf("saved.Domain = %q, want %q", saved.Domain, "www.novo.com.br")
	}
	if saved.RenderMode != db.RenderModeHeadless {
		t.Errorf("saved.RenderMode = %q, want %q", saved.RenderMode, db.RenderModeHeadless)
	}
	if saved.Selectors.ListingContainer != ".card" {
		t.Errorf("saved.Selectors.ListingContainer = %q, want %q", saved.Selectors.ListingContainer, ".card")
	}

	if got.RenderMode != db.RenderModeHeadless || got.Status != db.StatusValid {
		t.Errorf("EnsureSelectors() = %+v, want render_mode headless e status valid", got)
	}
}

func TestEnsureSelectorsBrokenRowUsesStoredRenderMode(t *testing.T) {
	// Linha quebrada com render_mode headless: buscar a página com o cliente
	// estático devolveria o shell vazio da SPA e a redescoberta falharia.
	stored := validStored()
	stored.Status = db.StatusBroken
	stored.RenderMode = db.RenderModeHeadless

	fake := newFakeService(t, stubs{stored: stored, renderMode: db.RenderModeHeadless})

	const siteURL = "https://www.exemplo.com.br/imoveis"
	if _, err := fake.EnsureSelectors(context.Background(), stored.Domain, siteURL); err != nil {
		t.Fatalf("EnsureSelectors() error = %v, want nil", err)
	}

	if len(fake.headlessCalls) != 1 || fake.headlessCalls[0] != siteURL {
		t.Errorf("headlessCalls = %v, want [%q]", fake.headlessCalls, siteURL)
	}
	if len(fake.staticCalls) != 0 {
		t.Errorf("linha headless usou o cliente estático: %v", fake.staticCalls)
	}
	if len(fake.upserted) != 1 {
		t.Errorf("upserted = %d chamadas, want 1", len(fake.upserted))
	}
}

func TestRecoverSelectorsIgnoresValidStatus(t *testing.T) {
	// A auto-recuperação existe justamente para o caso em que o banco diz
	// "valid" mas o site mudou de layout: reusar a linha aqui seria devolver
	// seletores que já se sabe que não extraem nada.
	stored := validStored()
	fake := newFakeService(t, stubs{stored: stored})

	got, err := fake.RecoverSelectors(context.Background(), stored.Domain, "https://www.exemplo.com.br/imoveis")
	if err != nil {
		t.Fatalf("RecoverSelectors() error = %v, want nil", err)
	}

	if len(fake.analyzeCalls) != 1 {
		t.Errorf("analyzeCalls = %d, want 1", len(fake.analyzeCalls))
	}
	if len(fake.upserted) != 1 || fake.upserted[0].Status != db.StatusValid {
		t.Errorf("upserted = %+v, want uma gravação com status valid", fake.upserted)
	}
	if got.Selectors.ListingContainer != ".card" {
		t.Errorf("RecoverSelectors() devolveu os seletores antigos: %+v", got.Selectors)
	}
}

func TestRecoverSelectorsWithoutRowFallsBackToStatic(t *testing.T) {
	fake := newFakeService(t, stubs{staticHTML: "<html>ok</html>"})

	if _, err := fake.RecoverSelectors(context.Background(), "www.novo.com.br", "https://www.novo.com.br/imoveis"); err != nil {
		t.Fatalf("RecoverSelectors() error = %v, want nil", err)
	}
	if len(fake.staticCalls) != 1 {
		t.Errorf("staticCalls = %v, want uma busca estática", fake.staticCalls)
	}
	if len(fake.headlessCalls) != 0 {
		t.Errorf("headlessCalls = %v, want nenhuma", fake.headlessCalls)
	}
}

func TestDiscoveryErrorsArePropagated(t *testing.T) {
	analyzeErr := errors.New("anthropic: overloaded")
	fetchErr := errors.New("connection refused")
	dbErr := errors.New("connection reset")

	tests := []struct {
		name       string
		stubs      stubs
		wantErr    error
		wantUpsert bool
	}{
		{
			name:    "erro da Anthropic sobe sem fallback",
			stubs:   stubs{analyzeErr: analyzeErr},
			wantErr: analyzeErr,
		},
		{
			name:    "erro de rede na busca da página",
			stubs:   stubs{staticErr: fetchErr},
			wantErr: fetchErr,
		},
		{
			name:    "erro de leitura no banco",
			stubs:   stubs{storedErr: dbErr},
			wantErr: dbErr,
		},
		{
			name:       "erro de gravação no banco",
			stubs:      stubs{staticHTML: "<html></html>", upsertErr: dbErr},
			wantErr:    dbErr,
			wantUpsert: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeService(t, tt.stubs)

			config, err := fake.EnsureSelectors(context.Background(), "www.exemplo.com.br", "https://www.exemplo.com.br/imoveis")
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("EnsureSelectors() error = %v, want %v", err, tt.wantErr)
			}
			if config != nil {
				t.Errorf("EnsureSelectors() = %+v, want nil junto com o erro", config)
			}
			// Nunca devolver seletores parciais: quem chama trataria um struct
			// vazio como configuração válida e extrairia zero anúncios.
			if got := len(fake.upserted) > 0; got != tt.wantUpsert {
				t.Errorf("gravou no banco = %v, want %v", got, tt.wantUpsert)
			}
		})
	}
}

func TestEnsureSelectorsRejectsEmptyArguments(t *testing.T) {
	fake := newFakeService(t, stubs{})

	if _, err := fake.EnsureSelectors(context.Background(), "   ", "https://x.com/imoveis"); !errors.Is(err, ErrMissingDomain) {
		t.Errorf("EnsureSelectors() error = %v, want ErrMissingDomain", err)
	}
	if _, err := fake.EnsureSelectors(context.Background(), "x.com", ""); !errors.Is(err, ErrMissingSiteURL) {
		t.Errorf("EnsureSelectors() error = %v, want ErrMissingSiteURL", err)
	}
	if _, err := fake.RecoverSelectors(context.Background(), "", "https://x.com/imoveis"); !errors.Is(err, ErrMissingDomain) {
		t.Errorf("RecoverSelectors() error = %v, want ErrMissingDomain", err)
	}
	if len(fake.analyzeCalls) != 0 {
		t.Errorf("argumento inválido chegou a acionar a IA: %v", fake.analyzeCalls)
	}
}

func TestDiscoveryLogsDetectedSelectors(t *testing.T) {
	// A mensagem é o critério de aceitação da task: é o que o operador procura
	// nos logs quando uma fonte nova entra na coleta.
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	fake := newFakeService(t, stubs{staticHTML: "<html></html>", renderMode: db.RenderModeHeadless})
	if _, err := fake.EnsureSelectors(context.Background(), "www.novo.com.br", "https://www.novo.com.br/imoveis"); err != nil {
		t.Fatalf("EnsureSelectors() error = %v, want nil", err)
	}

	var entry struct {
		Msg        string `json:"msg"`
		Domain     string `json:"domain"`
		RenderMode string `json:"render_mode"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &entry); err != nil {
		t.Fatalf("decoding log line %q: %v", buf.String(), err)
	}

	want := fmt.Sprintf("[%s] seletores detectados via Claude (render_mode: %s)", "www.novo.com.br", db.RenderModeHeadless)
	if entry.Msg != want {
		t.Errorf("log msg = %q, want %q", entry.Msg, want)
	}
	if entry.Domain != "www.novo.com.br" || entry.RenderMode != db.RenderModeHeadless {
		t.Errorf("log attrs = %+v, want domain e render_mode preenchidos", entry)
	}
}

func TestNewSelectorServiceRequiresDependencies(t *testing.T) {
	// pool nil é o primeiro a ser checado, então os casos de fetcher/chave usam
	// um pool não-nil (vazio, nunca acessado).
	pool := &pgxpool.Pool{}
	fetcher := PageFetcher(func(context.Context, string) (string, error) { return "", nil })

	tests := []struct {
		name     string
		pool     *pgxpool.Pool
		static   PageFetcher
		headless PageFetcher
		apiKey   string
		wantErr  error
	}{
		{name: "sem pool", static: fetcher, headless: fetcher, apiKey: "k", wantErr: ErrMissingPool},
		{name: "sem cliente estático", pool: pool, headless: fetcher, apiKey: "k", wantErr: ErrMissingFetcher},
		{name: "sem cliente headless", pool: pool, static: fetcher, apiKey: "k", wantErr: ErrMissingFetcher},
		{name: "sem chave da API", pool: pool, static: fetcher, headless: fetcher, apiKey: "  ", wantErr: ErrMissingAPIKey},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service, err := NewSelectorService(tt.pool, tt.static, tt.headless, tt.apiKey)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("NewSelectorService() error = %v, want %v", err, tt.wantErr)
			}
			if service != nil {
				t.Errorf("NewSelectorService() = %+v, want nil junto com o erro", service)
			}
		})
	}
}

func TestNewSelectorServiceWiresDefaults(t *testing.T) {
	fetcher := PageFetcher(func(context.Context, string) (string, error) { return "", nil })

	service, err := NewSelectorService(&pgxpool.Pool{}, fetcher, fetcher, "test-key")
	if err != nil {
		t.Fatalf("NewSelectorService() error = %v, want nil", err)
	}
	if service.getSelectors == nil || service.upsertSelectors == nil || service.analyze == nil {
		t.Fatal("NewSelectorService() deixou alguma costura nil; produção usaria um serviço quebrado")
	}
	if service.apiKey != "test-key" {
		t.Errorf("service.apiKey = %q, want %q", service.apiKey, "test-key")
	}
}

func TestFetchModeOf(t *testing.T) {
	tests := []struct {
		name    string
		current *db.SelectorConfig
		want    string
	}{
		{name: "sem linha usa estático", current: nil, want: db.RenderModeStatic},
		{name: "linha estática", current: &db.SelectorConfig{RenderMode: db.RenderModeStatic}, want: db.RenderModeStatic},
		{name: "linha headless", current: &db.SelectorConfig{RenderMode: db.RenderModeHeadless}, want: db.RenderModeHeadless},
		{name: "render_mode vazio vira estático", current: &db.SelectorConfig{}, want: db.RenderModeStatic},
		{name: "maiúsculas e espaços", current: &db.SelectorConfig{RenderMode: " HeadLess "}, want: db.RenderModeHeadless},
		{name: "valor inesperado vira estático", current: &db.SelectorConfig{RenderMode: "browser"}, want: db.RenderModeStatic},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := fetchModeOf(tt.current); got != tt.want {
				t.Errorf("fetchModeOf() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStaticFetcherDropsFinalURL(t *testing.T) {
	// StaticFetcher precisa colapsar a assinatura de três retornos de
	// FetchStatic; o teste garante que o erro continua chegando ao chamador.
	fetcher := StaticFetcher("ImobHubBot/1.0")
	if fetcher == nil {
		t.Fatal("StaticFetcher() = nil")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // contexto cancelado: falha sem tocar na rede.

	if _, err := fetcher(ctx, "https://www.exemplo.com.br/imoveis"); err == nil {
		t.Error("StaticFetcher() com contexto cancelado error = nil, want error")
	}
}

func TestHeadlessFetcherRejectsEmptyURL(t *testing.T) {
	fetcher := HeadlessFetcher("ImobHubBot/1.0")

	// URL vazia é rejeitada por FetchHeadless antes de tentar subir o browser,
	// então este teste não depende de Chrome instalado.
	_, err := fetcher(context.Background(), "   ")
	if err == nil {
		t.Fatal("HeadlessFetcher() com URL vazia error = nil, want error")
	}
	if !strings.Contains(err.Error(), "httpclient") {
		t.Errorf("error = %q, want um erro do pacote httpclient", err)
	}
}
