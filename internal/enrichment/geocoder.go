package enrichment

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
)

// Providers de geocodificação aceitos. As mesmas constantes existem em
// internal/config: config é folha do grafo de importação (ver internal/CLAUDE.md)
// e não pode importar enrichment. config valida no boot — valor desconhecido é
// erro, nunca fallback silencioso — e NewGeocoder valida de novo devolvendo
// ErrProviderNotSupported. A duplicação é defesa em profundidade, consciente.
const (
	ProviderNominatim  = "nominatim"
	ProviderGoogleMaps = "googlemaps"
)

// DefaultGeocodingRateLimitMS é o intervalo mínimo, em milissegundos, entre
// requisições ao provider de geocodificação. O Nominatim permite no máximo
// 1 req/s; ultrapassar isso rende bloqueio do User-Agent.
const DefaultGeocodingRateLimitMS = 1000

// Erros sentinela do geocoder. O chamador reage de forma diferente a cada um,
// então todos são comparáveis com errors.Is e sempre chegam embrulhados com o
// prefixo "enrichment:".
var (
	// ErrAddressNotFound indica que o provider respondeu 200 com zero
	// resultados. NÃO é falha: significa "endereço não resolvível", e o
	// chamador deve gravar NULL em lat/lng e seguir. Falha de rede, timeout e
	// status >= 400 produzem erros distintos justamente para que a fila não
	// confunda "não existe" com "não deu para perguntar agora".
	ErrAddressNotFound = errors.New("enrichment: address not found")
	// ErrEmptyAddress indica endereço vazio ou só com espaços. Retorna sem
	// fazer requisição alguma — perguntar "" ao provider só gasta quota.
	ErrEmptyAddress = errors.New("enrichment: address is empty")
	// ErrProviderNotSupported indica um valor desconhecido em
	// GEOCODING_PROVIDER, ou um provider reconhecido porém ainda não
	// implementado (Google Maps).
	ErrProviderNotSupported = errors.New("enrichment: geocoding provider is not supported")
	// ErrInvalidCoordinates indica que a resposta do provider trouxe uma
	// coordenada não parseável ou fora de faixa. Vira erro em vez de virar
	// coordenada: gravar um ponto errado é pior que não gravar nada.
	ErrInvalidCoordinates = errors.New("enrichment: provider returned invalid coordinates")
	// ErrMissingUserAgent indica construção sem User-Agent. Sem esse header o
	// Nominatim responde 403 — falhar no construtor é melhor que descobrir
	// isso na primeira requisição da run.
	ErrMissingUserAgent = errors.New("enrichment: geocoder requires a non-empty User-Agent")
)

// Geocoder converte o endereço textual de um anúncio em coordenadas
// geográficas.
//
// city e state seguem a mesma lógica de NeighborhoodNormalizer.Normalize: hoje
// nenhum chamador tem esses dados (listings só tem address_raw), então "" é
// entrada válida e significa "não informado". Os parâmetros existem agora para
// que a assinatura não mude quando o parsing de endereço evoluir.
//
// Fronteira deliberada: o geocoder NÃO consulta o banco. O filtro "anúncio já
// geocodificado" (lat IS NULL) é da fila de enriquecimento, não daqui — este
// pacote não importa internal/db (ver internal/enrichment/CLAUDE.md).
type Geocoder interface {
	// Geocode resolve address em coordenadas.
	//
	// Erros esperados, todos detectáveis com errors.Is: ErrEmptyAddress,
	// ErrAddressNotFound (o chamador grava NULL, não é falha),
	// ErrInvalidCoordinates e ErrProviderNotSupported. Falha de rede, timeout
	// e status >= 400 chegam embrulhados e não casam com nenhum deles.
	Geocode(ctx context.Context, address, city, state string) (lat, lng float64, err error)
}

