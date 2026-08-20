package scraper

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/imobhub/api/internal/config"
	"github.com/imobhub/api/internal/db"
	"github.com/imobhub/api/internal/ratelimit"
	"github.com/imobhub/api/internal/robots"
	"github.com/imobhub/api/internal/selectors"
	"github.com/imobhub/api/internal/sources"
)

// Erros de construção do pipeline. São de wiring (bug de inicialização), não de
// coleta: derrubam o boot em vez de pular uma fonte.
var (
	// ErrMissingConfig indica RunPipeline chamado sem configuração.
	ErrMissingConfig = errors.New("scraper: config is required to run the pipeline")
	// ErrMissingPool indica RunPipeline chamado sem pool de conexões.
	ErrMissingPool = errors.New("scraper: database pool is required to run the pipeline")
	// ErrMissingDependency indica que uma das dependências de Deps ficou nula.
	// Todas são obrigatórias: um campo nulo só apareceria como panic no meio da
	// primeira coleta, horas depois do boot.
	ErrMissingDependency = errors.New("scraper: pipeline dependency is missing")
)

// errNilSelectors cobre um serviço de seletores que devolva (nil, nil), o que o
// contrato de selectors.SelectorService não permite. Existe para que a violação
// vire falha daquela fonte em vez de um panic que derruba o run inteiro.
var errNilSelectors = errors.New("scraper: selector service returned no config and no error")

// Vocabulário do atributo `event`. Ele existe para que filtro e agregação de
// log não dependam do texto de `msg` — que é humano, contratual e por isso
// mesmo não pode virar chave de query.
const (
	eventRunStarted                = "scraper_run_started"
	eventRunNoSources              = "scraper_run_no_sources"
	eventRunAborted                = "scraper_run_aborted"
	eventRunFinished               = "scraper_run_finished"
	eventSiteScraped               = "site_scraped"
	eventSiteBlocked               = "site_blocked"
	eventSelectorsRecovering       = "site_selectors_recovering"
	eventSelectorsMarkBrokenFailed = "site_selectors_mark_broken_failed"
	eventSummaryCountFailed        = "scraper_summary_count_failed"
	eventLastRunPublishFailed      = "scraper_last_run_publish_failed"
	eventLastRunDisabled           = "scraper_last_run_persistence_disabled"
)

// Deps são as dependências do pipeline, injetadas como funções em vez de
// interfaces: cada colaborador tem uma operação só, e o projeto inteiro já usa
// esse estilo (ver selectors.PageFetcher e as costuras de syncer.go). O ganho
// prático é o teste — um closure por campo, sem mock e sem PostgreSQL.
//
// Todos os campos são obrigatórios; NewPipeline valida.
type Deps struct {
	// SourcesFile é o caminho do arquivo de fontes (config.SourcesFile).
	SourcesFile string
	// ReadSources carrega as URLs a coletar (sources.ReadSources).
	ReadSources func(filePath string) ([]string, error)
	// IsAllowed avalia o robots.txt da fonte (robots.Checker.IsAllowed).
	IsAllowed func(ctx context.Context, rawURL string) (bool, error)
	// Wait espaça as requisições ao mesmo domínio
	// (ratelimit.DomainLimiter.Wait).
	Wait func(ctx context.Context, domain string) error
	// FetchStatic busca o HTML sem executar JavaScript.
	FetchStatic func(ctx context.Context, siteURL string) (string, error)
	// FetchHeadless busca o HTML depois de o JavaScript da página rodar.
	FetchHeadless func(ctx context.Context, siteURL string) (string, error)
	// EnsureSelectors devolve os seletores utilizáveis do domínio
	// (selectors.SelectorService.EnsureSelectors).
	EnsureSelectors func(ctx context.Context, domain, siteURL string) (*db.SelectorConfig, error)
	// RecoverSelectors força a redescoberta pela IA
	// (selectors.SelectorService.RecoverSelectors).
	RecoverSelectors func(ctx context.Context, domain, siteURL string) (*db.SelectorConfig, error)
	// MarkBroken sinaliza que os seletores do domínio precisam de redescoberta
	// na próxima execução (db.MarkSelectorsBroken).
	MarkBroken func(ctx context.Context, domain string) error
	// Extract converte o HTML em anúncios (ExtractListings).
	Extract func(pageHTML string, config db.SelectorConfig, sourceDomain string) ([]db.RawListing, error)
	// Sync reconcilia os anúncios do domínio com o banco (SyncListings).
	Sync func(ctx context.Context, domain string, listings []db.RawListing, runStartedAt time.Time) (SyncStats, error)
	// CountListings devolve o total de anúncios no banco, usado só no resumo
	// final (db.CountListings).
	CountListings func(ctx context.Context) (int64, error)
	// PublishLastRun grava o resumo do run em LastRunCacheKey. É o **único**
	// campo opcional de Deps: sem Redis o pipeline roda igual, só não publica.
	PublishLastRun func(ctx context.Context, run LastRun)
}

