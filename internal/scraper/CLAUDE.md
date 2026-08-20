# internal/scraper

## Purpose
Orquestra a coleta das páginas das fontes: robots.txt, rate limiting, busca do
HTML, extração dos dados e sincronização com o banco.
- **orquestração** (`pipeline.go`): `RunPipeline` monta os módulos de coleta e
  processa as fontes do arquivo uma a uma — é o que `cmd/scraper` executa;
- **extração** (`extractor.go`): `ExtractListings` converte o HTML de uma página
  de listagem em `[]db.RawListing` aplicando os seletores CSS salvos em
  `site_selectors`;
- **sincronização** (`syncer.go`): `SyncListings` reconcilia esses anúncios com
  o banco — grava os vistos agora e apaga os que sumiram do site.

## Business logic (`RunPipeline`)
Por fonte, em ordem: domínio ← host da URL (minúsculo) → `robots.IsAllowed` →
`limiter.Wait` → carimbo do `runStartedAt` → `EnsureSelectors` → busca do HTML
pelo `render_mode` → `ExtractListings` → (se zero: `RecoverSelectors`, re-busca,
re-extração) → `SyncListings` → log `[domínio] ✓ N listings (...)`.
- **Uma fonte com problema nunca derruba o run**: é logada com o domínio,
  contada no resumo e o pipeline segue — um portal fora do ar não pode zerar a
  coleta do dia. `Run` só erra no que é fatal: arquivo de fontes ilegível,
  wiring incompleto, contexto cancelado. **Cancelamento encerra o run** (checado
  entre as fontes e após cada falha): insistir viraria uma cascata de erros
  idênticos que esconde a causa real.
- **`runStartedAt` é carimbado por domínio**, imediatamente antes de processá-lo
  — contrato de `SyncListings`. Não reaproveite um corte global.
- **Zero anúncios ⇒ redescoberta, não DELETE.** Se ainda vier zero depois dela, a
  fonte falha **e** os seletores viram `broken` (`db.MarkSelectorsBroken`):
  `RecoverSelectors` acabou de gravar a linha como `valid`, e sem a marcação o
  run seguinte reusaria seletores que já sabemos não extrair nada.
- **Bloqueio por robots.txt é `Warn` e não conta como falha** — o site pediu para
  não ser coletado e nós obedecemos. A checagem vem antes do rate limiting e da
  descoberta: não se requisita um site proibido nem para descobrir como lê-lo.
- **`Wait` é chamado duas vezes por fonte** (início do domínio e antes da busca
  da página): `EnsureSelectors`/`RecoverSelectors` buscam o HTML por conta
  própria no meio. Esperar a mais é boa vizinhança; a menos é levar bloqueio.
- **Domínio = host minúsculo, com porta e com `www`** — é a chave de
  `site_selectors` e de `listings.source_domain`; duas grafias do mesmo host
  viveriam como duas fontes, com dois catálogos.
- Os logs `[domínio] ✓ %d listings (upserted: %d, deletados: %d)` e
  `[domínio] bloqueado por robots.txt` são contratuais e têm teste.

## Logs: o atributo `event` (`pipeline.go`)
Os textos de `msg` são humanos, contratuais e testados — **não os reescreva**.
Quem filtra e agrega usa o atributo `event`, que é a chave estável:

| `event` | quando | nível | campos próprios |
|---|---|---|---|
| `scraper_run_started` | início do run | Info | `sites_total`, `sources_file` |
| `scraper_run_no_sources` | arquivo sem fontes utilizáveis | Warn | `sources_file` |
| `scraper_run_aborted` | cancelamento entre fontes | Warn | `processed`, `sites_total`, `error` |
| `site_blocked` | robots.txt proibiu | Warn | `domain`, `site_url`, `duration_ms` |
| `site_scraped` | desfecho **terminal** de uma fonte | Info/Error | `domain`, `site_url`, `listings_found`, `listings_removed`, `duration_ms`, `error` (só em falha) |
| `site_selectors_recovering` | zero anúncios, redescoberta acionada | Warn | `domain`, `render_mode` |
| `site_selectors_mark_broken_failed` | `MarkBroken` falhou | Warn | `domain`, `error` |
| `scraper_summary_count_failed` | `CountListings` falhou | Warn | `error` |
| `scraper_run_finished` | fim do run | Info | `sites_total`, `sites_succeeded`, `sites_failed`, `sites_blocked`, `listings_found`, `listings_removed`, `listings_total_in_db`, `duration_ms` |
| `scraper_last_run_publish_failed` | gravação no Redis falhou | Warn | `key`, `error` |
| `scraper_last_run_persistence_disabled` | pipeline sem seam de Redis | Debug | — |

