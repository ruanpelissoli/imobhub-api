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

// Property é o registro canônico de um imóvel: a versão consolidada e já
// normalizada do que vários anúncios (RawListing) dizem sobre o mesmo imóvel
// físico. A relação é 1:N — cada linha de listings aponta para no máximo uma
// property, via listings.property_id.
//
// Os campos consolidados são **ponteiros** porque a coluna é nullable e a
// ausência é informação: nil significa "ainda não consolidado/não informado",
// enquanto 0 significaria "zero quartos" e "" significaria "endereço vazio".
// Trocar por valores não-ponteiro apagaria essa distinção — e pgx nem escaneia
// NULL para string/int.
//
// Não há chave de deduplicação aqui (nenhum UNIQUE além da PK): qual combinação
// de endereço/geo/atributos identifica o mesmo imóvel é decisão da task de
// deduplicação.
type Property struct {
	// ID é o UUID gerado pelo PostgreSQL (gen_random_uuid()). Fica vazio até o
	// CreateProperty retornar; é string (e não um tipo UUID) para não trazer
	// dependência nova — o cast para uuid é feito no SQL.
	ID string
	// CanonicalAddress é o endereço consolidado a partir dos address_raw dos
	// anúncios. TEXT livre: os portais brasileiros variam demais.
	CanonicalAddress *string
	Neighborhood     *string
	City             *string
	State            *string
	// Lat e Lng são o resultado da geocodificação. nil enquanto o imóvel não
	// foi geocodificado — nem todo endereço bruto é resolvível. Propriedades
	// com nil aqui são ignoradas por FindPropertiesByCoordinates.
	Lat *float64
	Lng *float64
	// Atributos físicos consolidados.
	BedroomCount  *int
	BathroomCount *int
	ParkingSpots  *int
	// AreaSqm é a área em m². float porque os portais publicam valores
	// fracionários (72,5 m²).
	AreaSqm *float64
	// Amenities vai para a coluna amenities (TEXT[]). nil é gravado como array
	// vazio, nunca como NULL — mesma regra de RawListing.ImageURLs. Na leitura,
	// NULL e vazio são tratados igual.
	Amenities   []string
	Description *string
	// Photos segue a mesma regra de Amenities.
	Photos []string
	// TransactionType ("venda"/"aluguel") e PropertyType ("apartamento"/...)
	// são TEXT livre de propósito: o vocabulário ainda não foi fechado e este
	// pacote **não** valida os valores.
	TransactionType *string
	PropertyType    *string
	// ActiveListingCount é o contador denormalizado de anúncios apontando para
	// este imóvel. É preenchido na leitura e **ignorado** na escrita: quem o
	// mantém são LinkListingToProperty/UnlinkListingFromProperty, dentro da
	// mesma transação que altera listings.property_id. Deixar o caller gravá-lo
	// permitiria dessincronizar o contador com um Update descuidado.
	ActiveListingCount int
	CreatedAt          time.Time
	UpdatedAt          time.Time
}