// RunSummary é o resultado agregado de uma execução do pipeline.
type RunSummary struct {
	// Sites é quantas fontes o arquivo devolveu.
	Sites int
	// Succeeded é quantas fontes chegaram até a sincronização com o banco.
	Succeeded int
	// Failed é quantas fontes falharam em alguma etapa.
	Failed int
	// Blocked é quantas fontes o robots.txt proibiu — não são erro: o site
	// pediu para não ser coletado e nós obedecemos.
	Blocked int
	// Listings é quantos anúncios foram coletados nesta execução.
	Listings int
	// Removed é quantos anúncios sumiram das fontes e foram apagados nesta
	// execução (soma de SyncStats.Deleted).
	Removed int64
	// TotalInDB é o total de anúncios na tabela listings ao fim do run. Fica
	// zerado se a contagem falhar (o resumo é informativo, não bloqueia o run).
	TotalInDB int64
}

// Pipeline executa a coleta de todas as fontes. Use NewPipeline (wiring de
// produção) ou New (wiring explícito, usado nos testes).
type Pipeline struct {
	deps Deps
}

// siteOutcome é o desfecho da coleta de uma fonte.
type siteOutcome int

const (
	outcomeSuccess siteOutcome = iota
	outcomeBlocked
	outcomeFailed
	// outcomeAborted encerra o run inteiro: o contexto foi cancelado
	// (SIGINT/SIGTERM), então não faz sentido tentar a próxima fonte.
	outcomeAborted
)

// siteResult é o que uma fonte contribui para o resumo do run.
type siteResult struct {
	outcome  siteOutcome
	listings int
	removed  int64
}

// RunPipeline monta o pipeline com o wiring de produção e executa a coleta de
// todas as fontes.
//
// A montagem mora aqui, e não em cmd/scraper, por dois motivos: a assinatura
// exigida (ctx, cfg, pool) não tem por onde receber módulos prontos, e
// cmd/scraper/CLAUDE.md determina que nenhuma regra de negócio viva no main —
// escolher User-Agent, limiter e clientes de busca é regra de coleta.
//
// O erro retornado é reservado a falhas fatais (arquivo de fontes ilegível,
// wiring incompleto, contexto cancelado). Fonte que falha **não** vira erro do
// run: ela é logada, contada no resumo e o pipeline segue para a próxima.
func RunPipeline(ctx context.Context, cfg *config.Config, pool *pgxpool.Pool, redisClient *redis.Client) error {
	pipeline, err := NewPipeline(cfg, pool, redisClient)
	if err != nil {
		return err
	}

	// O resumo é descartado de propósito: ele já foi logado por Run, e a
	// assinatura exigida pelo binário é só "deu certo ou não". Quem precisar dos
	// números em memória (um teste, um futuro endpoint) usa Run direto.
	_, err = pipeline.Run(ctx)
	return err
}

