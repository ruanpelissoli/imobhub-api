// Package httpclient centraliza as requisições HTTP do scraper, garantindo que
// todas carreguem o mesmo User-Agent e o mesmo timeout.
package httpclient

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// DefaultTimeout limita cada requisição. Páginas de portais imobiliários podem
// demorar; 30s é generoso o bastante sem travar o worker indefinidamente.
const DefaultTimeout = 30 * time.Second

// Client é um wrapper fino sobre *http.Client que injeta o User-Agent
// configurado em todas as requisições.
type Client struct {
	http      *http.Client
	userAgent string
}

// New cria um Client com o User-Agent informado e o timeout padrão.
func New(userAgent string) *Client {
	return &Client{
		http:      &http.Client{Timeout: DefaultTimeout},
		userAgent: userAgent,
	}
}

// UserAgent retorna o User-Agent usado nas requisições. O pacote robots precisa
// dele para avaliar as regras corretas do robots.txt.
func (c *Client) UserAgent() string {
	return c.userAgent
}

// Get executa um GET com o User-Agent configurado. O corpo da resposta é de
// responsabilidade do chamador (deve fechá-lo).
func (c *Client) Get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("httpclient: could not build request for %q: %w", url, err)
	}
	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("httpclient: GET %q failed: %w", url, err)
	}
	return resp, nil
}

// ParseHTML converte um corpo de resposta em um documento navegável. Fica neste
// pacote para que o parsing use sempre a mesma biblioteca.
func ParseHTML(r io.Reader) (*goquery.Document, error) {
	doc, err := goquery.NewDocumentFromReader(r)
	if err != nil {
		return nil, fmt.Errorf("httpclient: could not parse HTML: %w", err)
	}
	return doc, nil
}
