package enrichment

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/imobhub/api/internal/ratelimit"
)

// NominatimBaseURL é o endpoint de busca do Nominatim (OpenStreetMap). É
// gratuito e não exige chave; em troca, a política de uso pede User-Agent
// identificável e no máximo 1 requisição por segundo.
const NominatimBaseURL = "https://nominatim.openstreetmap.org/search"

// nominatimTimeout limita cada requisição de geocodificação. É o mesmo teto de
// robots.FetchTimeout e bem menor que httpclient.DefaultTimeout: a resposta é
// um JSON minúsculo, então demorar mais que isso significa que o serviço está
// degradado e a fila ganha mais deixando o endereço para a próxima run.
const nominatimTimeout = 10 * time.Second

// maxGeocodeBody limita o corpo lido da resposta. Um resultado com limit=1 tem
// poucos KB; o teto existe para que um servidor hostil (ou um proxy que devolve
// uma página de erro gigante) não consuma memória do processo.
const maxGeocodeBody = 64 << 10

// Doer é a costura de injeção do cliente HTTP: `*http.Client` já a satisfaz, e
// os testes passam um fake para não tocar a rede. Fica no pacote (e não em
// httpclient) porque é a única dependência HTTP daqui — o geocoder não usa
// `httpclient.FetchStatic`, que é feito para HTML e não limita o corpo.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// nominatimGeocoder implementa Geocoder sobre a API do Nominatim.
//
// Não é copiável depois do primeiro uso: cache e limiter contêm mutexes. Sempre
// use o ponteiro devolvido pelos construtores.
type nominatimGeocoder struct {
	userAgent string
	baseURL   string
	// host é extraído da baseURL na construção e serve de chave do limiter.
	// Guardá-lo evita reparsear a URL a cada chamada.
	host    string
	client  Doer
	limiter *ratelimit.DomainLimiter
	cache   *geocodeCache
}

// NewNominatimGeocoder cria o geocoder padrão do projeto.
//
// userAgent vem de config.Config.ScraperUserAgent e é obrigatório: sem esse
// header o Nominatim responde 403. Falhar aqui é melhor que descobrir isso na
// primeira requisição da run — por isso o construtor erra com
// ErrMissingUserAgent em vez de assumir um valor genérico.
//
// rateLimitMs é o intervalo mínimo entre requisições (default
// DefaultGeocodingRateLimitMS, vindo de GEOCODING_RATE_LIMIT_MS). Zero desativa
// o espaçamento — só para testes locais.
//
// O cliente HTTP é criado aqui, com timeout próprio e **sem cookie jar**: o
// mesmo raciocínio de httpclient.FetchStatic, um jar só carregaria estado de
// sessão sem serventia para uma API pública.
func NewNominatimGeocoder(userAgent string, rateLimitMs int) (Geocoder, error) {
	client := &http.Client{Timeout: nominatimTimeout}
	return newNominatimGeocoder(userAgent, NominatimBaseURL, client, rateLimitMs)
}

// newNominatimGeocoder é o construtor completo, não exportado. Existe para que
// os testes apontem o geocoder a um httptest.Server e injetem um Doer fake.
func newNominatimGeocoder(userAgent, baseURL string, client Doer, rateLimitMs int) (*nominatimGeocoder, error) {
	if strings.TrimSpace(userAgent) == "" {
		return nil, fmt.Errorf("%w", ErrMissingUserAgent)
	}

	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("enrichment: invalid geocoding base URL %q: %w", baseURL, err)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("enrichment: geocoding base URL %q has no host", baseURL)
	}

	return &nominatimGeocoder{
		userAgent: strings.TrimSpace(userAgent),
		baseURL:   baseURL,
		host:      parsed.Host,
		client:    client,
		limiter:   ratelimit.NewDomainLimiter(rateLimitMs),
		cache:     newGeocodeCache(),
	}, nil
}

// nominatimResult é o subconjunto da resposta que interessa. O Nominatim
// devolve lat/lon como **string** (não como número), então o parsing é
// explícito — decodificar em float64 direto falharia em runtime.
type nominatimResult struct {
	Lat string `json:"lat"`
	Lon string `json:"lon"`
}

