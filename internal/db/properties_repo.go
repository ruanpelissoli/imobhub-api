package db

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Sentinelas comparáveis com errors.Is. Existem porque o chamador precisa
// distinguir "id errado" de "regra de negócio barrou" sem inspecionar texto de
// erro: a UI de correção de deduplicação, por exemplo, reage a
// ErrPropertyHasListings desvinculando os anúncios antes de tentar de novo.
var (
	// ErrPropertyNotFound indica que o id não existe em properties. Note que
	// GetPropertyByID **não** o usa: lá a ausência é resultado normal (nil, nil).
	ErrPropertyNotFound = errors.New("db: property not found")
	// ErrPropertyHasListings barra DeleteProperty enquanto houver anúncios
	// apontando para o imóvel (active_listing_count > 0).
	ErrPropertyHasListings = errors.New("db: property still has active listings")
	// ErrListingNotFound indica que o anúncio a vincular não existe.
	ErrListingNotFound = errors.New("db: listing not found")
)

// propertyColumns é a lista de colunas lida por todo SELECT deste arquivo, na
// ordem em que scanProperty as consome. Centralizada para que acrescentar uma
// coluna não exija sincronizar três queries à mão.
const propertyColumns = `
	id, canonical_address, neighborhood, city, state, lat, lng,
	bedroom_count, bathroom_count, parking_spots, area_sqm, amenities,
	description, photos, transaction_type, property_type,
	active_listing_count, created_at, updated_at`

// insertPropertySQL cria o registro canônico. id, created_at e updated_at são
// deixados para o banco (gen_random_uuid() e NOW()) e devolvidos pelo RETURNING:
// gerar o UUID em Go traria uma dependência nova sem ganho, já que o INSERT é o
// único momento em que o id nasce.
//
// active_listing_count fica de fora de propósito — ele é consequência dos
// vínculos com listings, não um dado que o chamador informa (ver
// LinkListingToProperty).
const insertPropertySQL = `
INSERT INTO properties (
	canonical_address, neighborhood, city, state, lat, lng,
	bedroom_count, bathroom_count, parking_spots, area_sqm, amenities,
	description, photos, transaction_type, property_type,
	created_at, updated_at
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, NOW(), NOW())
RETURNING id, created_at, updated_at`

// selectPropertyByIDSQL lê um imóvel pelo id. O cast explícito ::uuid evita
// depender da inferência de tipo do pgx para um parâmetro que viaja como texto.
const selectPropertyByIDSQL = `
SELECT` + propertyColumns + `
FROM properties
WHERE id = $1::uuid`

// updatePropertySQL reescreve os campos consolidados. Não toca em
// active_listing_count (mantido pelo par link/unlink) nem em created_at, e seta
// updated_at à mão porque a tabela não tem trigger (ver migrations/CLAUDE.md).
const updatePropertySQL = `
UPDATE properties SET
	canonical_address = $1,
	neighborhood      = $2,
	city              = $3,
	state             = $4,
	lat               = $5,
	lng               = $6,
	bedroom_count     = $7,
	bathroom_count    = $8,
	parking_spots     = $9,
	area_sqm          = $10,
	amenities         = $11,
	description       = $12,
	photos            = $13,
	transaction_type  = $14,
	property_type     = $15,
	updated_at        = NOW()
WHERE id = $16::uuid`

// deletePropertySQL apaga o imóvel **com a guarda embutida**: a condição
// active_listing_count = 0 vive no WHERE, e não num SELECT anterior, porque um
// read-then-delete permitiria que um vínculo criado entre as duas queries fosse
// perdido (a FK é ON DELETE SET NULL — o anúncio sobreviveria órfão, sem aviso).
const deletePropertySQL = `
DELETE FROM properties
WHERE id = $1::uuid AND active_listing_count = 0`

// selectActiveListingCountSQL lê o contador denormalizado. É de propósito que
// não seja um COUNT(*) em listings: a coluna é a fonte que a listagem consulta,
// e trocá-la por uma contagem real aqui mascararia uma eventual divergência em
// vez de expô-la.
const selectActiveListingCountSQL = `
SELECT active_listing_count FROM properties WHERE id = $1::uuid`

