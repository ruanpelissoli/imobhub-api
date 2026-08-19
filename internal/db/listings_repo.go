package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// upsertBatchSize limita quantos INSERTs são enfileirados num único
// pgx.Batch. Uma coleta pode devolver milhares de anúncios, e mandar tudo de
// uma vez faria o pool segurar em memória o pacote inteiro (parâmetros +
// resultados) antes do primeiro flush. 100 é o suficiente para diluir o
// round-trip de rede sem que o lote vire um problema de memória.
const upsertBatchSize = 100

// upsertListingSQL grava um anúncio visto na coleta atual. O ON CONFLICT usa a
// identidade (source_domain, listing_url) e reescreve todos os campos "_raw":
// o site é a fonte da verdade, então um anúncio que mudou de preço ou título
// simplesmente sobrescreve a versão anterior.
//
// last_seen_at = NOW() é obrigatório em toda gravação, inclusive no INSERT: é
// ele que protege o anúncio do DELETE de anúncios sumidos no fim do run (ver
// DeleteStaleListings). updated_at é setado à mão porque a tabela não tem
// trigger (ver migrations/CLAUDE.md).
//
// **updated_at só é bumpado quando algum campo coletado mudou de fato.** Ele é
// metade do predicado da fila de enriquecimento
// (enriched_at IS NULL OR updated_at > enriched_at): um NOW() incondicional
// devolveria à fila todo anúncio visto em toda coleta, e o catálogo inteiro
// seria reprocessado — com chamada paga de IA por anúncio — todo dia, sem que
// nada tivesse mudado. A comparação é campo a campo com IS DISTINCT FROM (e não
// ROW(...) IS DISTINCT FROM ROW(...)) para ficar legível qual campo dispara o
// bump e para não depender da sutileza de comparação de linha com jsonb/text[].
//
// last_seen_at, ao contrário, continua **incondicional**: ele não responde "o
// anúncio mudou?", e sim "o anúncio ainda está no site". É ele que protege a
// linha de DeleteStaleListings, e condicioná-lo apagaria todo anúncio que não
// mudou desde a coleta anterior.
const upsertListingSQL = `
INSERT INTO listings (
	source_domain, listing_url, title_raw, price_raw, address_raw,
	description_raw, bedrooms_raw, area_raw, image_urls, extra_data,
	last_seen_at, created_at, updated_at
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NOW(), NOW(), NOW())
ON CONFLICT (source_domain, listing_url) DO UPDATE SET
	title_raw       = EXCLUDED.title_raw,
	price_raw       = EXCLUDED.price_raw,
	address_raw     = EXCLUDED.address_raw,
	description_raw = EXCLUDED.description_raw,
	bedrooms_raw    = EXCLUDED.bedrooms_raw,
	area_raw        = EXCLUDED.area_raw,
	image_urls      = EXCLUDED.image_urls,
	extra_data      = EXCLUDED.extra_data,
	last_seen_at    = NOW(),
	updated_at      = CASE WHEN
		listings.title_raw       IS DISTINCT FROM EXCLUDED.title_raw
		OR listings.price_raw       IS DISTINCT FROM EXCLUDED.price_raw
		OR listings.address_raw     IS DISTINCT FROM EXCLUDED.address_raw
		OR listings.description_raw IS DISTINCT FROM EXCLUDED.description_raw
		OR listings.bedrooms_raw    IS DISTINCT FROM EXCLUDED.bedrooms_raw
		OR listings.area_raw        IS DISTINCT FROM EXCLUDED.area_raw
		OR listings.image_urls      IS DISTINCT FROM EXCLUDED.image_urls
		OR listings.extra_data      IS DISTINCT FROM EXCLUDED.extra_data
	THEN NOW() ELSE listings.updated_at END`

// selectPendingListingsSQL lê um lote da fila de enriquecimento.
//
// O predicado tem dois ramos: enriched_at IS NULL (nunca enriquecido, o caso
// dominante) e updated_at > enriched_at (o anúncio mudou no site depois do
// último passe). O segundo só é confiável porque upsertListingSQL passou a
// bumpar updated_at condicionalmente — ver o comentário de lá.
//
// A paginação é por keyset (id > $1), e não por OFFSET: um anúncio que falha
// **não** recebe enriched_at e continua na fila, então o OFFSET faria o lote
// seguinte devolver as mesmas linhas para sempre. Com o cursor, a drenagem
// avança mesmo quando parte do lote falha.
const selectPendingListingsSQL = `
SELECT id, source_domain, listing_url, title_raw, description_raw, address_raw,
       area_raw, bedrooms_raw, normalized_neighborhood, bedroom_count,
       amenities, image_urls, lat, lng, property_id
FROM listings
WHERE (enriched_at IS NULL OR updated_at > enriched_at) AND id > $1
ORDER BY id
LIMIT $2`

