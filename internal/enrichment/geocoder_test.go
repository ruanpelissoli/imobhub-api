package enrichment

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testUserAgent identifica o bot nos testes. O valor em si não importa; o que
// importa é que ele chegue ao servidor no header User-Agent.
const testUserAgent = "ImobHubBot/1.0 (teste)"

// nominatimPayload é uma resposta realista do Nominatim: lat/lon vêm como
// **string**, não como número. Decodificar direto em float64 falharia.
const nominatimPayload = `[
  {
    "place_id": 123456,
    "licence": "Data © OpenStreetMap contributors",
    "osm_type": "way",
    "osm_id": 987654,
    "lat": "-15.7942287",
    "lon": "-47.8821658",
    "category": "highway",
    "type": "residential",
    "place_rank": 26,
    "importance": 0.1,
    "addresstype": "road",
    "name": "Rua das Acácias",
    "display_name": "Rua das Acácias, Brasília, Distrito Federal, Brasil"
  }
]`

// countingServer devolve body com o status informado e conta as requisições
// recebidas, para que os testes de cache possam afirmar "uma requisição só".
func countingServer(t *testing.T, status int, body string) (*httptest.Server, *atomic.Int64, *[]*http.Request) {
	t.Helper()

	var count atomic.Int64
	var mu sync.Mutex
	var requests []*http.Request

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)

		mu.Lock()
		requests = append(requests, r.Clone(context.Background()))
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)

	return server, &count, &requests
}

// newTestGeocoder aponta o geocoder ao servidor fake, com o rate limit
// desativado para os testes não somarem segundos de espera.
func newTestGeocoder(t *testing.T, server *httptest.Server) *nominatimGeocoder {
	t.Helper()

	geocoder, err := newNominatimGeocoder(testUserAgent, server.URL+"/search", server.Client(), 0)
	if err != nil {
		t.Fatalf("newNominatimGeocoder() error = %v, want nil", err)
	}
	return geocoder
}

// doerFunc adapta uma função à interface Doer. Serve aos casos que não têm um
// servidor do outro lado (erro de transporte, contexto cancelado).
type doerFunc func(req *http.Request) (*http.Response, error)

func (f doerFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }

func TestGeocodeReturnsCoordinates(t *testing.T) {
	server, count, _ := countingServer(t, http.StatusOK, nominatimPayload)
	geocoder := newTestGeocoder(t, server)

	lat, lng, err := geocoder.Geocode(context.Background(), "Rua das Acácias, 10", "", "")
	if err != nil {
		t.Fatalf("Geocode() error = %v, want nil", err)
	}

	if lat != -15.7942287 {
		t.Errorf("lat = %v, want %v", lat, -15.7942287)
	}
	if lng != -47.8821658 {
		t.Errorf("lng = %v, want %v", lng, -47.8821658)
	}
	if got := count.Load(); got != 1 {
		t.Errorf("requests = %d, want 1", got)
	}
}

func TestGeocodeSendsExpectedHeadersAndQuery(t *testing.T) {
	server, _, requests := countingServer(t, http.StatusOK, nominatimPayload)
	geocoder := newTestGeocoder(t, server)

	if _, _, err := geocoder.Geocode(context.Background(), "Rua das Acácias, 10", "Brasília", "DF"); err != nil {
		t.Fatalf("Geocode() error = %v, want nil", err)
	}

	if len(*requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(*requests))
	}
	req := (*requests)[0]

	if got := req.Header.Get("User-Agent"); got != testUserAgent {
		t.Errorf("User-Agent = %q, want %q", got, testUserAgent)
	}
	if got := req.Header.Get("Accept-Language"); got != "pt-BR" {
		t.Errorf("Accept-Language = %q, want %q", got, "pt-BR")
	}

	query := req.URL.Query()
	wantParams := map[string]string{
		"format":         "jsonv2",
		"limit":          "1",
		"countrycodes":   "br",
		"addressdetails": "0",
		"q":              "Rua das Acácias, 10, Brasília, DF",
	}
	for name, want := range wantParams {
		if got := query.Get(name); got != want {
			t.Errorf("query %s = %q, want %q", name, got, want)
		}
	}
}

