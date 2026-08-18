// Package scraper orquestra a coleta das páginas das fontes. Neste momento
// expõe apenas o renderizador headless usado por páginas que dependem de
// JavaScript; a lógica de extração será adicionada nas tasks seguintes.
package scraper

import (
	"context"
	"fmt"
	"time"

	"github.com/chromedp/chromedp"
)

// DefaultRenderTimeout limita o tempo total de uma renderização headless.
// Sem ele, uma página que nunca termina de carregar seguraria o worker.
const DefaultRenderTimeout = 45 * time.Second

// RenderHTML abre a URL em um Chrome headless e devolve o HTML após a execução
// dos scripts da página. Use apenas quando a página não entrega o conteúdo no
// HTML inicial: subir um browser é ordens de magnitude mais caro que um GET.
//
// Requer um Chrome/Chromium instalado na máquina — a ausência dele aparece como
// erro na alocação do contexto.
func RenderHTML(ctx context.Context, url, userAgent string) (string, error) {
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx,
		append(chromedp.DefaultExecAllocatorOptions[:], chromedp.UserAgent(userAgent))...)
	defer cancelAlloc()

	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	defer cancelBrowser()

	timeoutCtx, cancelTimeout := context.WithTimeout(browserCtx, DefaultRenderTimeout)
	defer cancelTimeout()

	var html string
	err := chromedp.Run(timeoutCtx,
		chromedp.Navigate(url),
		chromedp.OuterHTML("html", &html),
	)
	if err != nil {
		return "", fmt.Errorf("scraper: could not render %q: %w", url, err)
	}
	return html, nil
}