// updateListingEnrichmentSQL grava as colunas derivadas de um anúncio.
//
// **Não toca em updated_at, de propósito.** Ele é metade do predicado da fila:
// setá-lo aqui deixaria updated_at > enriched_at verdadeiro logo após o carimbo,
// e o anúncio voltaria à fila em todo run, para sempre. É a mesma razão pela
// qual enriched_at é carimbado por uma função separada, depois do agrupamento.
const updateListingEnrichmentSQL = `
UPDATE listings SET
	normalized_neighborhood = $2,
	bedroom_count           = $3,
	amenities               = $4,
	lat                     = $5,
	lng                     = $6
WHERE id = $1`

// markListingEnrichedSQL carimba o fim do enriquecimento do anúncio. Assim como
// updateListingEnrichmentSQL, **não** toca em updated_at — ver o comentário de
// lá.
const markListingEnrichedSQL = `UPDATE listings SET enriched_at = NOW() WHERE id = $1`

// deleteStaleListingsSQL remove os anúncios de um domínio que não apareceram na
// coleta atual. O corte é o instante em que o run começou: tudo que foi visto
// desde então teve last_seen_at renovado pelo upsert.
//
// O RETURNING property_id é o que fecha o ciclo de vida do vínculo: sem ele o
// hard delete deixaria properties.active_listing_count inflado para sempre, e o
// imóvel canônico viraria órfão e indeletável (a guarda de DeleteProperty exige
// contador zerado). NULL é o caso normal de um anúncio nunca agrupado.
const deleteStaleListingsSQL = `
DELETE FROM listings
WHERE source_domain = $1 AND last_seen_at < $2
RETURNING property_id`

// countListingsSQL conta o catálogo inteiro. COUNT(*) exato (e não a estimativa
// de pg_class.reltuples) porque o número vai para o resumo do run, que é o
// indicador que o operador acompanha entre execuções — uma estimativa que oscila
// sem que nada tenha mudado destruiria a confiança nele. A tabela tem ordem de
// dezenas de milhares de linhas; o custo do seq scan é irrelevante uma vez por run.
const countListingsSQL = `SELECT COUNT(*) FROM listings`

// selectListingsByPropertyIDSQL lê os anúncios vinculados a um imóvel canônico,
// só com as colunas que a consolidação usa. O cast ::uuid segue o padrão de
// properties_repo.go, e o índice idx_listings_property_id atende ao WHERE.
//
// O ORDER BY id **é contrato**, não conveniência: é ele que dá ao merge uma
// ordem estável de fotos e comodidades, e sem ela o corte em 50 fotos escolheria
// um conjunto diferente a cada execução.
//
// Não há filtro de status porque não existe status: todo anúncio presente na
// tabela é ativo (os que somem do site são apagados por DeleteStaleListings).
const selectListingsByPropertyIDSQL = `
SELECT id, source_domain, listing_url, description_raw, bedroom_count, amenities, image_urls
FROM listings
WHERE property_id = $1::uuid
ORDER BY id`

