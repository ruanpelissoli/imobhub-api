package enrichqueue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/imobhub/api/internal/db"
	"github.com/imobhub/api/internal/enrichment"
	"github.com/imobhub/api/internal/grouping"
)

// outcome é o desfecho do enriquecimento de um anúncio.
type outcome int

const (
	// outcomeEnriched é o caminho feliz: agrupado, consolidado e carimbado.
	outcomeEnriched outcome = iota
	// outcomeSkipped é o anúncio que saiu da fila sem ser agrupado — endereço
	// irresolvível ou sem coordenadas. **Não é falha**: carimbar é deliberado,
	// porque o geocoder tem negative caching e readiantar a mesma pergunta a
	// cada run só gasta quota.
	outcomeSkipped
	// outcomeFailed é o anúncio que continua pendente para o próximo run.
	outcomeFailed
	// outcomeAborted é o contexto cancelado no meio do anúncio. Não entra em
	// nenhum contador: o run inteiro está terminando, e contá-lo como falha
	// inflaria o resumo com anúncios que ninguém chegou a processar.
	outcomeAborted
)

// processListing executa as sete etapas do enriquecimento de um anúncio, nesta
// ordem exata:
//
//  1. NormalizeNeighborhood(address_raw) — em memória;
//  2. ExtractTerms(description_raw + bedrooms_raw) → bedroom_count, amenities;
//  3. Geocode(address_raw) → lat/lng;
//  4. SaveEnrichment — **persiste antes do merge**;
//  5. GroupListing → vincula property_id;
//  6. MergeProperty → consolida o canônico;
//  7. MarkEnriched → enriched_at = NOW().
//
// A ordem 4 → 6 é invariante, não estilo: grouping.MergePropertyData relê
// bedroom_count, amenities, image_urls e description_raw **do banco**
// (db.ListListingsByPropertyID). Consolidar antes de persistir gravaria NULL no
// canônico.
//
// city e state vão vazios em todas as chamadas porque listings não tem essas
// colunas (ver migrations/004 e enrichment/CLAUDE.md); os parâmetros existem
// para a assinatura não mudar quando o parsing de endereço evoluir.
func (p *Pipeline) processListing(ctx context.Context, listing db.PendingListing) outcome {
	neighborhood := p.deps.NormalizeNeighborhood(listing.AddressRaw, "")
	extracted := p.deps.ExtractTerms(extractionText(listing))

	enriched := db.ListingEnrichment{
		ID: listing.ID,
		// Bairro não reconhecido grava NULL, nunca "": é o contrato do
		// normalizador, e string vazia faria o agrupamento comparar anúncios
		// com um bairro em branco como se fosse um bairro de verdade.
		NormalizedNeighborhood: optionalString(neighborhood),
		BedroomCount:           extracted.BedroomCount,
		Amenities:              extracted.Amenities,
	}

	lat, lng, err := p.deps.Geocode(ctx, listing.AddressRaw, "", "")
	switch {
	case err == nil:
		enriched.Lat = &lat
		enriched.Lng = &lng

	case errors.Is(err, enrichment.ErrAddressNotFound), errors.Is(err, enrichment.ErrEmptyAddress):
		// **Não é falha**: o endereço não é resolvível, não é que não deu para
		// perguntar. lat/lng ficam NULL, o agrupamento é pulado (um canônico sem
		// geo nunca mais casa com nada) e o anúncio **é carimbado** — o geocoder
		// faz negative caching, então repetir a pergunta a cada run não muda o
		// resultado e só gasta quota.
		slog.InfoContext(ctx, "anúncio sem endereço resolvível: geocodificação e agrupamento pulados",
			"listing_id", listing.ID,
			"source_domain", listing.SourceDomain,
			"listing_url", listing.ListingURL,
			"error", err,
		)
		if !p.save(ctx, listing, enriched) {
			return p.outcomeFor(ctx, outcomeFailed)
		}
		if !p.markEnriched(ctx, listing) {
			return p.outcomeFor(ctx, outcomeFailed)
		}
		return p.outcomeFor(ctx, outcomeSkipped)

	default:
		// Rede, timeout ou status >= 400: transitório. Nada é persistido — um
		// UPDATE aqui gravaria lat/lng NULL **por cima** de um geocode válido de
		// um passe anterior — e enriched_at não é carimbado, para o próximo run
		// tentar de novo.
		p.logFailure(ctx, listing, "falha transitória na geocodificação do anúncio", err)
		return p.outcomeFor(ctx, outcomeFailed)
	}

	if !p.save(ctx, listing, enriched) {
		return p.outcomeFor(ctx, outcomeFailed)
	}

	propertyID, isNew, err := p.deps.GroupListing(ctx, groupingListing(listing, enriched))
	if err != nil {
		if errors.Is(err, grouping.ErrListingNotGeocoded) {
			// Defensivo: o ramo acima já garante coordenadas aqui. Se acontecer,
			// é um anúncio que não pode ser agrupado por dado que não existe, e
			// não uma falha a repetir — mesmo tratamento de ErrAddressNotFound.
			slog.WarnContext(ctx, "anúncio chegou ao agrupamento sem coordenadas: agrupamento pulado",
				"listing_id", listing.ID,
				"source_domain", listing.SourceDomain,
				"error", err,
			)
			if !p.markEnriched(ctx, listing) {
				return p.outcomeFor(ctx, outcomeFailed)
			}
			return p.outcomeFor(ctx, outcomeSkipped)
		}

		// Erro da IA ou do banco: o anúncio fica pendente (sem enriched_at) e a
		// próxima run tenta de novo. Os campos derivados já persistidos são
		// idempotentes, então repeti-los não custa nada.
		p.logFailure(ctx, listing, "falha ao agrupar o anúncio num imóvel canônico", err)
		return p.outcomeFor(ctx, outcomeFailed)
	}

	if err := p.deps.MergeProperty(ctx, propertyID); err != nil {
		// O vínculo já foi gravado; sem o carimbo o anúncio volta à fila, e lá
		// GroupListing sai cedo (anúncio já tem property_id) — a repetição custa
		// zero chamada de IA.
		p.logFailure(ctx, listing, fmt.Sprintf("falha ao consolidar o imóvel canônico %q", propertyID), err)
		return p.outcomeFor(ctx, outcomeFailed)
	}

	if !p.markEnriched(ctx, listing) {
		return p.outcomeFor(ctx, outcomeFailed)
	}

	slog.DebugContext(ctx, "anúncio enriquecido",
		"listing_id", listing.ID,
		"source_domain", listing.SourceDomain,
		"property_id", propertyID,
		"property_created", isNew,
	)

	return outcomeEnriched
}

