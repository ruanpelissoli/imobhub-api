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

// Listing é uma linha de listings na forma que a consolidação do imóvel
// canônico consome: só as colunas que entram no merge, mais o que identifica o
// anúncio nos logs.
//
// É separado de RawListing de propósito. RawListing é o modelo de **escrita** da
// coleta (identidade `(SourceDomain, ListingURL)`, sem `id`, com `ExtraData`);
// este é o modelo de **leitura** do enriquecimento, e precisa do `id` — é ele
// que dá a ordem determinística de que o merge depende. Fundir os dois obrigaria
// a coleta a carregar campos que ela não preenche.
type Listing struct {
	// ID é listings.id, a chave da ordenação estável usada pelo merge.
	ID int64
	// SourceDomain e ListingURL identificam o anúncio na origem; entram apenas
	// nos logs e nas mensagens de erro.
	SourceDomain string
	ListingURL   string
	// DescriptionRaw é o texto cru do anúncio. NULL na coluna chega aqui como
	// "", mesma convenção dos campos "_raw" de RawListing.
	DescriptionRaw string
	// BedroomCount é o valor já enriquecido. nil quando o anúncio não informa —
	// 0 significaria "sem quarto".
	BedroomCount *int
	// Amenities e ImageURLs vêm de colunas TEXT[] nullable; na leitura NULL e
	// vazio são a mesma coisa (ver normalizeTextArray).
	Amenities []string
	ImageURLs []string
}

// PendingListing é uma linha de listings na forma que a fila de enriquecimento
// consome: os campos "_raw" que alimentam os enrichers mais o estado atual do
// enriquecimento, que é o que permite montar um grouping.Listing sem uma segunda
// leitura.
//
// É um modelo **novo**, e não uma extensão de Listing, de propósito. Listing é o
// modelo de leitura do merge, amarrado à ordem de colunas de
// selectListingsByPropertyIDSQL; ampliá-lo obrigaria scanListing a preencher
// colunas que aquela query não seleciona, ou a manter dois scanners para o mesmo
// tipo — e o primeiro campo esquecido viraria um canônico consolidado com NULL.
type PendingListing struct {
	// ID é listings.id, a chave da ordenação e do cursor da fila.
	ID int64
	// SourceDomain e ListingURL identificam o anúncio na origem; entram nos
	// logs e nas mensagens de erro de cada anúncio processado.
	SourceDomain string
	ListingURL   string
	// Campos "_raw": NULL na coluna chega aqui como "", mesma convenção de
	// RawListing.
	TitleRaw       string
	DescriptionRaw string
	AddressRaw     string
	AreaRaw        string
	BedroomsRaw    string
	// NormalizedNeighborhood é o resultado do passe anterior, quando houve.
	// String vazia é o que o normalizador devolve para "não reconhecido"; a
	// coluna guarda NULL nesse caso (ver UpdateListingEnrichment).
	NormalizedNeighborhood string
	// BedroomCount é ponteiro porque a ausência é informação: nil é "o anúncio
	// não informa", 0 seria "sem quarto separado".
	BedroomCount *int
	// Amenities e ImageURLs vêm de colunas TEXT[] nullable; NULL e vazio são a
	// mesma coisa na leitura (ver normalizeTextArray).
	Amenities []string
	ImageURLs []string
	// Lat e Lng são o resultado da geocodificação anterior. nil = nunca
	// geocodificado ou endereço irresolvível.
	Lat *float64
	Lng *float64
	// PropertyID é o vínculo já existente. Quando presente, o agrupamento sai
	// cedo sem chamar a IA — é a saída barata de grouping.GroupListing.
	PropertyID *string
}

