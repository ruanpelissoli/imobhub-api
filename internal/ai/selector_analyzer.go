package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/imobhub/api/internal/db"
)

// SelectorModel é o modelo usado na análise de seletores. É deliberadamente
// mais barato que DefaultModel: identificar classes CSS num HTML é uma tarefa
// de extração, não de raciocínio, e roda uma vez por domínio novo.
const SelectorModel = anthropic.ModelClaudeHaiku4_5

const (
	// MaxHTMLChars é o teto de caracteres do HTML enviado ao modelo. O contexto
	// do Haiku é de 200k tokens; páginas de listagem passam fácil disso quando
	// vêm com scripts e SVGs inline. O começo do documento é onde estão o
	// <head>, os primeiros cards e os sinais de SPA, então truncar o fim é o
	// corte que menos custa.
	MaxHTMLChars = 80_000

	// selectorAnalysisTimeout limita a chamada inteira (incluindo os retries
	// automáticos do SDK), não cada tentativa isolada — é o tempo máximo que um
	// domínio novo pode segurar o worker.
	selectorAnalysisTimeout = 60 * time.Second

	// selectorToolName é o nome da ferramenta que o modelo é obrigado a chamar.
	// Trocá-lo exige trocar também o ToolChoice.
	selectorToolName = "report_listing_selectors"

	// selectorMaxTokens cobre com folga um objeto de 8 campos curtos.
	selectorMaxTokens = 2048
)

// Erros retornados por AnalyzeSelectors. São sentinelas para que o chamador
// possa decidir entre marcar a fonte como não-suportada (ErrNoListingContainer)
// e simplesmente tentar de novo mais tarde.
var (
	// ErrEmptyHTML indica que o HTML recebido é vazio. Não faz sentido gastar
	// tokens nesse caso: se a página estática veio vazia, o caminho certo é
	// renderizar com headless e analisar o resultado.
	ErrEmptyHTML = errors.New("ai: page HTML is empty")

	// ErrNoToolUse indica que a resposta não trouxe nenhuma chamada da
	// ferramenta — normalmente porque o modelo respondeu em texto livre.
	ErrNoToolUse = errors.New("ai: model did not call the selector tool")

	// ErrNoListingContainer indica que o modelo chamou a ferramenta mas não
	// conseguiu apontar o container de anúncio. Sem ele os demais seletores são
	// inúteis, já que todos são relativos ao container.
	ErrNoListingContainer = errors.New("ai: model could not identify the listing container selector")

	// ErrInvalidRenderMode indica que o modelo devolveu um render_mode fora do
	// enum, o que quebraria a CHECK constraint de site_selectors.
	ErrInvalidRenderMode = errors.New("ai: model returned an invalid render mode")
)

const selectorSystemPrompt = `You are a web scraping expert analyzing the HTML of a Brazilian real estate agency ("imobiliária") website. The page you receive is a property listing page: it shows many properties for sale or rent, each rendered as a repeated card or row.

Your job is to identify the CSS selectors needed to extract every property from that page, and to report them by calling the ` + selectorToolName + ` tool. Always call the tool — never answer in prose.

Rules for the selectors:
1. "listing_container" must match ONE property card. It must match every card on the page and nothing else (no wrapper that contains all cards at once, no filter or pagination element). Everything else is extracted from inside it.
2. All other selectors are RELATIVE to the container: they are applied with the container as the root. Write ".card-title", not "div.listing .card-title", and never repeat the container selector inside them.
3. Prefer simple, semantic, stable selectors — class names that describe the content ("price", "imovel-titulo", "endereco"), or tags with a distinctive attribute. Avoid positional selectors such as :nth-child, :first-child or long descendant chains: they break as soon as the site reorders or adds an element.
4. Avoid class names that look machine-generated or hashed (for example "css-1a2b3c" or "sc-hKgILt"), because they change on every deploy of the site.
5. "image" must select the <img> (or the element carrying the image), and "listing_url" must select the <a> that links to the property detail page. Do not include attribute accessors — return the element selector only, the extractor reads the attribute.
6. If a field genuinely does not exist on the card, return an empty string for it. Never invent a selector. Only "listing_container" is mandatory: if you cannot identify a reliable container, return an empty string for it as well.

Rules for "render_mode":
- Use "static" when the property data is already present in the HTML you received.
- Use "headless" when the HTML shows signs that the listing is rendered by JavaScript in the browser: a Single Page Application framework (React, Vue, Angular, Next.js, Nuxt), an empty mount point such as <div id="root"></div> or <div id="__next"></div>, skeleton/loading placeholders, or a page where the property cards simply are not in the markup.

The HTML may have been truncated: judge only by what you were given.`