func TestGeocodeOmitsEmptyCityAndState(t *testing.T) {
	server, _, requests := countingServer(t, http.StatusOK, nominatimPayload)
	geocoder := newTestGeocoder(t, server)

	if _, _, err := geocoder.Geocode(context.Background(), "Rua das Acácias, 10", "", "  "); err != nil {
		t.Fatalf("Geocode() error = %v, want nil", err)
	}

	if got := (*requests)[0].URL.Query().Get("q"); got != "Rua das Acácias, 10" {
		t.Errorf("query q = %q, want %q", got, "Rua das Acácias, 10")
	}
}

func TestGeocodeEmptyResultIsAddressNotFound(t *testing.T) {
	server, _, _ := countingServer(t, http.StatusOK, `[]`)
	geocoder := newTestGeocoder(t, server)

	_, _, err := geocoder.Geocode(context.Background(), "Endereço inexistente", "", "")
	if !errors.Is(err, ErrAddressNotFound) {
		t.Fatalf("Geocode() error = %v, want ErrAddressNotFound", err)
	}
}

func TestGeocodeEmptyAddressMakesNoRequest(t *testing.T) {
	for _, address := range []string{"", "   ", "\t\n"} {
		t.Run(fmt.Sprintf("%q", address), func(t *testing.T) {
			server, count, _ := countingServer(t, http.StatusOK, nominatimPayload)
			geocoder := newTestGeocoder(t, server)

			_, _, err := geocoder.Geocode(context.Background(), address, "", "")
			if !errors.Is(err, ErrEmptyAddress) {
				t.Fatalf("Geocode() error = %v, want ErrEmptyAddress", err)
			}
			if got := count.Load(); got != 0 {
				t.Errorf("requests = %d, want 0", got)
			}
		})
	}
}

func TestGeocodeWrapsErrorStatuses(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server, _, _ := countingServer(t, status, `{"error":"nope"}`)
			geocoder := newTestGeocoder(t, server)

			_, _, err := geocoder.Geocode(context.Background(), "Rua das Acácias, 10", "", "")
			if err == nil {
				t.Fatalf("Geocode() error = nil, want error")
			}
			// A distinção importa para a fila: "não deu para perguntar agora"
			// não pode virar "endereço inexistente" e gravar NULL para sempre.
			if errors.Is(err, ErrAddressNotFound) {
				t.Errorf("Geocode() error = %v, want an error that is not ErrAddressNotFound", err)
			}
			if !strings.Contains(err.Error(), "enrichment:") {
				t.Errorf("error %q does not carry the %q prefix", err.Error(), "enrichment:")
			}
			if !strings.Contains(err.Error(), fmt.Sprint(status)) {
				t.Errorf("error %q does not mention status %d", err.Error(), status)
			}
		})
	}
}

func TestGeocodeRejectsMalformedJSON(t *testing.T) {
	server, _, _ := countingServer(t, http.StatusOK, `{"lat": broken`)
	geocoder := newTestGeocoder(t, server)

	_, _, err := geocoder.Geocode(context.Background(), "Rua das Acácias, 10", "", "")
	if err == nil {
		t.Fatalf("Geocode() error = nil, want error")
	}
	if errors.Is(err, ErrAddressNotFound) {
		t.Errorf("Geocode() error = %v, want an error that is not ErrAddressNotFound", err)
	}
}

func TestGeocodeRejectsInvalidCoordinates(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"latitude fora de faixa", `[{"lat": "95.0", "lon": "-47.88"}]`},
		{"longitude fora de faixa", `[{"lat": "-15.79", "lon": "-200.5"}]`},
		{"latitude não numérica", `[{"lat": "abc", "lon": "-47.88"}]`},
		{"longitude não numérica", `[{"lat": "-15.79", "lon": "abc"}]`},
		{"NaN", `[{"lat": "NaN", "lon": "-47.88"}]`},
		{"infinito", `[{"lat": "-15.79", "lon": "Inf"}]`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, _, _ := countingServer(t, http.StatusOK, tt.body)
			geocoder := newTestGeocoder(t, server)

			_, _, err := geocoder.Geocode(context.Background(), "Rua das Acácias, 10", "", "")
			if !errors.Is(err, ErrInvalidCoordinates) {
				t.Fatalf("Geocode() error = %v, want ErrInvalidCoordinates", err)
			}
		})
	}
}

