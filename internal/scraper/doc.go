// Package scraper orquestra a coleta das páginas das fontes: consulta ao
// robots.txt, rate limiting, busca do HTML e extração dos dados.
//
// A renderização headless que morava aqui (RenderHTML) foi movida para
// httpclient.FetchHeadless, ao lado de httpclient.FetchStatic: manter os dois
// modos de busca no mesmo pacote evita duas implementações de chromedp e deixa
// a escolha "estático ou headless" numa decisão só.
//
// Ainda não implementado: a orquestração chega nas tasks seguintes.
package scraper
