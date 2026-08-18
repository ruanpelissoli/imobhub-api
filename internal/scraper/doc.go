// Package scraper orquestra a coleta das páginas das fontes: consulta ao
// robots.txt, rate limiting, busca do HTML, extração dos dados e sincronização
// com o banco.
//
// A porta de entrada é RunPipeline (pipeline.go), que monta os módulos de
// coleta e processa as fontes do arquivo uma a uma. As etapas que ele costura
// são ExtractListings (extractor.go), que transforma o HTML de uma página de
// listagem em []db.RawListing usando os seletores CSS salvos, e SyncListings
// (syncer.go), que reconcilia esses anúncios com o banco — grava os vistos
// agora e apaga os que sumiram do site.
//
// A renderização headless que morava aqui (RenderHTML) foi movida para
// httpclient.FetchHeadless, ao lado de httpclient.FetchStatic: manter os dois
// modos de busca no mesmo pacote evita duas implementações de chromedp e deixa
// a escolha "estático ou headless" numa decisão só.
package scraper