// UpsertListings insere ou atualiza os anúncios recebidos, renovando
// last_seen_at de cada um.
//
// Tudo roda numa única transação, e não como Execs soltos, por causa da
// interação com DeleteStaleListings: um upsert que falhasse no meio deixaria
// parte dos anúncios com last_seen_at antigo, e a limpeza do fim do run os
// apagaria como se tivessem sumido do site. Ou o run inteiro entra, ou nada
// entra.
//
// Slice vazia é no-op (nem transação é aberta): um domínio pode legitimamente
// não ter devolvido anúncios nesta página.
func UpsertListings(ctx context.Context, pool *pgxpool.Pool, listings []RawListing) error {
	if len(listings) == 0 {
		return nil
	}

	rows := make([][]any, 0, len(listings))
	for i, listing := range listings {
		args, err := listingArgs(listing)
		if err != nil {
			// O índice é a única pista útil: o chamador tem a slice em mãos.
			return fmt.Errorf("db: upsert listings: listing %d: %w", i, err)
		}
		rows = append(rows, args)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: upsert listings: begin transaction: %w", err)
	}
	// Rollback após um Commit bem-sucedido é no-op no pgx; deixar o defer aqui
	// cobre todos os returns de erro abaixo sem duplicação.
	defer func() { _ = tx.Rollback(ctx) }()

	for chunk := range slices.Chunk(rows, upsertBatchSize) {
		if err := execBatch(ctx, tx, upsertListingSQL, chunk); err != nil {
			return fmt.Errorf("db: upsert listings: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: upsert listings: commit: %w", err)
	}

	return nil
}

// DeleteStaleListings apaga os anúncios do domínio que não foram vistos na
// coleta iniciada em runStartedAt e fecha o ciclo de vida do vínculo com
// properties: devolve quantos anúncios saíram e quantos imóveis canônicos
// ficaram sem nenhum anúncio e foram removidos junto.
//
// **Tudo numa única transação**, e não como Execs soltos: o hard delete dos
// anúncios sem o decremento correspondente deixaria active_listing_count
// inflado para sempre, e o imóvel viraria órfão *e* indeletável — a guarda de
// DeleteProperty exige contador zerado, e ele nunca mais chegaria a zero. Ou os
// três passos entram, ou nenhum entra.
//
// Chame **apenas** quando a coleta do domínio tiver terminado com sucesso: uma
// coleta interrompida no meio (site fora do ar, seletores quebrados) teria
// renovado só parte dos anúncios, e esta função apagaria o restante do
// catálogo.
//
// runStartedAt precisa ser o instante lido antes do primeiro upsert do run.
// Usar "agora" apagaria também os anúncios acabados de gravar.
func DeleteStaleListings(ctx context.Context, pool *pgxpool.Pool, domain string, runStartedAt time.Time) (int64, int64, error) {
	domain = strings.TrimSpace(domain)
	if domain == "" {
		return 0, 0, errors.New("db: delete stale listings: domain is required")
	}
	// Sem esta guarda o zero value não apagaria nada e o erro de programação
	// (esquecer de carimbar o início do run) passaria despercebido.
	if runStartedAt.IsZero() {
		return 0, 0, fmt.Errorf("db: delete stale listings for domain %q: runStartedAt is required", domain)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("db: delete stale listings for domain %q: begin transaction: %w", domain, err)
	}
	// Rollback após um Commit bem-sucedido é no-op no pgx; o defer cobre todos
	// os returns de erro abaixo sem duplicação.
	defer func() { _ = tx.Rollback(ctx) }()

	deletedListings, propertyIDs, err := deleteStaleRows(ctx, tx, domain, runStartedAt)
	if err != nil {
		return 0, 0, fmt.Errorf("db: delete stale listings for domain %q: %w", domain, err)
	}

	deltas := staleCountsByProperty(propertyIDs)
	if len(deltas) == 0 {
		// Nada foi apagado, ou tudo que saiu nunca tinha sido agrupado: nenhum
		// contador a mexer e nenhuma property a avaliar.
		if err := tx.Commit(ctx); err != nil {
			return 0, 0, fmt.Errorf("db: delete stale listings for domain %q: commit: %w", domain, err)
		}
		return deletedListings, 0, nil
	}

	decrements := make([][]any, 0, len(deltas))
	affected := make([]string, 0, len(deltas))
	for _, delta := range deltas {
		decrements = append(decrements, []any{delta.propertyID, delta.count})
		affected = append(affected, delta.propertyID)
	}

	for chunk := range slices.Chunk(decrements, upsertBatchSize) {
		if err := execBatch(ctx, tx, decrementListingCountBySQL, chunk); err != nil {
			return 0, 0, fmt.Errorf("db: delete stale listings for domain %q: decrement listing counts: %w", domain, err)
		}
	}

	tag, err := tx.Exec(ctx, deleteOrphanPropertiesSQL, affected)
	if err != nil {
		return 0, 0, fmt.Errorf("db: delete stale listings for domain %q: delete orphan properties: %w", domain, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, 0, fmt.Errorf("db: delete stale listings for domain %q: commit: %w", domain, err)
	}

	return deletedListings, tag.RowsAffected(), nil
}

// deleteStaleRows executa o DELETE e consome o RETURNING, devolvendo quantas
// linhas saíram e o property_id de cada uma (nil para anúncio nunca agrupado),
// **com multiplicidade** — é ela que diz de quanto cada imóvel precisa ser
// decrementado.
//
// O pgx não permite outra query na mesma conexão enquanto as linhas não forem
// consumidas, então o rows.Close() (via defer) acontece antes de qualquer
// statement seguinte da transação.
func deleteStaleRows(ctx context.Context, tx pgx.Tx, domain string, runStartedAt time.Time) (int64, []*string, error) {
	rows, err := tx.Query(ctx, deleteStaleListingsSQL, domain, runStartedAt)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()

	var deleted int64
	propertyIDs := make([]*string, 0, 64)
	for rows.Next() {
		var propertyID *string
		if err := rows.Scan(&propertyID); err != nil {
			return 0, nil, fmt.Errorf("scan deleted property_id: %w", err)
		}
		deleted++
		propertyIDs = append(propertyIDs, propertyID)
	}
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}

	return deleted, propertyIDs, nil
}

// propertyDelta é quantos anúncios um imóvel canônico perdeu nesta limpeza.
type propertyDelta struct {
	propertyID string
	count      int
}

// staleCountsByProperty agrega os property_id devolvidos pelo DELETE em um
// delta por imóvel. Os nil (anúncios nunca agrupados) são ignorados: não há
// contador a mexer.
//
// A saída é **ordenada por id**, e isso não é cosmético: dois runs concorrentes
// que decrementassem o mesmo par de imóveis em ordens opostas se travariam
// mutuamente. Ordem fixa é a ordem de aquisição de locks.
func staleCountsByProperty(propertyIDs []*string) []propertyDelta {
	counts := make(map[string]int, len(propertyIDs))
	for _, propertyID := range propertyIDs {
		if propertyID == nil {
			continue
		}
		id := strings.TrimSpace(*propertyID)
		if id == "" {
			continue
		}
		counts[id]++
	}

	deltas := make([]propertyDelta, 0, len(counts))
	for id, count := range counts {
		deltas = append(deltas, propertyDelta{propertyID: id, count: count})
	}
	slices.SortFunc(deltas, func(a, b propertyDelta) int {
		return strings.Compare(a.propertyID, b.propertyID)
	})

	return deltas
}

// CountListings devolve quantos anúncios existem hoje na tabela listings,
// somando todos os domínios.
//
// Serve ao resumo do fim da coleta, não a decisões de negócio: é uma foto tirada
// depois da sincronização de todas as fontes, e o número muda a cada run.
func CountListings(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	var total int64
	if err := pool.QueryRow(ctx, countListingsSQL).Scan(&total); err != nil {
		return 0, fmt.Errorf("db: could not count listings: %w", err)
	}
	return total, nil
}

// ListListingsByPropertyID devolve todos os anúncios vinculados ao imóvel
// canônico, **ordenados por id**, na forma que a consolidação consome.
//
// Imóvel sem anúncios não é erro: devolve uma slice vazia (nunca nil). O vínculo
// pode ter acabado de ser desfeito, e quem consolida trata isso como no-op.
func ListListingsByPropertyID(ctx context.Context, pool *pgxpool.Pool, propertyID string) ([]Listing, error) {
	propertyID = strings.TrimSpace(propertyID)
	if propertyID == "" {
		return nil, errors.New("db: list listings by property: property id is required")
	}

	rows, err := pool.Query(ctx, selectListingsByPropertyIDSQL, propertyID)
	if err != nil {
		return nil, fmt.Errorf("db: list listings for property %q: %w", propertyID, err)
	}
	defer rows.Close()

	listings := make([]Listing, 0, 8)
	for rows.Next() {
		listing, err := scanListing(rows)
		if err != nil {
			return nil, fmt.Errorf("db: list listings for property %q: %w", propertyID, err)
		}
		listings = append(listings, listing)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: list listings for property %q: %w", propertyID, err)
	}

	return listings, nil
}

// ListPendingListings devolve um lote da fila de enriquecimento: os anúncios
// nunca enriquecidos ou modificados desde o último passe, **ordenados por id**,
// com id maior que afterID.
//
// afterID é o cursor da drenagem: 0 no primeiro lote, e o maior id do lote
// anterior nos seguintes. Ele é obrigatório para que a fila avance mesmo quando
// um anúncio falha — um anúncio que falha não recebe enriched_at e voltaria no
// lote seguinte indefinidamente.
//
// Fila vazia devolve slice vazia (nunca nil) e erro nulo: é o caso normal de um
// catálogo já enriquecido.
func ListPendingListings(ctx context.Context, pool *pgxpool.Pool, afterID int64, limit int) ([]PendingListing, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("db: list pending listings: limit must be greater than 0, got %d", limit)
	}

	rows, err := pool.Query(ctx, selectPendingListingsSQL, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("db: list pending listings after id %d: %w", afterID, err)
	}
	defer rows.Close()

	listings := make([]PendingListing, 0, limit)
	for rows.Next() {
		listing, err := scanPendingListing(rows)
		if err != nil {
			return nil, fmt.Errorf("db: list pending listings after id %d: %w", afterID, err)
		}
		listings = append(listings, listing)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: list pending listings after id %d: %w", afterID, err)
	}

	return listings, nil
}

