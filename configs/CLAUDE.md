# configs/

## Purpose
Arquivos de **vocabulário e configuração de domínio** que mudam sem recompilar:
listas de termos, dicionários, tabelas de equivalência. É o par de `migrations/`
(schema) e `sources.txt` (fontes) — dado do produto, versionado em git, lido em
runtime a partir do working directory do processo.

Não confundir com configuração de ambiente: credenciais, URLs e chaves vivem em
variáveis de ambiente e passam por `internal/config`. Aqui só entra o que é o
mesmo em todos os ambientes.

## Conteúdo
- `amenities.yaml` — vocabulário de comodidades usado por `internal/enrichment`
  para extrair `listings.amenities` do texto livre do anúncio. Cada item tem um
  `canonical` (o valor gravado no banco) e os `synonyms` reconhecidos no texto.
  Carregado por `enrichment.NewTermExtractor`, com o caminho vindo de
  `config.AmenitiesFile` (`AMENITIES_FILE`, default `configs/amenities.yaml`).

## Key decisions
- **YAML, não Go.** O vocabulário é dado de produto: quem ajusta os termos olha
  a qualidade da extração, não o código. Em Go, cada termo novo viraria um
  deploy e um code review.
- **Nome canônico separado dos sinônimos.** O que vai para o banco é uma coisa
  (`ar-condicionado`), o que aparece no anúncio é outra (`split`, `climatizado`).
  Sem essa separação, o mesmo conceito viraria N valores distintos na coluna.
- **Ordem do arquivo é contrato.** `enrichment` devolve as comodidades na ordem
  em que aparecem aqui — é o que torna o resultado determinístico entre passes.

## Gotchas
- **Todo arquivo novo aqui precisa entrar no `Dockerfile`.** O diretório inteiro
  é copiado (`COPY configs /app/configs`), então isso já está coberto — mas se
  algum dia a cópia virar seletiva, um arquivo esquecido só falha no startup do
  container, não no build.
- **Mudar um `canonical` muda dado já gravado.** Os valores viram conteúdo de
  `listings.amenities`/`properties.amenities`; renomear exige reprocessar os
  anúncios já enriquecidos.
- Regras de escrita dos termos (limite de palavra, acento, hífen) estão
  documentadas no cabeçalho de `amenities.yaml` e em
  `internal/enrichment/CLAUDE.md`. Resumo: não liste variações de caixa, acento
  ou hífen — o matching já cobre — e o termo precisa começar e terminar em
  letra ou dígito.
