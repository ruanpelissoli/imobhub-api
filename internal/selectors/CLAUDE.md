# internal/selectors

## Purpose
Decide quais seletores CSS usar para extrair os anúncios de cada domínio.
`SelectorService` reaproveita a linha de `site_selectors` enquanto ela estiver
válida e só aciona o Claude (`internal/ai`) quando a fonte é nova ou os
seletores foram marcados como quebrados. É a camada que impede que uma chamada
paga à Anthropic entre no caminho de cada coleta.

Duas portas de entrada:
- `EnsureSelectors` — caminho normal da coleta. `status='valid'` retorna direto,
  sem rede e sem IA; sem linha ou `status='broken'`, descobre e persiste.
- `RecoverSelectors` — auto-recuperação. Ignora o status gravado e força uma
  nova análise. É chamado quando o extrator não encontrou **nenhum** anúncio com
  os seletores atuais (o site mudou de layout).

## Key decisions
- **Status decide, não idade.** `EnsureSelectors` não reanalisa por
  `last_validated_at` antigo: seletores velhos que funcionam são seletores bons.
  O gatilho é sempre um sinal de falha (`broken`) ou a ausência de linha.
- **Primeira visita usa o cliente estático, sempre.** É o caminho barato, e o
  próprio HTML estático carrega os sinais de SPA (`<div id="__next">`, skeletons)
  que o Claude usa para pedir `headless`. Subir um Chrome "por precaução" custaria
  ordens de magnitude mais em toda fonte nova.
- **Redescoberta usa o `render_mode` gravado.** Se a última descoberta precisou
  de headless, buscar a página com o cliente estático devolveria o shell vazio
  da SPA e a análise falharia. Mudança de layout raramente muda a natureza da
  página.
- **O `render_mode` gravado é o que o Claude devolve, não o usado na busca.** É
  assim que um site que virou SPA passa a ser coletado com headless na execução
  seguinte.
- **`PageFetcher` é um tipo função, não interface.** A dependência tem uma
  operação só; `StaticFetcher`/`HeadlessFetcher` fecham sobre o User-Agent e os
  testes passam um closure, sem mock nem gerador.
- **Costuras não exportadas (`getSelectors`, `upsertSelectors`, `analyze`).** O
  acesso ao banco e à Anthropic são funções livres (decisão de `internal/db`),
  então esses campos são o único jeito de exercitar o fluxo sem PostgreSQL e sem
  gastar tokens. O construtor sempre injeta o wiring de produção — não exporte
  esses campos, ou o call site passa a poder trocar o banco em produção.
- **Dependências validadas no construtor.** `NewSelectorService` devolve erro com
  pool/fetcher/chave faltando, para que o wiring incompleto derrube o boot em vez
  de falhar na primeira fonte nova, possivelmente horas depois.

## Business logic / invariantes
- **Erros da Anthropic são propagados sem fallback.** Não há seletor "chutado"
  nem reuso de linha quebrada: quem orquestra a coleta decide entre pular a
  fonte e abortar o run. Devolver seletores parciais faria o extrator achar zero
  anúncios e marcar o site como vazio, que é pior que falhar alto.
- **Nunca devolve config junto com erro.** Um `db.SelectorConfig` meio
  preenchido seria tratado como configuração válida pelo chamador.
- **O domínio chega já normalizado** (host sem esquema e sem barra final, o mesmo
  formato de `site_selectors.domain`). Este pacote só faz `TrimSpace`; normalizar
  o host é responsabilidade de quem lê `sources.txt`. Domínio divergente lê e
  grava linhas diferentes e refaz a análise (paga) em toda execução.
- **A URL final após redirecionamentos é descartada** em `StaticFetcher`: a
  identidade da fonte é o domínio passado, e trocá-la no meio da descoberta
  gravaria os seletores sob outro domínio.
- O log `[domínio] seletores detectados via Claude (render_mode: ...)` é
  contratual (critério de aceitação da task) e tem teste: é o evento que o
  operador procura quando uma fonte nova entra na coleta. Os atributos `domain`
  e `render_mode` acompanham a mensagem para permitir filtro estruturado.

## Dependencies
`internal/db` (leitura/gravação de `site_selectors` e os enums de render mode),
`internal/ai` (`AnalyzeSelectors`), `internal/httpclient` (`FetchStatic`,
`FetchHeadless`) e `pgxpool`. Será importado por `internal/scraper` e montado em
`cmd/scraper` com o User-Agent de `config`.

## Gotchas
- **`LastValidatedAt` volta `nil` numa descoberta.** O `UpsertSelectors` grava
  `NOW()` no banco, mas o serviço não relê a linha; o valor só aparece na leitura
  seguinte. Não use esse campo para decidir nada logo após a descoberta.
- **`RecoverSelectors` paga uma chamada à Anthropic em toda invocação.** Chame
  apenas depois de uma extração que trouxe zero itens, nunca em retry de rede.
- `MarkSelectorsBroken` (em `internal/db`) não é chamado aqui: quem detecta a
  falha de extração é o extrator, e é ele quem marca. Este pacote só reage ao
  status.
- HTML estático vazio faz a análise falhar com `ai.ErrEmptyHTML` **sem** chamar a
  API (nenhum token gasto). Não há fallback automático para headless: na prática
  SPAs devolvem um shell não-vazio e o Claude já pede `headless` a partir dele.
- `render_mode` fora do enum (só possível se a CHECK constraint for afrouxada)
  cai no cliente estático — errar para o lado barato.
