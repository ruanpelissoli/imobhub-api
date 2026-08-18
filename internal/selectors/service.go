package selectors

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/imobhub/api/internal/ai"
	"github.com/imobhub/api/internal/db"
	"github.com/imobhub/api/internal/httpclient"
)

// Erros de construção e de uso do serviço. São sentinelas para que o chamador
// distinga "o serviço foi montado errado" (bug de wiring, falha no boot) de
// "esta fonte falhou" (pular o site e seguir).
var (
	// ErrMissingPool indica que o serviço foi construído sem pool de conexões.
	ErrMissingPool = errors.New("selectors: database pool is required")
	// ErrMissingFetcher indica que faltou um dos dois clientes de busca de
	// página. Ambos são obrigatórios: o render_mode gravado no banco decide
	// qual deles é usado, e o serviço não tem como saber de antemão qual será.
	ErrMissingFetcher = errors.New("selectors: static and headless fetchers are required")
	// ErrMissingAPIKey indica que faltou a chave da API da Anthropic. É checada
	// na construção para que a falta apareça no boot, e não na primeira fonte
	// nova encontrada — que pode ser horas depois.
	ErrMissingAPIKey = errors.New("selectors: anthropic API key is required")
	// ErrMissingDomain indica chamada sem domínio. O domínio é a identidade da
	// linha em site_selectors; sem ele não há o que ler nem o que gravar.
	ErrMissingDomain = errors.New("selectors: domain is required")
	// ErrMissingSiteURL indica chamada sem a URL da página de listagens.
	ErrMissingSiteURL = errors.New("selectors: site URL is required")
)

// PageFetcher busca o HTML de uma página. É um tipo função (e não uma
// interface) porque a dependência tem uma operação só: os adaptadores
// StaticFetcher/HeadlessFetcher fecham sobre o User-Agent e os testes passam um
// closure, sem precisar de mock.
type PageFetcher func(ctx context.Context, siteURL string) (html string, err error)

// StaticFetcher adapta httpclient.FetchStatic à assinatura de PageFetcher. A
// URL final após redirecionamentos é descartada de propósito: a identidade da
// fonte é o domínio já normalizado que o chamador passou, e trocá-la no meio da
// descoberta gravaria os seletores sob outro domínio.
func StaticFetcher(userAgent string) PageFetcher {
	return func(ctx context.Context, siteURL string) (string, error) {
		html, _, err := httpclient.FetchStatic(ctx, siteURL, userAgent)
		return html, err
	}
}

// HeadlessFetcher adapta httpclient.FetchHeadless à assinatura de PageFetcher.
// Exige Chrome/Chromium no PATH (ver internal/httpclient/CLAUDE.md).
func HeadlessFetcher(userAgent string) PageFetcher {
	return func(ctx context.Context, siteURL string) (string, error) {
		return httpclient.FetchHeadless(ctx, siteURL, userAgent)
	}
}

// SelectorService orquestra a descoberta e a manutenção dos seletores CSS de
// cada domínio: lê o que já está em site_selectors e só aciona o Claude quando
// não há seletores conhecidos ou quando os conhecidos foram marcados como
// quebrados.
//
// O ponto todo do serviço é que a chamada à IA é cara e é paga: o caminho
// normal de uma coleta é um SELECT e mais nada.
type SelectorService struct {
	pool     *pgxpool.Pool
	static   PageFetcher
	headless PageFetcher
	apiKey   string

	// Costuras para teste. O acesso ao banco e à Anthropic são funções livres
	// (db.GetSelectorsByDomain, ai.AnalyzeSelectors), então estes campos são o
	// jeito de exercitar o fluxo sem PostgreSQL e sem gastar tokens. Ficam não
	// exportados: o wiring de produção é sempre o default do construtor.
	getSelectors    func(ctx context.Context, pool *pgxpool.Pool, domain string) (*db.SelectorConfig, error)
	upsertSelectors func(ctx context.Context, pool *pgxpool.Pool, config db.SelectorConfig) error
	analyze         func(ctx context.Context, apiKey, pageHTML, siteURL string) (*db.SelectorFields, string, error)
}

// NewSelectorService monta o serviço com as dependências injetadas. Todas são
// obrigatórias e validadas aqui para que um wiring incompleto derrube o boot,
// em vez de falhar na primeira fonte nova.
func NewSelectorService(pool *pgxpool.Pool, staticClient, headlessClient PageFetcher, anthropicAPIKey string) (*SelectorService, error) {
	if pool == nil {
		return nil, ErrMissingPool
	}
	if staticClient == nil || headlessClient == nil {
		return nil, ErrMissingFetcher
	}
	if strings.TrimSpace(anthropicAPIKey) == "" {
		return nil, ErrMissingAPIKey
	}

	return &SelectorService{
		pool:            pool,
		static:          staticClient,
		headless:        headlessClient,
		apiKey:          anthropicAPIKey,
		getSelectors:    db.GetSelectorsByDomain,
		upsertSelectors: db.UpsertSelectors,
		analyze:         ai.AnalyzeSelectors,
	}, nil
}