// NewPipeline monta o pipeline com as dependências de produção, na ordem
// documentada em cmd/scraper/CLAUDE.md: robots checker → rate limiter →
// cliente estático → cliente headless → serviço de seletores (que embute o
// analisador da Anthropic) → extrator → sincronizador.
//
// O DomainLimiter nasce aqui, **uma vez por run**, e é compartilhado por todas
// as fontes: dois limiters teriam relógios independentes e dobrariam a carga na
// fonte, que é justamente o que o pacote ratelimit existe para evitar.
//
// redisClient nulo é wiring válido: o run acontece igual, sem publicar o resumo
// em LastRunCacheKey.
func NewPipeline(cfg *config.Config, pool *pgxpool.Pool, redisClient *redis.Client) (*Pipeline, error) {
	if cfg == nil {
		return nil, ErrMissingConfig
	}
	if pool == nil {
		return nil, ErrMissingPool
	}

	checker := robots.NewChecker(cfg.ScraperUserAgent)
	limiter := ratelimit.NewDomainLimiter(int(cfg.ScraperRateLimit.Milliseconds()))

	// Os mesmos fetchers vão para o pipeline e para o serviço de seletores: o
	// User-Agent precisa ser idêntico ao avaliado no robots.txt em toda
	// requisição, inclusive nas que a descoberta de seletores faz.
	staticFetcher := selectors.StaticFetcher(cfg.ScraperUserAgent)
	headlessFetcher := selectors.HeadlessFetcher(cfg.ScraperUserAgent)

	selectorService, err := selectors.NewSelectorService(pool, staticFetcher, headlessFetcher, cfg.AnthropicAPIKey)
	if err != nil {
		return nil, err
	}

	deps := Deps{
		SourcesFile:      cfg.SourcesFile,
		ReadSources:      sources.ReadSources,
		IsAllowed:        checker.IsAllowed,
		Wait:             limiter.Wait,
		FetchStatic:      staticFetcher,
		FetchHeadless:    headlessFetcher,
		EnsureSelectors:  selectorService.EnsureSelectors,
		RecoverSelectors: selectorService.RecoverSelectors,
		MarkBroken: func(ctx context.Context, domain string) error {
			return db.MarkSelectorsBroken(ctx, pool, domain)
		},
		Extract: ExtractListings,
		Sync: func(ctx context.Context, domain string, listings []db.RawListing, runStartedAt time.Time) (SyncStats, error) {
			return SyncListings(ctx, pool, domain, listings, runStartedAt)
		},
		CountListings: func(ctx context.Context) (int64, error) {
			return db.CountListings(ctx, pool)
		},
	}

	if publisher := newLastRunPublisher(redisClient); publisher != nil {
		deps.PublishLastRun = publisher.publish
	}

	return New(deps)
}

// New monta o pipeline a partir de dependências explícitas. Existe para os
// testes e para qualquer chamador que precise trocar um colaborador; o caminho
// normal é NewPipeline.
func New(deps Deps) (*Pipeline, error) {
	if strings.TrimSpace(deps.SourcesFile) == "" {
		return nil, fmt.Errorf("%w: SourcesFile", ErrMissingDependency)
	}

	required := []struct {
		name  string
		isNil bool
	}{
		{"ReadSources", deps.ReadSources == nil},
		{"IsAllowed", deps.IsAllowed == nil},
		{"Wait", deps.Wait == nil},
		{"FetchStatic", deps.FetchStatic == nil},
		{"FetchHeadless", deps.FetchHeadless == nil},
		{"EnsureSelectors", deps.EnsureSelectors == nil},
		{"RecoverSelectors", deps.RecoverSelectors == nil},
		{"MarkBroken", deps.MarkBroken == nil},
		{"Extract", deps.Extract == nil},
		{"Sync", deps.Sync == nil},
		{"CountListings", deps.CountListings == nil},
	}
	for _, field := range required {
		if field.isNil {
			return nil, fmt.Errorf("%w: %s", ErrMissingDependency, field.name)
		}
	}

	return &Pipeline{deps: deps}, nil
}

