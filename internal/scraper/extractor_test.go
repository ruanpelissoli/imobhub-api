package scraper

import (
	"reflect"
	"strings"
	"testing"

	"github.com/imobhub/api/internal/db"
)

// defaultSelectors espelha o formato que a IA devolve para um site típico.
func defaultSelectors() db.SelectorConfig {
	return db.SelectorConfig{
		Domain: "www.exemplo.com.br",
		Selectors: db.SelectorFields{
			ListingContainer: ".card",
			Title:            ".titulo",
			Price:            ".preco",
			Address:          ".endereco",
			Description:      ".descricao",
			Image:            ".foto img",
			ListingURL:       "a.link",
		},
	}
}

const samplePage = `
<html><body>
  <div class="lista">
    <div class="card">
      <a class="link" href="/imoveis/1">ver</a>
      <h2 class="titulo">  Apartamento 2 quartos </h2>
      <span class="preco"> R$ 350.000 </span>
      <p class="endereco">Rua A, 100 - Centro</p>
      <p class="descricao">Ótima localização</p>
      <div class="foto"><img src="/img/1.jpg"></div>
    </div>
    <div class="card">
      <a class="link" href="https://cdn.exemplo.com.br/imoveis/2">ver</a>
      <h2 class="titulo">Casa 3 quartos</h2>
      <span class="preco">R$ 800.000</span>
      <p class="endereco">Rua B, 200</p>
      <p class="descricao">Com quintal</p>
      <div class="foto"><img src="/img/2.jpg"></div>
    </div>
  </div>
</body></html>`

func TestExtractListingsExtractsFieldsAndResolvesURLs(t *testing.T) {
	got, err := ExtractListings(samplePage, defaultSelectors(), "www.exemplo.com.br")
	if err != nil {
		t.Fatalf("ExtractListings() error = %v, want nil", err)
	}

	want := []db.RawListing{
		{
			SourceDomain:   "www.exemplo.com.br",
			ListingURL:     "https://www.exemplo.com.br/imoveis/1",
			TitleRaw:       "Apartamento 2 quartos",
			PriceRaw:       "R$ 350.000",
			AddressRaw:     "Rua A, 100 - Centro",
			DescriptionRaw: "Ótima localização",
			ImageURLs:      []string{"https://www.exemplo.com.br/img/1.jpg"},
		},
		{
			SourceDomain:   "www.exemplo.com.br",
			ListingURL:     "https://cdn.exemplo.com.br/imoveis/2",
			TitleRaw:       "Casa 3 quartos",
			PriceRaw:       "R$ 800.000",
			AddressRaw:     "Rua B, 200",
			DescriptionRaw: "Com quintal",
			ImageURLs:      []string{"https://www.exemplo.com.br/img/2.jpg"},
		},
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExtractListings() =\n%#v\nwant\n%#v", got, want)
	}
}

