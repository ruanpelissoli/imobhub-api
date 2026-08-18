# internal/scraper

## Purpose
Orquestrará a coleta das páginas das fontes: robots.txt, rate limiting, busca do
HTML e extração dos dados. Hoje só a **extração** está implementada
(`extractor.go`): `ExtractListings` converte o HTML de uma página de listagem em
`[]db.RawListing` aplicando os seletores CSS salvos em `site_selectors`.

## Business logic (`ExtractListings`)
- **Função pura**: sem banco, sem rede. Entra HTML + `db.SelectorConfig` + host,
  sai um slice em memória.
- Cada elemento que casa com `listing_container` vira um anúncio; os demais
  seletores são aplicados **relativos ao container** (`FindMatcher`), nunca ao
  documento — senão o preço do card seguinte vazaria para o anterior.
- Campos de texto saem crus, só com `TrimSpace`. Normalizar preço/área/quartos é
  de outra etapa; os campos `_raw` existem justamente para permitir refazer o
  parsing sem raspar o site de novo. `BedroomsRaw`/`AreaRaw` ficam vazios: não há
  seletor correspondente em `SelectorFields`.
- **Anúncio sem `ListingURL` é descartado.** `(source_domain, listing_url)` é o
  alvo do `ON CONFLICT` em `UpsertListings`; anúncios sem URL colidiriam entre si.
- **Zero containers ⇒ slice vazio + erro nulo.** O chamador lê "zero resultados"
  como sinal de seletores possivelmente quebrados e aciona a redescoberta pela
  IA. Erro é reservado a entrada inválida (HTML impossível de parsear, domínio
  vazio/sem host, `listing_container` ausente ou malformado) — problema de
  configuração, não do site.
- `SourceDomain` recebe o argumento apenas com `TrimSpace`, igual a
  `internal/db`: **normalizar o host é responsabilidade do chamador**. Se ele
  passar um domínio com esquema, o esquema vai junto para o banco.

## Key decisions
- **Seletores compilados uma vez com `cascadia`, não `Find(string)` por card.**
  `goquery.Find` compila o seletor a cada chamada; com N anúncios × 6 campos isso
  é 6N compilações por página. `FindMatcher` com `cascadia.Compile` compila 6.
  O ganho secundário é o que mais importa: `Find` **engole seletor inválido** e
  devolve zero resultados, indistinguível de "página mudou". Com `Compile` o erro
  aparece.
- **Container inválido é erro; campo inválido é warning.** A IA erra seletor com
  alguma frequência: perder o preço de todos os anúncios é ruim, perder todos os
  anúncios é pior. Só o `listing_container` aborta.
- **`href` procurado em todos os elementos casados**, não só no primeiro: um
  seletor composto pode casar antes com um wrapper sem `href`. Se nada casar
  dentro do container, o próprio container é testado (`IsMatcher`) — é comum o
  card inteiro ser um `<a>`, e `Find` nunca inclui o elemento de origem.
- **Imagens: seletor configurado primeiro, depois todas as `<img>` do container**,
  deduplicadas mantendo a ordem. A varredura ampla salva cards em que a foto
  principal não casa com o seletor salvo; a dedup preserva a preferência pela
  primeira. Lê `src` **e** `data-src` porque em site com lazy loading o `src` do
  HTML estático costuma ser placeholder.
- **URLs não navegáveis são descartadas** em vez de gravadas: vazio, `#âncora`
  (resolveria para a própria página de listagem, criando um "anúncio" que aponta
  para ela), `javascript:`, `mailto:`, `data:` (um data URI de imagem pode ter
  centenas de KB e não tem por que ir para o banco).
- **Base de resolução = `https://<domínio>/`**, salvo esquema explícito no
  argumento (alguns sites antigos só respondem em `http`). A barra final não é
  cosmética: sem ela `ResolveReference("imovel/1")` descartaria o último segmento
  do path.
- **`RenderHTML` saiu daqui** (virou `httpclient.FetchHeadless`): manter duas
  implementações de chromedp garantiria divergência de timeout/flags/User-Agent,
  e "estático ou headless" é uma decisão de *como buscar a página*.

## Gotchas
- `.Text()` do goquery concatena o texto de **todos** os elementos casados. Um
  seletor largo demais (`div`) devolve o card inteiro num campo só — o sintoma é
  título gigante, não erro.
- Espaços internos **não** são colapsados, só as pontas. Comparar `PriceRaw` com
  string literal em teste exige atenção a quebras de linha dentro da tag.
- A extração não valida se o anúncio é de fato um imóvel; um `listing_container`
  largo demais captura banners e paginação. O sinal disso é volume anômalo, não
  erro — vale olhar a contagem por domínio.

## Dependencies
`goquery` + `cascadia` (dependência direta desde esta task, para compilar os
seletores e detectar CSS inválido) e `internal/db` (`SelectorConfig`,
`RawListing`). Passará a importar `httpclient`, `robots`, `ratelimit`, `sources`
e `selectors` conforme a orquestração for implementada. Consumido pelo pipeline
de coleta em `cmd/scraper`.
