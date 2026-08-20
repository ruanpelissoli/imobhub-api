package scraper

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/imobhub/api/internal/db"
)

// stubDeps monta um pipeline sem rede, sem IA e sem PostgreSQL: cada
// dependência é um closure que registra o que foi chamado. Os testes sobrescrevem
// só o campo que interessa ao caso e chamam New(stub.Deps).
type stubDeps struct {
	Deps

	// Registro das chamadas, na ordem em que aconteceram.
	waits     []string
	fetches   []string // "static <url>" ou "headless <url>"
	ensures   []string
	recovers  []string
	extracts  []db.SelectorConfig
	syncs     []syncArgs
	brokens   []string
	countCall int

	// lastRun é o Redis fake ligado a Deps.PublishLastRun.
	lastRun *fakeLastRunStore
}

type syncArgs struct {
	domain       string
	listings     []db.RawListing
	runStartedAt time.Time
}

// stubSelectors é a configuração devolvida por EnsureSelectors no caminho
// feliz: seletores válidos e página estática.
func stubSelectors(domain string) *db.SelectorConfig {
	return &db.SelectorConfig{
		Domain:     domain,
		Selectors:  db.SelectorFields{ListingContainer: ".card", Title: "h2", ListingURL: "a"},
		RenderMode: db.RenderModeStatic,
		Status:     db.StatusValid,
	}
}

// newStub devolve dependências que fazem a coleta de urls terminar em sucesso,
// com listingsPerSite anúncios por fonte.
func newStub(urls []string, listingsPerSite int) *stubDeps {
	s := &stubDeps{lastRun: &fakeLastRunStore{}}

	s.Deps = Deps{
		SourcesFile: "sources.txt",
		ReadSources: func(string) ([]string, error) { return urls, nil },
		IsAllowed:   func(context.Context, string) (bool, error) { return true, nil },
		Wait: func(_ context.Context, domain string) error {
			s.waits = append(s.waits, domain)
			return nil
		},
		FetchStatic: func(_ context.Context, siteURL string) (string, error) {
			s.fetches = append(s.fetches, "static "+siteURL)
			return "<html>estático</html>", nil
		},
		FetchHeadless: func(_ context.Context, siteURL string) (string, error) {
			s.fetches = append(s.fetches, "headless "+siteURL)
			return "<html>renderizado</html>", nil
		},
		EnsureSelectors: func(_ context.Context, domain, _ string) (*db.SelectorConfig, error) {
			s.ensures = append(s.ensures, domain)
			return stubSelectors(domain), nil
		},
		RecoverSelectors: func(_ context.Context, domain, _ string) (*db.SelectorConfig, error) {
			s.recovers = append(s.recovers, domain)
			// A redescoberta costuma trocar o modo quando o site virou SPA; o
			// stub sempre troca, para que os testes vejam a re-busca headless.
			recovered := stubSelectors(domain)
			recovered.RenderMode = db.RenderModeHeadless
			return recovered, nil
		},
		MarkBroken: func(_ context.Context, domain string) error {
			s.brokens = append(s.brokens, domain)
			return nil
		},
		Extract: func(_ string, config db.SelectorConfig, domain string) ([]db.RawListing, error) {
			s.extracts = append(s.extracts, config)
			return sampleListings(listingsPerSite), nil
		},
		Sync: func(_ context.Context, domain string, listings []db.RawListing, runStartedAt time.Time) (SyncStats, error) {
			s.syncs = append(s.syncs, syncArgs{domain: domain, listings: listings, runStartedAt: runStartedAt})
			return SyncStats{Upserted: int64(len(listings)), Deleted: 2}, nil
		},
		CountListings: func(context.Context) (int64, error) {
			s.countCall++
			return 42, nil
		},
		PublishLastRun: (&lastRunPublisher{store: s.lastRun}).publish,
	}

	return s
}

// run monta o pipeline com as dependências do stub e o executa.
func (s *stubDeps) run(t *testing.T, ctx context.Context) (RunSummary, error) {
	t.Helper()

	pipeline, err := New(s.Deps)
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}
	return pipeline.Run(ctx)
}

// captureLogs redireciona o logger padrão para um buffer pelo tempo do teste.
// Vários logs deste pacote são critério de aceitação, então precisam ser
// verificados e não só observados no terminal.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()

	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	return &buf
}