// Geocode implementa Geocoder. Ver a documentação da interface para a semântica
// de cada erro sentinela.
func (g *nominatimGeocoder) Geocode(ctx context.Context, address, city, state string) (float64, float64, error) {
	address = strings.TrimSpace(address)
	if address == "" {
		// Sem requisição alguma: perguntar "" ao provider só gasta quota.
		return 0, 0, fmt.Errorf("%w", ErrEmptyAddress)
	}

	key := addressCacheKey(address, city, state)
	if entry, ok := g.cache.lookup(key); ok {
		if entry.notFound {
			return 0, 0, fmt.Errorf("%w: %q", ErrAddressNotFound, address)
		}
		return entry.lat, entry.lng, nil
	}

	// O timeout é aplicado sobre o ctx do chamador, não sobre context.Background:
	// o cancelamento de cima (SIGTERM) continua governando e não fica preso
	// atrás da requisição.
	reqCtx, cancel := context.WithTimeout(ctx, nominatimTimeout)
	defer cancel()

	results, err := g.search(reqCtx, address, city, state)
	if err != nil {
		// Erro de rede/timeout/status é transitório: NÃO entra no cache.
		slog.Warn("enrichment: geocoding request failed",
			"address", address, "provider", ProviderNominatim, "error", err)
		return 0, 0, err
	}

	if len(results) == 0 {
		// Negative caching: reperguntar o mesmo endereço irresolvível a cada
		// anúncio do mesmo condomínio é o pior desperdício de quota possível.
		g.cache.storeNotFound(key)
		slog.Warn("enrichment: address not found by geocoding provider",
			"address", address, "provider", ProviderNominatim)
		return 0, 0, fmt.Errorf("%w: %q", ErrAddressNotFound, address)
	}

	lat, lng, err := parseNominatimCoordinates(results[0])
	if err != nil {
		// Coordenada inválida também não é cacheada: é resposta corrompida do
		// provider, não um fato sobre o endereço.
		return 0, 0, err
	}

	g.cache.store(key, lat, lng)
	return lat, lng, nil
}

// search monta a query, respeita o rate limit e devolve os resultados brutos.
func (g *nominatimGeocoder) search(ctx context.Context, address, city, state string) ([]nominatimResult, error) {
	endpoint := g.baseURL + "?" + geocodeQuery(address, city, state).Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("enrichment: could not build geocoding request: %w", err)
	}
	// User-Agent identificável é exigência da política do Nominatim (sem ele,
	// 403). Accept-Language garante os nomes em pt-BR na resposta.
	req.Header.Set("User-Agent", g.userAgent)
	req.Header.Set("Accept-Language", "pt-BR")

	// Wait imediatamente antes do Do: o tempo gasto entre o Wait e a requisição
	// sai do intervalo efetivo (ver internal/ratelimit/CLAUDE.md).
	if err := g.limiter.Wait(ctx, g.host); err != nil {
		return nil, fmt.Errorf("enrichment: waiting for the geocoding rate limit: %w", err)
	}

	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("enrichment: geocoding request to %q failed: %w", g.host, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusBadRequest {
		// Drena um pedaço do corpo para que a conexão volte ao pool; corpos de
		// erro podem ser páginas inteiras, daí o teto.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("enrichment: geocoding request to %q returned HTTP %d %s",
			g.host, resp.StatusCode, http.StatusText(resp.StatusCode))
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxGeocodeBody))
	if err != nil {
		return nil, fmt.Errorf("enrichment: could not read the geocoding response: %w", err)
	}

	var results []nominatimResult
	if err := json.Unmarshal(body, &results); err != nil {
		return nil, fmt.Errorf("enrichment: could not decode the geocoding response: %w", err)
	}

	return results, nil
}

// geocodeQuery monta os parâmetros com net/url — nunca por concatenação de
// string, que quebraria em endereços com "&", "#" ou acento.
//
// countrycodes=br é obrigatório para a correção do dado, não uma otimização: o
// projeto raspa apenas portais brasileiros e, sem ele, "Centro, Rio" casa com
// um lugar em Portugal. addressdetails=0 evita trazer o objeto de endereço
// completo, que não é usado.
func geocodeQuery(address, city, state string) url.Values {
	parts := make([]string, 0, 3)
	for _, part := range []string{address, city, state} {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			parts = append(parts, trimmed)
		}
	}

	values := url.Values{}
	values.Set("q", strings.Join(parts, ", "))
	values.Set("format", "jsonv2")
	values.Set("limit", "1")
	values.Set("countrycodes", "br")
	values.Set("addressdetails", "0")
	return values
}

// parseNominatimCoordinates converte o par lat/lon (strings) em float64 e
// valida a faixa. Coordenada não parseável ou impossível vira erro: gravar um
// ponto errado no mapa é pior que não gravar nada.
func parseNominatimCoordinates(result nominatimResult) (float64, float64, error) {
	lat, err := strconv.ParseFloat(strings.TrimSpace(result.Lat), 64)
	if err != nil {
		return 0, 0, fmt.Errorf("%w: latitude %q is not a number", ErrInvalidCoordinates, result.Lat)
	}

	lng, err := strconv.ParseFloat(strings.TrimSpace(result.Lon), 64)
	if err != nil {
		return 0, 0, fmt.Errorf("%w: longitude %q is not a number", ErrInvalidCoordinates, result.Lon)
	}

	if err := validateCoordinates(lat, lng); err != nil {
		return 0, 0, err
	}

	return lat, lng, nil
}