// Run coleta todas as fontes do arquivo, uma de cada vez, e devolve o resumo do
// que aconteceu.
//
// As fontes são processadas **sequencialmente**: paralelizar por site pareceria
// um ganho óbvio, mas o rate limiting é por domínio (fontes diferentes não
// competem entre si), então o ganho real vem só de sites lentos — e o custo
// seria N Chromes headless simultâneos e um rastro de erros entrelaçado nos
// logs. Fica para quando o volume de fontes justificar.
//
// Erro só é devolvido em falha fatal: arquivo de fontes ilegível ou contexto
// cancelado. Falha de uma fonte é logada e contada, e a coleta segue — um
// portal fora do ar não pode zerar a coleta do dia inteiro.
//
// O resumo é publicado em LastRunCacheKey por um defer, e não no caminho feliz:
// um run morto por SIGTERM no meio é exatamente o que o operador precisa
// enxergar, com os números parciais e success=false.
func (p *Pipeline) Run(ctx context.Context) (summary RunSummary, err error) {
	runStartedAt := time.Now()
	defer func() { p.publishLastRun(ctx, summary, err, runStartedAt) }()

	urls, err := p.deps.ReadSources(p.deps.SourcesFile)
	if err != nil {
		return summary, fmt.Errorf("scraper: could not load sources: %w", err)
	}
	summary.Sites = len(urls)

	slog.InfoContext(ctx, fmt.Sprintf("pipeline iniciado: %d fontes carregadas", len(urls)),
		"event", eventRunStarted,
		"sources_file", p.deps.SourcesFile,
		"sites_total", len(urls),
	)

	if len(urls) == 0 {
		// Não é erro: o arquivo pode estar todo comentado. Mas é sempre suspeito
		// o bastante para um Warn — é a explicação de um run que não gravou nada.
		slog.WarnContext(ctx, "nenhuma fonte utilizável no arquivo de fontes",
			"event", eventRunNoSources,
			"sources_file", p.deps.SourcesFile,
		)
		return summary, nil
	}

	for _, siteURL := range urls {
		// Cancelamento é verificado entre as fontes para que um SIGTERM encerre
		// o run em vez de arrastar as fontes restantes até falharem uma a uma.
		if ctxErr := ctx.Err(); ctxErr != nil {
			slog.WarnContext(ctx, fmt.Sprintf("coleta interrompida: %d de %d fontes processadas", summary.processed(), summary.Sites),
				"event", eventRunAborted,
				"processed", summary.processed(),
				"sites_total", summary.Sites,
				"error", ctxErr,
			)
			return summary, fmt.Errorf("scraper: pipeline canceled: %w", ctxErr)
		}

		result, siteErr := p.processSite(ctx, siteURL)
		switch result.outcome {
		case outcomeSuccess:
			summary.Succeeded++
			summary.Listings += result.listings
			summary.Removed += result.removed
		case outcomeBlocked:
			summary.Blocked++
		case outcomeFailed:
			summary.Failed++
		case outcomeAborted:
			p.logSummary(ctx, &summary, time.Since(runStartedAt))
			return summary, siteErr
		}
	}

	p.logSummary(ctx, &summary, time.Since(runStartedAt))
	return summary, nil
}

// publishLastRun entrega o resumo ao seam do Redis. Sem seam configurado o run
// é normal — só não deixa rastro no cache.
func (p *Pipeline) publishLastRun(ctx context.Context, summary RunSummary, runErr error, startedAt time.Time) {
	if p.deps.PublishLastRun == nil {
		slog.DebugContext(ctx, "resumo da última coleta não publicado: sem client de Redis no pipeline",
			"event", eventLastRunDisabled,
		)
		return
	}

	p.deps.PublishLastRun(ctx, LastRun{
		RanAt:             startedAt.UTC().Format(time.RFC3339),
		Success:           summary.Failed == 0 && runErr == nil,
		SitesTotal:        summary.Sites,
		SitesSucceeded:    summary.Succeeded,
		SitesFailed:       summary.Failed,
		SitesBlocked:      summary.Blocked,
		ListingsFound:     summary.Listings,
		ListingsRemoved:   summary.Removed,
		ListingsTotalInDB: summary.TotalInDB,
		DurationMS:        time.Since(startedAt).Milliseconds(),
	})
}

