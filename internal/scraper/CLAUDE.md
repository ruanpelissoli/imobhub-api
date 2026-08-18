# internal/scraper

## Purpose
Orquestrará a coleta das páginas das fontes: robots.txt, rate limiting, busca do
HTML e extração dos dados. Implementado hoje:
- **extração** (`extractor.go`): `ExtractListings` converte o HTML de uma página
  de listagem em `[]db.RawListing` aplicando os seletores CSS salvos em
  `site_selectors`;
- **sincronização** (`syncer.go`): `SyncListings` reconcilia esses anúncios com
  o banco — grava os vistos agora e apaga os que sumiram do site.

A orquestração que costura busca, extração e sincronização ainda não existe.

## Business logic (`ExtractListings`)
- **Função pura**: sem banco, sem rede. Entra HTML + `db.SelectorConfig` + host,
  sai um slice em memória.
- Cada elemento que casa com `listing_container` vira um anúncio; os demais
  seletores são aplicados **relativos ao container** (`FindMatcher`), nunca ao
  documento — senão o preço do card seguinte vazaria para o anterior.
- Campos de texto saem crus, só com `TrimSpace`: os campos `_raw` existem para
  permitir refazer o parsing sem raspar o site de novo. `BedroomsRaw`/`AreaRaw`
  ficam vazios — não há seletor correspondente em `SelectorFields`.
- **Anúncio sem `ListingURL` é descartado.** `(source_domain, listing_url)` é o
  alvo do `ON CONFLICT` em `UpsertListings`; anúncios sem URL colidiriam entre si.
- **Zero containers ⇒ slice vazio + erro nulo.** O chamador lê "zero resultados"
  como sinal de seletores possivelmente quebrados e aciona a redescoberta pela
  IA. Erro é reservado a entrada inválida (HTML impossível de parsear, domínio
  vazio/sem host, `listing_container` ausente ou malformado) — problema de
  configuração, não do site.
- `SourceDomain` recebe o argumento apenas com `TrimSpace`, igual a
  `internal/db`: **normalizar o host é responsabilidade do chamador**.

## Business logic (`SyncListings`)
- **Zero anúncios extraídos ⇒ nenhuma operação de banco.** É a proteção central
  do módulo, não uma otimização: uma coleta devolve zero anúncios quando o site
  mudou de layout, respondeu com página de erro em HTTP 200 ou o JS não
  renderizou. Em nenhum desses casos o catálogo sumiu de verdade, e prosseguir
  apagaria todo o histórico do domínio por causa de uma falha silenciosa. O
  caminho correto para zero anúncios é `selectors.RecoverSelectors`, não o
  DELETE.
- **Ordem fixa: upsert e só então DELETE.** O upsert renova o `last_seen_at` de
  tudo que foi visto agora; inverter apagaria anúncios vivos na janela entre as
  duas operações. Se o upsert falha, o DELETE **não** roda — parte dos anúncios
  ficou com `last_seen_at` antigo e seria interpretada como sumida.
- **`runStartedAt` é o instante lido antes de processar *aquele* domínio**, não
  o início global do pipeline: com o corte global, um domínio processado no
  início do run já teria `last_seen_at` posterior ao corte de outro e a
  comparação perderia o sentido. Chega intacto ao repositório (o teste garante),
  porque "agora" no lugar dele apagaria o que acabou de ser gravado.
- **Estatísticas voltam zeradas em qualquer erro.** Parte do trabalho pode ter
  entrado no banco, mas o número não é confiável para relatório; o chamador deve
  ler o erro como "este domínio falhou".
- **Costuras `upsertListings`/`deleteStaleListings` (vars de pacote).** O acesso
  a dados em `internal/db` são funções livres, então trocá-las é o único jeito de
  testar a guarda de zero anúncios e a ordem das operações sem PostgreSQL. Não
  exporte: em produção o wiring é sempre o default.
- O log `[domínio] sync concluído: %d upserted, %d deletados` é contratual
  (critério de aceitação) e tem teste. O caso ignorado loga em **Warn**, não
  Info: contagem parada no dia seguinte se explica por essa linha.

## Key decisions
- **Seletores compilados uma vez com `cascadia`, não `Find(string)` por card.**
  `Find` compila o seletor a cada chamada (6N compilações por página) e, pior,
  **engole seletor inválido** devolvendo zero resultados — indistinguível de
  "página mudou". Com `Compile` o erro aparece.
- **Container inválido é erro; campo inválido é warning.** A IA erra seletor com
  alguma frequência: perder o preço de todos os anúncios é ruim, perder todos os
  anúncios é pior. Só o `listing_container` aborta.
- **`href` procurado em todos os elementos casados**, não só no primeiro: um
  seletor composto pode casar antes com um wrapper sem `href`. Se nada casar
  dentro do container, o próprio container é testado (`IsMatcher`) — é comum o
  card inteiro ser um `<a>`, e `Find` nunca inclui o elemento de origem.
- **Imagens: seletor configurado primeiro, depois todas as `<img>` do container**,
  deduplicadas mantendo a ordem. A varredura ampla salva cards em que a foto
  principal não casa com o seletor salvo. Lê `src` **e** `data-src` porque em
  site com lazy loading o `src` do HTML estático costuma ser placeholder.
- **URLs não navegáveis são descartadas** em vez de gravadas: vazio, `#âncora`
  (resolveria para a própria página de listagem, criando um "anúncio" que aponta
  para ela), `javascript:`, `mailto:`, `data:` (centenas de KB no banco).
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
- **`SyncStats.Upserted` é `len(extracted)`, não linhas afetadas pelo banco:** o
  `ON CONFLICT` não distingue insert de update. O número responde "quantos
  anúncios o domínio tinha agora", não "quantos são novos".
- **O `domain` de `SyncListings` precisa ser o mesmo texto gravado em
  `listings.source_domain`.** Divergência não dá erro: o DELETE filtra outra
  coisa e sai com zero deletados, e os anúncios velhos ficam no banco para
  sempre. Pelo mesmo motivo, não misture domínios numa chamada — os anúncios são
  todos gravados (cada `RawListing` traz seu `source_domain`), mas a limpeza só
  cobre `domain`.

## Dependencies
`goquery` + `cascadia` (dependência direta desde a task da extração, para
compilar os seletores e detectar CSS inválido), `internal/db`
(`SelectorConfig`, `RawListing`, `UpsertListings`, `DeleteStaleListings`) e
`pgxpool`. Passará a importar `httpclient`, `robots`, `ratelimit`, `sources` e
`selectors` conforme a orquestração for implementada. Consumido pelo pipeline de
coleta em `cmd/scraper`.