// Seletores relativos ao container: o preço do segundo anúncio não pode
// aparecer no primeiro, mesmo estando na mesma página.
func TestExtractListingsScopesSelectorsToContainer(t *testing.T) {
	got, err := ExtractListings(samplePage, defaultSelectors(), "www.exemplo.com.br")
	if err != nil {
		t.Fatalf("ExtractListings() error = %v, want nil", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(listings) = %d, want 2", len(got))
	}
	if strings.Contains(got[0].PriceRaw, "800.000") {
		t.Errorf("listing[0].PriceRaw = %q, leaked the next card's price", got[0].PriceRaw)
	}
}

func TestExtractListingsReturnsEmptySliceWhenNoContainerMatches(t *testing.T) {
	got, err := ExtractListings(`<html><body><p>nada por aqui</p></body></html>`, defaultSelectors(), "www.exemplo.com.br")
	if err != nil {
		t.Fatalf("ExtractListings() error = %v, want nil", err)
	}
	if got == nil {
		t.Fatal("ExtractListings() = nil, want empty non-nil slice")
	}
	if len(got) != 0 {
		t.Errorf("len(listings) = %d, want 0", len(got))
	}
}

// Sem ListingURL não há identidade para o ON CONFLICT de UpsertListings.
func TestExtractListingsDropsListingsWithoutURL(t *testing.T) {
	const page = `
<div class="card"><h2 class="titulo">Sem link</h2></div>
<div class="card"><a class="link">âncora sem href</a><h2 class="titulo">Sem href</h2></div>
<div class="card"><a class="link" href="#">Só fragmento</a><h2 class="titulo">Fragmento</h2></div>
<div class="card"><a class="link" href="javascript:void(0)">JS</a><h2 class="titulo">JS</h2></div>
<div class="card"><a class="link" href="/imoveis/9">ok</a><h2 class="titulo">Válido</h2></div>`

	got, err := ExtractListings(page, defaultSelectors(), "www.exemplo.com.br")
	if err != nil {
		t.Fatalf("ExtractListings() error = %v, want nil", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(listings) = %d, want 1 (%#v)", len(got), got)
	}
	if got[0].TitleRaw != "Válido" {
		t.Errorf("listing.TitleRaw = %q, want %q", got[0].TitleRaw, "Válido")
	}
}

// O primeiro elemento casado pode não ter href (wrapper antes do link); a
// extração precisa continuar procurando.
func TestExtractListingsUsesFirstMatchWithHref(t *testing.T) {
	config := defaultSelectors()
	config.Selectors.ListingURL = "a"

	const page = `<div class="card"><a name="topo">sem href</a><a href="/imoveis/7">ver</a></div>`

	got, err := ExtractListings(page, config, "www.exemplo.com.br")
	if err != nil {
		t.Fatalf("ExtractListings() error = %v, want nil", err)
	}
	if len(got) != 1 || got[0].ListingURL != "https://www.exemplo.com.br/imoveis/7" {
		t.Fatalf("listings = %#v, want single listing pointing to /imoveis/7", got)
	}
}

// Card inteiro embrulhado num <a>: Find nunca inclui o próprio container.
func TestExtractListingsFallsBackToContainerItself(t *testing.T) {
	config := defaultSelectors()
	config.Selectors.ListingContainer = "a.card"
	config.Selectors.ListingURL = "a.card"

	const page = `<a class="card" href="/imoveis/3"><h2 class="titulo">Cobertura</h2></a>`

	got, err := ExtractListings(page, config, "www.exemplo.com.br")
	if err != nil {
		t.Fatalf("ExtractListings() error = %v, want nil", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(listings) = %d, want 1", len(got))
	}
	if got[0].ListingURL != "https://www.exemplo.com.br/imoveis/3" {
		t.Errorf("listing.ListingURL = %q, want %q", got[0].ListingURL, "https://www.exemplo.com.br/imoveis/3")
	}
}

func TestExtractListingsCollectsImagesInOrderWithoutDuplicates(t *testing.T) {
	const page = `
<div class="card">
  <a class="link" href="/imoveis/4">ver</a>
  <div class="foto">
    <img src="/img/principal.jpg" data-src="/img/principal-hd.jpg">
  </div>
  <img src="/img/principal.jpg">
  <img data-src="/img/planta.jpg">
  <img src="data:image/gif;base64,R0lGOD">
  <img>
</div>`

	got, err := ExtractListings(page, defaultSelectors(), "www.exemplo.com.br")
	if err != nil {
		t.Fatalf("ExtractListings() error = %v, want nil", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(listings) = %d, want 1", len(got))
	}

	want := []string{
		"https://www.exemplo.com.br/img/principal.jpg",
		"https://www.exemplo.com.br/img/principal-hd.jpg",
		"https://www.exemplo.com.br/img/planta.jpg",
	}
	if !reflect.DeepEqual(got[0].ImageURLs, want) {
		t.Errorf("ImageURLs = %#v, want %#v", got[0].ImageURLs, want)
	}
}

// O seletor de imagem pode apontar para o wrapper, não para a <img>.
func TestExtractListingsReadsImagesFromWrapperSelector(t *testing.T) {
	config := defaultSelectors()
	config.Selectors.Image = "figure.foto"

	const page = `
<div class="card">
  <a class="link" href="/imoveis/5">ver</a>
  <figure class="foto"><img data-src="/img/5.jpg"></figure>
</div>`

	got, err := ExtractListings(page, config, "www.exemplo.com.br")
	if err != nil {
		t.Fatalf("ExtractListings() error = %v, want nil", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(listings) = %d, want 1", len(got))
	}
	if want := []string{"https://www.exemplo.com.br/img/5.jpg"}; !reflect.DeepEqual(got[0].ImageURLs, want) {
		t.Errorf("ImageURLs = %#v, want %#v", got[0].ImageURLs, want)
	}
}

func TestExtractListingsHonoursExplicitScheme(t *testing.T) {
	cases := []struct {
		name   string
		domain string
		want   string
	}{
		{name: "bare host defaults to https", domain: "exemplo.com.br", want: "https://exemplo.com.br/imoveis/6"},
		{name: "explicit http is preserved", domain: "http://exemplo.com.br", want: "http://exemplo.com.br/imoveis/6"},
		{name: "explicit https is preserved", domain: "https://exemplo.com.br", want: "https://exemplo.com.br/imoveis/6"},
	}

	const page = `<div class="card"><a class="link" href="/imoveis/6">ver</a></div>`

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ExtractListings(page, defaultSelectors(), tc.domain)
			if err != nil {
				t.Fatalf("ExtractListings() error = %v, want nil", err)
			}
			if len(got) != 1 {
				t.Fatalf("len(listings) = %d, want 1", len(got))
			}
			if got[0].ListingURL != tc.want {
				t.Errorf("listing.ListingURL = %q, want %q", got[0].ListingURL, tc.want)
			}
			if got[0].SourceDomain != tc.domain {
				t.Errorf("listing.SourceDomain = %q, want %q", got[0].SourceDomain, tc.domain)
			}
		})
	}
}

// Um seletor de campo quebrado não pode derrubar a extração inteira: perder o
// preço é ruim, perder todos os anúncios é pior.
func TestExtractListingsToleratesMissingAndInvalidFieldSelectors(t *testing.T) {
	config := defaultSelectors()
	config.Selectors.Price = "[[["
	config.Selectors.Address = ""
	config.Selectors.Description = ".inexistente"

	got, err := ExtractListings(samplePage, config, "www.exemplo.com.br")
	if err != nil {
		t.Fatalf("ExtractListings() error = %v, want nil", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(listings) = %d, want 2", len(got))
	}
	if got[0].PriceRaw != "" || got[0].AddressRaw != "" || got[0].DescriptionRaw != "" {
		t.Errorf("listing[0] = %#v, want empty price/address/description", got[0])
	}
	if got[0].TitleRaw != "Apartamento 2 quartos" {
		t.Errorf("listing[0].TitleRaw = %q, want the title to survive", got[0].TitleRaw)
	}
}

func TestExtractListingsRejectsInvalidInput(t *testing.T) {
	emptyContainer := defaultSelectors()
	emptyContainer.Selectors.ListingContainer = "   "

	invalidContainer := defaultSelectors()
	invalidContainer.Selectors.ListingContainer = "div[unclosed"

	cases := []struct {
		name   string
		config db.SelectorConfig
		domain string
	}{
		{name: "empty domain", config: defaultSelectors(), domain: "  "},
		{name: "domain without host", config: defaultSelectors(), domain: "https://"},
		{name: "empty container selector", config: emptyContainer, domain: "www.exemplo.com.br"},
		{name: "invalid container selector", config: invalidContainer, domain: "www.exemplo.com.br"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ExtractListings(samplePage, tc.config, tc.domain)
			if err == nil {
				t.Fatalf("ExtractListings() error = nil, want an error (got %#v)", got)
			}
			if got != nil {
				t.Errorf("ExtractListings() = %#v, want nil on error", got)
			}
			if !strings.HasPrefix(err.Error(), "scraper: ") {
				t.Errorf("err = %q, want it prefixed with the package name", err)
			}
		})
	}
}

// O TrimSpace vale para todos os campos de texto, inclusive quando o HTML tem
// quebras de linha e indentação entre as tags.
func TestExtractListingsTrimsTextFields(t *testing.T) {
	const page = `
<div class="card">
  <a class="link" href="/imoveis/8">ver</a>
  <h2 class="titulo">
      Sobrado
  </h2>
  <span class="preco">
	R$ 1.000
  </span>
</div>`

	got, err := ExtractListings(page, defaultSelectors(), "www.exemplo.com.br")
	if err != nil {
		t.Fatalf("ExtractListings() error = %v, want nil", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(listings) = %d, want 1", len(got))
	}
	if got[0].TitleRaw != "Sobrado" {
		t.Errorf("TitleRaw = %q, want %q", got[0].TitleRaw, "Sobrado")
	}
	if got[0].PriceRaw != "R$ 1.000" {
		t.Errorf("PriceRaw = %q, want %q", got[0].PriceRaw, "R$ 1.000")
	}
}