// lockListingForLinkSQL trava a linha do anúncio e responde, de uma vez, qual é
// o imóvel atual e se ele já é o imóvel de destino. A comparação é feita pelo
// PostgreSQL (tipo uuid) e não em Go: comparar strings erraria em formas
// alternativas do mesmo UUID (maiúsculas, sem hífens).
//
// O FOR UPDATE é o que torna o contador confiável: sem ele, dois links
// concorrentes do mesmo anúncio leriam ambos property_id NULL e incrementariam
// duas vezes.
const lockListingForLinkSQL = `
SELECT property_id, COALESCE(property_id = $2::uuid, FALSE)
FROM listings
WHERE id = $1
FOR UPDATE`

// lockListingForUnlinkSQL é a versão sem destino, usada pelo desvínculo.
const lockListingForUnlinkSQL = `
SELECT property_id FROM listings WHERE id = $1 FOR UPDATE`

// setListingPropertySQL e clearListingPropertySQL alteram o vínculo do anúncio.
// updated_at é setado à mão (tabela sem trigger).
const setListingPropertySQL = `
UPDATE listings SET property_id = $1::uuid, updated_at = NOW() WHERE id = $2`

const clearListingPropertySQL = `
UPDATE listings SET property_id = NULL, updated_at = NOW() WHERE id = $1`

// incrementListingCountSQL e decrementListingCountSQL mantêm o contador
// denormalizado. A aritmética acontece dentro do UPDATE (e não em Go) para que
// dois vínculos concorrentes de anúncios diferentes somem corretamente.
//
// GREATEST(..., 0) é uma rede de segurança: um contador que já estivesse
// dessincronizado não deve virar negativo e envenenar a guarda do
// DeleteProperty (que exige exatamente 0).
const incrementListingCountSQL = `
UPDATE properties
SET active_listing_count = active_listing_count + 1, updated_at = NOW()
WHERE id = $1::uuid`

const decrementListingCountSQL = `
UPDATE properties
SET active_listing_count = GREATEST(active_listing_count - 1, 0), updated_at = NOW()
WHERE id = $1::uuid`

// decrementListingCountBySQL é a versão em lote, usada pela limpeza de anúncios
// sumidos (DeleteStaleListings): um mesmo imóvel pode perder vários anúncios no
// mesmo run, e emitir um UPDATE por anúncio multiplicaria os round-trips sem
// nenhum ganho. A aritmética continua dentro do UPDATE, pelo mesmo motivo da
// versão unitária.
const decrementListingCountBySQL = `
UPDATE properties
SET active_listing_count = GREATEST(active_listing_count - $2, 0), updated_at = NOW()
WHERE id = $1::uuid`

// deleteOrphanPropertiesSQL apaga, de uma vez, os imóveis que ficaram sem
// nenhum anúncio. A guarda active_listing_count = 0 vive no próprio DELETE pelo
// mesmo motivo de deletePropertySQL: um read-then-delete deixaria um vínculo
// criado no meio virar anúncio órfão (a FK é ON DELETE SET NULL).
//
// Imóveis da lista que ainda têm anúncios simplesmente não casam com o WHERE —
// não é erro, é o caso normal de um property que perdeu um anúncio e manteve
// outros.
const deleteOrphanPropertiesSQL = `
DELETE FROM properties
WHERE id = ANY($1::uuid[]) AND active_listing_count = 0`

// selectPropertiesInBoundingBoxSQL é o **pré-filtro** da busca por raio: um
// retângulo de lat/lng que o índice btree composto idx_properties_lat_lng
// consegue usar. O recorte fino por distância real é feito em Go — o banco não
// tem PostGIS e uma fórmula de distância no WHERE inviabilizaria o índice.
//
// Imóveis sem geocodificação (lat/lng NULL) ficam de fora: não há como afirmar
// que estão dentro do raio.
const selectPropertiesInBoundingBoxSQL = `
SELECT` + propertyColumns + `
FROM properties
WHERE lat IS NOT NULL AND lng IS NOT NULL
  AND lat BETWEEN $1 AND $2
  AND lng BETWEEN $3 AND $4`

