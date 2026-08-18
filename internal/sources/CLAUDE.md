# internal/sources

## Purpose
Lê o arquivo de fontes (o caminho vem de `config.SourcesFile`, default
`sources.txt` na raiz) e devolve a lista de URLs de imobiliárias a serem
raspadas, já validada e sem duplicatas. É a porta de entrada do pipeline de
coleta: tudo o que o scraper visita começa aqui.

## Business logic
- Uma URL por linha. `TrimSpace` em cada linha antes de qualquer decisão —
  arquivos editados à mão trazem espaços e tabs com frequência.
- Linhas em branco e linhas iniciadas por `#` são ignoradas.
- Válida = `url.Parse` sem erro **e** scheme `http`/`https` **e** `Host` não
  vazio. Sem a checagem de scheme/host, `url.Parse("imobiliaria.com.br")`
  passaria (vira path relativo) e o erro só apareceria na requisição.
- Deduplicação preserva a **primeira** ocorrência; a ordem do arquivo é a ordem
  de coleta.
- Arquivo válido sem nenhuma fonte utilizável ⇒ slice vazio + erro nulo.

## Key decisions
- **Linha inválida avisa e segue; arquivo inválido aborta.** Um erro de digitação
  numa fonte não pode impedir a coleta das outras — mas um arquivo inexistente
  ou ilegível é erro de configuração e precisa quebrar cedo, com o caminho na
  mensagem. O erro embrulha o de `os.Open`, então
  `errors.Is(err, fs.ErrNotExist)` funciona no chamador.
- **`#` só comenta no início da linha.** `#` é caractere legítimo de URL
  (fragmento); tratar comentário inline cortaria fontes válidas em silêncio.
- **Deduplicação pela string exata (após trim), sem normalização.** Não
  normalizamos host minúsculo, `www.`, barra final ou query: duas grafias podem
  devolver páginas diferentes, e escolher uma delas seria uma decisão do
  scraper, não do leitor. Se aparecer duplicata "lógica" no futuro, normalize
  aqui — não nos chamadores.
- **`slog` direto, sem logger injetado.** O projeto configura o handler default
  em `cmd/scraper`; injetar `*slog.Logger` só nesse pacote destoaria do resto.

## Dependencies
Apenas a stdlib (`bufio`, `net/url`, `os`, `log/slog`). Não importa nada do
projeto — nem `config`, que só fornece a string do caminho. Consumido por
`cmd/scraper` (orquestração), que passa `cfg.SourcesFile`.

## Gotchas
- **Nenhuma requisição de rede acontece aqui.** URL sintaticamente válida não
  significa host no ar nem coleta permitida — `robots`/`httpclient` decidem isso.
- `bufio.Scanner` tem limite de ~64 KB por linha; uma linha maior vira
  `bufio.ErrTooLong` em `scanner.Err()` e a função devolve erro em vez da lista
  parcial (lista parcial esconderia fontes perdidas). Se algum dia existir linha
  gigante, use `scanner.Buffer`.
- `sources.txt` versionado na raiz vem com as três URLs de exemplo
  **comentadas** — é documentação de formato, não uma lista real. Por isso
  `ReadSources` sobre ele devolve zero fontes; `TestRepoSourcesFileIsValid` só
  garante que o arquivo continua legível.