// processSite executa o fluxo completo de uma fonte. O erro devolvido é fatal
// (encerra o run) e vem sempre acompanhado de outcomeAborted; falhas da própria
// fonte são logadas aqui e sinalizadas por outcomeFailed, com erro nulo.
//
// Cada fonte produz **exatamente uma** linha com event=site_scraped (ou
// event=site_blocked, que não é falha): é essa invariante que permite contar
// fontes por agregação de log em vez de por texto de mensagem.
func (p *Pipeline) processSite(ctx context.Context, siteURL string) (siteResult, error) {
	siteStartedAt := time.Now()

	domain, err := domainOf(siteURL)
	if err != nil {
		// Sem host não há identidade de domínio: nem robots.txt, nem rate
		// limiting, nem linha em site_selectors.
		logSiteScraped(ctx, slog.LevelError, fmt.Sprintf("[%s] fonte ignorada: URL sem domínio utilizável", siteURL),
			"", siteURL, 0, 0, time.Since(siteStartedAt), err)
		return siteResult{outcome: outcomeFailed}, nil
	}

	allowed, err := p.deps.IsAllowed(ctx, siteURL)
	if err != nil {
		return failSite(ctx, domain, siteURL, "falha ao avaliar o robots.txt", siteStartedAt, err)
	}
	if !allowed {
		// Warn e não Error: o site pediu para não ser coletado e nós obedecemos
		// — é o pipeline funcionando, não falhando.
		slog.WarnContext(ctx, fmt.Sprintf("[%s] bloqueado por robots.txt", domain),
			"event", eventSiteBlocked,
			"domain", domain,
			"site_url", siteURL,
			"duration_ms", time.Since(siteStartedAt).Milliseconds(),
		)
		return siteResult{outcome: outcomeBlocked}, nil
	}

	// Antes de qualquer requisição ao domínio, inclusive a que a descoberta de
	// seletores pode fazer logo abaixo.
	if err := p.deps.Wait(ctx, domain); err != nil {
		return siteResult{outcome: outcomeAborted}, fmt.Errorf("scraper: pipeline canceled while rate limiting %q: %w", domain, err)
	}

	// O corte é lido por domínio e antes de qualquer escrita: é ele que separa
	// "visto nesta coleta" de "sumiu do site" em SyncListings. O início global
	// do run não serviria — o primeiro domínio já teria anúncios mais novos que
	// o corte do último.
	runStartedAt := time.Now()

	config, err := p.deps.EnsureSelectors(ctx, domain, siteURL)
	if err != nil {
		return failSite(ctx, domain, siteURL, "falha ao obter os seletores", siteStartedAt, err)
	}
	// O serviço de seletores nunca devolve (nil, nil), mas a alternativa a esta
	// guarda é um panic que derruba o run inteiro por causa de uma fonte.
	if config == nil {
		return failSite(ctx, domain, siteURL, "falha ao obter os seletores", siteStartedAt, errNilSelectors)
	}

	listings, err := p.collect(ctx, domain, siteURL, *config)
	if err != nil {
		return failSite(ctx, domain, siteURL, "falha ao coletar a página de listagem", siteStartedAt, err)
	}

	if len(listings) == 0 {
		// Zero anúncios com seletores tidos como válidos é o sintoma de mudança
		// de layout. SyncListings não deleta nada nesse estado (proteção do
		// catálogo), então o caminho certo é redescobrir os seletores.
		slog.WarnContext(ctx, fmt.Sprintf("[%s] nenhum anúncio extraído, redescobrindo seletores via Claude", domain),
			"event", eventSelectorsRecovering,
			"domain", domain,
			"site_url", siteURL,
			"render_mode", config.RenderMode,
		)

		recovered, err := p.deps.RecoverSelectors(ctx, domain, siteURL)
		if err != nil {
			return failSite(ctx, domain, siteURL, "falha ao redescobrir os seletores", siteStartedAt, err)
		}
		if recovered == nil {
			return failSite(ctx, domain, siteURL, "falha ao redescobrir os seletores", siteStartedAt, errNilSelectors)
		}

		// A redescoberta pode ter trocado o render_mode (site que virou SPA):
		// collect relê o modo de config, então a re-busca já usa o novo.
		listings, err = p.collect(ctx, domain, siteURL, *recovered)
		if err != nil {
			return failSite(ctx, domain, siteURL, "falha ao recoletar a página após a redescoberta dos seletores", siteStartedAt, err)
		}

		if len(listings) == 0 {
			logSiteScraped(ctx, slog.LevelError, fmt.Sprintf("[%s] nenhum anúncio extraído mesmo após a redescoberta dos seletores", domain),
				domain, siteURL, 0, 0, time.Since(siteStartedAt), nil,
				"render_mode", recovered.RenderMode)
			// RecoverSelectors acabou de gravar a linha como 'valid'; sem esta
			// marcação o run de amanhã reusaria seletores que já sabemos não
			// extrair nada, e nunca mais acionaria a redescoberta.
			p.markBroken(ctx, domain)
			return siteResult{outcome: outcomeFailed}, nil
		}
	}

	stats, err := p.deps.Sync(ctx, domain, listings, runStartedAt)
	if err != nil {
		return failSite(ctx, domain, siteURL, "falha ao sincronizar os anúncios com o banco", siteStartedAt, err)
	}

	// Mensagem em formato humano por decisão de produto (é a linha que o
	// operador procura por domínio); os atributos estruturados existem para
	// filtrar e somar.
	logSiteScraped(ctx, slog.LevelInfo,
		fmt.Sprintf("[%s] ✓ %d listings (upserted: %d, deletados: %d)", domain, len(listings), stats.Upserted, stats.Deleted),
		domain, siteURL, len(listings), stats.Deleted, time.Since(siteStartedAt), nil,
		"upserted", stats.Upserted)

	return siteResult{outcome: outcomeSuccess, listings: len(listings), removed: stats.Deleted}, nil
}