// selectorToolSchema é o JSON Schema da ferramenta. As chaves são exatamente as
// tags json de db.SelectorFields (mais render_mode), de modo que a resposta do
// modelo desserializa direto no struct persistido.
func selectorToolSchema() anthropic.ToolInputSchemaParam {
	return anthropic.ToolInputSchemaParam{
		Properties: map[string]any{
			"listing_container": map[string]any{
				"type":        "string",
				"description": "CSS selector matching a single property listing card on the page. It must match every card and nothing else. All other selectors are applied relative to this element. Empty string if no reliable container can be identified.",
			},
			"title": map[string]any{
				"type":        "string",
				"description": "CSS selector, relative to the listing container, for the element holding the property title or headline. Empty string if absent.",
			},
			"price": map[string]any{
				"type":        "string",
				"description": "CSS selector, relative to the listing container, for the element holding the price (sale or rent value). Empty string if absent.",
			},
			"address": map[string]any{
				"type":        "string",
				"description": "CSS selector, relative to the listing container, for the element holding the address, neighborhood or city. Empty string if absent.",
			},
			"description": map[string]any{
				"type":        "string",
				"description": "CSS selector, relative to the listing container, for the element holding the short description or property summary. Empty string if absent.",
			},
			"image": map[string]any{
				"type":        "string",
				"description": "CSS selector, relative to the listing container, for the element carrying the property photo (usually an img tag). Return the element selector only, without any attribute accessor. Empty string if absent.",
			},
			"listing_url": map[string]any{
				"type":        "string",
				"description": "CSS selector, relative to the listing container, for the anchor element linking to the property detail page. Return the element selector only, without any attribute accessor. Empty string if absent.",
			},
			"render_mode": map[string]any{
				"type":        "string",
				"enum":        []string{db.RenderModeStatic, db.RenderModeHeadless},
				"description": "How this page must be fetched. Use \"static\" when the listing data is already present in the HTML. Use \"headless\" when the listing is rendered by JavaScript, for example on a React, Vue, Angular or Next.js single page application, or when the HTML only contains an empty mount point or loading placeholders.",
			},
		},
		Required: []string{
			"listing_container",
			"title",
			"price",
			"address",
			"description",
			"image",
			"listing_url",
			"render_mode",
		},
	}
}

// selectorToolInput espelha o schema da ferramenta. Os campos de seletor usam
// as mesmas tags json de db.SelectorFields de propósito.
type selectorToolInput struct {
	ListingContainer string `json:"listing_container"`
	Title            string `json:"title"`
	Price            string `json:"price"`
	Address          string `json:"address"`
	Description      string `json:"description"`
	Image            string `json:"image"`
	ListingURL       string `json:"listing_url"`
	RenderMode       string `json:"render_mode"`
}

// AnalyzeSelectors pede ao Claude os seletores CSS de uma página de listagens.
// Devolve os seletores, o modo de renderização (db.RenderModeStatic ou
// db.RenderModeHeadless) e o erro.
//
// A chave chega por parâmetro, e não de um Client já construído, porque este é
// um fluxo raro (uma vez por domínio novo) e o chamador — internal/selectors —
// não precisa carregar um client vivo entre coletas.
//
// pageHTML pode vir de qualquer tamanho: é truncado em MaxHTMLChars antes do
// envio. siteURL entra no prompt como contexto e nas mensagens de erro.
func AnalyzeSelectors(ctx context.Context, apiKey, pageHTML, siteURL string) (*db.SelectorFields, string, error) {
	client, err := New(apiKey)
	if err != nil {
		return nil, "", err
	}
	return client.analyzeSelectors(ctx, pageHTML, siteURL)
}

