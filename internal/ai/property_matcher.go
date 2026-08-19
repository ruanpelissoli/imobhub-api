package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/imobhub/api/internal/db"
)

// MatcherModel é o modelo usado na comparação de imóveis. É o terceiro modelo do
// pacote e fica no meio do caminho de propósito: comparar fotos e endereços
// exige visão e julgamento, que o Haiku não entrega, mas este caminho é pago
// **por anúncio** — o Opus do DefaultModel sairia caro demais no volume de uma
// coleta inteira. Trocá-lo continua sendo uma mudança de uma linha.
const MatcherModel = anthropic.ModelClaudeSonnet4_5

const (
	// propertyMatchTimeout limita a chamada inteira (incluindo os retries
	// automáticos do SDK e o retry somente-texto), não cada tentativa isolada.
	propertyMatchTimeout = 60 * time.Second

	// propertyMatchToolName é o nome da ferramenta que o modelo é obrigado a
	// chamar. Trocá-lo exige trocar também o ToolChoice e o system prompt.
	propertyMatchToolName = "report_property_match"

	// propertyMatchMaxTokens cobre com folga um objeto de 4 campos curtos.
	propertyMatchMaxTokens = 1024

	// MaxMatchDescriptionChars é o teto de caracteres da descrição enviada ao
	// modelo, por lado. Descrições de portal são longas e repetitivas; o começo
	// é onde estão os atributos do imóvel, o fim costuma ser texto comercial da
	// imobiliária.
	MaxMatchDescriptionChars = 600

	// Tetos de imagens. São o principal componente variável do custo da
	// requisição: cada foto vale centenas de tokens.
	maxListingImages   = 3
	maxCandidateImages = 2
	maxTotalImages     = 12
)

// Erros retornados por MatchProperty. São sentinelas para que o chamador
// distinga "a resposta veio inutilizável" de "a rede falhou, tente de novo".
var (
	// ErrNoCandidates indica que MatchProperty foi chamado sem candidatos.
	// Retornado **antes** de qualquer requisição: comparar um anúncio com uma
	// lista vazia gastaria tokens para produzir sempre a mesma resposta.
	ErrNoCandidates = errors.New("ai: property match requires at least one candidate")

	// ErrNoPropertyMatchToolUse indica que a resposta não trouxe nenhuma chamada
	// da ferramenta — normalmente porque o modelo respondeu em texto livre.
	ErrNoPropertyMatchToolUse = errors.New("ai: model did not call the property match tool")

	// ErrInvalidConfidence indica que o modelo devolveu uma confiança fora de
	// [0,1]. Aceitá-la corromperia a comparação com o threshold de agrupamento.
	ErrInvalidConfidence = errors.New("ai: model returned a confidence outside [0,1]")
)

// MatchListing é o anúncio a comparar, já enriquecido. Só carrega o que o
// modelo usa para decidir: campos ausentes viram "não informado" no prompt.
type MatchListing struct {
	AddressRaw     string
	Neighborhood   string
	AreaRaw        string
	TitleRaw       string
	DescriptionRaw string
	BedroomCount   *int
	// ImageURLs são as fotos do anúncio. Até maxListingImages são enviadas.
	ImageURLs []string
}

// PropertyMatch é a decisão do modelo. **O threshold não é aplicado aqui**: a
// confiança mínima aceitável é regra de negócio de internal/grouping, e este
// pacote não deve conhecê-la.
type PropertyMatch struct {
	// SameProperty é o veredito do modelo.
	SameProperty bool
	// PropertyID é o id do candidato apontado, ou vazio. **Não é validado
	// contra a lista de candidatos aqui** — quem tem a lista é o chamador.
	PropertyID string
	// Confidence está em [0,1] (validado).
	Confidence float64
	// Reason é uma justificativa curta, destinada apenas a log.
	Reason string
}