// CreateProperty insere um novo imóvel canônico e devolve a cópia com id,
// created_at e updated_at preenchidos pelo banco.
//
// property.ID é ignorado (o id é do banco) e property.ActiveListingCount também:
// um imóvel nasce sem anúncios vinculados, e o contador só se move por
// LinkListingToProperty/UnlinkListingFromProperty.
func CreateProperty(ctx context.Context, pool *pgxpool.Pool, property Property) (Property, error) {
	created := property
	created.ActiveListingCount = 0
	created.Amenities = normalizeTextArray(property.Amenities)
	created.Photos = normalizeTextArray(property.Photos)

	err := pool.QueryRow(ctx, insertPropertySQL, propertyInsertArgs(property)...).Scan(
		&created.ID,
		&created.CreatedAt,
		&created.UpdatedAt,
	)
	if err != nil {
		return Property{}, fmt.Errorf("db: create property: %w", err)
	}

	return created, nil
}

// GetPropertyByID devolve o imóvel canônico de um id.
//
// Id inexistente **não** é erro: retorna (nil, nil), mesmo contrato de
// GetSelectorsByDomain. Quem consulta um imóvel que pode ter sido apagado
// (correção de deduplicação) trata a ausência como caso normal; erro de banco
// continua sendo erro.
func GetPropertyByID(ctx context.Context, pool *pgxpool.Pool, id string) (*Property, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errors.New("db: get property: id is required")
	}

	property, err := scanProperty(pool.QueryRow(ctx, selectPropertyByIDSQL, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: get property %q: %w", id, err)
	}

	return &property, nil
}

// UpdateProperty reescreve os campos consolidados do imóvel identificado por
// property.ID e carimba updated_at.
//
// active_listing_count e created_at são preservados: o contador é mantido pelo
// par link/unlink, e permitir que um Update o sobrescrevesse dessincronizaria a
// contagem sem nenhum aviso.
//
// Id inexistente é erro (ErrPropertyNotFound), ao contrário do Get: aqui a
// ausência significa que uma atualização foi silenciosamente perdida.
func UpdateProperty(ctx context.Context, pool *pgxpool.Pool, property Property) error {
	args, err := propertyUpdateArgs(property)
	if err != nil {
		return fmt.Errorf("db: update property: %w", err)
	}

	tag, err := pool.Exec(ctx, updatePropertySQL, args...)
	if err != nil {
		return fmt.Errorf("db: update property %q: %w", property.ID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("db: update property %q: %w", property.ID, ErrPropertyNotFound)
	}

	return nil
}

// DeleteProperty apaga o imóvel canônico, **desde que** nenhum anúncio aponte
// para ele (active_listing_count = 0); caso contrário devolve
// ErrPropertyHasListings.
//
// A guarda está no próprio DELETE, e não num SELECT anterior, para não haver
// janela entre checar e apagar. Quando nada é apagado, uma segunda query
// distingue "não existe" de "ainda tem anúncios" — a essa altura já não há
// corrida a perder, porque o resultado é apenas a mensagem de erro.
//
// Não há cascata: a FK listings.property_id é ON DELETE SET NULL de propósito
// (os anúncios brutos não se recuperam sem nova raspagem). Na prática a guarda
// acima significa que os anúncios precisam ser desvinculados antes.
func DeleteProperty(ctx context.Context, pool *pgxpool.Pool, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("db: delete property: id is required")
	}

	tag, err := pool.Exec(ctx, deletePropertySQL, id)
	if err != nil {
		return fmt.Errorf("db: delete property %q: %w", id, err)
	}
	if tag.RowsAffected() > 0 {
		return nil
	}

	var count int
	err = pool.QueryRow(ctx, selectActiveListingCountSQL, id).Scan(&count)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("db: delete property %q: %w", id, ErrPropertyNotFound)
	}
	if err != nil {
		return fmt.Errorf("db: delete property %q: %w", id, err)
	}

	return fmt.Errorf("db: delete property %q: %w (%d listings)", id, ErrPropertyHasListings, count)
}

