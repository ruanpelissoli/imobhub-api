package httpclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// HeadlessTimeout é o orçamento total de uma renderização: navegação, espera
// pelo `networkIdle` e captura do HTML. Sem esse teto, uma página que nunca
// termina de carregar (polling infinito, script travado) seguraria o worker.
const HeadlessTimeout = 20 * time.Second

// captureReserve é a fatia final do orçamento guardada para capturar o HTML.
// A espera pelo `networkIdle` termina em `HeadlessTimeout - captureReserve`
// para que sempre sobre tempo de rodar o OuterHTML dentro do mesmo contexto —
// caso contrário uma página que não fica ociosa consumiria o orçamento inteiro
// na espera e a captura falharia com deadline exceeded.
const captureReserve = 3 * time.Second

// lifecycleNetworkIdle é o nome do evento de ciclo de vida que o Chrome emite
// quando a página fica ~500ms sem requisições de rede pendentes. É o sinal mais
// próximo de "o JavaScript terminou de montar o conteúdo".
const lifecycleNetworkIdle = "networkIdle"

// headlessFlags são as flags passadas ao Chrome além das
// chromedp.DefaultExecAllocatorOptions. São declaradas como dado (e não inline)
// para ficarem verificáveis em teste — em ambiente de container a ausência de
// qualquer uma delas troca um erro de rede por uma falha na subida do browser,
// que é bem mais difícil de diagnosticar.
var headlessFlags = []struct {
	Name  string
	Value any
}{
	// O scraper roda em container como root; sem isso o Chrome se recusa a subir.
	{"no-sandbox", true},
	{"disable-setuid-sandbox", true},
	// O /dev/shm padrão do Docker (64MB) estoura em páginas pesadas e o Chrome
	// morre no meio da renderização.
	{"disable-dev-shm-usage", true},
	// Redundantes com os defaults do chromedp, mas explícitos: a lista de
	// defaults é da biblioteca e pode mudar entre versões.
	{"headless", true},
	{"disable-gpu", true},
}

// FetchHeadless abre a URL em um Chrome headless e devolve o HTML depois que os
// scripts da página rodaram. Use apenas quando FetchStatic não traz o conteúdo:
// subir um browser é ordens de magnitude mais caro (memória, CPU, latência) que
// um GET.
//
// Cada chamada cria e destrói o próprio processo de browser. É proposital para
// o MVP: nada de estado (cookies, cache, service workers) vaza de um site para
// outro, e headless é usado só na minoria das fontes que exigem JavaScript.
//
// A espera é pelo evento `networkIdle` da navegação principal, limitada ao
// orçamento de HeadlessTimeout; se ele não chegar a tempo, o HTML disponível é
// capturado mesmo assim (ver CLAUDE.md). Erros de navegação e estouro do
// orçamento viram erro.
//
// Requer Chrome/Chromium no PATH; a ausência aparece como erro na alocação do
// browser, não como erro de rede.
func FetchHeadless(ctx context.Context, url string, userAgent string) (html string, err error) {
	if strings.TrimSpace(url) == "" {
		return "", errors.New("httpclient: headless fetch needs a non-empty URL")
	}

	opts := make([]chromedp.ExecAllocatorOption, 0, len(chromedp.DefaultExecAllocatorOptions)+len(headlessFlags)+1)
	opts = append(opts, chromedp.DefaultExecAllocatorOptions[:]...)
	for _, f := range headlessFlags {
		opts = append(opts, chromedp.Flag(f.Name, f.Value))
	}
	// O User-Agent precisa ser o mesmo do FetchStatic e o mesmo avaliado no
	// robots.txt — identificação coerente do bot em todas as requisições.
	opts = append(opts, chromedp.UserAgent(userAgent))

	allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx, opts...)
	defer cancelAlloc()

	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	defer cancelBrowser()

	// O timeout fica no contexto do browser (e não no do alocador) para que o
	// estouro cancele apenas esta renderização; os cancels em ordem inversa
	// derrubam o processo do Chrome ao final — sem eles sobram browsers órfãos.
	runCtx, cancelTimeout := context.WithTimeout(browserCtx, HeadlessTimeout)
	defer cancelTimeout()

	if runErr := chromedp.Run(runCtx,
		navigateAndWaitIdle(url),
		chromedp.OuterHTML("html", &html),
	); runErr != nil {
		if errors.Is(runErr, context.DeadlineExceeded) {
			return "", fmt.Errorf("httpclient: headless fetch of %q timed out after %s: %w", url, HeadlessTimeout, runErr)
		}
		return "", fmt.Errorf("httpclient: headless fetch of %q failed: %w", url, runErr)
	}

	return html, nil
}

