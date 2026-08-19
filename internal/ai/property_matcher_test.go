package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/imobhub/api/internal/db"
)

// matchContentBlock cobre tanto os blocos de texto quanto os de imagem — o
// capturedRequest de selector_analyzer_test.go não modela `source`, e este
// arquivo precisa conferir a URL de cada foto enviada.
type matchContentBlock struct {
	Type   string `json:"type"`
	Text   string `json:"text"`
	Source struct {
		Type string `json:"type"`
		URL  string `json:"url"`
	} `json:"source"`
}

type capturedMatchRequest struct {
	Model     string `json:"model"`
	MaxTokens int64  `json:"max_tokens"`
	System    []struct {
		Text string `json:"text"`
	} `json:"system"`
	Messages []struct {
		Role    string              `json:"role"`
		Content []matchContentBlock `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		InputSchema struct {
			Type       string                     `json:"type"`
			Required   []string                   `json:"required"`
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"input_schema"`
	} `json:"tools"`
	ToolChoice struct {
		Type string `json:"type"`
		Name string `json:"name"`
	} `json:"tool_choice"`
}

// newMatcherClient aponta o SDK para um httptest.Server. O handler recebe o
// índice da requisição para que um teste possa responder diferente na primeira
// e na segunda — é o que permite exercitar o retry somente-texto.
func newMatcherClient(t *testing.T, handler func(index int, w http.ResponseWriter)) (*Client, *[]capturedMatchRequest) {
	t.Helper()

	captured := &[]capturedMatchRequest{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading request body: %v", err)
		}
		var request capturedMatchRequest
		if err := json.Unmarshal(body, &request); err != nil {
			t.Errorf("decoding request body: %v", err)
		}
		index := len(*captured)
		*captured = append(*captured, request)

		w.Header().Set("Content-Type", "application/json")
		handler(index, w)
	}))
	t.Cleanup(server.Close)

	client, err := newClient("test-key", option.WithBaseURL(server.URL))
	if err != nil {
		t.Fatalf("newClient() error = %v, want nil", err)
	}
	return client, captured
}

// alwaysRespond devolve o mesmo corpo em toda requisição.
func alwaysRespond(body string) func(int, http.ResponseWriter) {
	return func(_ int, w http.ResponseWriter) {
		_, _ = io.WriteString(w, body)
	}
}

func matchToolUseResponse(input string) string {
	return `{
		"id": "msg_01",
		"type": "message",
		"role": "assistant",
		"model": "claude-sonnet-4-5",
		"stop_reason": "tool_use",
		"usage": {"input_tokens": 10, "output_tokens": 20},
		"content": [
			{"type": "text", "text": "Comparei o anúncio com os candidatos."},
			{"type": "tool_use", "id": "toolu_01", "name": "` + propertyMatchToolName + `", "input": ` + input + `}
		]
	}`
}

const validMatchInput = `{
	"same_property": true,
	"confidence": 0.93,
	"property_id": "  11111111-1111-1111-1111-111111111111  ",
	"reason": "  Mesmo endereço, mesma metragem e a mesma varanda nas fotos.  "
}`

func matchTestListing() MatchListing {
	bedrooms := 2
	return MatchListing{
		AddressRaw:     "Rua das Flores, 100",
		Neighborhood:   "Centro",
		AreaRaw:        "72 m²",
		TitleRaw:       "Apartamento 2 quartos no Centro",
		DescriptionRaw: "Apartamento reformado, varanda e vaga.",
		BedroomCount:   &bedrooms,
		ImageURLs:      []string{"https://exemplo.com.br/a1.jpg", "https://exemplo.com.br/a2.jpg"},
	}
}

func matchTestCandidate(id string, photos ...string) db.Property {
	address := "Rua das Flores, 100"
	neighborhood := "Centro"
	city := "São Paulo"
	state := "SP"
	propertyType := "apartamento"
	bedrooms := 2
	area := 72.5
	description := "Imóvel canônico consolidado."
	return db.Property{
		ID:               id,
		CanonicalAddress: &address,
		Neighborhood:     &neighborhood,
		City:             &city,
		State:            &state,
		BedroomCount:     &bedrooms,
		AreaSqm:          &area,
		PropertyType:     &propertyType,
		Description:      &description,
		Photos:           photos,
	}
}