// logMessages devolve o campo "msg" de cada linha logada, já decodificado — a
// comparação é com o texto real, não com a forma escapada do JSON.
func logMessages(t *testing.T, buf *bytes.Buffer) []string {
	t.Helper()

	var messages []string
	scanner := bufio.NewScanner(bytes.NewReader(buf.Bytes()))
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var entry struct {
			Msg string `json:"msg"`
		}
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("decoding log line %q: %v", line, err)
		}
		messages = append(messages, entry.Msg)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("reading captured logs: %v", err)
	}

	return messages
}

func requireLogged(t *testing.T, buf *bytes.Buffer, want string) {
	t.Helper()

	messages := logMessages(t, buf)
	for _, msg := range messages {
		if msg == want {
			return
		}
	}
	t.Fatalf("log %q não encontrado; logs:\n%s", want, strings.Join(messages, "\n"))
}

// logRecords devolve cada linha logada como mapa, para os testes que verificam
// atributos além do `msg`.
func logRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()

	var records []map[string]any
	scanner := bufio.NewScanner(bytes.NewReader(buf.Bytes()))
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("decoding log line %q: %v", line, err)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("reading captured logs: %v", err)
	}

	return records
}

// requireLoggedEvent casa a linha pelo `msg` contratual e verifica o `event`
// que ela precisa carregar, devolvendo o registro para asserções de atributo.
func requireLoggedEvent(t *testing.T, buf *bytes.Buffer, msg, event string) map[string]any {
	t.Helper()

	for _, record := range logRecords(t, buf) {
		if record["msg"] != msg {
			continue
		}
		if record["event"] != event {
			t.Fatalf("log %q com event = %v, want %q", msg, record["event"], event)
		}
		return record
	}

	t.Fatalf("log %q não encontrado; logs:\n%s", msg, strings.Join(logMessages(t, buf), "\n"))
	return nil
}

func requireAttrs(t *testing.T, record map[string]any, msg string, want map[string]any) {
	t.Helper()

	for key, value := range want {
		got, ok := record[key]
		if !ok {
			t.Errorf("log %q sem o atributo %q", msg, key)
			continue
		}
		if got != value {
			t.Errorf("log %q: %s = %v, want %v", msg, key, got, value)
		}
	}
}

func requireNotLogged(t *testing.T, buf *bytes.Buffer, unwanted string) {
	t.Helper()

	for _, msg := range logMessages(t, buf) {
		if msg == unwanted {
			t.Fatalf("log %q não deveria ter sido emitido", unwanted)
		}
	}
}

