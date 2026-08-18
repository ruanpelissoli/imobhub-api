package db

import "time"

// Modos de renderização aceitos por site_selectors.render_mode (espelham a
// CHECK constraint site_selectors_render_mode_check).
const (
	// RenderModeStatic indica que o HTML devolvido pelo GET já contém a
	// listagem.
	RenderModeStatic = "static"
	// RenderModeHeadless indica que a listagem só existe após a execução de
	// JavaScript, exigindo navegador.
	RenderModeHeadless = "headless"
)

// Estados aceitos por site_selectors.status (espelham a CHECK constraint
// site_selectors_status_check).
const (
	// StatusValid indica que os seletores extraíram itens na última execução.
	StatusValid = "valid"
	// StatusBroken indica que a extração falhou e a IA precisa redescobrir os
	// seletores.
	StatusBroken = "broken"
)

// SelectorFields são os seletores CSS de um domínio. É o conteúdo do campo
// JSONB site_selectors.selectors, e as tags `json` são o contrato gravado no
// banco: renomeá-las invalida todas as linhas já persistidas.
//
// Os valores são seletores CSS crus, exatamente como a IA os devolveu — podem
// ser compostos ("h2.title, h2 a") e são interpretados pelo pacote de extração,
// não por este pacote.
type SelectorFields struct {
	// ListingContainer delimita cada anúncio na página de listagem; os demais
	// seletores são aplicados relativamente a ele.
	ListingContainer string `json:"listing_container"`
	Title            string `json:"title"`
	Price            string `json:"price"`
	Address          string `json:"address"`
	Description      string `json:"description"`
	// Image seleciona o elemento da imagem (o atributo lido é decisão do
	// extrator, não do seletor).
	Image string `json:"image"`
	// ListingURL seleciona o link para a página do anúncio.
	ListingURL string `json:"listing_url"`
}

// SelectorConfig é uma linha de site_selectors: o "manual de leitura" de um
// domínio, consultado pelo scraper antes de raspar o site.
type SelectorConfig struct {
	// Domain é o host normalizado (sem esquema e sem barra final), ex.:
	// "www.exemplo.com.br". É a identidade da linha.
	Domain string
	// Selectors é o campo JSONB selectors já desserializado.
	Selectors SelectorFields
	// RenderMode é RenderModeStatic ou RenderModeHeadless. Vazio é aceito na
	// escrita e tratado como RenderModeStatic.
	RenderMode string
	// Status é StatusValid ou StatusBroken. Preenchido na leitura; ignorado em
	// UpsertSelectors, que sempre grava StatusValid.
	Status string
	// LastValidatedAt é nil enquanto os seletores nunca tiverem sido
	// exercitados numa coleta.
	LastValidatedAt *time.Time
}

// RawListing é um anúncio como saiu do HTML: os campos "_raw" guardam o texto
// extraído sem nenhuma normalização, para que a limpeza/parsing possa ser
// refeita depois sem raspar o site de novo.
//
// A identidade do anúncio é o par (SourceDomain, ListingURL) — é o alvo do
// ON CONFLICT em UpsertListings, e ambos são obrigatórios.
type RawListing struct {
	// SourceDomain usa o mesmo formato de SelectorConfig.Domain.
	SourceDomain string
	// ListingURL é a URL absoluta do anúncio.
	ListingURL     string
	TitleRaw       string
	PriceRaw       string
	AddressRaw     string
	DescriptionRaw string
	BedroomsRaw    string
	AreaRaw        string
	// ImageURLs vai para a coluna image_urls (TEXT[]). nil é gravado como
	// array vazio, nunca como NULL.
	ImageURLs []string
	// ExtraData são campos específicos do site que não cabem nas colunas
	// acima. nil é gravado como objeto vazio, nunca como NULL.
	ExtraData map[string]any
}