func TestMatchPropertyParsesToolUse(t *testing.T) {
	client, _ := newMatcherClient(t, alwaysRespond(matchToolUseResponse(validMatchInput)))

	match, err := client.MatchProperty(context.Background(), matchTestListing(),
		[]db.Property{matchTestCandidate("11111111-1111-1111-1111-111111111111")})
	if err != nil {
		t.Fatalf("MatchProperty() error = %v, want nil", err)
	}

	if !match.SameProperty {
		t.Error("SameProperty = false, want true")
	}
	if match.Confidence != 0.93 {
		t.Errorf("Confidence = %v, want 0.93", match.Confidence)
	}
	// O id e a justificativa chegam sem espaços nas bordas: o id vai ser
	// comparado com a lista de candidatos pelo chamador.
	if match.PropertyID != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("PropertyID = %q, want the trimmed id", match.PropertyID)
	}
	if match.Reason != "Mesmo endereço, mesma metragem e a mesma varanda nas fotos." {
		t.Errorf("Reason = %q, want the trimmed reason", match.Reason)
	}
}

func TestMatchPropertyParsesNegativeVerdict(t *testing.T) {
	input := `{"same_property": false, "confidence": 0.2, "property_id": "", "reason": "Endereços diferentes."}`
	client, _ := newMatcherClient(t, alwaysRespond(matchToolUseResponse(input)))

	match, err := client.MatchProperty(context.Background(), matchTestListing(),
		[]db.Property{matchTestCandidate("prop-a")})
	if err != nil {
		t.Fatalf("MatchProperty() error = %v, want nil", err)
	}
	if match.SameProperty || match.PropertyID != "" {
		t.Errorf("match = %+v, want a negative verdict with an empty id", match)
	}
}

func TestMatchPropertyFailsWithoutToolUse(t *testing.T) {
	response := `{
		"id": "msg_01",
		"type": "message",
		"role": "assistant",
		"model": "claude-sonnet-4-5",
		"stop_reason": "end_turn",
		"usage": {"input_tokens": 10, "output_tokens": 20},
		"content": [{"type": "text", "text": "Acho que sim, mas não tenho certeza."}]
	}`
	client, _ := newMatcherClient(t, alwaysRespond(response))

	_, err := client.MatchProperty(context.Background(), matchTestListing(),
		[]db.Property{matchTestCandidate("prop-a")})
	if !errors.Is(err, ErrNoPropertyMatchToolUse) {
		t.Fatalf("error = %v, want ErrNoPropertyMatchToolUse", err)
	}
	if !strings.Contains(err.Error(), "end_turn") {
		t.Errorf("error %q does not report the stop_reason", err)
	}
}

// Uma confiança fora de [0,1] corromperia a comparação com o threshold de
// agrupamento — é erro, não recorte para a borda.
func TestMatchPropertyRejectsConfidenceOutsideUnitInterval(t *testing.T) {
	for _, raw := range []string{"1.4", "-0.2"} {
		t.Run(raw, func(t *testing.T) {
			input := fmt.Sprintf(`{"same_property": true, "confidence": %s, "property_id": "prop-a", "reason": "x"}`, raw)
			client, _ := newMatcherClient(t, alwaysRespond(matchToolUseResponse(input)))

			_, err := client.MatchProperty(context.Background(), matchTestListing(),
				[]db.Property{matchTestCandidate("prop-a")})
			if !errors.Is(err, ErrInvalidConfidence) {
				t.Fatalf("error = %v, want ErrInvalidConfidence", err)
			}
			if !strings.Contains(err.Error(), raw) {
				t.Errorf("error %q does not report the offending value %q", err, raw)
			}
		})
	}
}