// EnsureSelectors devolve os seletores utilizáveis do domínio, descobrindo-os
// quando necessário.
//
//   - Linha com status 'valid': devolvida direto, sem rede e sem IA.
//   - Sem linha (primeira visita): busca o HTML com o cliente estático e pede os
//     seletores ao Claude. O render_mode devolvido por ele é o que fica gravado.
//   - Linha com status 'broken': busca o HTML com o render_mode já gravado
//     (foi ele que funcionou da última vez) e refaz a análise.
//
// Erros da Anthropic são propagados sem fallback: quem orquestra a coleta é que
// decide entre pular a fonte e abortar o run.
func (s *SelectorService) EnsureSelectors(ctx context.Context, domain string, siteURL string) (*db.SelectorConfig, error) {
	domain, siteURL, err := validateTarget(domain, siteURL)
	if err != nil {
		return nil, err
	}

	current, err := s.getSelectors(ctx, s.pool, domain)
	if err != nil {
		return nil, fmt.Errorf("selectors: ensuring selectors for %q: %w", domain, err)
	}

	if current != nil && current.Status == db.StatusValid {
		return current, nil
	}

	return s.discover(ctx, domain, siteURL, fetchModeOf(current))
}

// RecoverSelectors força uma nova análise pelo Claude, ignorando o status
// gravado, e regrava a linha do domínio como válida. É o caminho de
// auto-recuperação: o extrator não achou nenhum anúncio com os seletores atuais
// (o site mudou de layout) e quem chama já sabe que reusá-los é inútil.
//
// O HTML é buscado com o render_mode atual do banco — é o modo que funcionou na
// última descoberta, e o layout ter mudado não costuma mudar a natureza da
// página. Sem linha no banco, o padrão é o cliente estático, exatamente como na
// primeira visita.
func (s *SelectorService) RecoverSelectors(ctx context.Context, domain string, siteURL string) (*db.SelectorConfig, error) {
	domain, siteURL, err := validateTarget(domain, siteURL)
	if err != nil {
		return nil, err
	}

	current, err := s.getSelectors(ctx, s.pool, domain)
	if err != nil {
		return nil, fmt.Errorf("selectors: recovering selectors for %q: %w", domain, err)
	}

	return s.discover(ctx, domain, siteURL, fetchModeOf(current))
}

// discover executa o fluxo completo de descoberta: busca a página com o modo
// indicado, pede os seletores ao Claude e persiste o resultado.
//
// fetchMode é o modo usado para *buscar* o HTML agora; o render_mode gravado é
// o que o Claude devolve, que pode ser diferente — é assim que um site que
// virou SPA passa a ser coletado com headless na próxima execução.
func (s *SelectorService) discover(ctx context.Context, domain, siteURL, fetchMode string) (*db.SelectorConfig, error) {
	html, err := s.fetchPage(ctx, siteURL, fetchMode)
	if err != nil {
		return nil, fmt.Errorf("selectors: fetching listing page of %q: %w", domain, err)
	}

	fields, renderMode, err := s.analyze(ctx, s.apiKey, html, siteURL)
	if err != nil {
		return nil, fmt.Errorf("selectors: analyzing listing page of %q: %w", domain, err)
	}

	config := db.SelectorConfig{
		Domain:     domain,
		Selectors:  *fields,
		RenderMode: renderMode,
		// UpsertSelectors sempre grava 'valid'; o campo é preenchido aqui só
		// para que o valor devolvido reflita o que ficou no banco.
		Status: db.StatusValid,
	}

	if err := s.upsertSelectors(ctx, s.pool, config); err != nil {
		return nil, fmt.Errorf("selectors: saving selectors for %q: %w", domain, err)
	}

	// Mensagem em formato humano por decisão de produto (é o evento que o
	// operador procura nos logs quando uma fonte nova entra); os atributos
	// estruturados existem para filtrar por domínio e por modo.
	slog.InfoContext(ctx, fmt.Sprintf("[%s] seletores detectados via Claude (render_mode: %s)", domain, renderMode),
		"domain", domain,
		"render_mode", renderMode,
		"site_url", siteURL,
	)

	return &config, nil
}

// fetchPage escolhe o cliente HTTP pelo modo de renderização. Modo
// desconhecido cai no estático: é o default da coluna render_mode e errar para
// o lado barato é melhor do que subir um Chrome à toa.
func (s *SelectorService) fetchPage(ctx context.Context, siteURL, fetchMode string) (string, error) {
	if fetchMode == db.RenderModeHeadless {
		return s.headless(ctx, siteURL)
	}
	return s.static(ctx, siteURL)
}

// fetchModeOf devolve o modo de busca a usar para o HTML que vai ser analisado.
// Sem linha no banco (primeira visita) o modo é o estático: é o caminho barato,
// e o próprio HTML estático carrega os sinais de SPA que o Claude usa para
// pedir headless depois.
func fetchModeOf(current *db.SelectorConfig) string {
	if current == nil {
		return db.RenderModeStatic
	}
	if strings.EqualFold(strings.TrimSpace(current.RenderMode), db.RenderModeHeadless) {
		return db.RenderModeHeadless
	}
	return db.RenderModeStatic
}

// validateTarget normaliza as bordas dos argumentos e rejeita os vazios. O
// domínio em si não é normalizado aqui (host sem esquema, sem barra final): ele
// é a identidade da linha em site_selectors e precisa chegar já no formato
// gravado, senão a leitura e a escrita apontam para linhas diferentes.
func validateTarget(domain, siteURL string) (string, string, error) {
	domain = strings.TrimSpace(domain)
	if domain == "" {
		return "", "", ErrMissingDomain
	}

	siteURL = strings.TrimSpace(siteURL)
	if siteURL == "" {
		return "", "", fmt.Errorf("%w (domain %q)", ErrMissingSiteURL, domain)
	}

	return domain, siteURL, nil
}
