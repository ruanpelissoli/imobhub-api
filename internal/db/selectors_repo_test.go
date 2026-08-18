package db

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDecodeSelectorsMapsJSONKeys(t *testing.T) {
	// As chaves são o contrato gravado no banco: se alguém renomear uma tag
	// `json` em SelectorFields, este teste quebra antes de as linhas já
	// persistidas ficarem ilegíveis.
	raw := []byte(`{
		"listing_container": ".card",
		"title": "h2",
		"price": ".price",
		"address": ".address",
		"description": ".desc",
		"image": "img",
		"listing_url": "a.link"
	}`)

	fields, err := decodeSelectors(raw)
	if err != nil {
		t.Fatalf("decodeSelectors() error = %v, want nil", err)
	}

	want := SelectorFields{
		ListingContainer: ".card",
		Title:            "h2",
		Price:            ".price",
		Address:          ".address",
		Description:      ".desc",
		Image:            "img",
		ListingURL:       "a.link",
	}
	if fields != want {
		t.Errorf("decodeSelectors() = %+v, want %+v", fields, want)
	}
}

func TestDecodeSelectorsIgnoresUnknownKeys(t *testing.T) {
	// A IA pode devolver campos extras; derrubar a coleta por causa disso seria
	// pior do que trabalhar com os seletores conhecidos.
	fields, err := decodeSelectors([]byte(`{"title":"h1","pagination":".next"}`))
	if err != nil {
		t.Fatalf("decodeSelectors() error = %v, want nil", err)
	}
	if fields.Title != "h1" {
		t.Errorf("fields.Title = %q, want %q", fields.Title, "h1")
	}
}

func TestDecodeSelectorsRejectsEmptyAndInvalid(t *testing.T) {
	tests := []struct {
		name string
		raw  []byte
	}{
		{name: "coluna vazia", raw: nil},
		{name: "JSON inválido", raw: []byte(`{"title":`)},
		{name: "array em vez de objeto", raw: []byte(`["h1"]`)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := decodeSelectors(tt.raw); err == nil {
				t.Fatal("decodeSelectors() error = nil, want error")
			}
		})
	}
}

func TestNormalizeRenderMode(t *testing.T) {
	tests := []struct {
		name    string
		mode    string
		want    string
		wantErr bool
	}{
		{name: "vazio vira static", mode: "", want: RenderModeStatic},
		{name: "espaços viram static", mode: "   ", want: RenderModeStatic},
		{name: "static", mode: RenderModeStatic, want: RenderModeStatic},
		{name: "headless", mode: RenderModeHeadless, want: RenderModeHeadless},
		{name: "maiúsculas e espaços", mode: " HeadLess ", want: RenderModeHeadless},
		{name: "valor inválido", mode: "browser", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeRenderMode(tt.mode)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("normalizeRenderMode(%q) error = nil, want error", tt.mode)
				}
				// A mensagem precisa citar o valor recebido — é o que a CHECK
				// constraint do PostgreSQL não diria.
				if !strings.Contains(err.Error(), tt.mode) {
					t.Errorf("error = %q, want it to mention %q", err, tt.mode)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeRenderMode(%q) error = %v, want nil", tt.mode, err)
			}
			if got != tt.want {
				t.Errorf("normalizeRenderMode(%q) = %q, want %q", tt.mode, got, tt.want)
			}
		})
	}
}

func TestSelectorFieldsRoundTrip(t *testing.T) {
	// UpsertSelectors grava o resultado deste Marshal e GetSelectorsByDomain o
	// lê de volta: os dois lados precisam usar as mesmas chaves.
	want := SelectorFields{
		ListingContainer: "article.item",
		Title:            "h2.title, h2 a",
		Price:            ".price strong",
		Address:          ".address",
		Description:      ".description",
		Image:            "img.photo",
		ListingURL:       "a.item-link",
	}

	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v, want nil", err)
	}

	got, err := decodeSelectors(raw)
	if err != nil {
		t.Fatalf("decodeSelectors() error = %v, want nil", err)
	}
	if got != want {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
}
