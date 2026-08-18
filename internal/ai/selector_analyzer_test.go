package ai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/imobhub/api/internal/db"
)

// capturedRequest é o corpo enviado ao endpoint /v1/messages, desserializado só
// nos campos que os testes checam.
type capturedRequest struct {
	Model     string `json:"model"`
	MaxTokens int64  `json:"max_tokens"`
	System    []struct {
		Text string `json:"text"`
	} `json:"system"`
	Messages []struct {
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
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

// newAnalyzerClient devolve um Client apontado para um servidor que responde
// com responseBody, e um ponteiro para a última requisição recebida.
func newAnalyzerClient(t *testing.T, responseBody string) (*Client, *capturedRequest) {
	t.Helper()

	captured := &capturedRequest{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading request body: %v", err)
		}
		if err := json.Unmarshal(body, captured); err != nil {
			t.Errorf("decoding request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, responseBody)
	}))
	t.Cleanup(server.Close)

	client, err := newClient("test-key", option.WithBaseURL(server.URL))
	if err != nil {
		t.Fatalf("newClient() error = %v, want nil", err)
	}
	return client, captured
}

// toolUseResponse monta uma resposta da API com um único bloco tool_use cujo
// input é o JSON informado.
func toolUseResponse(input string) string {
	return `{
		"id": "msg_01",
		"type": "message",
		"role": "assistant",
		"model": "claude-haiku-4-5",
		"stop_reason": "tool_use",
		"usage": {"input_tokens": 10, "output_tokens": 20},
		"content": [
			{"type": "text", "text": "Analisei a página."},
			{"type": "tool_use", "id": "toolu_01", "name": "` + selectorToolName + `", "input": ` + input + `}
		]
	}`
}

const validToolInput = `{
	"listing_container": " .card-imovel ",
	"title": ".card-titulo",
	"price": ".card-preco",
	"address": ".card-endereco",
	"description": ".card-descricao",
	"image": "img.card-foto",
	"listing_url": "a.card-link",
	"render_mode": "static"
}`

func TestAnalyzeSelectorsReturnsFieldsAndRenderMode(t *testing.T) {
	client, _ := newAnalyzerClient(t, toolUseResponse(validToolInput))

	fields, renderMode, err := client.analyzeSelectors(context.Background(), "<html><div class=\"card-imovel\"></div></html>", "https://exemplo.com.br/imoveis")
	if err != nil {
		t.Fatalf("analyzeSelectors() error = %v, want nil", err)
	}

	// O container vem com espaços de propósito: todos os seletores são
	// normalizados antes de virar SelectorFields.
	want := db.SelectorFields{
		ListingContainer: ".card-imovel",
		Title:            ".card-titulo",
		Price:            ".card-preco",
		Address:          ".card-endereco",
		Description:      ".card-descricao",
		Image:            "img.card-foto",
		ListingURL:       "a.card-link",
	}
	if *fields != want {
		t.Errorf("fields = %+v, want %+v", *fields, want)
	}
	if renderMode != db.RenderModeStatic {
		t.Errorf("renderMode = %q, want %q", renderMode, db.RenderModeStatic)
	}
}

func TestAnalyzeSelectorsAcceptsHeadlessRenderMode(t *testing.T) {
	input := strings.Replace(validToolInput, `"render_mode": "static"`, `"render_mode": "HEADLESS"`, 1)
	client, _ := newAnalyzerClient(t, toolUseResponse(input))

	_, renderMode, err := client.analyzeSelectors(context.Background(), "<html></html>", "https://exemplo.com.br")
	if err != nil {
		t.Fatalf("analyzeSelectors() error = %v, want nil", err)
	}
	if renderMode != db.RenderModeHeadless {
		t.Errorf("renderMode = %q, want %q", renderMode, db.RenderModeHeadless)
	}
}

// A requisição precisa levar o modelo barato, a ferramenta com todos os campos
// obrigatórios e o tool_choice forçado — sem isso o modelo pode responder em
// texto livre e a análise falha.
func TestAnalyzeSelectorsSendsToolAndForcesItsUse(t *testing.T) {
	client, captured := newAnalyzerClient(t, toolUseResponse(validToolInput))

	if _, _, err := client.analyzeSelectors(context.Background(), "<html></html>", "https://exemplo.com.br"); err != nil {
		t.Fatalf("analyzeSelectors() error = %v, want nil", err)
	}

	if captured.Model != string(SelectorModel) {
		t.Errorf("model = %q, want %q", captured.Model, SelectorModel)
	}
	if captured.Model != "claude-haiku-4-5" {
		t.Errorf("model = %q, want claude-haiku-4-5", captured.Model)
	}
	if len(captured.Tools) != 1 {
		t.Fatalf("len(tools) = %d, want 1", len(captured.Tools))
	}
	tool := captured.Tools[0]
	if tool.Name != selectorToolName {
		t.Errorf("tool name = %q, want %q", tool.Name, selectorToolName)
	}
	if tool.InputSchema.Type != "object" {
		t.Errorf("input_schema.type = %q, want object", tool.InputSchema.Type)
	}

	wantFields := []string{
		"listing_container", "title", "price", "address",
		"description", "image", "listing_url", "render_mode",
	}
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

	if captured.ToolChoice.Type != "tool" || captured.ToolChoice.Name != selectorToolName {
		t.Errorf("tool_choice = %+v, want {tool %s}", captured.ToolChoice, selectorToolName)
	}
}

// render_mode precisa ser um enum: um valor livre quebraria a CHECK constraint
// de site_selectors só na hora do INSERT.
func TestAnalyzeSelectorsDeclaresRenderModeEnum(t *testing.T) {
	client, captured := newAnalyzerClient(t, toolUseResponse(validToolInput))

	if _, _, err := client.analyzeSelectors(context.Background(), "<html></html>", "https://exemplo.com.br"); err != nil {
		t.Fatalf("analyzeSelectors() error = %v, want nil", err)
	}

	var renderMode struct {
		Enum []string `json:"enum"`
	}
	if err := json.Unmarshal(captured.Tools[0].InputSchema.Properties["render_mode"], &renderMode); err != nil {
		t.Fatalf("decoding render_mode property: %v", err)
	}
	want := []string{db.RenderModeStatic, db.RenderModeHeadless}
	if len(renderMode.Enum) != len(want) {
		t.Fatalf("render_mode enum = %v, want %v", renderMode.Enum, want)
	}
	for i, value := range want {
		if renderMode.Enum[i] != value {
			t.Errorf("render_mode enum[%d] = %q, want %q", i, renderMode.Enum[i], value)
		}
	}
}

// O prompt do sistema carrega o contexto de imobiliária brasileira e as regras
// de seletor relativo/estável. Sem isso o modelo devolve seletores absolutos.
func TestAnalyzeSelectorsSystemPromptCarriesTheRules(t *testing.T) {
	client, captured := newAnalyzerClient(t, toolUseResponse(validToolInput))

	if _, _, err := client.analyzeSelectors(context.Background(), "<html></html>", "https://exemplo.com.br"); err != nil {
		t.Fatalf("analyzeSelectors() error = %v, want nil", err)
	}

	if len(captured.System) == 0 {
		t.Fatal("system prompt is empty")
	}
	system := captured.System[0].Text
	for _, fragment := range []string{"real estate", "RELATIVE", "nth-child", "headless"} {
		if !strings.Contains(system, fragment) {
			t.Errorf("system prompt does not mention %q", fragment)
		}
	}
}

func TestAnalyzeSelectorsTruncatesHTML(t *testing.T) {
	client, captured := newAnalyzerClient(t, toolUseResponse(validToolInput))

	// Caracteres multibyte: truncar por bytes cortaria um "ç" ao meio e o
	// prompt sairia com UTF-8 inválido.
	html := strings.Repeat("ç", MaxHTMLChars+5_000)
	if _, _, err := client.analyzeSelectors(context.Background(), html, "https://exemplo.com.br"); err != nil {
		t.Fatalf("analyzeSelectors() error = %v, want nil", err)
	}

	prompt := captured.Messages[0].Content[0].Text
	_, body, found := strings.Cut(prompt, "HTML:\n")
	if !found {
		t.Fatalf("prompt has no HTML section: %q", prompt[:min(len(prompt), 200)])
	}
	if got := len([]rune(body)); got != MaxHTMLChars {
		t.Errorf("HTML length = %d chars, want %d", got, MaxHTMLChars)
	}
	if !strings.Contains(prompt, "truncated") {
		t.Error("prompt does not warn the model that the HTML was truncated")
	}
	if !strings.Contains(prompt, "https://exemplo.com.br") {
		t.Error("prompt does not carry the site URL")
	}
}

func TestAnalyzeSelectorsSendsShortHTMLWhole(t *testing.T) {
	client, captured := newAnalyzerClient(t, toolUseResponse(validToolInput))

	html := "<html><body>Imóvel à venda</body></html>"
	if _, _, err := client.analyzeSelectors(context.Background(), html, "https://exemplo.com.br"); err != nil {
		t.Fatalf("analyzeSelectors() error = %v, want nil", err)
	}

	prompt := captured.Messages[0].Content[0].Text
	if !strings.HasSuffix(prompt, html) {
		t.Errorf("prompt does not end with the original HTML: %q", prompt)
	}
	if strings.Contains(prompt, "truncated") {
		t.Error("prompt warns about truncation but the HTML fits")
	}
}

func TestAnalyzeSelectorsFailsWithoutToolUse(t *testing.T) {
	response := `{
		"id": "msg_01",
		"type": "message",
		"role": "assistant",
		"model": "claude-haiku-4-5",
		"stop_reason": "end_turn",
		"usage": {"input_tokens": 10, "output_tokens": 20},
		"content": [{"type": "text", "text": "Não consegui analisar esta página."}]
	}`
	client, _ := newAnalyzerClient(t, response)

	_, _, err := client.analyzeSelectors(context.Background(), "<html></html>", "https://exemplo.com.br")
	if !errors.Is(err, ErrNoToolUse) {
		t.Fatalf("error = %v, want ErrNoToolUse", err)
	}
	if !strings.Contains(err.Error(), "https://exemplo.com.br") {
		t.Errorf("error %q does not identify the site", err)
	}
}

func TestAnalyzeSelectorsFailsWithoutListingContainer(t *testing.T) {
	input := strings.Replace(validToolInput, `"listing_container": " .card-imovel "`, `"listing_container": "   "`, 1)
	client, _ := newAnalyzerClient(t, toolUseResponse(input))

	_, _, err := client.analyzeSelectors(context.Background(), "<html></html>", "https://exemplo.com.br")
	if !errors.Is(err, ErrNoListingContainer) {
		t.Fatalf("error = %v, want ErrNoListingContainer", err)
	}
}

func TestAnalyzeSelectorsFailsOnInvalidRenderMode(t *testing.T) {
	input := strings.Replace(validToolInput, `"render_mode": "static"`, `"render_mode": "browser"`, 1)
	client, _ := newAnalyzerClient(t, toolUseResponse(input))

	_, _, err := client.analyzeSelectors(context.Background(), "<html></html>", "https://exemplo.com.br")
	if !errors.Is(err, ErrInvalidRenderMode) {
		t.Fatalf("error = %v, want ErrInvalidRenderMode", err)
	}
	if !strings.Contains(err.Error(), "browser") {
		t.Errorf("error %q does not report the offending value", err)
	}
}

// render_mode ausente cai no default "static": renderizar com Chrome é caro, e
// falhar na extração é barato e visível.
func TestAnalyzeSelectorsDefaultsToStaticRenderMode(t *testing.T) {
	input := strings.Replace(validToolInput, `"render_mode": "static"`, `"render_mode": ""`, 1)
	client, _ := newAnalyzerClient(t, toolUseResponse(input))

	_, renderMode, err := client.analyzeSelectors(context.Background(), "<html></html>", "https://exemplo.com.br")
	if err != nil {
		t.Fatalf("analyzeSelectors() error = %v, want nil", err)
	}
	if renderMode != db.RenderModeStatic {
		t.Errorf("renderMode = %q, want %q", renderMode, db.RenderModeStatic)
	}
}

// HTML vazio não deve gastar token nenhum: o servidor não pode ser chamado.
func TestAnalyzeSelectorsRejectsEmptyHTMLWithoutCallingTheAPI(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	t.Cleanup(server.Close)

	client, err := newClient("test-key", option.WithBaseURL(server.URL))
	if err != nil {
		t.Fatalf("newClient() error = %v, want nil", err)
	}

	if _, _, err := client.analyzeSelectors(context.Background(), "   \n\t ", "https://exemplo.com.br"); !errors.Is(err, ErrEmptyHTML) {
		t.Fatalf("error = %v, want ErrEmptyHTML", err)
	}
	if called {
		t.Error("the API was called for an empty HTML")
	}
}

func TestAnalyzeSelectorsRequiresAPIKey(t *testing.T) {
	_, _, err := AnalyzeSelectors(context.Background(), "", "<html></html>", "https://exemplo.com.br")
	if !errors.Is(err, ErrMissingAPIKey) {
		t.Fatalf("error = %v, want ErrMissingAPIKey", err)
	}
}

func TestTruncateChars(t *testing.T) {
	tests := []struct {
		name          string
		input         string
		max           int
		want          string
		wantTruncated bool
	}{
		{name: "shorter than limit", input: "abc", max: 5, want: "abc"},
		{name: "exactly at limit", input: "abcde", max: 5, want: "abcde"},
		{name: "longer than limit", input: "abcdef", max: 5, want: "abcde", wantTruncated: true},
		{name: "multibyte is not split", input: "ççç", max: 2, want: "çç", wantTruncated: true},
		{name: "empty", input: "", max: 5, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, truncated := truncateChars(tt.input, tt.max)
			if got != tt.want {
				t.Errorf("truncateChars(%q, %d) = %q, want %q", tt.input, tt.max, got, tt.want)
			}
			if truncated != tt.wantTruncated {
				t.Errorf("truncated = %v, want %v", truncated, tt.wantTruncated)
			}
		})
	}
}