// UpdateListingEnrichment grava as colunas derivadas do anúncio
// (normalized_neighborhood, bedroom_count, amenities, lat, lng).
//
// **Precisa rodar antes do merge do imóvel canônico**: grouping.MergePropertyData
// relê bedroom_count/amenities/image_urls/description_raw do banco, então um
// merge feito antes desta gravação consolidaria os valores antigos.
//
// Anúncio inexistente é no-op sem erro (mesma convenção de MarkSelectorsBroken):
// a linha pode ter sido apagada por DeleteStaleListings entre a leitura da fila
// e a gravação, e isso é ciclo de vida normal, não falha.
func UpdateListingEnrichment(ctx context.Context, pool *pgxpool.Pool, enrichment ListingEnrichment) error {
	if enrichment.ID <= 0 {
		return fmt.Errorf("db: update listing enrichment: listing id is required, got %d", enrichment.ID)
	}

	_, err := pool.Exec(ctx, updateListingEnrichmentSQL, listingEnrichmentArgs(enrichment)...)
	if err != nil {
		return fmt.Errorf("db: update enrichment of listing %d: %w", enrichment.ID, err)
	}

	return nil
}

// MarkListingEnriched carimba enriched_at = NOW() no anúncio, tirando-o da fila.
//
// É uma função separada de UpdateListingEnrichment (e não um booleano nela) de
// propósito: o carimbo é o **último** passo do fluxo, depois do agrupamento e da
// consolidação do canônico, e um parâmetro permitiria carimbar cedo por engano —
// o anúncio sairia da fila sem nunca ter sido agrupado.
//
// Anúncio inexistente é no-op sem erro, mesma razão de UpdateListingEnrichment.
func MarkListingEnriched(ctx context.Context, pool *pgxpool.Pool, listingID int64) error {
	if listingID <= 0 {
		return fmt.Errorf("db: mark listing enriched: listing id is required, got %d", listingID)
	}

	if _, err := pool.Exec(ctx, markListingEnrichedSQL, listingID); err != nil {
		return fmt.Errorf("db: mark listing %d as enriched: %w", listingID, err)
	}

	return nil
}