// save persiste as colunas derivadas e devolve se deu certo. Falha aqui impede
// o carimbo: o merge leria do banco valores que nunca foram gravados.
func (p *Pipeline) save(ctx context.Context, listing db.PendingListing, enriched db.ListingEnrichment) bool {
	if err := p.deps.SaveEnrichment(ctx, enriched); err != nil {
		p.logFailure(ctx, listing, "falha ao persistir os campos enriquecidos do anúncio", err)
		return false
	}
	return true
}

// markEnriched carimba enriched_at e devolve se deu certo. Falhar aqui deixa o
// anúncio na fila com o trabalho já feito — a repetição é barata (o agrupamento
// sai cedo com o property_id já gravado) e é o lado seguro do erro.
func (p *Pipeline) markEnriched(ctx context.Context, listing db.PendingListing) bool {
	if err := p.deps.MarkEnriched(ctx, listing.ID); err != nil {
		p.logFailure(ctx, listing, "falha ao carimbar o anúncio como enriquecido", err)
		return false
	}
	return true
}

// logFailure registra a falha de um anúncio com o que identifica a linha e a
// fonte. Contexto cancelado não vira erro de anúncio: é SIGTERM chegando no meio
// de uma requisição, e uma cascata de erros idênticos esconderia a causa real —
// mesma regra de scraper.failSite.
func (p *Pipeline) logFailure(ctx context.Context, listing db.PendingListing, message string, err error) {
	if ctx.Err() != nil {
		return
	}

	slog.ErrorContext(ctx, message,
		"listing_id", listing.ID,
		"source_domain", listing.SourceDomain,
		"listing_url", listing.ListingURL,
		"error", err,
	)
}

// outcomeFor converte o desfecho em outcomeAborted quando o contexto já foi
// cancelado: o anúncio não falhou, o run acabou.
func (p *Pipeline) outcomeFor(ctx context.Context, result outcome) outcome {
	if ctx.Err() != nil {
		return outcomeAborted
	}
	return result
}

// extractionText junta o que alimenta o TermExtractor. bedrooms_raw entra porque
// alguns portais publicam o número só nesse campo ("3"), e a descrição sozinha
// não o traria; a quebra de linha impede que o fim de um texto cole no início do
// outro e crie um match que não existe em nenhum dos dois.
func extractionText(listing db.PendingListing) string {
	return listing.DescriptionRaw + "\n" + listing.BedroomsRaw
}

// groupingListing monta a entrada do agrupamento com os valores **recém
// calculados**, não com os da linha lida: o bairro e os quartos deste passe são
// os que o modelo deve comparar.
//
// PropertyID vem da linha porque é ele que dá a saída barata de
// grouping.GroupListing (anúncio já vinculado devolve o vínculo sem IA e sem
// banco) — é o que impede um passe pago sobre o catálogo inteiro.
func groupingListing(listing db.PendingListing, enriched db.ListingEnrichment) grouping.Listing {
	return grouping.Listing{
		ID:                     listing.ID,
		SourceDomain:           listing.SourceDomain,
		ListingURL:             listing.ListingURL,
		AddressRaw:             listing.AddressRaw,
		NormalizedNeighborhood: derefString(enriched.NormalizedNeighborhood),
		Lat:                    enriched.Lat,
		Lng:                    enriched.Lng,
		BedroomCount:           enriched.BedroomCount,
		AreaRaw:                listing.AreaRaw,
		Amenities:              enriched.Amenities,
		ImageURLs:              listing.ImageURLs,
		TitleRaw:               listing.TitleRaw,
		DescriptionRaw:         listing.DescriptionRaw,
		PropertyID:             listing.PropertyID,
	}
}

// optionalString devolve nil para texto vazio (ou só espaços). É o que traduz
// "não reconhecido" em NULL na coluna, preservando a distinção entre ausência e
// valor vazio.
func optionalString(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