const propertyMatchSystemPrompt = `You are a Brazilian real estate deduplication expert. You receive one property listing ("ANÚNCIO") scraped from an agency website and a list of canonical properties ("CANDIDATE") already stored in the database, all located within a few dozen meters of it.

Your job is to decide whether the listing describes the SAME PHYSICAL PROPERTY as one of the candidates, and to report your decision by calling the ` + propertyMatchToolName + ` tool. Always call the tool — never answer in prose.

How to decide:
1. The same physical property is often advertised by several agencies, with different titles, different photos and slightly different addresses. Different unit numbers, different floors or different blocks of the same condominium are DIFFERENT properties.
2. Weigh the address and the neighborhood first, then the physical attributes (bedrooms, area, property type), then the photos. Matching photos of the same rooms, the same view or the same building facade are strong evidence; a generic facade shot of a large condominium is weak evidence, because every unit in it shares the same facade.
3. Fields marked "não informado" are simply absent from the data. Treat them as unknown — never as evidence of a difference.
4. Proximity is NOT evidence by itself: every candidate you receive is already nearby. Two different apartments in the same building are equally close.
5. Prefer "no match" when in doubt. Merging two distinct properties is expensive to undo; creating a duplicate canonical property is not.

How to answer:
- "same_property": true only if you are confident the listing is one of the candidates.
- "property_id": when "same_property" is true, the id of the matching CANDIDATE, copied EXACTLY as it appears in the prompt. When it is false, an empty string. Never invent an id and never return one that is not in the list.
- "confidence": how sure you are of the value in "same_property", from 0 to 1.
- "reason": one short sentence naming the decisive evidence. It is only read by humans tuning this pipeline.`

// propertyMatchToolSchema é o JSON Schema da ferramenta. As chaves são as tags
// json de propertyMatchToolInput.
func propertyMatchToolSchema() anthropic.ToolInputSchemaParam {
	return anthropic.ToolInputSchemaParam{
		Properties: map[string]any{
			"same_property": map[string]any{
				"type":        "boolean",
				"description": "True only if the listing describes the same physical property as one of the candidates. False when it is a different property, including a different unit or floor of the same building.",
			},
			"confidence": map[string]any{
				"type":        "number",
				"minimum":     0,
				"maximum":     1,
				"description": "How confident you are in the value of same_property, from 0 (no confidence) to 1 (certain).",
			},
			"property_id": map[string]any{
				"type":        "string",
				"description": "The id of the matching CANDIDATE, copied exactly as it appears in the prompt. Empty string when same_property is false. Never an id that is not in the candidate list.",
			},
			"reason": map[string]any{
				"type":        "string",
				"description": "One short sentence naming the decisive evidence for the decision. Read only by humans tuning the pipeline.",
			},
		},
		Required: []string{"same_property", "confidence", "property_id", "reason"},
	}
}

// propertyMatchToolInput espelha o schema da ferramenta.
type propertyMatchToolInput struct {
	SameProperty bool    `json:"same_property"`
	Confidence   float64 `json:"confidence"`
	PropertyID   string  `json:"property_id"`
	Reason       string  `json:"reason"`
}

