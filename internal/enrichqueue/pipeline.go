// Package enrichqueue liga os enrichers de internal/enrichment e o agrupamento
// de internal/grouping num pipeline assíncrono que drena a fila de anúncios
// pendentes (listings.enriched_at) ao final de cada coleta diária.
//
// O pacote existe separado de internal/enrichment pelo mesmo motivo de
// internal/grouping: aquele pacote não importa db nem ai por decisão registrada
// em internal/CLAUDE.md, e a fila precisa dos dois. O molde é o de
// scraper.Pipeline — dependências como campos de função em Deps, construtor que
// rejeita campo nulo e um NewPipeline(cfg, pool) com o wiring de produção.
package enrichqueue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/imobhub/api/internal/db"
	"github.com/imobhub/api/internal/enrichment"
	"github.com/imobhub/api/internal/grouping"
)

// Erros de construção do pipeline. São de wiring (bug de inicialização), não de
// processamento: derrubam o boot em vez de pular um anúncio.
var (
	// ErrMissingConfig indica RunEnrichment chamado sem configuração.
	ErrMissingConfig = errors.New("enrichqueue: config is required to run the pipeline")
	// ErrMissingPool indica RunEnrichment chamado sem pool de conexões.
	ErrMissingPool = errors.New("enrichqueue: database pool is required to run the pipeline")
	// ErrMissingDependency indica que uma das dependências de Deps ficou nula.
	// Todas são obrigatórias: um campo nulo só apareceria como panic no meio do
	// primeiro anúncio da fila, horas depois do boot.
	ErrMissingDependency = errors.New("enrichqueue: pipeline dependency is missing")
	// ErrInvalidWorkers indica Workers não positivo. Zero worker é uma fila
	// parada que parece "funcionando" nos logs.
	ErrInvalidWorkers = errors.New("enrichqueue: workers must be greater than 0")
	// ErrInvalidBatchSize indica BatchSize não positivo. Lote zero drenaria a
	// fila em zero anúncios por vez, ou seja, nunca.
	ErrInvalidBatchSize = errors.New("enrichqueue: batch size must be greater than 0")
)

// DefaultBatchSize é quantos anúncios pendentes são lidos por vez. O lote não é
// a unidade de trabalho (os workers consomem anúncio a anúncio); ele existe para
// que a fila não carregue o catálogo inteiro em memória de uma vez.
const DefaultBatchSize = 200

// Deps são as dependências do pipeline, injetadas como funções em vez de
// interfaces — mesmo estilo de scraper.Deps e pelo mesmo motivo: cada
// colaborador tem uma operação só, e o teste vira um closure por campo, sem
// mock, sem PostgreSQL, sem rede e sem gastar token da Anthropic.
//
// Todos os campos são obrigatórios; New valida.
//
// **Invariante de wiring**: os colaboradores por trás de Geocode, ExtractTerms,
// NormalizeNeighborhood, GroupListing e MergeProperty precisam ser instâncias
// **únicas**, compartilhadas por todos os workers. Ver NewPipeline.
type Deps struct {
	// ListPending lê um lote da fila a partir do cursor
	// (db.ListPendingListings).
	ListPending func(ctx context.Context, afterID int64, limit int) ([]db.PendingListing, error)
	// NormalizeNeighborhood deriva o bairro canônico do endereço bruto
	// (enrichment.NeighborhoodNormalizer.Normalize). Em memória, sem I/O.
	NormalizeNeighborhood func(raw, city string) string
	// ExtractTerms reconhece quartos e comodidades no texto livre
	// (enrichment.TermExtractor.Extract). Em memória, sem I/O.
	ExtractTerms func(text string) enrichment.ExtractedData
	// Geocode converte o endereço em coordenadas
	// (enrichment.Geocoder.Geocode). É a etapa que serializa em 1 req/s.
	Geocode func(ctx context.Context, address, city, state string) (lat, lng float64, err error)
	// SaveEnrichment persiste as colunas derivadas
	// (db.UpdateListingEnrichment). Roda **antes** do merge: a consolidação
	// relê esses campos do banco.
	SaveEnrichment func(ctx context.Context, enrichment db.ListingEnrichment) error
	// GroupListing vincula o anúncio a um imóvel canônico
	// (grouping.PropertyGrouper.GroupListing).
	GroupListing func(ctx context.Context, listing grouping.Listing) (string, bool, error)
	// MergeProperty reconsolida o canônico a partir de todos os anúncios
	// vinculados (grouping.PropertyGrouper.MergePropertyData).
	MergeProperty func(ctx context.Context, propertyID string) error
	// MarkEnriched carimba enriched_at, tirando o anúncio da fila
	// (db.MarkListingEnriched). É sempre o último passo.
	MarkEnriched func(ctx context.Context, listingID int64) error

	// Workers é a concorrência do pool (config.EnrichmentWorkers).
	Workers int
	// BatchSize é o LIMIT de cada leitura da fila.
	BatchSize int
}