func TestGeocodeCancelledContextIsNotAddressNotFound(t *testing.T) {
	server, _, _ := countingServer(t, http.StatusOK, nominatimPayload)
	geocoder := newTestGeocoder(t, server)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := geocoder.Geocode(ctx, "Rua das Acácias, 10", "", "")
	if err == nil {
		t.Fatalf("Geocode() error = nil, want error")
	}
	if errors.Is(err, ErrAddressNotFound) {
		t.Errorf("Geocode() error = %v, want an error that is not ErrAddressNotFound", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Geocode() error = %v, want it to wrap context.Canceled", err)
	}
}

func TestGeocodeTimeoutIsNotAddressNotFound(t *testing.T) {
	// Um Doer que devolve o erro de deadline direto, sem servidor: o teste
	// verifica a classificação do erro, não o relógio do http.Client (esperar
	// os 10s reais de nominatimTimeout deixaria a suíte inviável).
	failing := doerFunc(func(req *http.Request) (*http.Response, error) {
		return nil, context.DeadlineExceeded
	})

	geocoder, err := newNominatimGeocoder(testUserAgent, "https://example.test/search", failing, 0)
	if err != nil {
		t.Fatalf("newNominatimGeocoder() error = %v, want nil", err)
	}

	_, _, err = geocoder.Geocode(context.Background(), "Rua das Acácias, 10", "", "")
	if err == nil {
		t.Fatalf("Geocode() error = nil, want error")
	}
	if errors.Is(err, ErrAddressNotFound) {
		t.Errorf("Geocode() error = %v, want an error that is not ErrAddressNotFound", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Geocode() error = %v, want it to wrap context.DeadlineExceeded", err)
	}
}

func TestGeocodeCachesSuccessAcrossSpellings(t *testing.T) {
	server, count, _ := countingServer(t, http.StatusOK, nominatimPayload)
	geocoder := newTestGeocoder(t, server)

	// Mesma chave normalizada: caixa e acento diferentes, endereço igual.
	spellings := []string{"Rua das Acácias, 10", "RUA DAS ACACIAS, 10", "  rua  das acacias, 10  "}
	for _, address := range spellings {
		lat, lng, err := geocoder.Geocode(context.Background(), address, "", "")
		if err != nil {
			t.Fatalf("Geocode(%q) error = %v, want nil", address, err)
		}
		if lat != -15.7942287 || lng != -47.8821658 {
			t.Errorf("Geocode(%q) = (%v, %v), want (%v, %v)", address, lat, lng, -15.7942287, -47.8821658)
		}
	}

	if got := count.Load(); got != 1 {
		t.Errorf("requests = %d, want 1 (the cache should have served the rest)", got)
	}
}

func TestGeocodeCachesAddressNotFound(t *testing.T) {
	server, count, _ := countingServer(t, http.StatusOK, `[]`)
	geocoder := newTestGeocoder(t, server)

	for i := 0; i < 3; i++ {
		_, _, err := geocoder.Geocode(context.Background(), "Endereço inexistente", "", "")
		if !errors.Is(err, ErrAddressNotFound) {
			t.Fatalf("Geocode() #%d error = %v, want ErrAddressNotFound", i, err)
		}
	}

	if got := count.Load(); got != 1 {
		t.Errorf("requests = %d, want 1 (negative caching)", got)
	}
}

func TestGeocodeDoesNotCacheTransientFailures(t *testing.T) {
	// Falha na primeira chamada e responde na segunda: se o erro entrasse no
	// cache, a segunda chamada devolveria o erro sem nem tentar.
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(nominatimPayload))
	}))
	t.Cleanup(server.Close)

	geocoder := newTestGeocoder(t, server)

	if _, _, err := geocoder.Geocode(context.Background(), "Rua das Acácias, 10", "", ""); err == nil {
		t.Fatalf("first Geocode() error = nil, want error")
	}

	lat, _, err := geocoder.Geocode(context.Background(), "Rua das Acácias, 10", "", "")
	if err != nil {
		t.Fatalf("second Geocode() error = %v, want nil (transient errors must not be cached)", err)
	}
	if lat != -15.7942287 {
		t.Errorf("lat = %v, want %v", lat, -15.7942287)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("requests = %d, want 2", got)
	}
}