// MatchProperty pergunta ao Claude se o anúncio se refere ao mesmo imóvel
// físico de algum dos candidatos.
//
// Faz **uma** requisição à API (mais, no máximo, um retry somente-texto quando
// as imagens são rejeitadas). Lista de candidatos vazia é ErrNoCandidates, sem
// tocar na rede.
//
// O veredito volta cru: aplicar o threshold de confiança e conferir se o
// property_id devolvido pertence à lista de candidatos é responsabilidade do
// chamador (internal/grouping), que é quem tem essa regra de negócio.
func (c *Client) MatchProperty(ctx context.Context, listing MatchListing, candidates []db.Property) (PropertyMatch, error) {
	if len(candidates) == 0 {
		return PropertyMatch{}, fmt.Errorf("ai: matching property: %w", ErrNoCandidates)
	}

	prompt := propertyMatchPrompt(listing, candidates)
	slog.DebugContext(ctx, "property match prompt", "prompt", prompt, "candidates", len(candidates))

	tool := anthropic.ToolParam{
		Name:        propertyMatchToolName,
		Description: anthropic.String("Report whether the listing refers to the same physical property as one of the candidates."),
		InputSchema: propertyMatchToolSchema(),
	}

	ctx, cancel := context.WithTimeout(ctx, propertyMatchTimeout)
	defer cancel()

	newParams := func(blocks []anthropic.ContentBlockParamUnion) anthropic.MessageNewParams {
		return anthropic.MessageNewParams{
			Model:     MatcherModel,
			MaxTokens: propertyMatchMaxTokens,
			System: []anthropic.TextBlockParam{
				{Text: propertyMatchSystemPrompt},
			},
			Tools:      []anthropic.ToolUnionParam{{OfTool: &tool}},
			ToolChoice: anthropic.ToolChoiceParamOfTool(propertyMatchToolName),
			Messages: []anthropic.MessageParam{
				anthropic.NewUserMessage(blocks...),
			},
		}
	}

	blocks := matchContentBlocks(prompt, listing, candidates)
	withImages := len(blocks) > 1

	message, err := c.api.Messages.New(ctx, newParams(blocks))
	if err != nil && withImages && isImageRejection(err) {
		// Uma foto inacessível ou num formato que a API recusa não pode custar a
		// comparação textual, que é a parte mais determinante da decisão.
		slog.WarnContext(ctx, "property match: retrying without images after the API rejected them", "error", err)
		message, err = c.api.Messages.New(ctx, newParams(blocks[:1]))
	}
	if err != nil {
		return PropertyMatch{}, fmt.Errorf("ai: matching property against %d candidates: %w", len(candidates), err)
	}

	slog.DebugContext(ctx, "property match response", "response", message.RawJSON())

	input, err := propertyMatchToolCall(message)
	if err != nil {
		return PropertyMatch{}, fmt.Errorf("ai: matching property against %d candidates: %w", len(candidates), err)
	}

	match, err := input.toPropertyMatch()
	if err != nil {
		return PropertyMatch{}, fmt.Errorf("ai: matching property against %d candidates: %w", len(candidates), err)
	}

	return match, nil
}

// isImageRejection identifica a recusa das imagens pela API: um 400 do lado do
// cliente. Rate limit (429), indisponibilidade (5xx) e timeout não entram —
// nesses casos repetir a requisição sem as fotos só gastaria dinheiro de novo.
func isImageRejection(err error) bool {
	var apiErr *anthropic.Error
	return errors.As(err, &apiErr) && apiErr.StatusCode == 400
}

// propertyMatchPrompt monta o texto da comparação. É função pura de propósito:
// é o material que se lê no log de debug para calibrar o threshold, e precisa
// ser testável sem servidor.
//
// A regra que não pode ser quebrada: campo ausente sai como "não informado".
// Preencher com um valor plausível faria o modelo comparar dados inventados.
func propertyMatchPrompt(listing MatchListing, candidates []db.Property) string {
	var b strings.Builder

	b.WriteString("ANÚNCIO (o anúncio a classificar)\n")
	writeField(&b, "endereço", listing.AddressRaw)
	writeField(&b, "bairro", listing.Neighborhood)
	writeIntField(&b, "quartos", listing.BedroomCount)
	writeField(&b, "área", listing.AreaRaw)
	writeField(&b, "título", listing.TitleRaw)
	writeDescription(&b, listing.DescriptionRaw)

	fmt.Fprintf(&b, "\n%d CANDIDATE(s) já cadastrados nas proximidades:\n", len(candidates))
	for _, candidate := range candidates {
		fmt.Fprintf(&b, "\nCANDIDATE %s\n", candidate.ID)
		writeField(&b, "endereço", derefString(candidate.CanonicalAddress))
		writeField(&b, "bairro", derefString(candidate.Neighborhood))
		writeField(&b, "cidade/UF", cityState(candidate))
		writeIntField(&b, "quartos", candidate.BedroomCount)
		writeField(&b, "área", areaSqm(candidate.AreaSqm))
		writeField(&b, "tipo de imóvel", derefString(candidate.PropertyType))
		writeDescription(&b, derefString(candidate.Description))
	}

	return b.String()
}

const unknownValue = "não informado"

func writeField(b *strings.Builder, label, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = unknownValue
	}
	fmt.Fprintf(b, "- %s: %s\n", label, value)
}