// Sem candidatos não há o que comparar: a API não pode ser chamada.
func TestMatchPropertyRejectsEmptyCandidatesWithoutCallingTheAPI(t *testing.T) {
	called := false
	client, _ := newMatcherClient(t, func(int, http.ResponseWriter) { called = true })

	_, err := client.MatchProperty(context.Background(), matchTestListing(), nil)
	if !errors.Is(err, ErrNoCandidates) {
		t.Fatalf("error = %v, want ErrNoCandidates", err)
	}
	if called {
		t.Error("the API was called with an empty candidate list")
	}
}

func TestMatchPropertySendsToolAndForcesItsUse(t *testing.T) {
	client, captured := newMatcherClient(t, alwaysRespond(matchToolUseResponse(validMatchInput)))

	if _, err := client.MatchProperty(context.Background(), matchTestListing(),
		[]db.Property{matchTestCandidate("prop-a")}); err != nil {
		t.Fatalf("MatchProperty() error = %v, want nil", err)
	}

	if len(*captured) != 1 {
		t.Fatalf("requests = %d, want exactly 1 (the API is paid per listing)", len(*captured))
	}
	request := (*captured)[0]

	if request.Model != string(MatcherModel) {
		t.Errorf("model = %q, want %q", request.Model, MatcherModel)
	}
	if request.Model != "claude-sonnet-4-5" {
		t.Errorf("model = %q, want claude-sonnet-4-5", request.Model)
	}
	if len(request.Tools) != 1 {
		t.Fatalf("len(tools) = %d, want 1", len(request.Tools))
	}
	tool := request.Tools[0]
	if tool.Name != propertyMatchToolName {
		t.Errorf("tool name = %q, want %q", tool.Name, propertyMatchToolName)
	}
	if tool.InputSchema.Type != "object" {
		t.Errorf("input_schema.type = %q, want object", tool.InputSchema.Type)
	}

	wantFields := []string{"same_property", "confidence", "property_id", "reason"}
	for _, field := range wantFields {
		property, ok := tool.InputSchema.Properties[field]
		if !ok {
			t.Errorf("input_schema.properties[%q] missing", field)
			continue
		}
		var described struct {
			Description string `json:"description"`
		}
		if err := json.Unmarshal(property, &described); err != nil {
			t.Errorf("decoding property %q: %v", field, err)
			continue
		}
		if described.Description == "" {
			t.Errorf("property %q has no description", field)
		}
	}
	if len(tool.InputSchema.Required) != len(wantFields) {
		t.Errorf("required = %v, want the %d fields %v", tool.InputSchema.Required, len(wantFields), wantFields)
	}

	if request.ToolChoice.Type != "tool" || request.ToolChoice.Name != propertyMatchToolName {
		t.Errorf("tool_choice = %+v, want {tool %s}", request.ToolChoice, propertyMatchToolName)
	}

	if len(request.System) == 0 {
		t.Fatal("system prompt is empty")
	}
	system := request.System[0].Text
	for _, fragment := range []string{"same physical property", "não informado", "no match"} {
		if !strings.Contains(system, fragment) {
			t.Errorf("system prompt does not mention %q", fragment)
		}
	}
}