// listingEnrichmentArgs monta os argumentos de updateListingEnrichmentSQL na
// ordem dos placeholders. Os ponteiros nil chegam ao banco como NULL — é
// exatamente a distinção que o struct existe para preservar.
func listingEnrichmentArgs(enrichment ListingEnrichment) []any {
	return []any{
		enrichment.ID,
		enrichment.NormalizedNeighborhood,
		enrichment.BedroomCount,
		normalizeTextArray(enrichment.Amenities),
		enrichment.Lat,
		enrichment.Lng,
	}
}

// scanPendingListing lê uma linha na ordem de selectPendingListingsSQL. Todas as
// colunas de texto envolvidas são nullable e passam por *string temporário: pgx
// não escaneia NULL para string, e aqui NULL e "" são a mesma coisa.
func scanPendingListing(row pgx.Row) (PendingListing, error) {
	var (
		listing      PendingListing
		title        *string
		description  *string
		address      *string
		area         *string
		bedrooms     *string
		neighborhood *string
	)
	err := row.Scan(
		&listing.ID,
		&listing.SourceDomain,
		&listing.ListingURL,
		&title,
		&description,
		&address,
		&area,
		&bedrooms,
		&neighborhood,
		&listing.BedroomCount,
		&listing.Amenities,
		&listing.ImageURLs,
		&listing.Lat,
		&listing.Lng,
		&listing.PropertyID,
	)
	if err != nil {
		return PendingListing{}, err
	}

	listing.TitleRaw = derefText(title)
	listing.DescriptionRaw = derefText(description)
	listing.AddressRaw = derefText(address)
	listing.AreaRaw = derefText(area)
	listing.BedroomsRaw = derefText(bedrooms)
	listing.NormalizedNeighborhood = derefText(neighborhood)
	listing.Amenities = normalizeTextArray(listing.Amenities)
	listing.ImageURLs = normalizeTextArray(listing.ImageURLs)

	return listing, nil
}