func (c *Client) analyzeSelectors(ctx context.Context, pageHTML, siteURL string) (*db.SelectorFields, string, error) {
	if strings.TrimSpace(pageHTML) == "" {
		return nil, "", fmt.Errorf("ai: analyzing %s: %w", siteURL, ErrEmptyHTML)
	}

	tool := anthropic.ToolParam{
		Name:        selectorToolName,
		Description: anthropic.String("Report the CSS selectors used to extract every property listing from this page, plus the render mode the page requires."),
		InputSchema: selectorToolSchema(),
	}

	ctx, cancel := context.WithTimeout(ctx, selectorAnalysisTimeout)
	defer cancel()

	message, err := c.api.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     SelectorModel,
		MaxTokens: selectorMaxTokens,
		System: []anthropic.TextBlockParam{
			{Text: selectorSystemPrompt},
		},
		Tools:      []anthropic.ToolUnionParam{{OfTool: &tool}},
		ToolChoice: anthropic.ToolChoiceParamOfTool(selectorToolName),
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(selectorUserPrompt(pageHTML, siteURL))),
		},
	})
	if err != nil {
		return nil, "", fmt.Errorf("ai: analyzing selectors for %s: %w", siteURL, err)
	}

	input, err := selectorToolCall(message)
	if err != nil {
		return nil, "", fmt.Errorf("ai: analyzing selectors for %s: %w", siteURL, err)
	}

	fields, renderMode, err := input.toSelectorFields()
	if err != nil {
		return nil, "", fmt.Errorf("ai: analyzing selectors for %s: %w", siteURL, err)
	}
	return fields, renderMode, nil
}

func selectorUserPrompt(pageHTML, siteURL string) string {
	html, truncated := truncateChars(pageHTML, MaxHTMLChars)

	var b strings.Builder
	b.WriteString("Listing page URL: ")
	b.WriteString(siteURL)
	if truncated {
		fmt.Fprintf(&b, "\n\nNote: the HTML below was truncated to the first %d characters.", MaxHTMLChars)
	}
	b.WriteString("\n\nHTML:\n")
	b.WriteString(html)
	return b.String()
}

// selectorToolCall extrai o bloco tool_use da resposta. Percorre todos os
// blocos porque o modelo pode emitir texto antes da chamada da ferramenta.
func selectorToolCall(message *anthropic.Message) (*selectorToolInput, error) {
	for _, block := range message.Content {
		use, ok := block.AsAny().(anthropic.ToolUseBlock)
		if !ok || use.Name != selectorToolName {
			continue
		}

		var input selectorToolInput
		if err := json.Unmarshal(use.Input, &input); err != nil {
			return nil, fmt.Errorf("decoding tool input: %w", err)
		}
		return &input, nil
	}
	return nil, fmt.Errorf("%w (stop_reason=%q)", ErrNoToolUse, message.StopReason)
}

func (in *selectorToolInput) toSelectorFields() (*db.SelectorFields, string, error) {
	container := strings.TrimSpace(in.ListingContainer)
	if container == "" {
		return nil, "", ErrNoListingContainer
	}

	renderMode := strings.ToLower(strings.TrimSpace(in.RenderMode))
	switch renderMode {
	case "":
		// O schema marca render_mode como obrigatório; se ainda assim vier
		// vazio, "static" é o default seguro: falhar na extração é barato e
		// visível, enquanto subir um Chrome à toa é caro e silencioso.
		renderMode = db.RenderModeStatic
	case db.RenderModeStatic, db.RenderModeHeadless:
	default:
		return nil, "", fmt.Errorf("%w: %q", ErrInvalidRenderMode, in.RenderMode)
	}

	return &db.SelectorFields{
		ListingContainer: container,
		Title:            strings.TrimSpace(in.Title),
		Price:            strings.TrimSpace(in.Price),
		Address:          strings.TrimSpace(in.Address),
		Description:      strings.TrimSpace(in.Description),
		Image:            strings.TrimSpace(in.Image),
		ListingURL:       strings.TrimSpace(in.ListingURL),
	}, renderMode, nil
}

// truncateChars corta s nos primeiros max caracteres (runes, não bytes) e
// informa se houve corte. Cortar por bytes partiria um caractere acentuado ao
// meio e produziria UTF-8 inválido no meio do prompt.
func truncateChars(s string, max int) (string, bool) {
	count := 0
	for index := range s {
		if count == max {
			return s[:index], true
		}
		count++
	}
	return s, false
}
