// Package ai concentra a integração com a API da Anthropic. Os pacotes de
// negócio devem depender deste, nunca do SDK diretamente, para que a troca de
// modelo ou de provedor fique restrita a um único lugar.
package ai

import (
	"errors"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// DefaultModel é o modelo usado nas chamadas do scraper. Centralizado aqui para
// que a atualização de versão seja uma mudança de uma linha.
const DefaultModel = anthropic.ModelClaudeOpus4_8

// ErrMissingAPIKey indica que New foi chamado sem chave de API.
var ErrMissingAPIKey = errors.New("ai: anthropic API key is required")

// Client encapsula o SDK da Anthropic.
type Client struct {
	api   anthropic.Client
	model anthropic.Model
}

// New cria um Client com a chave informada. A chave é passada explicitamente
// (em vez de deixar o SDK ler ANTHROPIC_API_KEY do ambiente) para que toda a
// configuração passe pelo pacote config.
func New(apiKey string) (*Client, error) {
	return newClient(apiKey)
}

// newClient é a construção real. As opções extras existem para os testes do
// pacote, que apontam o SDK para um httptest.Server; elas não são expostas
// porque nenhum pacote de negócio deve importar tipos do SDK.
func newClient(apiKey string, opts ...option.RequestOption) (*Client, error) {
	if apiKey == "" {
		return nil, ErrMissingAPIKey
	}

	opts = append([]option.RequestOption{option.WithAPIKey(apiKey)}, opts...)

	return &Client{
		api:   anthropic.NewClient(opts...),
		model: DefaultModel,
	}, nil
}

// API expõe o client do SDK para os pacotes que precisam montar requisições
// específicas. Mantido enquanto não há um caso de uso concreto para envolver.
func (c *Client) API() *anthropic.Client {
	return &c.api
}

// Model retorna o modelo configurado para este client.
func (c *Client) Model() anthropic.Model {
	return c.model
}
