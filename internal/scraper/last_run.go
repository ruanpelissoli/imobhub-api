package scraper

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// LastRunCacheKey é a chave do resumo da última coleta no Redis. É exportada
// para que um leitor futuro (o endpoint de saúde do scraper na API) reuse a
// string em vez de duplicá-la — o desenho da chave mora em quem consome o
// cache, nunca em internal/cache.
const LastRunCacheKey = "scraper:last_run"

const (
	// lastRunTTL impede que o resumo de um scraper parado fique no Redis para
	// sempre: 48h cobre um ciclo diário perdido e expira no segundo, que é
	// exatamente o sinal que o operador precisa ver.
	lastRunTTL = 48 * time.Hour

	// lastRunWriteTimeout limita a gravação. O resumo é observabilidade: ele não
	// pode segurar o encerramento do processo se o Redis estiver lento.
	lastRunWriteTimeout = 3 * time.Second
)

// LastRun é o payload publicado em LastRunCacheKey ao fim de cada coleta.
//
// Success é true apenas quando nenhuma fonte falhou **e** o run não foi
// abortado (contexto cancelado ou falha fatal). Fonte bloqueada por robots.txt
// não derruba o Success: é o pipeline funcionando.
type LastRun struct {
	RanAt             string `json:"ran_at"`
	Success           bool   `json:"success"`
	SitesTotal        int    `json:"sites_total"`
	SitesSucceeded    int    `json:"sites_succeeded"`
	SitesFailed       int    `json:"sites_failed"`
	SitesBlocked      int    `json:"sites_blocked"`
	ListingsFound     int    `json:"listings_found"`
	ListingsRemoved   int64  `json:"listings_removed"`
	ListingsTotalInDB int64  `json:"listings_total_in_db"`
	DurationMS        int64  `json:"duration_ms"`
}

// lastRunStore é declarado no consumidor para que os testes deste pacote rodem
// sem Redis (mesma convenção de api.cacheStore).
type lastRunStore interface {
	Set(ctx context.Context, key string, value any, expiration time.Duration) *redis.StatusCmd
}

type lastRunPublisher struct {
	store lastRunStore
}

// newLastRunPublisher checa o nil **antes** da atribuição à interface: guardar
// um *redis.Client nil em lastRunStore produziria uma interface não-nil e um
// panic na primeira gravação. Um *lastRunPublisher nil é o caminho válido "sem
// persistência".
func newLastRunPublisher(client *redis.Client) *lastRunPublisher {
	if client == nil {
		return nil
	}
	return &lastRunPublisher{store: client}
}

// publish grava o resumo em LastRunCacheKey. Qualquer falha vira Warn e é
// engolida: o resultado e o exit code do run não dependem do Redis.
func (p *lastRunPublisher) publish(ctx context.Context, run LastRun) {
	if p == nil || p.store == nil {
		return
	}

	payload, err := json.Marshal(run)
	if err != nil {
		slog.WarnContext(ctx, "não foi possível serializar o resumo da última coleta",
			"event", eventLastRunPublishFailed,
			"error", err,
		)
		return
	}

	// No caminho de SIGTERM o contexto do run já chega cancelado aqui — que é
	// justamente o cenário em que o resumo mais importa. WithoutCancel corta a
	// herança do cancelamento; o timeout próprio mantém a gravação curta.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lastRunWriteTimeout)
	defer cancel()

	if err := p.store.Set(writeCtx, LastRunCacheKey, payload, lastRunTTL).Err(); err != nil {
		slog.WarnContext(ctx, "não foi possível publicar o resumo da última coleta no Redis",
			"event", eventLastRunPublishFailed,
			"key", LastRunCacheKey,
			"error", err,
		)
	}
}