func TestRunProcessesEverySourceInOrder(t *testing.T) {
	buf := captureLogs(t)
	stub := newStub([]string{"https://a.exemplo.com.br/imoveis", "https://b.exemplo.com.br/imoveis"}, 3)

	summary, err := stub.run(t, context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	want := RunSummary{Sites: 2, Succeeded: 2, Listings: 6, Removed: 4, TotalInDB: 42}
	if summary != want {
		t.Errorf("Run() = %+v, want %+v", summary, want)
	}
	if got := stub.ensures; len(got) != 2 || got[0] != "a.exemplo.com.br" || got[1] != "b.exemplo.com.br" {
		t.Errorf("EnsureSelectors chamado com %v, want os dois domínios na ordem do arquivo", got)
	}
	// Duas esperas por fonte: a do início do domínio e a que precede a busca da
	// página — a descoberta de seletores pode ter requisitado o site no meio.
	wantWaits := []string{"a.exemplo.com.br", "a.exemplo.com.br", "b.exemplo.com.br", "b.exemplo.com.br"}
	if strings.Join(stub.waits, ",") != strings.Join(wantWaits, ",") {
		t.Errorf("Wait chamado com %v, want %v", stub.waits, wantWaits)
	}
	if len(stub.syncs) != 2 {
		t.Fatalf("syncs = %d, want 2", len(stub.syncs))
	}
	if stub.syncs[0].domain != "a.exemplo.com.br" || len(stub.syncs[0].listings) != 3 {
		t.Errorf("primeiro sync = %+v, want 3 anúncios de a.exemplo.com.br", stub.syncs[0])
	}
	// Cada domínio carimba o próprio corte, lido antes de processá-lo: o corte
	// do segundo nunca pode ser anterior ao do primeiro, e nenhum pode ser zero
	// (SyncListings rejeita o zero value).
	if stub.syncs[0].runStartedAt.IsZero() || stub.syncs[1].runStartedAt.Before(stub.syncs[0].runStartedAt) {
		t.Errorf("runStartedAt inválido entre domínios: %v e %v", stub.syncs[0].runStartedAt, stub.syncs[1].runStartedAt)
	}

	// Formato contratual do log de sucesso (critério de aceitação da task).
	requireLogged(t, buf, "[a.exemplo.com.br] ✓ 3 listings (upserted: 3, deletados: 2)")
}

func TestRunSkipsSourcesBlockedByRobots(t *testing.T) {
	buf := captureLogs(t)
	stub := newStub([]string{"https://bloqueado.com.br/imoveis", "https://livre.com.br/imoveis"}, 2)
	stub.IsAllowed = func(_ context.Context, rawURL string) (bool, error) {
		return !strings.Contains(rawURL, "bloqueado"), nil
	}

	summary, err := stub.run(t, context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	if summary.Blocked != 1 || summary.Succeeded != 1 || summary.Failed != 0 {
		t.Errorf("Run() = %+v, want 1 bloqueada e 1 com sucesso", summary)
	}
	// Bloqueio pelo robots.txt tem que acontecer antes de qualquer requisição ao
	// site — inclusive antes do rate limiting e da descoberta de seletores.
	for _, call := range append(append([]string{}, stub.waits...), stub.fetches...) {
		if strings.Contains(call, "bloqueado") {
			t.Errorf("site bloqueado foi acessado: %q", call)
		}
	}
	if len(stub.ensures) != 1 || stub.ensures[0] != "livre.com.br" {
		t.Errorf("EnsureSelectors = %v, want só o domínio permitido", stub.ensures)
	}

	requireLogged(t, buf, "[bloqueado.com.br] bloqueado por robots.txt")
}

func TestRunUsesHeadlessFetcherWhenSelectorsSayHeadless(t *testing.T) {
	stub := newStub([]string{"https://spa.com.br/imoveis"}, 1)
	stub.EnsureSelectors = func(_ context.Context, domain, _ string) (*db.SelectorConfig, error) {
		config := stubSelectors(domain)
		config.RenderMode = db.RenderModeHeadless
		return config, nil
	}

	if _, err := stub.run(t, context.Background()); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	want := []string{"headless https://spa.com.br/imoveis"}
	if strings.Join(stub.fetches, ",") != strings.Join(want, ",") {
		t.Errorf("fetches = %v, want %v", stub.fetches, want)
	}
}

func TestRunRecoversSelectorsWhenNothingIsExtracted(t *testing.T) {
	stub := newStub([]string{"https://mudou.com.br/imoveis"}, 0)
	// Primeira extração vazia (layout mudou), segunda com anúncios — é o
	// caminho de auto-recuperação que a task descreve.
	attempt := 0
	stub.Extract = func(_ string, config db.SelectorConfig, _ string) ([]db.RawListing, error) {
		stub.extracts = append(stub.extracts, config)
		attempt++
		if attempt == 1 {
			return nil, nil
		}
		return sampleListings(4), nil
	}

	summary, err := stub.run(t, context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	if summary.Succeeded != 1 || summary.Listings != 4 {
		t.Errorf("Run() = %+v, want 1 sucesso com 4 anúncios", summary)
	}
	if len(stub.recovers) != 1 {
		t.Fatalf("RecoverSelectors chamado %d vezes, want 1", len(stub.recovers))
	}
	// A re-busca usa o render_mode devolvido pela redescoberta, não o antigo:
	// é assim que um site que virou SPA volta a ser coletado.
	want := []string{"static https://mudou.com.br/imoveis", "headless https://mudou.com.br/imoveis"}
	if strings.Join(stub.fetches, ",") != strings.Join(want, ",") {
		t.Errorf("fetches = %v, want %v", stub.fetches, want)
	}
	if len(stub.syncs) != 1 || len(stub.syncs[0].listings) != 4 {
		t.Errorf("syncs = %+v, want um sync com os 4 anúncios recuperados", stub.syncs)
	}
	if len(stub.brokens) != 0 {
		t.Errorf("seletores marcados como quebrados após recuperação bem-sucedida: %v", stub.brokens)
	}
}

func TestRunMarksSelectorsBrokenWhenRecoveryDoesNotHelp(t *testing.T) {
	buf := captureLogs(t)
	stub := newStub([]string{"https://vazio.com.br/imoveis", "https://ok.com.br/imoveis"}, 0)
	stub.Extract = func(_ string, config db.SelectorConfig, domain string) ([]db.RawListing, error) {
		stub.extracts = append(stub.extracts, config)
		if domain == "vazio.com.br" {
			return nil, nil
		}
		return sampleListings(2), nil
	}

	summary, err := stub.run(t, context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	if summary.Failed != 1 || summary.Succeeded != 1 {
		t.Errorf("Run() = %+v, want 1 falha e 1 sucesso", summary)
	}
	// Sem a marcação, o run de amanhã reusaria seletores que já sabemos não
	// extrair nada e nunca mais chamaria a IA.
	if len(stub.brokens) != 1 || stub.brokens[0] != "vazio.com.br" {
		t.Errorf("MarkBroken = %v, want [vazio.com.br]", stub.brokens)
	}
	// Zero anúncios nunca chega ao sync: SyncListings não deleta nesse estado,
	// mas nem chamá-lo deixa a intenção explícita.
	if len(stub.syncs) != 1 || stub.syncs[0].domain != "ok.com.br" {
		t.Errorf("syncs = %+v, want só o domínio que extraiu anúncios", stub.syncs)
	}

	requireLogged(t, buf, "[vazio.com.br] nenhum anúncio extraído mesmo após a redescoberta dos seletores")
}

func TestRunKeepsGoingWhenOneSourceFails(t *testing.T) {
	buf := captureLogs(t)
	stub := newStub([]string{"https://quebrado.com.br/imoveis", "https://ok.com.br/imoveis"}, 2)
	stub.EnsureSelectors = func(_ context.Context, domain, _ string) (*db.SelectorConfig, error) {
		stub.ensures = append(stub.ensures, domain)
		if domain == "quebrado.com.br" {
			return nil, errors.New("anthropic: 429 rate limited")
		}
		return stubSelectors(domain), nil
	}

	summary, err := stub.run(t, context.Background())
	// Uma fonte fora do ar não pode zerar a coleta do dia: o erro fica no log e
	// no resumo, não no retorno.
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	want := RunSummary{Sites: 2, Succeeded: 1, Failed: 1, Listings: 2, Removed: 2, TotalInDB: 42}
	if summary != want {
		t.Errorf("Run() = %+v, want %+v", summary, want)
	}
	if len(stub.syncs) != 1 || stub.syncs[0].domain != "ok.com.br" {
		t.Errorf("syncs = %+v, want só o domínio saudável", stub.syncs)
	}

	requireLogged(t, buf, "[quebrado.com.br] falha ao obter os seletores")
}

func TestRunFailsSourceWhenSelectorServiceReturnsNothing(t *testing.T) {
	// Contrato violado (nem config, nem erro): tem que virar falha da fonte, não
	// um panic que derruba o run inteiro.
	stub := newStub([]string{"https://a.com.br/imoveis", "https://b.com.br/imoveis"}, 1)
	stub.EnsureSelectors = func(_ context.Context, domain, _ string) (*db.SelectorConfig, error) {
		stub.ensures = append(stub.ensures, domain)
		if domain == "a.com.br" {
			return nil, nil
		}
		return stubSelectors(domain), nil
	}

	summary, err := stub.run(t, context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if summary.Failed != 1 || summary.Succeeded != 1 {
		t.Errorf("Run() = %+v, want 1 falha e 1 sucesso", summary)
	}
}

func TestRunFailsSourceWithoutUsableHost(t *testing.T) {
	buf := captureLogs(t)
	stub := newStub([]string{"file:///etc/passwd", "https://ok.com.br/imoveis"}, 1)

	summary, err := stub.run(t, context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	if summary.Failed != 1 || summary.Succeeded != 1 {
		t.Errorf("Run() = %+v, want 1 falha e 1 sucesso", summary)
	}
	if len(stub.fetches) != 1 {
		t.Errorf("fetches = %v, want só o da fonte válida", stub.fetches)
	}

	requireLogged(t, buf, "[file:///etc/passwd] fonte ignorada: URL sem domínio utilizável")
}

func TestRunAbortsWhenContextIsCanceled(t *testing.T) {
	stub := newStub([]string{"https://a.com.br/imoveis", "https://b.com.br/imoveis"}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	stub.Sync = func(_ context.Context, domain string, listings []db.RawListing, runStartedAt time.Time) (SyncStats, error) {
		stub.syncs = append(stub.syncs, syncArgs{domain: domain, listings: listings, runStartedAt: runStartedAt})
		// Simula o SIGTERM chegando no meio do run.
		cancel()
		return SyncStats{Upserted: int64(len(listings))}, nil
	}
	t.Cleanup(cancel)

	summary, err := stub.run(t, ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if summary.Succeeded != 1 {
		t.Errorf("Run() = %+v, want a primeira fonte contabilizada", summary)
	}
	// Cancelamento encerra o run: insistir nas fontes restantes só produziria
	// uma cascata de erros idênticos.
	if len(stub.ensures) != 1 {
		t.Errorf("EnsureSelectors = %v, want só a fonte processada antes do cancelamento", stub.ensures)
	}
}

func TestRunAbortsWhenRateLimitWaitIsCanceled(t *testing.T) {
	stub := newStub([]string{"https://a.com.br/imoveis"}, 1)
	stub.Wait = func(context.Context, string) error { return context.Canceled }

	summary, err := stub.run(t, context.Background())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if summary.Succeeded != 0 || len(stub.fetches) != 0 {
		t.Errorf("coleta seguiu após o cancelamento do rate limiting: %+v / %v", summary, stub.fetches)
	}
}

func TestRunReturnsErrorWhenSourcesCannotBeRead(t *testing.T) {
	sentinel := errors.New("permission denied")
	stub := newStub(nil, 0)
	stub.ReadSources = func(string) ([]string, error) { return nil, sentinel }

	if _, err := stub.run(t, context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("Run() error = %v, want %v embrulhado", err, sentinel)
	}
}

func TestRunWarnsWhenThereIsNothingToCollect(t *testing.T) {
	buf := captureLogs(t)
	stub := newStub(nil, 0)

	summary, err := stub.run(t, context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if summary != (RunSummary{}) {
		t.Errorf("Run() = %+v, want resumo zerado", summary)
	}

	requireLogged(t, buf, "nenhuma fonte utilizável no arquivo de fontes")
	requireNotLogged(t, buf, "coleta concluída: 0 fontes (0 com sucesso, 0 com erro, 0 bloqueadas), 0 anúncios coletados, 0 anúncios no banco")
}

func TestRunLogsFinalSummary(t *testing.T) {
	buf := captureLogs(t)
	stub := newStub([]string{"https://a.com.br/imoveis", "https://b.com.br/imoveis"}, 5)

	if _, err := stub.run(t, context.Background()); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	requireLogged(t, buf, "coleta concluída: 2 fontes (2 com sucesso, 0 com erro, 0 bloqueadas), 10 anúncios coletados, 42 anúncios no banco")
	if stub.countCall != 1 {
		t.Errorf("CountListings chamado %d vezes, want 1 (o resumo é uma foto no fim do run)", stub.countCall)
	}
}

func TestRunStillLogsSummaryWhenCountFails(t *testing.T) {
	buf := captureLogs(t)
	stub := newStub([]string{"https://a.com.br/imoveis"}, 1)
	stub.CountListings = func(context.Context) (int64, error) { return 0, errors.New("connection reset") }

	summary, err := stub.run(t, context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if summary.Succeeded != 1 {
		t.Errorf("Run() = %+v, want a fonte contabilizada mesmo sem a contagem", summary)
	}

	requireLogged(t, buf, "coleta concluída: 1 fontes (1 com sucesso, 0 com erro, 0 bloqueadas), 1 anúncios coletados, 0 anúncios no banco")
}

func TestRunLogsTheEventVocabulary(t *testing.T) {
	buf := captureLogs(t)
	stub := newStub([]string{"https://bloqueado.com.br/imoveis", "https://ok.com.br/imoveis"}, 3)
	stub.IsAllowed = func(_ context.Context, rawURL string) (bool, error) {
		return !strings.Contains(rawURL, "bloqueado"), nil
	}

	if _, err := stub.run(t, context.Background()); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	requireLoggedEvent(t, buf, "pipeline iniciado: 2 fontes carregadas", eventRunStarted)
	requireLoggedEvent(t, buf, "[bloqueado.com.br] bloqueado por robots.txt", eventSiteBlocked)

	// O texto das mensagens é contratual; o event e os campos agregáveis são a
	// camada nova, e é ela que os operadores filtram.
	scraped := requireLoggedEvent(t, buf, "[ok.com.br] ✓ 3 listings (upserted: 3, deletados: 2)", eventSiteScraped)
	requireAttrs(t, scraped, "site_scraped", map[string]any{
		"domain":           "ok.com.br",
		"listings_found":   float64(3),
		"listings_removed": float64(2),
	})
	if _, ok := scraped["duration_ms"]; !ok {
		t.Error("a linha de fonte coletada precisa carregar duration_ms")
	}

	finished := requireLoggedEvent(t,
		buf,
		"coleta concluída: 2 fontes (1 com sucesso, 0 com erro, 1 bloqueadas), 3 anúncios coletados, 42 anúncios no banco",
		eventRunFinished)
	requireAttrs(t, finished, "scraper_run_finished", map[string]any{
		"sites_total":          float64(2),
		"sites_succeeded":      float64(1),
		"sites_failed":         float64(0),
		"sites_blocked":        float64(1),
		"listings_found":       float64(3),
		"listings_removed":     float64(2),
		"listings_total_in_db": float64(42),
	})
	if _, ok := finished["duration_ms"]; !ok {
		t.Error("o resumo final precisa carregar duration_ms")
	}
}

func TestRunLogsSiteFailureWithTheScrapedEvent(t *testing.T) {
	buf := captureLogs(t)
	stub := newStub([]string{"https://quebrado.com.br/imoveis"}, 1)
	stub.EnsureSelectors = func(_ context.Context, domain, _ string) (*db.SelectorConfig, error) {
		return nil, errors.New("anthropic: 429 rate limited")
	}

	if _, err := stub.run(t, context.Background()); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	record := requireLoggedEvent(t, buf, "[quebrado.com.br] falha ao obter os seletores", eventSiteScraped)
	if record["error"] == nil {
		t.Error("a falha de uma fonte precisa carregar o atributo error")
	}
	requireAttrs(t, record, "site_scraped", map[string]any{
		"domain":         "quebrado.com.br",
		"listings_found": float64(0),
	})
}

func TestRunPublishesTheLastRunSummary(t *testing.T) {
	stub := newStub([]string{"https://a.com.br/imoveis", "https://b.com.br/imoveis"}, 3)

	before := time.Now().UTC().Truncate(time.Second)
	if _, err := stub.run(t, context.Background()); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	if stub.lastRun.sets != 1 {
		t.Fatalf("gravações em %s = %d, want 1", LastRunCacheKey, stub.lastRun.sets)
	}
	if stub.lastRun.lastKey != LastRunCacheKey {
		t.Errorf("chave = %q, want %q", stub.lastRun.lastKey, LastRunCacheKey)
	}
	if stub.lastRun.lastTTL != 48*time.Hour {
		t.Errorf("TTL = %v, want 48h", stub.lastRun.lastTTL)
	}

	run := stub.lastRun.decode(t)
	want := LastRun{
		RanAt:             run.RanAt,
		Success:           true,
		SitesTotal:        2,
		SitesSucceeded:    2,
		SitesBlocked:      0,
		SitesFailed:       0,
		ListingsFound:     6,
		ListingsRemoved:   4,
		ListingsTotalInDB: 42,
		DurationMS:        run.DurationMS,
	}
	if run != want {
		t.Errorf("resumo publicado = %+v, want %+v", run, want)
	}
	if run.DurationMS < 0 {
		t.Errorf("duration_ms = %d, want >= 0", run.DurationMS)
	}

	ranAt, err := time.Parse(time.RFC3339, run.RanAt)
	if err != nil {
		t.Fatalf("ran_at = %q, want RFC3339: %v", run.RanAt, err)
	}
	if ranAt.Before(before) {
		t.Errorf("ran_at = %v, want o instante de início do run (>= %v)", ranAt, before)
	}
}

func TestRunPublishesFailureWhenASourceFails(t *testing.T) {
	stub := newStub([]string{"https://quebrado.com.br/imoveis", "https://ok.com.br/imoveis"}, 2)
	stub.EnsureSelectors = func(_ context.Context, domain, _ string) (*db.SelectorConfig, error) {
		if domain == "quebrado.com.br" {
			return nil, errors.New("anthropic: 429 rate limited")
		}
		return stubSelectors(domain), nil
	}

	if _, err := stub.run(t, context.Background()); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	run := stub.lastRun.decode(t)
	if run.Success {
		t.Error("success = true com uma fonte falha, want false")
	}
	if run.SitesFailed != 1 || run.SitesSucceeded != 1 {
		t.Errorf("resumo = %+v, want 1 falha e 1 sucesso", run)
	}
}

func TestRunPublishesSuccessWhenASourceIsBlockedByRobots(t *testing.T) {
	// Bloqueio por robots.txt é o pipeline funcionando: não pode marcar o run
	// como malsucedido.
	stub := newStub([]string{"https://bloqueado.com.br/imoveis", "https://ok.com.br/imoveis"}, 2)
	stub.IsAllowed = func(_ context.Context, rawURL string) (bool, error) {
		return !strings.Contains(rawURL, "bloqueado"), nil
	}

	if _, err := stub.run(t, context.Background()); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	run := stub.lastRun.decode(t)
	if !run.Success {
		t.Error("success = false com fonte bloqueada por robots.txt, want true")
	}
	if run.SitesBlocked != 1 || run.SitesFailed != 0 {
		t.Errorf("resumo = %+v, want 1 bloqueada e 0 falhas", run)
	}
}

func TestRunPublishesPartialSummaryWhenInterrupted(t *testing.T) {
	stub := newStub([]string{"https://a.com.br/imoveis", "https://b.com.br/imoveis"}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	stub.Sync = func(_ context.Context, domain string, listings []db.RawListing, runStartedAt time.Time) (SyncStats, error) {
		stub.syncs = append(stub.syncs, syncArgs{domain: domain, listings: listings, runStartedAt: runStartedAt})
		cancel()
		return SyncStats{Upserted: int64(len(listings)), Deleted: 1}, nil
	}
	t.Cleanup(cancel)

	if _, err := stub.run(t, ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}

	if stub.lastRun.sets != 1 {
		t.Fatalf("gravações = %d, want 1 — o run interrompido é o que o operador precisa ver", stub.lastRun.sets)
	}
	// Sem o context.WithoutCancel a gravação falharia justamente aqui.
	if stub.lastRun.ctxErrDuringSet != nil {
		t.Errorf("ctx.Err() dentro do Set = %v, want nil", stub.lastRun.ctxErrDuringSet)
	}

	run := stub.lastRun.decode(t)
	if run.Success {
		t.Error("success = true num run abortado, want false")
	}
	if run.SitesSucceeded != 1 || run.ListingsFound != 1 || run.ListingsRemoved != 1 {
		t.Errorf("resumo = %+v, want os números parciais da primeira fonte", run)
	}
}

func TestRunPublishesFailureWhenSourcesCannotBeRead(t *testing.T) {
	stub := newStub(nil, 0)
	stub.ReadSources = func(string) ([]string, error) { return nil, errors.New("permission denied") }

	if _, err := stub.run(t, context.Background()); err == nil {
		t.Fatal("Run() error = nil, want erro fatal")
	}

	if stub.lastRun.sets != 1 {
		t.Fatalf("gravações = %d, want 1", stub.lastRun.sets)
	}
	if run := stub.lastRun.decode(t); run.Success {
		t.Errorf("success = true com o arquivo de fontes ilegível: %+v", run)
	}
}

func TestRunIgnoresLastRunPublishFailures(t *testing.T) {
	buf := captureLogs(t)
	stub := newStub([]string{"https://a.com.br/imoveis"}, 3)
	stub.lastRun.setErr = errors.New("dial tcp: connection refused")

	summary, err := stub.run(t, context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil — falha de Redis não pode mudar o resultado do run", err)
	}

	want := RunSummary{Sites: 1, Succeeded: 1, Listings: 3, Removed: 2, TotalInDB: 42}
	if summary != want {
		t.Errorf("Run() = %+v, want %+v", summary, want)
	}

	requireLoggedEvent(t, buf, "não foi possível publicar o resumo da última coleta no Redis", eventLastRunPublishFailed)
}

func TestRunWithoutLastRunPublisherStillCollects(t *testing.T) {
	// Client de Redis nulo é caminho válido "sem persistência": o run acontece
	// igual, sem panic e sem escrita.
	stub := newStub([]string{"https://a.com.br/imoveis"}, 2)
	stub.PublishLastRun = nil

	summary, err := stub.run(t, context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if summary.Succeeded != 1 || summary.Listings != 2 {
		t.Errorf("Run() = %+v, want a fonte coletada", summary)
	}
	if stub.lastRun.sets != 0 {
		t.Errorf("gravações = %d, want 0 sem seam configurado", stub.lastRun.sets)
	}
}

func TestNewRejectsIncompleteWiring(t *testing.T) {
	// Um campo nulo em Deps só apareceria como panic no meio da primeira coleta;
	// a validação transforma isso em falha de boot.
	complete := newStub(nil, 0).Deps

	tests := []struct {
		name string
		mut  func(*Deps)
	}{
		{"sem arquivo de fontes", func(d *Deps) { d.SourcesFile = "  " }},
		{"sem leitor de fontes", func(d *Deps) { d.ReadSources = nil }},
		{"sem robots", func(d *Deps) { d.IsAllowed = nil }},
		{"sem rate limiter", func(d *Deps) { d.Wait = nil }},
		{"sem cliente estático", func(d *Deps) { d.FetchStatic = nil }},
		{"sem cliente headless", func(d *Deps) { d.FetchHeadless = nil }},
		{"sem serviço de seletores", func(d *Deps) { d.EnsureSelectors = nil }},
		{"sem recuperação de seletores", func(d *Deps) { d.RecoverSelectors = nil }},
		{"sem marcação de quebrados", func(d *Deps) { d.MarkBroken = nil }},
		{"sem extrator", func(d *Deps) { d.Extract = nil }},
		{"sem sincronizador", func(d *Deps) { d.Sync = nil }},
		{"sem contagem", func(d *Deps) { d.CountListings = nil }},
	}
	// PublishLastRun fica fora da lista de propósito: é o único campo opcional
	// de Deps, e sem ele o pipeline roda — só não publica o resumo.

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps := complete
			tt.mut(&deps)

			pipeline, err := New(deps)
			if !errors.Is(err, ErrMissingDependency) {
				t.Fatalf("New() error = %v, want %v", err, ErrMissingDependency)
			}
			if pipeline != nil {
				t.Error("New() devolveu pipeline junto com o erro")
			}
		})
	}
}

func TestRunPipelineRejectsMissingConfigOrPool(t *testing.T) {
	if err := RunPipeline(context.Background(), nil, nil, nil); !errors.Is(err, ErrMissingConfig) {
		t.Errorf("RunPipeline(nil cfg) error = %v, want %v", err, ErrMissingConfig)
	}
}

func TestDomainOf(t *testing.T) {
	tests := []struct {
		name    string
		rawURL  string
		want    string
		wantErr bool
	}{
		{name: "host simples", rawURL: "https://www.exemplo.com.br/imoveis?page=1", want: "www.exemplo.com.br"},
		// Minúsculo sempre: o host é a chave de site_selectors e de
		// listings.source_domain — duas grafias seriam duas fontes.
		{name: "host com maiúsculas", rawURL: "https://WWW.Exemplo.COM.BR/imoveis", want: "www.exemplo.com.br"},
		// A porta faz parte do host e, portanto, da identidade da fonte.
		{name: "host com porta", rawURL: "http://localhost:8080/imoveis", want: "localhost:8080"},
		{name: "espaços nas bordas", rawURL: "  https://exemplo.com.br/imoveis  ", want: "exemplo.com.br"},
		{name: "sem host", rawURL: "file:///etc/passwd", wantErr: true},
		{name: "url inválida", rawURL: "://sem-esquema", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := domainOf(tt.rawURL)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("domainOf(%q) = %q, want erro", tt.rawURL, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("domainOf(%q) error = %v, want nil", tt.rawURL, err)
			}
			if got != tt.want {
				t.Errorf("domainOf(%q) = %q, want %q", tt.rawURL, got, tt.want)
			}
		})
	}
}
