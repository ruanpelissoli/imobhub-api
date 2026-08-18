package httpclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"golang.org/x/net/html/charset"
)

// MaxRedirects limita quantos saltos uma requisição pode seguir. Redirecionar
// indefinidamente é sintoma de loop de sessão/geolocalização em portais
// imobiliários; 10 é o mesmo teto do `net/http` e cobre casos legítimos
// (http→https, com/sem www, URL canônica).
const MaxRedirects = 10

// StatusError descreve uma resposta HTTP 4xx/5xx. É um tipo próprio porque a
// reação correta depende do código: 404 costuma significar anúncio removido
// (limpar do banco), enquanto 403/429 indicam bloqueio (recuar e tentar mais
// tarde). Use `errors.As` para inspecionar.
type StatusError struct {
	StatusCode int
	URL        string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("httpclient: GET %q returned HTTP %d %s", e.URL, e.StatusCode, http.StatusText(e.StatusCode))
}

// FetchStatic busca o HTML de uma página que não depende de JavaScript e o
// devolve já decodificado em UTF-8, junto da URL final após redirecionamentos.
//
// Cada chamada usa um `http.Client` próprio (sem cookie jar): o scraper visita
// muitos domínios diferentes e um cliente compartilhado vazaria estado de um
// site para outro. O custo é desprezível — o `Transport` padrão é global e
// mantém o pool de conexões entre chamadas.
//
// Respostas 4xx e 5xx viram erro (`*StatusError`); o corpo delas é descartado.
func FetchStatic(ctx context.Context, url string, userAgent string) (html string, finalURL string, err error) {
	client := &http.Client{
		Timeout: DefaultTimeout,
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= MaxRedirects {
				return fmt.Errorf("stopped after %d redirects", MaxRedirects)
			}
			return nil
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", fmt.Errorf("httpclient: could not build request for %q: %w", url, err)
	}
	// Apenas o User-Agent: qualquer header extra (Accept-Language, DNT, ...)
	// aumenta a superfície de fingerprinting sem melhorar a coleta. Não setar
	// Accept-Encoding é intencional — assim o `net/http` anuncia gzip sozinho e
	// descomprime a resposta de forma transparente.
	req.Header.Set("User-Agent", userAgent)

	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("httpclient: GET %q failed: %w", url, err)
	}
	defer resp.Body.Close()

	// A URL final vem de resp.Request, que é a última requisição da cadeia de
	// redirecionamentos — resolvida e absoluta, ao contrário do header Location.
	finalURL = resp.Request.URL.String()

	if resp.StatusCode >= http.StatusBadRequest {
		// Drena um pedaço do corpo para que a conexão volte ao pool; corpos de
		// erro podem ser páginas inteiras, por isso o teto.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return "", finalURL, &StatusError{StatusCode: resp.StatusCode, URL: finalURL}
	}

	// charset.NewReader detecta o encoding pelo BOM, pelo Content-Type e pela
	// meta tag do próprio HTML, convertendo para UTF-8. Sites brasileiros mais
	// antigos ainda servem ISO-8859-1, e ler esses bytes como UTF-8 corromperia
	// acentos justamente nos campos que o extrator precisa (bairro, descrição).
	reader, err := charset.NewReader(resp.Body, resp.Header.Get("Content-Type"))
	if errors.Is(err, io.EOF) {
		// Corpo vazio (204, ou um 200 sem conteúdo): a detecção de charset não
		// tem bytes para inspecionar. Página vazia não é falha de transporte —
		// quem extrai decide se isso invalida a coleta.
		return "", finalURL, nil
	}
	if err != nil {
		return "", finalURL, fmt.Errorf("httpclient: could not detect charset of %q: %w", finalURL, err)
	}

	body, err := io.ReadAll(reader)
	if err != nil {
		return "", finalURL, fmt.Errorf("httpclient: could not read body of %q: %w", finalURL, err)
	}

	return string(body), finalURL, nil
}

// IsStatus informa se err é um erro de status HTTP com o código informado.
// Açúcar sobre errors.As para os call sites que só querem tratar 404.
func IsStatus(err error, code int) bool {
	var statusErr *StatusError
	return errors.As(err, &statusErr) && statusErr.StatusCode == code
}
