# enrichment/ — normalização de bairros

## Purpose
Deriva a forma canônica do bairro a partir do texto bruto do anúncio. Cada
portal escreve o mesmo lugar de um jeito (`"Jd. Botânico"`, `"JARDIM BOTANICO"`,
`"jardim botanico"`); o agrupamento por região usa
`listings.normalized_neighborhood`, então grafias divergentes do mesmo bairro
quebram o agrupamento em silêncio.

O pacote entrega **só o normalizador puro**: entra string, sai string. Sem
banco, sem rede, sem IA.

## Key decisions
- **Tabela de aliases embutida via `//go:embed`, em JSON, dentro do pacote.**
  `//go:embed` não alcança caminhos acima do diretório do pacote, então
  `configs/*.yaml` está fora. Embutir evita uma variável de ambiente nova (o
  `internal/config` tem ritual de atualização em três lugares) e evita repetir o
  gotcha de `migrations/` e `sources.txt`, que precisam de `COPY` no Dockerfile
  e falham em runtime quando alguém esquece. JSON e não YAML porque
  `go.yaml.in/yaml/v4` é dependência **indireta** — `encoding/json` é stdlib e
  não mexe no `go.mod`.
- **`Setor` NÃO é removido.** Em Brasília e Goiânia, `Setor Sudoeste`,
  `Setor Bueno` e `Setor Oeste` são nomes reais de bairro; removê-la corromperia
  o dado sem deixar rastro. Só `Bairro` é tratada como palavra redundante. Casos
  em que `Setor` de fato sobra vão para a tabela de aliases — que é o mecanismo
  de escape por design. Isto substitui conscientemente o critério original da
  task ("remover sufixos redundantes: Bairro, Setor").
- **Alias vence e volta verbatim.** A tabela manda na grafia final, acentos e
  maiúsculas inclusive — nada de re-aplicar title-case por cima, senão
  `"Águas Claras"` viraria outra coisa a cada ajuste do caser.
- **Acento removido só na chave de comparação.** O fallback devolve a string
  limpa em title-case com os acentos **originais**. Inventar acento
  (`"Acacias"` → `"Acácias"`) exigiria dicionário; errar a grafia é pior que
  preservá-la. Acentuação correta vem por alias.
- **Chave duplicada ⇒ erro, não silêncio.** `encoding/json` aceita chave
  repetida (a última vence), então `AliasMap`/`CityAliasMap` têm `UnmarshalJSON`
  próprio que percorre tokens. Há uma segunda checagem depois de normalizar as
  chaves, que pega colisões lógicas (`"Asa Norte"` vs. `"asa  norte"`).

## Business logic
Pipeline de `Normalize(raw, city)`, nesta ordem: trim + colapso de espaços →
remoção de `Bairro` (prefixo ou sufixo, sem caixa e sem acento, tolerando
pontuação colada) → se sobrou vazio, `""` → chave de busca (minúsculas + sem
acento + espaços colapsados + pontuação de borda descartada) → alias da cidade →
alias global → fallback em title-case pt-BR.

Invariantes:
- Entrada vazia, só espaços ou só pontuação ⇒ `""`. O chamador grava **NULL**,
  nunca string vazia.
- `city == ""` consulta apenas a seção global. Hoje **nenhum chamador tem
  cidade**: `listings` não tem coluna `city` (só `properties` tem). O parâmetro
  existe porque aliases são conceitualmente por cidade ("Centro" existe em toda
  cidade) e para a assinatura não mudar quando o parsing de endereço evoluir.
- Cidade vence global: `"Sudoeste"` é `"Sudoeste"` em geral e `"Setor Sudoeste"`
  em Brasília.
- Função pura e determinística; `Normalize(Normalize(x)) == Normalize(x)`.

## Dependencies
Stdlib + `golang.org/x/text` (`cases`, `language`, `runes`, `transform`,
`unicode/norm`) — já dependência **direta** no `go.mod`, então nada foi somado.
O pacote **não importa `internal/db`**: é folha do grafo, junto com `config`.
Ninguém o consome ainda — persistir `normalized_neighborhood` (fila por
`enriched_at IS NULL`, UPDATE em `internal/db`, wiring em `cmd/scraper`) é task
de follow-up, compartilhada com os demais enrichers.

## Gotchas
- **`cases.Caser` e `transform.Transformer` guardam estado e não são seguros
  para uso concorrente.** Por isso são construídos **por chamada** dentro dos
  helpers, e não guardados como campo do struct — a fila de enriquecimento
  futura é concorrente e o sintoma seria saída corrompida, não panic. Se algum
  profiling mostrar que pesa, a correção é `sync.Pool`, nunca campo
  compartilhado. `TestNormalizeIsSafeForConcurrentUse` existe para flagrar isso;
  `-race` não roda neste ambiente (exige cgo/gcc).
- As chaves do JSON passam pela mesma normalização da entrada, então o arquivo
  pode ser escrito em forma legível (`"Jd. Botânico"`). Consequência: duas
  grafias que colapsam na mesma chave são erro de construção.
- `cases.Title` produziria `"D'água"` (trata o apóstrofo como fim de palavra);
  há uma regra explícita para o prefixo `d'`. Outros casos com apóstrofo ou
  hífen (`"Sant'Ana"`) resolvem-se por alias.
- Pontuação **interna** é preservada na chave: a de `"Jd. Botânico"` é
  `"jd. botanico"` — é assim que a entrada precisa estar escrita na tabela.