func writeIntField(b *strings.Builder, label string, value *int) {
	if value == nil {
		fmt.Fprintf(b, "- %s: %s\n", label, unknownValue)
		return
	}
	fmt.Fprintf(b, "- %s: %d\n", label, *value)
}

func writeDescription(b *strings.Builder, description string) {
	description = strings.TrimSpace(description)
	if description == "" {
		fmt.Fprintf(b, "- descrição: %s\n", unknownValue)
		return
	}
	truncated, cut := truncateChars(description, MaxMatchDescriptionChars)
	fmt.Fprintf(b, "- descrição: %s", truncated)
	if cut {
		b.WriteString(" [...]")
	}
	b.WriteString("\n")
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// cityState junta cidade e UF numa linha só; ambos são opcionais e o candidato
// pode ter só um deles.
func cityState(candidate db.Property) string {
	city := strings.TrimSpace(derefString(candidate.City))
	state := strings.TrimSpace(derefString(candidate.State))
	switch {
	case city != "" && state != "":
		return city + "/" + state
	case city != "":
		return city
	default:
		return state
	}
}

// areaSqm formata a área do candidato. O anúncio traz AreaRaw (texto livre do
// portal) e o candidato traz um número já consolidado — os dois entram no
// prompt sob o mesmo rótulo para o modelo poder compará-los.
func areaSqm(value *float64) string {
	if value == nil {
		return ""
	}
	return fmt.Sprintf("%g m²", *value)
}

// matchContentBlocks monta o conteúdo da mensagem: o texto da comparação e, em
// seguida, pares rótulo + imagem, para que o modelo saiba a que lado cada foto
// pertence — uma sequência de imagens soltas seria inútil aqui.
//
// Os tetos (maxListingImages, maxCandidateImages, maxTotalImages) existem por
// custo: cada foto vale centenas de tokens numa chamada que roda por anúncio.
func matchContentBlocks(prompt string, listing MatchListing, candidates []db.Property) []anthropic.ContentBlockParamUnion {
	blocks := []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock(prompt)}
	remaining := maxTotalImages

	appendImages := func(label string, urls []string, limit int) {
		count := 0
		for _, url := range urls {
			if count >= limit || remaining <= 0 {
				return
			}
			url = strings.TrimSpace(url)
			if url == "" {
				continue
			}
			count++
			remaining--
			blocks = append(blocks,
				anthropic.NewTextBlock(fmt.Sprintf("Foto %d do %s:", count, label)),
				anthropic.NewImageBlock(anthropic.URLImageSourceParam{URL: url}),
			)
		}
	}

	appendImages("ANÚNCIO", listing.ImageURLs, maxListingImages)
	for _, candidate := range candidates {
		appendImages("CANDIDATE "+candidate.ID, candidate.Photos, maxCandidateImages)
	}

	return blocks
}

// propertyMatchToolCall extrai o bloco tool_use da resposta. Percorre todos os
// blocos porque o modelo pode emitir texto antes da chamada da ferramenta.
func propertyMatchToolCall(message *anthropic.Message) (*propertyMatchToolInput, error) {
	for _, block := range message.Content {
		use, ok := block.AsAny().(anthropic.ToolUseBlock)
		if !ok || use.Name != propertyMatchToolName {
			continue
		}

		var input propertyMatchToolInput
		if err := json.Unmarshal(use.Input, &input); err != nil {
			return nil, fmt.Errorf("decoding tool input: %w", err)
		}
		return &input, nil
	}
	return nil, fmt.Errorf("%w (stop_reason=%q)", ErrNoPropertyMatchToolUse, message.StopReason)
}

func (in *propertyMatchToolInput) toPropertyMatch() (PropertyMatch, error) {
	if in.Confidence < 0 || in.Confidence > 1 {
		return PropertyMatch{}, fmt.Errorf("%w: %v", ErrInvalidConfidence, in.Confidence)
	}

	return PropertyMatch{
		SameProperty: in.SameProperty,
		PropertyID:   strings.TrimSpace(in.PropertyID),
		Confidence:   in.Confidence,
		Reason:       strings.TrimSpace(in.Reason),
	}, nil
}