- **Invariante: cada fonte produz exatamente uma linha `site_scraped`** (ou uma
  `site_blocked`, que não é falha). É o que permite contar fontes por agregação
  em vez de por texto. Os quatro pontos terminais de `processSite` passam pelo
  helper `logSiteScraped`, e é por isso que ele existe: campos divergentes entre
  os caminhos de sucesso e de falha quebrariam a contagem em silêncio.
- `duration_ms` da fonte é medido em volta do processamento **daquela** fonte
  (carimbo na entrada de `processSite`); o do run, em volta de `Run` inteiro.

## Resumo da última coleta no Redis (`last_run.go`)
Ao fim de **todo** run montado, o pipeline publica um JSON em
`scraper.LastRunCacheKey` (`scraper:last_run`) com TTL de **48h**:

```json
{"ran_at":"2026-08-19T03:00:00Z","success":true,"sites_total":3,
 "sites_succeeded":2,"sites_failed":0,"sites_blocked":1,"listings_found":120,
 "listings_removed":7,"listings_total_in_db":4200,"duration_ms":91000}
```

- **`success = Failed == 0 && err == nil`.** Fonte bloqueada por robots.txt
  **não** derruba o `success` — é o pipeline obedecendo, não falhando. Run
  abortado (SIGTERM, arquivo de fontes ilegível) é `success=false`.
- **A gravação é feita por `defer` em `Run`**, não no caminho feliz: um run
  morto no meio é justamente o que o operador precisa enxergar, com os números
  parciais. Só não é publicado quando o pipeline nem chegou a ser montado (erro
  de wiring/config), e `-only=enrich` não passa por aqui.
- **`ran_at` é o instante de *início* do run**, em RFC3339 UTC. Com
  `duration_ms` o fim é derivável, e o início é o carimbo estável mesmo quando
  o processo é morto no meio.
- **A gravação usa `context.WithoutCancel` + timeout próprio de 3s.** Derivar do
  `ctx` do run faria a escrita falhar sempre no caminho de SIGTERM — o cenário em
  que ela mais importa. O timeout curto impede que um Redis lento segure o
  encerramento.
- **Falha de Redis é `Warn` e nada mais**: não muda `RunSummary`, o erro
  devolvido por `Run` nem o exit code.
- **A constante da chave é exportada** para que o futuro leitor na API a reuse.
  Ela mora aqui e não em `internal/cache`, que é folha do grafo e não desenha
  chaves — mesma regra que `internal/api` segue com `properties:search:v1:`.
- **`TotalInDB` fica zerado nos caminhos de abort**, porque `logSummary` (que faz
  a contagem) não roda neles. É esperado: `listings_total_in_db: 0` num payload
  com `success=false` significa "não deu tempo de contar".

## Key decisions (`pipeline.go`)
- **Fontes sequenciais.** O rate limit é por domínio (fontes não competem entre
  si), então paralelizar só ganharia em sites lentos — contra N Chromes headless
  simultâneos e erros entrelaçados no log. Fica para quando o volume justificar.
- **O wiring mora em `NewPipeline`, não no `main`.** A assinatura exigida
  (`ctx, cfg, pool`) não recebe módulos prontos, e `cmd/scraper/CLAUDE.md` proíbe
  regra de negócio lá. O `DomainLimiter` passou a nascer aqui, ainda **um por
  run** (dois teriam relógios independentes e dobrariam a carga na fonte).
