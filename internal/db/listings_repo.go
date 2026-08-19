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
	updated_at      = NOW()`

// deleteStaleListingsSQL remove os anúncios de um domínio que não apareceram na
// coleta atual. O corte é o instante em que o run começou: tudo que foi visto
// desde então teve last_seen_at renovado pelo upsert.
const deleteStaleListingsSQL = `
DELETE FROM listings
WHERE source_domain = $1 AND last_seen_at < $2`

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
		if err := execUpsertBatch(ctx, tx, chunk); err != nil {
			return fmt.Errorf("db: upsert listings: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: upsert listings: commit: %w", err)
	}

	return nil
}

// DeleteStaleListings apaga os anúncios do domínio que não foram vistos na
// coleta iniciada em runStartedAt, e devolve quantos foram removidos.
//
// Chame **apenas** quando a coleta do domínio tiver terminado com sucesso: uma
// coleta interrompida no meio (site fora do ar, seletores quebrados) teria
// renovado só parte dos anúncios, e esta função apagaria o restante do
// catálogo.
//
// runStartedAt precisa ser o instante lido antes do primeiro upsert do run.
// Usar "agora" apagaria também os anúncios acabados de gravar.
func DeleteStaleListings(ctx context.Context, pool *pgxpool.Pool, domain string, runStartedAt time.Time) (int64, error) {
	domain = strings.TrimSpace(domain)
	if domain == "" {
		return 0, errors.New("db: delete stale listings: domain is required")
	}
	// Sem esta guarda o zero value não apagaria nada e o erro de programação
	// (esquecer de carimbar o início do run) passaria despercebido.
	if runStartedAt.IsZero() {
		return 0, fmt.Errorf("db: delete stale listings for domain %q: runStartedAt is required", domain)
	}

	tag, err := pool.Exec(ctx, deleteStaleListingsSQL, domain, runStartedAt)
	if err != nil {
		return 0, fmt.Errorf("db: delete stale listings for domain %q: %w", domain, err)
	}

	return tag.RowsAffected(), nil
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

// execUpsertBatch envia um lote de upserts pelo pipeline do pgx e consome um
// resultado por comando. Consumir todos os Exec antes do Close é o que faz um
// erro de banco aparecer aqui (com o comando que falhou) em vez de virar um
// erro genérico na transação.
func execUpsertBatch(ctx context.Context, tx pgx.Tx, rows [][]any) error {
	batch := &pgx.Batch{}
	for _, args := range rows {
		batch.Queue(upsertListingSQL, args...)
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