// PropertyListing é uma linha de listings na forma que a tela de comparação
// entre portais consome: o que identifica a fonte, o preço como ela publicou e
// quando o anúncio foi visto pela última vez.
//
// É um modelo **novo** pelo mesmo motivo de PendingListing: Listing é o modelo
// de leitura do merge, amarrado à ordem de colunas de
// selectListingsByPropertyIDSQL, e ampliá-lo obrigaria scanListing a preencher
// colunas que aquela query não seleciona.
type PropertyListing struct {
	// ID é listings.id, também a chave da ordenação estável da resposta.
	ID int64
	// SourceDomain é o host normalizado do portal ("www.exemplo.com.br"). É a
	// única identidade de fonte que existe no schema — não há nome comercial da
	// imobiliária em lugar nenhum.
	SourceDomain string
	// ListingURL é a URL absoluta do anúncio na origem.
	ListingURL string
	// PriceRaw é o texto cru publicado pelo portal ("R$ 450.000", "450 mil").
	// NULL na coluna chega aqui como "", mesma convenção dos campos "_raw".
	// **Não é numérico**: a coluna normalizada nasce em outra migration.
	PriceRaw string
	// LastSeenAt é listings.last_seen_at (NOT NULL): a última coleta em que o
	// anúncio ainda estava no site.
	LastSeenAt time.Time
}

// PropertyDetail é o imóvel canônico com todos os anúncios que apontam para
// ele. É o retorno de GetPropertyWithListings e existe para que o detalhe seja
// lido em duas queries fixas, sem N+1 e sem repetir amenities/photos por linha.
type PropertyDetail struct {
	Property Property
	// Listings é sempre não-nil (imóvel sem anúncios devolve slice vazia) e vem
	// ordenado por id.
	Listings []PropertyListing
}

// ListingEnrichment é o payload de escrita das colunas de enriquecimento de um
// anúncio. Os campos opcionais são ponteiros pela mesma razão de Property: nil
// vira NULL na coluna, e NULL é a forma honesta de dizer "não reconhecido".
//
// Não carrega enriched_at: o carimbo é uma operação separada
// (MarkListingEnriched), que só acontece depois do agrupamento e da consolidação.
type ListingEnrichment struct {
	// ID é listings.id do anúncio a atualizar. Obrigatório.
	ID int64
	// NormalizedNeighborhood é nil quando o normalizador não reconheceu nada —
	// nunca string vazia (contrato de NeighborhoodNormalizer.Normalize).
	NormalizedNeighborhood *string
	BedroomCount           *int
	// Amenities nil é gravado como array vazio, nunca NULL, seguindo a mesma
	// regra de RawListing.ImageURLs.
	Amenities []string
	// Lat e Lng são gravados juntos: um par pela metade não é geocodificação.
	Lat *float64
	Lng *float64
}

// PropertySearchParams são os filtros da busca paginada de imóveis
// (SearchProperties). É o contrato que o futuro handler HTTP preenche a partir
// da query string.
//
// Ao contrário de Property, os campos de filtro são **valores, não ponteiros**:
// aqui o zero value (`""`, `0`, slice vazia/nil) significa "filtro não
// aplicado", e não "não sei". Um ponteiro só faria sentido se "filtrar por
// exatamente zero quartos" fosse um pedido possível — não é, os filtros
// numéricos são pisos (`>=`).
//
// **Não há MinPrice/MaxPrice**, e isso é decisão registrada em
// migrations/CLAUDE.md: `properties` não tem coluna de preço, e o único dado de
// preço no schema é `listings.price_raw TEXT` (texto bruto: "R$ 450.000", "450
// mil"), sobre o qual nenhum filtro de faixa é possível. Os dois campos nascem
// junto da migration que criar a coluna normalizada em `properties`.
type PropertySearchParams struct {
	// TransactionType e PropertyType são comparados por igualdade exata, com o
	// mesmo vocabulário livre de Property (o pacote não valida os valores).
	TransactionType string
	PropertyType    string
	// City e Neighborhood são case- e acento-sensitive (`=`), de propósito:
	// ILIKE/unaccent inviabilizariam o btree idx_properties_search_filters, e a
	// normalização de bairro já acontece antes, no enriquecimento.
	City         string
	Neighborhood string
	// Pisos: geram `coluna >= $N`. Imóveis com a coluna NULL (ainda não
	// consolidados) não atendem o filtro — ver SearchProperties.
	MinBedrooms     int
	MinBathrooms    int
	MinParkingSpots int
	MinArea         float64
	// Amenities exige **todas** as comodidades pedidas (contenção `@>` sobre a
	// coluna TEXT[]).
	Amenities []string
	// Page começa em 1 e PageSize tem default 20 e teto 50; valores fora da
	// faixa são normalizados, nunca rejeitados.
	Page     int
	PageSize int
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
