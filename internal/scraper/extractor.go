package scraper

import (
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"github.com/andybalholm/cascadia"

	"github.com/imobhub/api/internal/db"
)

// imageAttrs são os atributos lidos de cada elemento de imagem, na ordem de
// preferência. `data-src` existe porque sites com lazy loading deixam o `src`
// com um placeholder (ou nem o definem) até o script de carregamento rodar —
// no HTML estático a URL real costuma estar só no `data-src`.
var imageAttrs = []string{"src", "data-src"}

// imgMatcher seleciona todas as imagens dentro do container. É compilado uma
// única vez porque é usado por anúncio, e o seletor é uma constante.
var imgMatcher = cascadia.MustCompile("img")

// ExtractListings extrai os anúncios de uma página de listagem aplicando os
// seletores CSS de config sobre pageHTML.
//
// É uma função pura: não acessa banco nem rede. Cada elemento que casa com
// config.Selectors.ListingContainer vira um db.RawListing, com os demais
// seletores aplicados **relativamente** ao container. Os campos de texto saem
// crus (apenas com strings.TrimSpace) — a normalização de preço, área e
// quartos é responsabilidade de outra etapa.
//
// URLs relativas são resolvidas contra "https://<sourceDomain>" (ou
// "http://…", se sourceDomain vier com esse esquema explícito). Anúncios sem
// ListingURL após a extração são descartados: o par
// (SourceDomain, ListingURL) é a identidade do anúncio no banco, e sem ele não
// há como deduplicar.
//
// Nenhum container encontrado devolve slice vazio e erro nulo — o chamador
// interpreta zero resultados como sinal de que os seletores podem ter quebrado
// (o que dispara a redescoberta pela IA). O erro fica reservado para entradas
// inválidas: HTML impossível de parsear, domínio vazio ou seletor de container
// ausente/malformado, que são erros de configuração e não do site.
//
// sourceDomain é copiado para RawListing.SourceDomain apenas com TrimSpace;
// assim como em internal/db, normalizar o host (minúsculo, sem barra final) é
// responsabilidade do chamador.
func ExtractListings(pageHTML string, config db.SelectorConfig, sourceDomain string) ([]db.RawListing, error) {
	sourceDomain = strings.TrimSpace(sourceDomain)
	if sourceDomain == "" {
		return nil, fmt.Errorf("scraper: source domain is required to resolve relative URLs")
	}

	baseURL, err := baseURLFor(sourceDomain)
	if err != nil {
		return nil, err
	}

	containerSelector := strings.TrimSpace(config.Selectors.ListingContainer)
	if containerSelector == "" {
		return nil, fmt.Errorf("scraper: listing_container selector is empty for domain %q", sourceDomain)
	}

	containerMatcher, err := cascadia.Compile(containerSelector)
	if err != nil {
		return nil, fmt.Errorf("scraper: invalid listing_container selector %q for domain %q: %w", containerSelector, sourceDomain, err)
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(pageHTML))
	if err != nil {
		return nil, fmt.Errorf("scraper: could not parse HTML of %q: %w", sourceDomain, err)
	}

	// Os seletores de campo são compilados uma vez, fora do laço: recompilar a
	// cada container multiplicaria o custo pelo número de anúncios da página.
	// Diferente do container, um seletor de campo inválido não aborta a
	// extração — perder o preço de todos os anúncios é ruim, perder os anúncios
	// inteiros é pior. O aviso no log é o que aponta o campo a corrigir.
	fields := compileFieldMatchers(config.Selectors, sourceDomain)

	listings := make([]db.RawListing, 0)

	doc.FindMatcher(containerMatcher).Each(func(_ int, container *goquery.Selection) {
		listingURL := extractListingURL(container, fields.listingURL, baseURL)
		if listingURL == "" {
			// Sem URL não há identidade: gravar o anúncio faria o ON CONFLICT
			// de UpsertListings colidir com qualquer outro anúncio sem URL.
			return
		}

		listings = append(listings, db.RawListing{
			SourceDomain:   sourceDomain,
			ListingURL:     listingURL,
			TitleRaw:       matcherText(container, fields.title),
			PriceRaw:       matcherText(container, fields.price),
			AddressRaw:     matcherText(container, fields.address),
			DescriptionRaw: matcherText(container, fields.description),
			ImageURLs:      extractImageURLs(container, fields.image, baseURL),
		})
	})

	return listings, nil
}

// fieldMatchers guarda os seletores já compilados de um domínio. Campo nil
// significa "seletor ausente ou inválido" — o campo correspondente sai vazio.
type fieldMatchers struct {
	title       goquery.Matcher
	price       goquery.Matcher
	address     goquery.Matcher
	description goquery.Matcher
	listingURL  goquery.Matcher
	image       goquery.Matcher
}

func compileFieldMatchers(selectors db.SelectorFields, sourceDomain string) fieldMatchers {
	return fieldMatchers{
		title:       compileOptional(selectors.Title, "title", sourceDomain),
		price:       compileOptional(selectors.Price, "price", sourceDomain),
		address:     compileOptional(selectors.Address, "address", sourceDomain),
		description: compileOptional(selectors.Description, "description", sourceDomain),
		listingURL:  compileOptional(selectors.ListingURL, "listing_url", sourceDomain),
		image:       compileOptional(selectors.Image, "image", sourceDomain),
	}
}