func TestGeocodeIsSafeForConcurrentUse(t *testing.T) {
	server, count, _ := countingServer(t, http.StatusOK, nominatimPayload)
	geocoder := newTestGeocoder(t, server)

	const goroutines = 16
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			if _, _, err := geocoder.Geocode(context.Background(), "Rua das Acácias, 10", "", ""); err != nil {
				t.Errorf("Geocode() error = %v, want nil", err)
			}
		}()
	}
	wg.Wait()

	// Não há singleflight: goroutines que chegam antes do primeiro store podem
	// disparar requisições próprias. O que o cache garante é que o número fique
	// muito abaixo de uma requisição por chamada — a asserção forte de "uma só"
	// vive no teste sequencial.
	if got := count.Load(); got < 1 || got > goroutines {
		t.Errorf("requests = %d, want between 1 and %d", got, goroutines)
	}
}

func TestGeocodeRespectsRateLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("teste baseado em tempo real")
	}

	server, _, _ := countingServer(t, http.StatusOK, nominatimPayload)
	geocoder, err := newNominatimGeocoder(testUserAgent, server.URL+"/search", server.Client(), 80)
	if err != nil {
		t.Fatalf("newNominatimGeocoder() error = %v, want nil", err)
	}

	start := time.Now()
	for _, address := range []string{"Rua A, 1", "Rua B, 2", "Rua C, 3"} {
		if _, _, err := geocoder.Geocode(context.Background(), address, "", ""); err != nil {
			t.Fatalf("Geocode(%q) error = %v, want nil", address, err)
		}
	}

	// Três requisições espaçadas de 80ms levam pelo menos ~160ms. A margem é
	// folgada de propósito: os testes de ratelimit são baseados em tempo real.
	if elapsed := time.Since(start); elapsed < 120*time.Millisecond {
		t.Errorf("3 requests took %v, want at least 120ms of spacing", elapsed)
	}
}

func TestNewNominatimGeocoderRequiresUserAgent(t *testing.T) {
	for _, userAgent := range []string{"", "   "} {
		_, err := NewNominatimGeocoder(userAgent, 0)
		if !errors.Is(err, ErrMissingUserAgent) {
			t.Errorf("NewNominatimGeocoder(%q) error = %v, want ErrMissingUserAgent", userAgent, err)
		}
	}
}

func TestNewGeocoderResolvesProviders(t *testing.T) {
	t.Run("nominatim", func(t *testing.T) {
		geocoder, err := NewGeocoder(ProviderNominatim, "", testUserAgent, DefaultGeocodingRateLimitMS)
		if err != nil {
			t.Fatalf("NewGeocoder() error = %v, want nil", err)
		}
		if _, ok := geocoder.(*nominatimGeocoder); !ok {
			t.Errorf("NewGeocoder() = %T, want *nominatimGeocoder", geocoder)
		}
	})

	t.Run("nominatim com espaços e maiúsculas", func(t *testing.T) {
		if _, err := NewGeocoder("  NOMINATIM  ", "", testUserAgent, 0); err != nil {
			t.Fatalf("NewGeocoder() error = %v, want nil", err)
		}
	})

	t.Run("googlemaps ainda não implementado", func(t *testing.T) {
		geocoder, err := NewGeocoder(ProviderGoogleMaps, "secret-key", testUserAgent, 0)
		if err != nil {
			t.Fatalf("NewGeocoder() error = %v, want nil", err)
		}

		_, _, err = geocoder.Geocode(context.Background(), "Rua das Acácias, 10", "", "")
		if !errors.Is(err, ErrProviderNotSupported) {
			t.Fatalf("Geocode() error = %v, want ErrProviderNotSupported", err)
		}
		// A chave nunca pode vazar em mensagem de erro nem em log.
		if strings.Contains(err.Error(), "secret-key") {
			t.Errorf("error %q leaks the API key", err.Error())
		}
	})

	t.Run("provider desconhecido", func(t *testing.T) {
		_, err := NewGeocoder("mapquest", "", testUserAgent, 0)
		if !errors.Is(err, ErrProviderNotSupported) {
			t.Fatalf("NewGeocoder() error = %v, want ErrProviderNotSupported", err)
		}
	})
}

func TestAddressCacheKeyDistinguishesFields(t *testing.T) {
	// Sem separador, ("a b", "") e ("a", "b") colidiriam.
	if addressCacheKey("a b", "", "") == addressCacheKey("a", "b", "") {
		t.Error("addressCacheKey collides between (address, city) boundaries")
	}
	if addressCacheKey("Rua das Acácias", "Brasília", "DF") != addressCacheKey("RUA DAS ACACIAS", "brasilia", "df") {
		t.Error("addressCacheKey should ignore case and accents")
	}
}