// navigateAndWaitIdle navega até a URL e bloqueia até o `networkIdle` da
// navegação principal, o fim do orçamento de espera ou o cancelamento do
// contexto — o que vier primeiro.
//
// Navegação e espera ficam na mesma ação porque o listener precisa estar
// registrado *antes* do Navigate: em páginas leves o `networkIdle` pode chegar
// junto com o load, e um listener registrado depois esperaria o orçamento
// inteiro por um evento que já passou.
func navigateAndWaitIdle(url string) chromedp.ActionFunc {
	return func(ctx context.Context) error {
		var (
			mu        sync.Mutex
			navigated bool
			notified  bool
			idle      = make(chan struct{})
		)

		listenCtx, cancelListen := context.WithCancel(ctx)
		defer cancelListen()

		// Os callbacks de ListenTarget rodam sequencialmente na goroutine de
		// eventos do target, então o par frameNavigated → networkIdle chega em
		// ordem. O mutex protege apenas contra a leitura feita aqui.
		chromedp.ListenTarget(listenCtx, func(ev any) {
			switch e := ev.(type) {
			case *page.EventFrameNavigated:
				// Só o frame principal: iframes de anúncio navegam sozinhos e
				// não dizem nada sobre a página que interessa.
				if e.Frame != nil && e.Frame.ParentID == "" {
					mu.Lock()
					navigated = true
					mu.Unlock()
				}
			case *page.EventLifecycleEvent:
				if e.Name != lifecycleNetworkIdle {
					return
				}
				mu.Lock()
				defer mu.Unlock()
				// Ignora o `networkIdle` do about:blank inicial, emitido antes
				// da navegação: aceitá-lo devolveria uma página em branco.
				if navigated && !notified {
					notified = true
					close(idle)
				}
			}
		})

		// Habilita os eventos de ciclo de vida; sem isso o Chrome nunca emite
		// `networkIdle`.
		if err := page.SetLifecycleEventsEnabled(true).Do(ctx); err != nil {
			return fmt.Errorf("could not enable lifecycle events: %w", err)
		}

		if err := chromedp.Navigate(url).Do(ctx); err != nil {
			return fmt.Errorf("could not navigate to %q: %w", url, err)
		}

		budget, bounded := idleBudget(ctx)
		if bounded && budget <= 0 {
			// A navegação consumiu o orçamento: captura o que já existe em vez
			// de esperar por um evento que não cabe mais no tempo restante.
			return nil
		}

		waitCtx := ctx
		var cancelWait context.CancelFunc = func() {}
		if bounded {
			waitCtx, cancelWait = context.WithTimeout(ctx, budget)
		}
		defer cancelWait()

		select {
		case <-idle:
			return nil
		case <-waitCtx.Done():
			if ctx.Err() != nil {
				// O contexto externo caiu (timeout total ou cancelamento): erro
				// de verdade, tratado por FetchHeadless.
				return ctx.Err()
			}
			// Só o orçamento da espera acabou. Muitos portais nunca ficam
			// ociosos (websocket, polling de analytics), então capturar o DOM
			// atual é melhor que falhar sempre nesses sites.
			slog.Warn("httpclient: networkIdle not reached, capturing current DOM",
				"url", url, "waited", budget)
			return nil
		}
	}
}

// idleBudget devolve quanto tempo ainda pode ser gasto esperando pelo
// `networkIdle`, guardando captureReserve para a captura do HTML. O segundo
// retorno é falso quando o contexto não tem prazo — aí a espera só termina no
// evento ou no cancelamento.
func idleBudget(ctx context.Context) (time.Duration, bool) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return 0, false
	}
	return time.Until(deadline) - captureReserve, true
}