// Summary é o resultado agregado de uma execução da fila.
type Summary struct {
	// Queued é quantos anúncios pendentes foram lidos da fila.
	Queued int
	// Enriched é quantos completaram o fluxo e receberam enriched_at.
	Enriched int
	// Skipped é quantos saíram da fila sem agrupamento — endereço irresolvível
	// ou anúncio sem coordenadas. Não são falha: carimbam enriched_at e não
	// voltam à fila.
	Skipped int
	// Failed é quantos falharam e continuam pendentes para o próximo run.
	Failed int
}

// Pipeline drena a fila de enriquecimento. Use NewPipeline (wiring de produção)
// ou New (wiring explícito, usado nos testes).
type Pipeline struct {
	deps Deps
}

// New monta o pipeline a partir de dependências explícitas. Existe para os
// testes e para qualquer chamador que precise trocar um colaborador; o caminho
// normal é NewPipeline.
func New(deps Deps) (*Pipeline, error) {
	required := []struct {
		name  string
		isNil bool
	}{
		{"ListPending", deps.ListPending == nil},
		{"NormalizeNeighborhood", deps.NormalizeNeighborhood == nil},
		{"ExtractTerms", deps.ExtractTerms == nil},
		{"Geocode", deps.Geocode == nil},
		{"SaveEnrichment", deps.SaveEnrichment == nil},
		{"GroupListing", deps.GroupListing == nil},
		{"MergeProperty", deps.MergeProperty == nil},
		{"MarkEnriched", deps.MarkEnriched == nil},
	}
	for _, field := range required {
		if field.isNil {
			return nil, fmt.Errorf("%w: %s", ErrMissingDependency, field.name)
		}
	}

	if deps.Workers <= 0 {
		return nil, fmt.Errorf("%w, got %d", ErrInvalidWorkers, deps.Workers)
	}
	if deps.BatchSize <= 0 {
		return nil, fmt.Errorf("%w, got %d", ErrInvalidBatchSize, deps.BatchSize)
	}

	return &Pipeline{deps: deps}, nil
}

// Run drena a fila até ela vir vazia e devolve o resumo do que aconteceu.
//
// A leitura é em lotes com cursor por id (keyset): um anúncio que falha **não**
// recebe enriched_at e continuaria casando com o predicado da fila, então um
// LIMIT sem cursor devolveria o mesmo lote para sempre. Com o cursor, a drenagem
// avança e os anúncios que falharam ficam para o run seguinte.
//
// Cada lote é distribuído entre Workers goroutines por um canal; o canal é
// sempre fechado e o Wait sempre acontece antes do return, inclusive nos
// caminhos de erro — nenhuma goroutine vaza.
//
// Erro só é devolvido em falha fatal: leitura da fila impossível ou contexto
// cancelado. Falha de um anúncio é logada com listing_id/source_domain, contada
// no resumo, e o pipeline segue para o próximo — um endereço que o Nominatim não
// resolve não pode parar o enriquecimento do catálogo inteiro.
func (p *Pipeline) Run(ctx context.Context) (Summary, error) {
	summary := Summary{}

	slog.InfoContext(ctx, fmt.Sprintf("fila de enriquecimento iniciada com %d workers", p.deps.Workers),
		"workers", p.deps.Workers,
		"batch_size", p.deps.BatchSize,
	)

	var cursor int64
	for {
		// Cancelamento é verificado entre os lotes para que um SIGTERM encerre a
		// fila em vez de arrastar os lotes restantes até falharem um a um.
		if ctxErr := ctx.Err(); ctxErr != nil {
			p.logSummary(ctx, summary, true)
			return summary, fmt.Errorf("enrichqueue: pipeline canceled: %w", ctxErr)
		}

		batch, err := p.deps.ListPending(ctx, cursor, p.deps.BatchSize)
		if err != nil {
			p.logSummary(ctx, summary, true)
			return summary, fmt.Errorf("enrichqueue: could not read the pending listings queue after id %d: %w", cursor, err)
		}
		if len(batch) == 0 {
			break
		}

		summary.Queued += len(batch)
		// O lote vem ordenado por id (contrato de db.ListPendingListings), então
		// o último é o maior — e é ele que faz o próximo lote avançar.
		cursor = batch[len(batch)-1].ID

		batchSummary := p.processBatch(ctx, batch)
		summary.Enriched += batchSummary.Enriched
		summary.Skipped += batchSummary.Skipped
		summary.Failed += batchSummary.Failed

		if ctxErr := ctx.Err(); ctxErr != nil {
			p.logSummary(ctx, summary, true)
			return summary, fmt.Errorf("enrichqueue: pipeline canceled: %w", ctxErr)
		}
	}

	p.logSummary(ctx, summary, false)
	return summary, nil
}