// logSiteScraped emite a linha terminal de uma fonte com o vocabulário fixo de
// event=site_scraped. Centralizar os atributos é o que garante que as quatro
// saídas de processSite tenham os mesmos campos agregáveis.
func logSiteScraped(ctx context.Context, level slog.Level, msg, domain, siteURL string, found int, removed int64, elapsed time.Duration, err error, extra ...any) {
	attrs := []any{
		"event", eventSiteScraped,
		"domain", domain,
		"site_url", siteURL,
		"listings_found", found,
		"listings_removed", removed,
		"duration_ms", elapsed.Milliseconds(),
	}
	if err != nil {
		attrs = append(attrs, "error", err)
	}

	slog.Log(ctx, level, msg, append(attrs, extra...)...)
}

// collect busca a página de listagem com o modo de renderização dos seletores e
// extrai os anúncios. Zero anúncios não é erro aqui — é sinal de seletores
// quebrados, e quem decide o que fazer com isso é processSite.
func (p *Pipeline) collect(ctx context.Context, domain, siteURL string, config db.SelectorConfig) ([]db.RawListing, error) {
	// Segundo Wait do domínio: a descoberta/redescoberta de seletores já pode
	// ter buscado a página, e o contrato do limiter é uma espera por requisição.
	// Esperar a mais é político; esperar a menos é levar bloqueio da fonte.
	if err := p.deps.Wait(ctx, domain); err != nil {
		return nil, fmt.Errorf("scraper: rate limiting %q: %w", domain, err)
	}

	html, err := p.fetch(ctx, siteURL, config.RenderMode)
	if err != nil {
		return nil, fmt.Errorf("scraper: fetching listing page of %q: %w", domain, err)
	}

	listings, err := p.deps.Extract(html, config, domain)
	if err != nil {
		return nil, fmt.Errorf("scraper: extracting listings of %q: %w", domain, err)
	}

	return listings, nil
}

