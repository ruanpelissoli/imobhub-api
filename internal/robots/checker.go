package robots

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// FetchTimeout limita a busca do robots.txt. É bem menor que o timeout das
// páginas (httpclient.DefaultTimeout) porque o robots.txt é um arquivo de texto
// pequeno: se ele demora mais que isso, algo está errado e seguimos sem ele.
const FetchTimeout = 10 * time.Second

// maxRobotsBody limita o corpo lido do robots.txt. A especificação (RFC 9309)
// obriga o parsing de pelo menos 500 KiB; nada além disso é relevante e um
// arquivo gigante (ou um servidor hostil) não pode consumir memória do scraper.
const maxRobotsBody = 512 * 1024

// Checker resolve o robots.txt de cada origem (scheme + host) sob demanda e
// mantém o resultado em cache pelo tempo de vida do processo.
//
// Use NewChecker: o zero value tem client nulo. Um Checker não pode ser copiado
// depois do primeiro uso (contém um sync.Map) — passe sempre o ponteiro.
type Checker struct {
	userAgent string
	client    *http.Client

	// cache mapeia origem ("https://exemplo.com.br") para *Rules. sync.Map é
	// adequado aqui porque o padrão de acesso é "escreve uma vez por host, lê
	// muitas vezes" — exatamente o caso em que ele supera um mutex.
	cache sync.Map
}

// NewChecker cria um Checker que avalia as regras do User-Agent informado
// (tipicamente cfg.ScraperUserAgent, ex.: "ImobHubBot/1.0").
func NewChecker(userAgent string) *Checker {
	return &Checker{
		userAgent: userAgent,
		client:    &http.Client{Timeout: FetchTimeout},
	}
}

// UserAgent retorna o User-Agent usado na avaliação das regras.
func (c *Checker) UserAgent() string {
	return c.userAgent
}

// IsAllowed informa se rawURL pode ser acessada pelo User-Agent do Checker.
//
// O robots.txt da origem é buscado na primeira consulta a cada host e reusado
// nas seguintes. Falha na busca (404, erro de rede, timeout, HTML no lugar do
// texto) resulta em permissão concedida — e essa decisão também é cacheada,
// para não repetir uma requisição que já sabemos que não responde.
//
// O erro retornado indica apenas URL inválida (erro do chamador); nesse caso o
// bool é false. Problemas de rede não viram erro.
func (c *Checker) IsAllowed(ctx context.Context, rawURL string) (bool, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return false, fmt.Errorf("robots: could not parse URL %q: %w", rawURL, err)
	}
	if parsed.Host == "" {
		return false, fmt.Errorf("robots: URL %q has no host", rawURL)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return false, fmt.Errorf("robots: URL %q has unsupported scheme %q", rawURL, parsed.Scheme)
	}

	rules := c.rulesFor(ctx, parsed.Scheme, parsed.Host)
	return rules.Allowed(c.userAgent, requestPath(parsed)), nil
}

// rulesFor devolve as regras da origem, buscando o robots.txt na primeira vez.
//
// Duas goroutines podem buscar o mesmo host simultaneamente (não há
// singleflight aqui): o custo é uma requisição extra no pior caso, contra a
// complexidade de coordenar as buscas. Vence a simplicidade — o scraper visita
// poucas dezenas de hosts por run.
func (c *Checker) rulesFor(ctx context.Context, scheme, host string) *Rules {
	origin := scheme + "://" + host

	if cached, ok := c.cache.Load(origin); ok {
		return cached.(*Rules)
	}

	rules := c.fetch(ctx, origin)
	actual, _ := c.cache.LoadOrStore(origin, rules)
	return actual.(*Rules)
}

// fetch busca e interpreta o robots.txt da origem. Nunca devolve nil: qualquer
// falha vira AllowAll, a chamada "falha permissiva" descrita em IsAllowed.
func (c *Checker) fetch(ctx context.Context, origin string) *Rules {
	robotsURL := origin + "/robots.txt"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, robotsURL, nil)
	if err != nil {
		slog.Warn("robots: could not build robots.txt request, assuming allowed",
			"url", robotsURL, "error", err)
		return AllowAll()
	}
	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.client.Do(req)
	if err != nil {
		slog.Warn("robots: could not fetch robots.txt, assuming allowed",
			"url", robotsURL, "error", err)
		return AllowAll()
	}
	defer resp.Body.Close()

	// Só um 2xx traz regras. 404 significa "não existe robots.txt" (permitido
	// pela especificação) e os demais status são tratados do mesmo jeito por
	// decisão de projeto: um 5xx transitório não deve parar a coleta do dia.
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		slog.Info("robots: robots.txt not available, assuming allowed",
			"url", robotsURL, "status", resp.StatusCode)
		return AllowAll()
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRobotsBody))
	if err != nil {
		slog.Warn("robots: could not read robots.txt, assuming allowed",
			"url", robotsURL, "error", err)
		return AllowAll()
	}

	rules, err := Parse(body)
	if err != nil {
		slog.Warn("robots: could not parse robots.txt, assuming allowed",
			"url", robotsURL, "error", err)
		return AllowAll()
	}
	return rules
}

// requestPath monta o caminho avaliado contra o robots.txt. A query faz parte
// da comparação: regras como "Disallow: /busca?*" só funcionam com ela.
func requestPath(u *url.URL) string {
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	return path
}
