# internal/ai

## Purpose
Encapsula a integração com a API da Anthropic. Será usado pelo pacote
`selectors` para descobrir seletores CSS de fontes novas, cujo HTML ainda não
tem mapeamento conhecido.

## Key decisions
- **Wrapper em vez de uso direto do SDK.** Os pacotes de negócio dependem de
  `ai`, nunca de `github.com/anthropics/anthropic-sdk-go`. Assim, trocar o
  modelo, mudar a estratégia de retry ou instrumentar as chamadas é uma mudança
  local em vez de um sweep pelo repositório.
- **`DefaultModel` como constante do pacote.** Atualizar a versão do modelo é
  uma mudança de uma linha e fica registrada no diff, em vez de espalhada por
  cada call site.
- **A API key é passada explicitamente para `New`**, em vez de deixar o SDK ler
  `ANTHROPIC_API_KEY` do ambiente por conta própria. Mantém a regra do projeto
  de que toda configuração passa por `internal/config`, e torna o pacote
  testável sem mexer no ambiente.

## Gotchas
- `anthropic.NewClient` devolve um **valor** (`anthropic.Client`), não um
  ponteiro. Por isso `Client.api` é um campo por valor e `API()` retorna
  `&c.api`. Copiar o `Client` do SDK é barato, mas evite copiar o nosso `Client`
  por valor se um dia ele ganhar mutex ou cache.
- `API()` expõe o client bruto de propósito enquanto não há um caso de uso
  concreto para modelar. Assim que o primeiro fluxo real existir (descoberta de
  seletores), prefira adicionar um método com a intenção de negócio
  (ex.: `DiscoverSelectors`) e reduzir a superfície de `API()`.
- Este pacote ainda não faz chamadas à API — não há consumo de tokens nem custo
  associado até que `selectors` seja implementado.

## Dependencies
`github.com/anthropics/anthropic-sdk-go` (+ `/option`). Será importado por
`internal/selectors`.
