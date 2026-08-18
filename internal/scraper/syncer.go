package scraper

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/imobhub/api/internal/db"
)

// Erros de uso de SyncListings. São sentinelas para que o chamador distinga um
// erro de programação (argumento faltando no wiring da orquestração) de uma
// falha de banco, que é transitória e vale retry.
var (
	// ErrMissingSyncDomain indica chamada sem domínio. O domínio é o filtro do
	// DELETE de anúncios sumidos; sem ele não há como delimitar a limpeza.
	ErrMissingSyncDomain = errors.New("scraper: domain is required to sync listings")
	// ErrMissingRunStart indica chamada sem o instante de início do run. Sem
	// esse corte, a limpeza não teria como distinguir o que foi visto agora do
	// que sumiu do site.
	ErrMissingRunStart = errors.New("scraper: runStartedAt is required to sync listings")
)

// Costuras de teste: o acesso ao banco em internal/db são funções livres
// (decisão daquele pacote), então trocá-las aqui é o único jeito de exercitar o
// fluxo de sincronização — inclusive a guarda de zero anúncios, que é o
// comportamento mais importante deste arquivo — sem um PostgreSQL de verdade.
// Não exporte: em produção o wiring é sempre este.
var (
	upsertListings      = db.UpsertListings
	deleteStaleListings = db.DeleteStaleListings
)

// SyncStats resume o que a sincronização de um domínio fez no banco.
type SyncStats struct {
	// Upserted é quantos anúncios foram gravados (inseridos ou atualizados) —
	// ou seja, o total extraído do site nesta coleta.
	Upserted int64
	// Deleted é quantos anúncios do domínio deixaram de existir no site e foram
	// removidos.
	Deleted int64
}

// SyncListings reconcilia os anúncios extraídos de um domínio com o que está
// gravado no banco: grava todos os extraídos (renovando o last_seen_at de cada
// um) e apaga os que não apareceram nesta coleta.
//
// runStartedAt precisa ser o instante lido **imediatamente antes de começar a
// processar este domínio**, não o início global do pipeline. Se fosse o início
// do pipeline, os anúncios gravados por um domínio processado no começo do run
// já teriam last_seen_at posterior ao corte de outro — e o corte perderia o
// sentido de "visto nesta coleta". Usar "agora" seria pior ainda: apagaria tudo
// o que acabou de ser gravado.
//
// **Zero anúncios extraídos não deleta nada.** Esta é a proteção central do
// módulo: uma coleta pode devolver zero anúncios porque o site mudou de layout,
// porque respondeu com uma página de erro com status 200 ou porque o JS não
// terminou de renderizar. Em nenhum desses casos o catálogo do domínio sumiu de
// verdade — e prosseguir com a limpeza apagaria todo o histórico acumulado por
// causa de uma falha silenciosa. Sem anúncios, nenhuma operação de banco é
// executada e as estatísticas voltam zeradas.
//
// Em caso de erro as estatísticas voltam zeradas: parte do trabalho pode ter
// sido feita no banco, mas o número não é confiável o bastante para alimentar
// relatório. O chamador deve tratar o erro como "este domínio falhou".
func SyncListings(ctx context.Context, pool *pgxpool.Pool, domain string, extracted []db.RawListing, runStartedAt time.Time) (SyncStats, error) {
	domain = strings.TrimSpace(domain)
	if domain == "" {
		return SyncStats{}, ErrMissingSyncDomain
	}
	// Validado aqui, e não só dentro de db.DeleteStaleListings, para que o
	// esquecimento apareça antes de qualquer escrita — e não depois de os
	// upserts já terem entrado.
	if runStartedAt.IsZero() {
		return SyncStats{}, fmt.Errorf("%w (domain %q)", ErrMissingRunStart, domain)
	}

	if len(extracted) == 0 {
		// Warn e não Info: zero anúncios num domínio configurado é sempre
		// suspeito, e este é o log que explica por que a contagem do domínio
		// ficou parada no dia seguinte.
		slog.WarnContext(ctx, fmt.Sprintf("[%s] sync ignorado: nenhum anúncio extraído, nada foi deletado", domain),
			"domain", domain,
		)
		return SyncStats{}, nil
	}

	// A ordem importa: o upsert renova o last_seen_at de tudo que foi visto
	// agora, e só depois o DELETE remove o que ficou para trás do corte. Inverter
	// apagaria anúncios vivos na janela entre as duas operações.
	if err := upsertListings(ctx, pool, extracted); err != nil {
		return SyncStats{}, fmt.Errorf("scraper: syncing listings of %q: %w", domain, err)
	}

	deleted, err := deleteStaleListings(ctx, pool, domain, runStartedAt)
	if err != nil {
		return SyncStats{}, fmt.Errorf("scraper: syncing listings of %q: %w", domain, err)
	}

	stats := SyncStats{Upserted: int64(len(extracted)), Deleted: deleted}

	// Mensagem em formato humano por decisão de produto (é o resumo que o
	// operador procura ao acompanhar uma coleta); os atributos estruturados
	// existem para filtrar e somar por domínio.
	slog.InfoContext(ctx, fmt.Sprintf("[%s] sync concluído: %d upserted, %d deletados", domain, stats.Upserted, stats.Deleted),
		"domain", domain,
		"upserted", stats.Upserted,
		"deleted", stats.Deleted,
	)

	return stats, nil
}