// fetch escolhe o cliente pelo render_mode gravado em site_selectors. Modo
// desconhecido cai no estático — é o default da coluna, e errar para o lado
// barato é melhor do que subir um Chrome à toa (mesma regra de
// selectors.fetchModeOf).
func (p *Pipeline) fetch(ctx context.Context, siteURL, renderMode string) (string, error) {
	if strings.EqualFold(strings.TrimSpace(renderMode), db.RenderModeHeadless) {
		return p.deps.FetchHeadless(ctx, siteURL)
	}
	return p.deps.FetchStatic(ctx, siteURL)
}

// markBroken sinaliza a linha do domínio para redescoberta no próximo run.
// Falhar aqui não muda o desfecho da fonte (que já falhou): vira um Warn, para
// que o operador saiba que o run seguinte vai reusar os seletores ruins.
func (p *Pipeline) markBroken(ctx context.Context, domain string) {
	if err := p.deps.MarkBroken(ctx, domain); err != nil {
		slog.WarnContext(ctx, fmt.Sprintf("[%s] não foi possível marcar os seletores como quebrados", domain),
			"event", eventSelectorsMarkBrokenFailed,
			"domain", domain,
			"error", err,
		)
	}
}

// logSummary emite o resumo final do run. A contagem no banco é informativa: se
// a query falhar, o resumo sai mesmo assim com o total zerado, porque perder o
// resumo inteiro por causa dela seria pior.
func (p *Pipeline) logSummary(ctx context.Context, summary *RunSummary, elapsed time.Duration) {
	total, err := p.deps.CountListings(ctx)
	if err != nil {
		slog.WarnContext(ctx, "não foi possível contar os anúncios no banco para o resumo",
			"event", eventSummaryCountFailed,
			"error", err,
		)
	}
	summary.TotalInDB = total

	slog.InfoContext(ctx, fmt.Sprintf("coleta concluída: %d fontes (%d com sucesso, %d com erro, %d bloqueadas), %d anúncios coletados, %d anúncios no banco",
		summary.Sites, summary.Succeeded, summary.Failed, summary.Blocked, summary.Listings, summary.TotalInDB),
		"event", eventRunFinished,
		"sites_total", summary.Sites,
		"sites_succeeded", summary.Succeeded,
		"sites_failed", summary.Failed,
		"sites_blocked", summary.Blocked,
		"listings_found", summary.Listings,
		"listings_removed", summary.Removed,
		"listings_total_in_db", summary.TotalInDB,
		"duration_ms", elapsed.Milliseconds(),
	)
}

// processed é quantas fontes já tiveram desfecho, usado no log de interrupção.
func (s RunSummary) processed() int {
	return s.Succeeded + s.Failed + s.Blocked
}

// failSite loga a falha de uma fonte e decide se ela encerra o run: contexto
// cancelado não é culpa da fonte (é SIGTERM chegando no meio de uma requisição)
// e tentar as próximas só produziria uma cascata de erros idênticos.
func failSite(ctx context.Context, domain, siteURL, message string, startedAt time.Time, err error) (siteResult, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return siteResult{outcome: outcomeAborted}, fmt.Errorf("scraper: pipeline canceled while processing %q: %w", domain, ctxErr)
	}

	logSiteScraped(ctx, slog.LevelError, fmt.Sprintf("[%s] %s", domain, message),
		domain, siteURL, 0, 0, time.Since(startedAt), err)
	return siteResult{outcome: outcomeFailed}, nil
}

// domainOf extrai a identidade do domínio a partir da URL da fonte. O host é
// minúsculo porque ele é a chave de site_selectors e de listings.source_domain:
// "Exemplo.com.br" e "exemplo.com.br" viveriam como duas fontes distintas, com
// dois conjuntos de seletores e dois catálogos. A porta faz parte do host (e
// portanto da identidade) e o "www" é preservado — são hosts diferentes de fato.
func domainOf(rawURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", fmt.Errorf("scraper: could not parse source URL %q: %w", rawURL, err)
	}

	host := strings.ToLower(strings.TrimSpace(parsed.Host))
	if host == "" {
		return "", fmt.Errorf("scraper: source URL %q has no host", rawURL)
	}

	return host, nil
}