// processBatch distribui o lote entre os workers e devolve os contadores
// agregados.
//
// Cada worker acumula seus próprios contadores e eles são somados no fim, em vez
// de sync/atomic espalhado: o ponto de junção é único e o código de cada etapa
// fica livre de sincronização.
func (p *Pipeline) processBatch(ctx context.Context, batch []db.PendingListing) Summary {
	workers := min(p.deps.Workers, len(batch))

	queue := make(chan db.PendingListing)
	results := make([]Summary, workers)

	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			for listing := range queue {
				// Sair do laço deixaria o canal com itens pendentes e travaria o
				// produtor; o produtor também checa o contexto, então os dois
				// lados convergem para o fechamento ordenado.
				if ctx.Err() != nil {
					return
				}
				p.tally(&results[slot], p.processListing(ctx, listing))
			}
		}(i)
	}

	for _, listing := range batch {
		select {
		case queue <- listing:
		case <-ctx.Done():
			// Fecha e espera antes de retornar: os workers que já saíram do
			// range não consomem mais nada, e sem o close os demais ficariam
			// bloqueados para sempre.
			close(queue)
			wg.Wait()
			return sumSummaries(results)
		}
	}

	close(queue)
	wg.Wait()

	return sumSummaries(results)
}

// tally contabiliza o desfecho de um anúncio no resumo do worker.
func (p *Pipeline) tally(summary *Summary, result outcome) {
	switch result {
	case outcomeEnriched:
		summary.Enriched++
	case outcomeSkipped:
		summary.Skipped++
	case outcomeFailed:
		summary.Failed++
	}
}

// sumSummaries agrega os contadores por worker num resumo do lote.
func sumSummaries(results []Summary) Summary {
	total := Summary{}
	for _, result := range results {
		total.Enriched += result.Enriched
		total.Skipped += result.Skipped
		total.Failed += result.Failed
	}
	return total
}

// logSummary emite o resumo final, no formato do scraper: mensagem humana (é a
// linha que o operador procura) mais os atributos estruturados para filtrar e
// somar.
func (p *Pipeline) logSummary(ctx context.Context, summary Summary, interrupted bool) {
	message := fmt.Sprintf("enriquecimento concluído: %d anúncios enfileirados (%d enriquecidos, %d pulados, %d com erro)",
		summary.Queued, summary.Enriched, summary.Skipped, summary.Failed)
	if interrupted {
		message = fmt.Sprintf("enriquecimento interrompido: %d anúncios enfileirados (%d enriquecidos, %d pulados, %d com erro)",
			summary.Queued, summary.Enriched, summary.Skipped, summary.Failed)
	}

	attrs := []any{
		"queued", summary.Queued,
		"enriched", summary.Enriched,
		"skipped", summary.Skipped,
		"failed", summary.Failed,
		"workers", p.deps.Workers,
	}

	if interrupted {
		slog.WarnContext(ctx, message, attrs...)
		return
	}
	slog.InfoContext(ctx, message, attrs...)
}
