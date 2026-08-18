package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// selectSelectorsByDomainSQL lê a linha de um domínio. O campo JSONB selectors
// é trazido cru e desserializado em Go (ver decodeSelectors), para que um JSON
// inválido produza um erro citando o domínio em vez de um erro genérico de scan.
const selectSelectorsByDomainSQL = `
SELECT domain, selectors, render_mode, status, last_validated_at
FROM site_selectors
WHERE domain = $1`

// upsertSelectorsSQL grava os seletores de um domínio. É chamado justamente
// quando a descoberta/validação deu certo, então status e last_validated_at são
// consequência da operação e não parâmetros: 'valid' e NOW() sempre.
//
// updated_at é setado à mão porque a tabela não tem trigger (ver
// migrations/CLAUDE.md).
const upsertSelectorsSQL = `
INSERT INTO site_selectors (domain, selectors, render_mode, status, last_validated_at, updated_at)
VALUES ($1, $2, $3, 'valid', NOW(), NOW())
ON CONFLICT (domain) DO UPDATE SET
	selectors         = EXCLUDED.selectors,
	render_mode       = EXCLUDED.render_mode,
	status            = 'valid',
	last_validated_at = NOW(),
	updated_at        = NOW()`

// markSelectorsBrokenSQL sinaliza que os seletores do domínio precisam de
// redescoberta. last_validated_at é preservado de propósito: continua sendo a
// data da última vez que os seletores funcionaram.
const markSelectorsBrokenSQL = `
UPDATE site_selectors
SET status = 'broken', updated_at = NOW()
WHERE domain = $1`

// GetSelectorsByDomain devolve a configuração de seletores do domínio.
//
// Domínio sem seletores conhecidos não é erro: retorna (nil, nil). É o caso
// normal de uma fonte nova, e o chamador reage acionando a descoberta pela IA.
func GetSelectorsByDomain(ctx context.Context, pool *pgxpool.Pool, domain string) (*SelectorConfig, error) {
	var (
		config    SelectorConfig
		rawFields []byte
	)

	err := pool.QueryRow(ctx, selectSelectorsByDomainSQL, domain).Scan(
		&config.Domain,
		&rawFields,
		&config.RenderMode,
		&config.Status,
		&config.LastValidatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: get selectors for domain %q: %w", domain, err)
	}

	config.Selectors, err = decodeSelectors(rawFields)
	if err != nil {
		return nil, fmt.Errorf("db: get selectors for domain %q: %w", domain, err)
	}

	return &config, nil
}

// UpsertSelectors insere ou atualiza os seletores do domínio, marcando-os como
// válidos e carimbando last_validated_at com NOW(). Só chame após ter
// confirmado que os seletores extraem itens: esta função é o registro de um
// sucesso, não uma gravação neutra.
//
// config.Status é ignorado (sempre grava 'valid'); use MarkSelectorsBroken para
// o caminho de falha.
func UpsertSelectors(ctx context.Context, pool *pgxpool.Pool, config SelectorConfig) error {
	domain := strings.TrimSpace(config.Domain)
	if domain == "" {
		return errors.New("db: upsert selectors: domain is required")
	}

	renderMode, err := normalizeRenderMode(config.RenderMode)
	if err != nil {
		return fmt.Errorf("db: upsert selectors for domain %q: %w", domain, err)
	}

	rawFields, err := json.Marshal(config.Selectors)
	if err != nil {
		return fmt.Errorf("db: upsert selectors for domain %q: encode selectors: %w", domain, err)
	}

	if _, err := pool.Exec(ctx, upsertSelectorsSQL, domain, rawFields, renderMode); err != nil {
		return fmt.Errorf("db: upsert selectors for domain %q: %w", domain, err)
	}

	return nil
}

// MarkSelectorsBroken marca os seletores do domínio como quebrados, para que a
// próxima coleta acione a redescoberta pela IA.
//
// Domínio inexistente não é erro (nada a invalidar), mas é logado: na prática
// significa que o chamador está usando um domínio diferente do que foi gravado
// — quase sempre falta de normalização do host.
func MarkSelectorsBroken(ctx context.Context, pool *pgxpool.Pool, domain string) error {
	tag, err := pool.Exec(ctx, markSelectorsBrokenSQL, domain)
	if err != nil {
		return fmt.Errorf("db: mark selectors broken for domain %q: %w", domain, err)
	}

	if tag.RowsAffected() == 0 {
		slog.Warn("no selectors row to mark as broken", "domain", domain)
	}

	return nil
}

// decodeSelectors desserializa o JSONB selectors. Chaves desconhecidas são
// ignoradas de propósito: a IA pode devolver campos extras, e derrubar a coleta
// por causa disso seria pior do que trabalhar com os seletores que se conhece.
func decodeSelectors(raw []byte) (SelectorFields, error) {
	var fields SelectorFields
	if len(raw) == 0 {
		return fields, errors.New("decode selectors: column is empty")
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return fields, fmt.Errorf("decode selectors: %w", err)
	}
	return fields, nil
}

// normalizeRenderMode valida o render_mode antes de ele chegar ao banco. Sem
// isso, um valor inesperado só falharia na CHECK constraint, com uma mensagem
// do PostgreSQL que não diz qual valor foi enviado.
//
// Vazio é aceito e vira RenderModeStatic: é o default da coluna e o modo da
// maioria dos sites.
func normalizeRenderMode(mode string) (string, error) {
	switch normalized := strings.ToLower(strings.TrimSpace(mode)); normalized {
	case "":
		return RenderModeStatic, nil
	case RenderModeStatic, RenderModeHeadless:
		return normalized, nil
	default:
		return "", fmt.Errorf("invalid render_mode %q (want %q or %q)", mode, RenderModeStatic, RenderModeHeadless)
	}
}