// O modelo precisa saber a que lado cada foto pertence, e os tetos existem
// porque cada imagem custa centenas de tokens numa chamada feita por anúncio.
func TestMatchPropertySendsLabelledImageBlocksWithinTheCaps(t *testing.T) {
	client, captured := newMatcherClient(t, alwaysRespond(matchToolUseResponse(validMatchInput)))

	listing := matchTestListing()
	listing.ImageURLs = []string{
		"https://exemplo.com.br/a1.jpg",
		"   ", // URLs em branco são descartadas sem consumir a cota
		"https://exemplo.com.br/a2.jpg",
		"https://exemplo.com.br/a3.jpg",
		"https://exemplo.com.br/a4.jpg", // acima de maxListingImages
	}
	candidates := []db.Property{
		matchTestCandidate("prop-a", "https://exemplo.com.br/c1.jpg", "https://exemplo.com.br/c2.jpg", "https://exemplo.com.br/c3.jpg"),
		matchTestCandidate("prop-b", "https://exemplo.com.br/d1.jpg"),
		matchTestCandidate("prop-c"),
	}

	if _, err := client.MatchProperty(context.Background(), listing, candidates); err != nil {
		t.Fatalf("MatchProperty() error = %v, want nil", err)
	}

	content := (*captured)[0].Messages[0].Content
	if content[0].Type != "text" || !strings.Contains(content[0].Text, "ANÚNCIO") {
		t.Fatalf("first block = %+v, want the comparison text", content[0])
	}

	var images []string
	var labels []string
	for i, block := range content[1:] {
		if block.Type != "image" {
			continue
		}
		if block.Source.Type != "url" {
			t.Errorf("image source type = %q, want url", block.Source.Type)
		}
		images = append(images, block.Source.URL)
		// O rótulo é o bloco de texto imediatamente anterior.
		labels = append(labels, content[i].Text)
	}

	wantImages := []string{
		"https://exemplo.com.br/a1.jpg", "https://exemplo.com.br/a2.jpg", "https://exemplo.com.br/a3.jpg",
		"https://exemplo.com.br/c1.jpg", "https://exemplo.com.br/c2.jpg",
		"https://exemplo.com.br/d1.jpg",
	}
	if len(images) != len(wantImages) {
		t.Fatalf("images = %v, want %v", images, wantImages)
	}
	for i, want := range wantImages {
		if images[i] != want {
			t.Errorf("image[%d] = %q, want %q", i, images[i], want)
		}
	}

	wantLabels := []string{"ANÚNCIO", "ANÚNCIO", "ANÚNCIO", "CANDIDATE prop-a", "CANDIDATE prop-a", "CANDIDATE prop-b"}
	for i, want := range wantLabels {
		if !strings.Contains(labels[i], want) {
			t.Errorf("label[%d] = %q, want it to mention %q", i, labels[i], want)
		}
	}
}

func TestMatchPropertyRespectsTheGlobalImageCap(t *testing.T) {
	client, captured := newMatcherClient(t, alwaysRespond(matchToolUseResponse(validMatchInput)))

	listing := matchTestListing()
	listing.ImageURLs = []string{"https://exemplo.com.br/a1.jpg", "https://exemplo.com.br/a2.jpg", "https://exemplo.com.br/a3.jpg"}

	var candidates []db.Property
	for i := range 8 {
		id := fmt.Sprintf("prop-%d", i)
		candidates = append(candidates, matchTestCandidate(id,
			fmt.Sprintf("https://exemplo.com.br/%s-1.jpg", id),
			fmt.Sprintf("https://exemplo.com.br/%s-2.jpg", id),
		))
	}

	if _, err := client.MatchProperty(context.Background(), listing, candidates); err != nil {
		t.Fatalf("MatchProperty() error = %v, want nil", err)
	}

	images := 0
	for _, block := range (*captured)[0].Messages[0].Content {
		if block.Type == "image" {
			images++
		}
	}
	if images != maxTotalImages {
		t.Errorf("images = %d, want %d (the global cap)", images, maxTotalImages)
	}
}