// NewGeocoder resolve o provider a partir da configuração
// (config.Config.GeocodingProvider, GeocodingAPIKey e GeocodingRateLimit).
//
// userAgent vem de cfg.ScraperUserAgent — nunca de os.Getenv aqui: config é a
// única fronteira do projeto com o ambiente. rateLimitMs em zero desativa o
// espaçamento entre requisições (só para testes locais).
func NewGeocoder(providerName, apiKey, userAgent string, rateLimitMs int) (Geocoder, error) {
	switch normalizeProviderName(providerName) {
	case ProviderNominatim:
		return NewNominatimGeocoder(userAgent, rateLimitMs)
	case ProviderGoogleMaps:
		return NewGoogleMapsGeocoder(apiKey)
	default:
		return nil, fmt.Errorf("%w: %q", ErrProviderNotSupported, providerName)
	}
}

// normalizeProviderName tolera espaços e caixa vindos do .env, do mesmo jeito
// que config.lookup faz com o valor bruto.
func normalizeProviderName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// validateCoordinates rejeita coordenadas impossíveis. NaN e Inf entram na
// checagem porque strconv.ParseFloat aceita "NaN" e "Inf" sem reclamar, e
// nenhum dos dois sobrevive a um DOUBLE PRECISION com significado geográfico.
func validateCoordinates(lat, lng float64) error {
	if math.IsNaN(lat) || math.IsNaN(lng) || math.IsInf(lat, 0) || math.IsInf(lng, 0) {
		return fmt.Errorf("%w: lat=%v lng=%v", ErrInvalidCoordinates, lat, lng)
	}
	if lat < -90 || lat > 90 {
		return fmt.Errorf("%w: latitude %v is out of range [-90, 90]", ErrInvalidCoordinates, lat)
	}
	if lng < -180 || lng > 180 {
		return fmt.Errorf("%w: longitude %v is out of range [-180, 180]", ErrInvalidCoordinates, lng)
	}
	return nil
}

// addressCacheKey produz a chave de cache do trio endereço/cidade/estado.
// Reusa normalizeKey (minúsculas, sem acento, espaços colapsados, pontuação de
// borda descartada), o mesmo tratamento aplicado às chaves de alias de bairro,
// para que "Rua das Acácias, 10" e "RUA DAS ACACIAS 10" sejam uma consulta só.
//
// O separador "|" não aparece em endereço e evita que ("a b", "") colida com
// ("a", "b").
func addressCacheKey(address, city, state string) string {
	return strings.Join([]string{
		normalizeKey(address),
		normalizeKey(city),
		normalizeKey(state),
	}, "|")
}

// geocodeEntry é o que o cache guarda por endereço. notFound registra o
// negative caching: reperguntar o mesmo endereço irresolvível a cada anúncio é
// o pior desperdício de quota possível.
type geocodeEntry struct {
	lat      float64
	lng      float64
	notFound bool
}

// geocodeCache memoriza respostas dentro de um run. A fila de enriquecimento é
// concorrente por design, então o mapa é protegido por mutex.
//
// Só entram sucesso e ErrAddressNotFound. Erro de rede, timeout e status >= 400
// são transitórios: cacheá-los condenaria o endereço até o fim do processo.
//
// Sem TTL e sem limite de tamanho — o cache vive enquanto o processo vive,
// mesma escolha do cache de robots.Checker. Uma run diária vê poucos milhares
// de endereços; se isso mudar, o lugar de corrigir é aqui.
type geocodeCache struct {
	mu      sync.Mutex
	entries map[string]geocodeEntry
}

func newGeocodeCache() *geocodeCache {
	return &geocodeCache{entries: make(map[string]geocodeEntry)}
}

// lookup devolve a entrada memorizada para key, se houver.
func (c *geocodeCache) lookup(key string) (geocodeEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[key]
	return entry, ok
}

// store memoriza um sucesso.
func (c *geocodeCache) store(key string, lat, lng float64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries[key] = geocodeEntry{lat: lat, lng: lng}
}

// storeNotFound memoriza um ErrAddressNotFound (negative caching).
func (c *geocodeCache) storeNotFound(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries[key] = geocodeEntry{notFound: true}
}