// compileOptional compila um seletor de campo. Devolve nil (e não um matcher
// que não casa com nada, como faria goquery.Find) para que o chamador possa
// distinguir "não configurado" de "não encontrado na página".
func compileOptional(selector, field, sourceDomain string) goquery.Matcher {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return nil
	}

	compiled, err := cascadia.Compile(selector)
	if err != nil {
		slog.Warn("scraper: ignoring invalid field selector",
			"domain", sourceDomain,
			"field", field,
			"selector", selector,
			"error", err,
		)
		return nil
	}

	return compiled
}

// matcherText devolve o texto do(s) elemento(s) casados, apenas com TrimSpace.
// O conteúdo interno não é normalizado de propósito: os campos "_raw" existem
// para permitir refazer o parsing sem raspar o site de novo.
func matcherText(container *goquery.Selection, matcher goquery.Matcher) string {
	if matcher == nil {
		return ""
	}

	return strings.TrimSpace(container.FindMatcher(matcher).Text())
}

// extractListingURL procura o primeiro href utilizável dentro do container.
//
// Percorre todos os elementos casados (e não apenas o primeiro) porque um
// seletor composto pode casar antes com um elemento sem href — por exemplo o
// wrapper do card antes do próprio link. Se nada casar dentro do container, o
// próprio container é testado contra o seletor: é comum o card inteiro ser um
// <a>, e Find nunca inclui o elemento de origem.
func extractListingURL(container *goquery.Selection, matcher goquery.Matcher, baseURL *url.URL) string {
	if matcher == nil {
		return ""
	}

	var found string
	container.FindMatcher(matcher).EachWithBreak(func(_ int, sel *goquery.Selection) bool {
		if abs := attrURL(sel, "href", baseURL); abs != "" {
			found = abs
			return false
		}
		return true
	})

	if found == "" && container.IsMatcher(matcher) {
		found = attrURL(container, "href", baseURL)
	}

	return found
}

// extractImageURLs coleta as URLs das imagens do anúncio, sem duplicatas e
// preservando a ordem de aparição.
//
// As imagens do seletor configurado vêm primeiro (é a escolha da IA para "a
// foto do anúncio"), seguidas de todas as <img> do container. Varrer o
// container inteiro é o que salva os cards em que a foto principal não casa com
// o seletor salvo — e a deduplicação garante que a preferência pela primeira
// não se perca.
func extractImageURLs(container *goquery.Selection, matcher goquery.Matcher, baseURL *url.URL) []string {
	var (
		urls []string
		seen = make(map[string]struct{})
	)

	collect := func(sel *goquery.Selection) {
		sel.Each(func(_ int, node *goquery.Selection) {
			for _, attr := range imageAttrs {
				abs := attrURL(node, attr, baseURL)
				if abs == "" {
					continue
				}
				if _, exists := seen[abs]; exists {
					continue
				}
				seen[abs] = struct{}{}
				urls = append(urls, abs)
			}
		})
	}

	if matcher != nil {
		selected := container.FindMatcher(matcher)
		collect(selected)
		// O seletor pode apontar para o wrapper (uma <figure>, um <div> com
		// background) em vez da própria <img>.
		collect(selected.FindMatcher(imgMatcher))
	}

	collect(container.FindMatcher(imgMatcher))

	return urls
}

// attrURL lê um atributo do elemento e devolve a URL absoluta correspondente,
// ou "" se o atributo não existe ou não vira uma URL http/https utilizável.
func attrURL(sel *goquery.Selection, attr string, baseURL *url.URL) string {
	raw, ok := sel.Attr(attr)
	if !ok {
		return ""
	}

	return resolveURL(baseURL, raw)
}

// resolveURL transforma uma referência do HTML em URL absoluta.
//
// Descarta o que não é navegável: referência vazia, âncora para a própria
// página ("#", "#contato") e esquemas como javascript:, mailto: e data:. Uma
// âncora resolveria para a própria página de listagem, o que criaria um
// "anúncio" apontando para ela; um data: URI de imagem pode ter centenas de KB
// e não tem por que ir para o banco.
func resolveURL(baseURL *url.URL, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "#") {
		return ""
	}

	ref, err := url.Parse(raw)
	if err != nil {
		return ""
	}

	abs := baseURL.ResolveReference(ref)
	if abs.Scheme != "http" && abs.Scheme != "https" || abs.Host == "" {
		return ""
	}

	return abs.String()
}

// baseURLFor monta a base usada para resolver as URLs relativas da página.
//
// sourceDomain normalmente é só o host ("www.exemplo.com.br"), e nesse caso
// assumimos https. Um esquema explícito é respeitado: alguns sites antigos só
// respondem em http, e forçar https ali geraria URLs que não abrem.
func baseURLFor(sourceDomain string) (*url.URL, error) {
	raw := sourceDomain
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		raw = "https://" + raw
	}

	// A barra final faz a base ser tratada como diretório; sem ela,
	// ResolveReference de "imovel/1" descartaria o último segmento do path.
	if !strings.HasSuffix(raw, "/") {
		raw += "/"
	}

	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return nil, fmt.Errorf("scraper: invalid source domain %q", sourceDomain)
	}

	return parsed, nil
}