// Perder a comparação textual por causa de uma foto inacessível seria o pior
// resultado possível: a API rejeita as imagens com 400 e a requisição é
// refeita uma única vez, só com o texto.
func TestMatchPropertyRetriesWithoutImagesAfterRejection(t *testing.T) {
	client, captured := newMatcherClient(t, func(index int, w http.ResponseWriter) {
		if index == 0 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"type": "error", "error": {"type": "invalid_request_error", "message": "Could not fetch image"}}`)
			return
		}
		_, _ = io.WriteString(w, matchToolUseResponse(validMatchInput))
	})

	match, err := client.MatchProperty(context.Background(), matchTestListing(),
		[]db.Property{matchTestCandidate("prop-a", "https://exemplo.com.br/c1.jpg")})
	if err != nil {
		t.Fatalf("MatchProperty() error = %v, want nil", err)
	}
	if !match.SameProperty {
		t.Error("SameProperty = false, want the verdict of the text-only retry")
	}

	if len(*captured) != 2 {
		t.Fatalf("requests = %d, want exactly 2 (one with images, one without)", len(*captured))
	}

	first := (*captured)[0].Messages[0].Content
	if countImages(first) == 0 {
		t.Error("the first request carried no image; there is nothing for the fallback to drop")
	}

	second := (*captured)[1].Messages[0].Content
	if countImages(second) != 0 {
		t.Errorf("the text-only retry still carries %d image block(s)", countImages(second))
	}
	if len(second) != 1 || second[0].Type != "text" {
		t.Errorf("text-only retry content = %+v, want a single text block", second)
	}
	if !strings.Contains(second[0].Text, "ANÚNCIO") {
		t.Error("the text-only retry lost the comparison text")
	}
}

// Um 429 (rate limit) ou um 500 não são rejeição de imagem: repetir sem as
// fotos gastaria dinheiro de novo pelo mesmo motivo.
func TestMatchPropertyDoesNotRetryOnNonImageErrors(t *testing.T) {
	client, captured := newMatcherClient(t, func(_ int, w http.ResponseWriter) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"type": "error", "error": {"type": "authentication_error", "message": "invalid api key"}}`)
	})

	if _, err := client.MatchProperty(context.Background(), matchTestListing(),
		[]db.Property{matchTestCandidate("prop-a", "https://exemplo.com.br/c1.jpg")}); err == nil {
		t.Fatal("MatchProperty() error = nil, want the API error")
	}

	if len(*captured) != 1 {
		t.Errorf("requests = %d, want 1 (no text-only retry for a non-image error)", len(*captured))
	}
}

func countImages(blocks []matchContentBlock) int {
	count := 0
	for _, block := range blocks {
		if block.Type == "image" {
			count++
		}
	}
	return count
}

// Campo ausente sai como "não informado"; preencher com um valor plausível
// faria o modelo comparar dados inventados.
func TestPropertyMatchPromptMarksAbsentFields(t *testing.T) {
	prompt := propertyMatchPrompt(
		MatchListing{TitleRaw: "Apartamento no Centro"},
		[]db.Property{{ID: "prop-vazio"}},
	)

	for _, label := range []string{"endereço", "bairro", "quartos", "área", "descrição", "cidade/UF", "tipo de imóvel"} {
		if !strings.Contains(prompt, "- "+label+": "+unknownValue) {
			t.Errorf("prompt does not mark %q as %q:\n%s", label, unknownValue, prompt)
		}
	}
	if !strings.Contains(prompt, "CANDIDATE prop-vazio") {
		t.Error("prompt does not label the candidate with its id")
	}
	if !strings.Contains(prompt, "Apartamento no Centro") {
		t.Error("prompt does not carry the listing title")
	}
}

func TestPropertyMatchPromptCarriesEveryKnownField(t *testing.T) {
	prompt := propertyMatchPrompt(matchTestListing(), []db.Property{matchTestCandidate("prop-a")})

	for _, fragment := range []string{
		"Rua das Flores, 100", "Centro", "- quartos: 2", "72 m²",
		"Apartamento 2 quartos no Centro", "varanda e vaga",
		"CANDIDATE prop-a", "São Paulo/SP", "72.5 m²", "apartamento",
	} {
		if !strings.Contains(prompt, fragment) {
			t.Errorf("prompt does not carry %q:\n%s", fragment, prompt)
		}
	}
}

// Descrições de portal são longas; o corte é por runes para não partir um
// acento ao meio.
func TestPropertyMatchPromptTruncatesLongDescriptions(t *testing.T) {
	listing := matchTestListing()
	listing.DescriptionRaw = strings.Repeat("ç", MaxMatchDescriptionChars+500)

	prompt := propertyMatchPrompt(listing, []db.Property{matchTestCandidate("prop-a")})

	_, rest, found := strings.Cut(prompt, "- descrição: ")
	if !found {
		t.Fatalf("prompt has no description line:\n%s", prompt)
	}
	line, _, _ := strings.Cut(rest, "\n")
	if !strings.HasSuffix(line, " [...]") {
		t.Errorf("description line does not mark the truncation: %q", line)
	}
	body := strings.TrimSuffix(line, " [...]")
	if got := len([]rune(body)); got != MaxMatchDescriptionChars {
		t.Errorf("description length = %d chars, want %d", got, MaxMatchDescriptionChars)
	}
}