// derefText converte uma coluna de texto nullable no "" que os campos "_raw"
// usam para ausência.
func derefText(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// scanListing lê uma linha na ordem de selectListingsByPropertyIDSQL.
// description_raw é nullable e passa por um *string temporário: pgx não escaneia
// NULL para string, e aqui NULL e "" são a mesma coisa.
func scanListing(row pgx.Row) (Listing, error) {
	var (
		listing        Listing
		descriptionRaw *string
	)
	err := row.Scan(
		&listing.ID,
		&listing.SourceDomain,
		&listing.ListingURL,
		&descriptionRaw,
		&listing.BedroomCount,
		&listing.Amenities,
		&listing.ImageURLs,
	)
	if err != nil {
		return Listing{}, err
	}

	if descriptionRaw != nil {
		listing.DescriptionRaw = *descriptionRaw
	}
	listing.Amenities = normalizeTextArray(listing.Amenities)
	listing.ImageURLs = normalizeTextArray(listing.ImageURLs)

	return listing, nil
}

// execBatch envia um lote do mesmo statement pelo pipeline do pgx e consome um
// resultado por comando. Consumir todos os Exec antes do Close é o que faz um
// erro de banco aparecer aqui (com o comando que falhou) em vez de virar um
// erro genérico na transação.
func execBatch(ctx context.Context, tx pgx.Tx, sql string, rows [][]any) error {
	batch := &pgx.Batch{}
	for _, args := range rows {
		batch.Queue(sql, args...)
	}

	results := tx.SendBatch(ctx, batch)
	for range rows {
		if _, err := results.Exec(); err != nil {
			// Fecha antes de retornar: sem isso a conexão fica com o restante
			// do pipeline pendente e é descartada pelo pool.
			_ = results.Close()
			return err
		}
	}

	if err := results.Close(); err != nil {
		return fmt.Errorf("close batch: %w", err)
	}

	return nil
}

// listingArgs valida o anúncio e monta os argumentos de upsertListingSQL na
// ordem dos placeholders.
func listingArgs(listing RawListing) ([]any, error) {
	domain := strings.TrimSpace(listing.SourceDomain)
	if domain == "" {
		return nil, errors.New("source_domain is required")
	}

	url := strings.TrimSpace(listing.ListingURL)
	if url == "" {
		return nil, fmt.Errorf("listing_url is required (source_domain %q)", domain)
	}

	extraData, err := encodeExtraData(listing.ExtraData)
	if err != nil {
		return nil, fmt.Errorf("listing_url %q: %w", url, err)
	}

	return []any{
		domain,
		url,
		listing.TitleRaw,
		listing.PriceRaw,
		listing.AddressRaw,
		listing.DescriptionRaw,
		listing.BedroomsRaw,
		listing.AreaRaw,
		normalizeImageURLs(listing.ImageURLs),
		extraData,
	}, nil
}

// normalizeImageURLs troca nil por slice vazia. A coluna aceita NULL, mas
// gravar NULL obrigaria todo leitor a distinguir "sem imagens" de "não sei" —
// distinção que a coleta não faz. A regra é a mesma de amenities/photos em
// properties, então mora num lugar só (ver normalizeTextArray).
func normalizeImageURLs(urls []string) []string {
	return normalizeTextArray(urls)
}

// encodeExtraData serializa extra_data. nil vira objeto vazio pelo mesmo motivo
// de normalizeImageURLs: a coluna nunca guarda NULL.
func encodeExtraData(data map[string]any) ([]byte, error) {
	if len(data) == 0 {
		return []byte("{}"), nil
	}

	encoded, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("encode extra_data: %w", err)
	}

	return encoded, nil
}