// GetActiveListingCount devolve quantos anúncios estão vinculados ao imóvel,
// lendo a coluna denormalizada. Id inexistente é ErrPropertyNotFound: pedir a
// contagem de um imóvel que não existe é erro de programação, não um zero.
func GetActiveListingCount(ctx context.Context, pool *pgxpool.Pool, propertyID string) (int, error) {
	propertyID = strings.TrimSpace(propertyID)
	if propertyID == "" {
		return 0, errors.New("db: get active listing count: property id is required")
	}

	var count int
	err := pool.QueryRow(ctx, selectActiveListingCountSQL, propertyID).Scan(&count)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("db: get active listing count for property %q: %w", propertyID, ErrPropertyNotFound)
	}
	if err != nil {
		return 0, fmt.Errorf("db: get active listing count for property %q: %w", propertyID, err)
	}

	return count, nil
}

// LinkListingToProperty associa o anúncio ao imóvel canônico e mantém
// properties.active_listing_count coerente — as duas coisas na **mesma
// transação**, porque um contador atualizado sem o vínculo (ou vice-versa) é
// uma inconsistência que nenhuma leitura posterior consegue detectar.
//
// É idempotente: religar o anúncio ao mesmo imóvel não incrementa de novo.
// Religar um anúncio que apontava para **outro** imóvel decrementa o antigo e
// incrementa o novo, na mesma transação.
//
// Anúncio inexistente é ErrListingNotFound e imóvel inexistente é
// ErrPropertyNotFound (a FK também barraria, mas com uma mensagem do PostgreSQL
// que não diz qual dos dois ids está errado).
func LinkListingToProperty(ctx context.Context, pool *pgxpool.Pool, propertyID string, listingID int64) error {
	propertyID = strings.TrimSpace(propertyID)
	if propertyID == "" {
		return errors.New("db: link listing to property: property id is required")
	}
	if listingID <= 0 {
		return fmt.Errorf("db: link listing to property %q: listing id is required", propertyID)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: link listing %d to property %q: begin transaction: %w", listingID, propertyID, err)
	}
	// Rollback após um Commit bem-sucedido é no-op no pgx; o defer cobre todos
	// os returns de erro abaixo sem duplicação.
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		currentPropertyID *string
		alreadyLinked     bool
	)
	err = tx.QueryRow(ctx, lockListingForLinkSQL, listingID, propertyID).Scan(&currentPropertyID, &alreadyLinked)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("db: link listing %d to property %q: %w", listingID, propertyID, ErrListingNotFound)
	}
	if err != nil {
		return fmt.Errorf("db: link listing %d to property %q: %w", listingID, propertyID, err)
	}

	if alreadyLinked {
		// Nada a fazer, mas ainda é preciso encerrar a transação para soltar o
		// FOR UPDATE. Commit e não Rollback porque não houve falha alguma.
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("db: link listing %d to property %q: commit: %w", listingID, propertyID, err)
		}
		return nil
	}

	// O incremento vem antes do UPDATE em listings para que um imóvel
	// inexistente produza ErrPropertyNotFound em vez do erro de FK.
	tag, err := tx.Exec(ctx, incrementListingCountSQL, propertyID)
	if err != nil {
		return fmt.Errorf("db: link listing %d to property %q: increment listing count: %w", listingID, propertyID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("db: link listing %d to property %q: %w", listingID, propertyID, ErrPropertyNotFound)
	}

	if _, err := tx.Exec(ctx, setListingPropertySQL, propertyID, listingID); err != nil {
		return fmt.Errorf("db: link listing %d to property %q: %w", listingID, propertyID, err)
	}

	if currentPropertyID != nil {
		if _, err := tx.Exec(ctx, decrementListingCountSQL, *currentPropertyID); err != nil {
			return fmt.Errorf("db: link listing %d to property %q: decrement previous property %q: %w",
				listingID, propertyID, *currentPropertyID, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: link listing %d to property %q: commit: %w", listingID, propertyID, err)
	}

	return nil
}

// UnlinkListingFromProperty desfaz o vínculo do anúncio e decrementa o contador
// do imóvel — na mesma transação, pelo mesmo motivo de
// LinkListingToProperty.
//
// Devolve o id do imóvel de que o anúncio acabou de ser desvinculado, ou string
// vazia quando não havia vínculo algum (anúncio já solto ou inexistente). O id
// vem do mesmo SELECT ... FOR UPDATE que a transação já faz: quem precisa
// decidir se o imóvel ficou órfão (ver grouping.HandleListingRemoval) usaria um
// SELECT extra, e entre ele e o desvínculo caberia outro vínculo.
//
// É idempotente nos dois sentidos: desvincular um anúncio que já estava solto é
// no-op (nunca decrementa, então o contador não fica negativo), e anúncio
// inexistente apenas loga um warning, seguindo o precedente de
// MarkSelectorsBroken — desvincular é limpeza, e falhar nela travaria o
// reprocessamento de uma deduplicação errada.
func UnlinkListingFromProperty(ctx context.Context, pool *pgxpool.Pool, listingID int64) (string, error) {
	if listingID <= 0 {
		return "", errors.New("db: unlink listing from property: listing id is required")
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("db: unlink listing %d: begin transaction: %w", listingID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var currentPropertyID *string
	err = tx.QueryRow(ctx, lockListingForUnlinkSQL, listingID).Scan(&currentPropertyID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("db: unlink listing %d: %w", listingID, err)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		slog.Warn("no listing row to unlink from property", "listing_id", listingID)
		return "", nil
	}

	// Já solto: sair sem tocar no contador é justamente o que impede um
	// desvínculo repetido de zerar (ou negativar) a contagem de um imóvel.
	if currentPropertyID == nil {
		if err := tx.Commit(ctx); err != nil {
			return "", fmt.Errorf("db: unlink listing %d: commit: %w", listingID, err)
		}
		return "", nil
	}

	if _, err := tx.Exec(ctx, clearListingPropertySQL, listingID); err != nil {
		return "", fmt.Errorf("db: unlink listing %d: %w", listingID, err)
	}

	if _, err := tx.Exec(ctx, decrementListingCountSQL, *currentPropertyID); err != nil {
		return "", fmt.Errorf("db: unlink listing %d: decrement property %q: %w", listingID, *currentPropertyID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("db: unlink listing %d: commit: %w", listingID, err)
	}

	return *currentPropertyID, nil
}

// FindPropertiesByCoordinates devolve os imóveis geocodificados a até
// radiusMeters do ponto informado, ordenados do mais próximo ao mais distante.
//
// A busca tem dois passos: um pré-filtro por bounding box no banco (que usa o
// índice btree composto idx_properties_lat_lng) e o corte fino por distância
// real (haversine) em Go. Só o retângulo devolveria os cantos, que estão fora do
// raio; só a distância impediria o uso do índice.
//
// **A conversão metros→graus é aproximada**: um grau de latitude é tratado como
// 111.320 m e o de longitude encolhe com o cosseno da latitude — o retângulo é
// calculado pela borda mais próxima do polo, de modo a nunca ficar estreito
// demais e perder um resultado válido. Perto dos polos e sobre o antimeridiano
// ele se abre para todas as longitudes (correto, apenas menos seletivo). Busca
// geoespacial "de verdade" (PostGIS ou índice apropriado) é assunto de outra
// task; aqui a Terra é uma esfera e a altitude não existe.
//
// Imóveis sem geocodificação (lat/lng NULL) são ignorados: não dá para afirmar
// que estão dentro do raio.
func FindPropertiesByCoordinates(ctx context.Context, pool *pgxpool.Pool, lat, lng float64, radiusMeters int) ([]Property, error) {
	if err := validateCoordinates(lat, lng); err != nil {
		return nil, fmt.Errorf("db: find properties by coordinates: %w", err)
	}
	if radiusMeters <= 0 {
		return nil, fmt.Errorf("db: find properties by coordinates: radiusMeters must be greater than 0, got %d", radiusMeters)
	}

	minLat, maxLat, minLng, maxLng := boundingBox(lat, lng, radiusMeters)

	rows, err := pool.Query(ctx, selectPropertiesInBoundingBoxSQL, minLat, maxLat, minLng, maxLng)
	if err != nil {
		return nil, fmt.Errorf("db: find properties by coordinates: %w", err)
	}
	defer rows.Close()

	candidates := make([]Property, 0, 16)
	for rows.Next() {
		property, err := scanProperty(rows)
		if err != nil {
			return nil, fmt.Errorf("db: find properties by coordinates: %w", err)
		}
		candidates = append(candidates, property)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: find properties by coordinates: %w", err)
	}

	return filterWithinRadius(candidates, lat, lng, radiusMeters), nil
}

// scanProperty lê uma linha na ordem de propertyColumns. Recebe pgx.Row para
// servir tanto ao QueryRow quanto ao laço de pgx.Rows.
func scanProperty(row pgx.Row) (Property, error) {
	var property Property
	err := row.Scan(
		&property.ID,
		&property.CanonicalAddress,
		&property.Neighborhood,
		&property.City,
		&property.State,
		&property.Lat,
		&property.Lng,
		&property.BedroomCount,
		&property.BathroomCount,
		&property.ParkingSpots,
		&property.AreaSqm,
		&property.Amenities,
		&property.Description,
		&property.Photos,
		&property.TransactionType,
		&property.PropertyType,
		&property.ActiveListingCount,
		&property.CreatedAt,
		&property.UpdatedAt,
	)
	if err != nil {
		return Property{}, err
	}

	// A coluna aceita NULL (ver migrations/CLAUDE.md); na leitura, NULL e vazio
	// são a mesma coisa, e o chamador não deveria ter que saber disso.
	property.Amenities = normalizeTextArray(property.Amenities)
	property.Photos = normalizeTextArray(property.Photos)

	return property, nil
}

// propertyInsertArgs monta os argumentos de insertPropertySQL na ordem dos
// placeholders. Os ponteiros nil chegam ao banco como NULL — é exatamente a
// distinção que o struct existe para preservar.
func propertyInsertArgs(property Property) []any {
	return []any{
		property.CanonicalAddress,
		property.Neighborhood,
		property.City,
		property.State,
		property.Lat,
		property.Lng,
		property.BedroomCount,
		property.BathroomCount,
		property.ParkingSpots,
		property.AreaSqm,
		normalizeTextArray(property.Amenities),
		property.Description,
		normalizeTextArray(property.Photos),
		property.TransactionType,
		property.PropertyType,
	}
}

// propertyUpdateArgs reaproveita a ordem do INSERT e acrescenta o id no fim —
// updatePropertySQL foi escrito com os mesmos placeholders justamente para que
// as duas listas não possam divergir.
func propertyUpdateArgs(property Property) ([]any, error) {
	id := strings.TrimSpace(property.ID)
	if id == "" {
		return nil, errors.New("id is required")
	}
	return append(propertyInsertArgs(property), id), nil
}

// validateCoordinates rejeita coordenadas fora da faixa antes de a query sair.
// Sem isso, um lat/lng invertido (o erro mais comum) devolveria silenciosamente
// zero resultados em vez de apontar o problema.
func validateCoordinates(lat, lng float64) error {
	if math.IsNaN(lat) || lat < -90 || lat > 90 {
		return fmt.Errorf("lat must be between -90 and 90, got %v", lat)
	}
	if math.IsNaN(lng) || lng < -180 || lng > 180 {
		return fmt.Errorf("lng must be between -180 and 180, got %v", lng)
	}
	return nil
}

// earthRadiusMeters é o raio médio da Terra (IUGG), usado pelo haversine.
const earthRadiusMeters = 6371008.8

// metersPerDegreeLat é o comprimento de um grau de latitude **na mesma esfera**
// que o haversine usa (≈111.195 m). Derivar do mesmo raio, em vez de usar um
// número tabelado, é o que garante que o retângulo e o filtro fino concordem:
// com constantes diferentes o retângulo pode ficar ligeiramente mais apertado
// que o círculo e perder resultados.
const metersPerDegreeLat = earthRadiusMeters * math.Pi / 180

// boundingBoxMargin alarga o retângulo em 0,1%. Cobre o achatamento da Terra
// (um grau de latitude varia ~0,3% entre equador e polo) e o arredondamento de
// ponto flutuante, sem custo perceptível de seletividade. Errar para mais é de
// graça (o haversine descarta o excesso); errar para menos perde imóveis.
const boundingBoxMargin = 1.001

// boundingBox devolve o retângulo lat/lng que **contém** o círculo de
// radiusMeters em torno do ponto. O contrato importante é este: o retângulo
// pode sobrar (e sobra), mas nunca pode faltar — quem corta de verdade é
// haversineMeters, e um resultado deixado de fora aqui não é recuperável.
//
// Por isso o delta de longitude usa a borda do retângulo mais próxima do polo
// (onde um grau de longitude é mais curto), e não a latitude central.
func boundingBox(lat, lng float64, radiusMeters int) (minLat, maxLat, minLng, maxLng float64) {
	deltaLat := boundingBoxMargin * float64(radiusMeters) / metersPerDegreeLat
	minLat = math.Max(lat-deltaLat, -90)
	maxLat = math.Min(lat+deltaLat, 90)

	worstLat := math.Max(math.Abs(minLat), math.Abs(maxLat))
	cosLat := math.Cos(worstLat * math.Pi / 180)
	if cosLat <= 1e-9 {
		// O retângulo toca um polo: todas as longitudes estão a menos de
		// radiusMeters dali.
		return minLat, maxLat, -180, 180
	}

	deltaLng := boundingBoxMargin * float64(radiusMeters) / (metersPerDegreeLat * cosLat)
	minLng = lng - deltaLng
	maxLng = lng + deltaLng
	if minLng < -180 || maxLng > 180 {
		// O intervalo cruza o antimeridiano, e um BETWEEN simples devolveria o
		// complemento do que se quer. Abrir para o mundo todo mantém o
		// resultado correto (o haversine faz o corte), só menos seletivo — e
		// esse é um caso irrelevante para imóveis brasileiros.
		return minLat, maxLat, -180, 180
	}

	return minLat, maxLat, minLng, maxLng
}

// filterWithinRadius descarta os candidatos que caíram no retângulo mas estão
// fora do círculo e ordena o resultado do mais próximo ao mais distante — a
// ordenação é feita aqui, e não com um ORDER BY, porque a distância só existe
// depois do haversine.
//
// Empates mantêm a ordem de chegada (SliceStable), para que a mesma consulta
// devolva sempre a mesma sequência.
func filterWithinRadius(candidates []Property, lat, lng float64, radiusMeters int) []Property {
	type scored struct {
		property Property
		distance float64
	}

	within := make([]scored, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Lat == nil || candidate.Lng == nil {
			continue
		}
		distance := haversineMeters(lat, lng, *candidate.Lat, *candidate.Lng)
		if distance > float64(radiusMeters) {
			continue
		}
		within = append(within, scored{property: candidate, distance: distance})
	}

	slices.SortStableFunc(within, func(a, b scored) int {
		switch {
		case a.distance < b.distance:
			return -1
		case a.distance > b.distance:
			return 1
		default:
			return 0
		}
	})

	result := make([]Property, 0, len(within))
	for _, item := range within {
		result = append(result, item.property)
	}

	return result
}

// haversineMeters é a distância em metros entre dois pontos sobre uma esfera de
// raio earthRadiusMeters. O erro em relação ao elipsoide real fica abaixo de
// 0,5% — irrelevante para "imóveis a 500 m daqui" e muito mais barato que
// Vincenty.
func haversineMeters(lat1, lng1, lat2, lng2 float64) float64 {
	const degToRad = math.Pi / 180

	deltaLat := (lat2 - lat1) * degToRad
	deltaLng := (lng2 - lng1) * degToRad

	sinLat := math.Sin(deltaLat / 2)
	sinLng := math.Sin(deltaLng / 2)

	a := sinLat*sinLat + math.Cos(lat1*degToRad)*math.Cos(lat2*degToRad)*sinLng*sinLng

	return 2 * earthRadiusMeters * math.Asin(math.Sqrt(math.Min(a, 1)))
}

// normalizeTextArray troca nil por slice vazia nas colunas TEXT[]. As colunas
// aceitam NULL, mas gravar NULL obrigaria todo leitor a distinguir "sem
// comodidades" de "não sei" — distinção que a consolidação não faz.
func normalizeTextArray(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