- **Dependências como campos de função em `Deps`, não interfaces** — cada
  colaborador tem uma operação só, e o pacote já usa esse estilo. O ganho é o
  teste: um closure por campo, sem mock, sem rede, sem PostgreSQL e sem gastar
  tokens da Anthropic. `New` rejeita campo nulo, que viraria panic na 1ª coleta.
- **`Run` devolve `RunSummary`; `RunPipeline` descarta** — o binário só precisa do
  exit code, mas os números são o que os testes verificam. `RunSummary` é
  comparada com `==` nos testes: **não acrescente campo não determinístico a
  ela** (a duração do run é medida em `Run` e passada adiante, nunca guardada
  na struct).
- **`PublishLastRun` é o único campo opcional de `Deps`** e o único seam de
  Redis do pacote (`lastRunStore`, declarado no consumidor como `api.cacheStore`).
  `NewPipeline` só o preenche quando recebe um client não-nulo — a checagem mora
  em `newLastRunPublisher`, **antes** da atribuição à interface, porque um
  `*redis.Client` nil guardado numa interface produz interface **não-nil** e
  panic na primeira gravação.
- **Falha na contagem do resumo (`db.CountListings`) não derruba o run**: o
  número é informativo, e perder o resumo inteiro por causa dele seria pior.

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
  apagaria todo o histórico do domínio por causa de uma falha silenciosa. Desde
  que `DeleteStaleListings` também remove imóveis canônicos órfãos, essa guarda
  protege **os dois** níveis: sem anúncios, nenhuma `property` é apagada. O
  caminho correto para zero anúncios é `selectors.RecoverSelectors`, não o
  DELETE.
- **`SyncStats.PropertiesDeleted` vem de `db.DeleteStaleListings`**, que apaga os
  imóveis canônicos que ficaram sem nenhum anúncio na mesma transação do DELETE.
  Não é uma contagem por domínio: um `property` pode agrupar anúncios de várias
  fontes e só some quando o último cai.
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
  (critério de aceitação) e tem teste — **não acrescente campos a ele**. O caso
  ignorado loga em **Warn**, não Info: contagem parada no dia seguinte se
  explica por essa linha.
- **A remoção de imóveis órfãos sai numa linha separada e condicional**
  (`[domínio] %d imóveis canônicos removidos por terem ficado sem anúncios`,
  Info, só com `PropertiesDeleted > 0`). Separada para não mexer no log
  contratual; condicional porque a remoção é rara e uma linha "0 removidos" em
  todo domínio de todo run só polui.

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
- **O pipeline não valida a URL final após redirecionamento.** Os fetchers usados
  aqui descartam a `finalURL` (contrato de `selectors.PageFetcher`): a identidade
  da fonte continua sendo o host do `sources.txt`, mesmo que o site redirecione
  para outro domínio. É proposital — trocar a identidade no meio do run gravaria
  seletores e anúncios sob um domínio que ninguém configurou.
- **O `domain` de `SyncListings` precisa ser o mesmo texto gravado em
  `listings.source_domain`.** Divergência não dá erro: o DELETE filtra outra
  coisa e sai com zero deletados, e os anúncios velhos ficam no banco para
  sempre. Pelo mesmo motivo, não misture domínios numa chamada — os anúncios são
  todos gravados (cada `RawListing` traz seu `source_domain`), mas a limpeza só
  cobre `domain`.

## Dependencies
`goquery` + `cascadia` (dependência direta desde a task da extração, para
compilar os seletores e detectar CSS inválido), `internal/db`
(`SelectorConfig`, `RawListing`, `UpsertListings`, `DeleteStaleListings`,
`MarkSelectorsBroken`, `CountListings`) e `pgxpool`. Desde `pipeline.go` também
`internal/config`, `internal/sources`, `internal/robots`, `internal/ratelimit` e
`internal/selectors` (de onde vêm os adaptadores `StaticFetcher`/`HeadlessFetcher`
sobre `httpclient`). Desde `last_run.go`, `go-redis` — o client chega pronto por
parâmetro de `NewPipeline`/`RunPipeline`, vindo de `cache.New` em `cmd/scraper`;
este pacote **não** importa `internal/cache`. Consumido por `cmd/scraper`, que só
chama `RunPipeline`.
