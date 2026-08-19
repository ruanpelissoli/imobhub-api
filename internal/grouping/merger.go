package grouping

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/imobhub/api/internal/db"
)

// Erros da consolidação. São sentinelas (errors.Is) pelo mesmo motivo dos erros
// do agrupamento: a fila precisa distinguir "este id não existe mais" (imóvel
// apagado numa correção de deduplicação — descartar o item) de um erro de banco
// transitório, que merece nova tentativa.
var (
	// ErrMissingPropertyID indica chamada sem o id do imóvel canônico.
	ErrMissingPropertyID = errors.New("grouping: property id is required")
	// ErrPropertyNotFound indica que o imóvel a consolidar não existe mais.
	ErrPropertyNotFound = errors.New("grouping: property not found")
)

// maxMergedPhotos limita quantas fotos vão para properties.photos. O corte
// existe porque um imóvel muito anunciado acumularia centenas de URLs quase
// idênticas, engordando toda leitura do canônico sem acrescentar informação.
// Junto com a ordem determinística da leitura (listings.id asc, e a ordem
// original dentro de cada array), ele torna o resultado reproduzível: são
// sempre as **mesmas** 50 fotos.
const maxMergedPhotos = 50

// MergePropertyData reconsolida o imóvel canônico a partir de **todos** os
// anúncios vinculados a ele: união das fotos, melhor descrição disponível, união
// das comodidades e contagem de quartos.
//
// Existe porque propertyFrom só enxerga o anúncio que criou o canônico. Sem esta
// passagem, o registro ficaria congelado nos dados do primeiro anúncio mesmo
// depois de outros portais trazerem mais fotos e um texto melhor.
//
// O fluxo é **read-modify-write, e isso é obrigatório**: db.UpdateProperty
// reescreve todas as colunas consolidadas de uma vez, então montar um db.Property
// só com os campos deste merge apagaria endereço, bairro, cidade, estado,
// lat/lng e área — e um canônico sem geo nunca mais casa com nada em
// FindPropertiesByCoordinates. Ler primeiro e sobrescrever só o que este merge
// calcula é o que preserva o restante.
//
// Imóvel sem nenhum anúncio é **no-op sem erro**: o vínculo pode ter acabado de
// ser desfeito, e apagar as fotos por causa disso seria perda de dado.
func (g *PropertyGrouper) MergePropertyData(ctx context.Context, propertyID string) error {
	propertyID = strings.TrimSpace(propertyID)
	if propertyID == "" {
		return ErrMissingPropertyID
	}

	property, err := g.store.GetPropertyByID(ctx, propertyID)
	if err != nil {
		return fmt.Errorf("grouping: merging property %q: %w", propertyID, err)
	}
	if property == nil {
		return fmt.Errorf("grouping: merging property %q: %w", propertyID, ErrPropertyNotFound)
	}

	listings, err := g.store.ListListingsByPropertyID(ctx, propertyID)
	if err != nil {
		return fmt.Errorf("grouping: listing the listings of property %q: %w", propertyID, err)
	}
	if len(listings) == 0 {
		slog.DebugContext(ctx, "property has no listings to merge", "property_id", propertyID)
		return nil
	}

	property.Photos = mergePhotos(listings)
	property.Amenities = mergeAmenities(listings)
	property.BedroomCount = pickBedroomCount(listings)

	// A descrição é o único campo que **não** é substituído quando os anúncios
	// não têm nada a dizer: um texto já consolidado é melhor que nenhum, e
	// apagá-lo não devolveria informação alguma ao canônico.
	description := pickDescription(listings)
	descriptionChanged := false
	if description != "" {
		descriptionChanged = property.Description == nil || *property.Description != description
		property.Description = &description
	}

	if err := g.store.UpdateProperty(ctx, *property); err != nil {
		return fmt.Errorf("grouping: updating merged property %q: %w", propertyID, err)
	}

	slog.DebugContext(ctx, "property data merged",
		"property_id", propertyID,
		"listings", len(listings),
		"photos", len(property.Photos),
		"amenities", len(property.Amenities),
		"description_changed", descriptionChanged,
	)

	return nil
}

// mergePhotos concatena as image_urls dos anúncios na ordem em que eles vieram
// do banco (listings.id asc) e, dentro de cada um, na ordem original do array.
//
// A deduplicação é por igualdade **exata**: barra final e query string não são
// normalizadas de propósito, porque num CDN elas costumam distinguir recortes e
// tamanhos diferentes da mesma foto — e descartar o "duplicado" errado perderia
// a única versão utilizável.
func mergePhotos(listings []db.Listing) []string {
	photos := make([]string, 0, maxMergedPhotos)
	seen := make(map[string]struct{}, maxMergedPhotos)

	for _, listing := range listings {
		for _, url := range listing.ImageURLs {
			if strings.TrimSpace(url) == "" {
				continue
			}
			if _, duplicate := seen[url]; duplicate {
				continue
			}
			seen[url] = struct{}{}
			photos = append(photos, url)
			if len(photos) == maxMergedPhotos {
				return photos
			}
		}
	}

	return photos
}

// mergeAmenities é a união das comodidades, na mesma ordem determinística de
// mergePhotos e sem limite: a lista é curta por natureza e cortá-la esconderia
// justamente o diferencial que um portal descreveu e outro não.
func mergeAmenities(listings []db.Listing) []string {
	amenities := make([]string, 0, 8)
	seen := make(map[string]struct{}, 8)

	for _, listing := range listings {
		for _, amenity := range listing.Amenities {
			if strings.TrimSpace(amenity) == "" {
				continue
			}
			if _, duplicate := seen[amenity]; duplicate {
				continue
			}
			seen[amenity] = struct{}{}
			amenities = append(amenities, amenity)
		}
	}

	return amenities
}

// pickDescription devolve a descrição mais longa entre os anúncios, ou "" se
// nenhum tiver texto.
//
// "Mais longa" é heurística assumida: o texto maior costuma ser o que traz
// planta, condomínio e proximidades, e não há sinal melhor sem gastar uma
// chamada de IA por imóvel. A contagem é em **runes**, não em bytes — os textos
// são pt-BR e cada acento vale dois bytes, o que faria o mesmo texto "crescer"
// só por estar acentuado.
//
// A comparação é estritamente maior, então um empate fica com o anúncio de menor
// id: é o que torna a escolha reproduzível entre execuções.
func pickDescription(listings []db.Listing) string {
	best := ""
	bestLength := 0

	for _, listing := range listings {
		description := strings.TrimSpace(listing.DescriptionRaw)
		if description == "" {
			continue
		}
		if length := utf8.RuneCountInString(description); length > bestLength {
			best, bestLength = description, length
		}
	}

	return best
}

// pickBedroomCount consolida a contagem de quartos por voto de maioria entre os
// anúncios que a informam, com o empate resolvido pelo valor visto primeiro
// (menor listings.id).
//
// A maioria, e não "o anúncio mais recente", porque o dado vem de um parser
// sobre texto livre: um erro isolado de extração não deve reescrever o canônico
// que dois outros portais confirmam.
//
// Devolve nil quando **nenhum** anúncio informa — nunca 0. Em db.Property a
// ausência é informação, e 0 significaria "sem quarto".
func pickBedroomCount(listings []db.Listing) *int {
	votes := make(map[int]int, 4)
	order := make([]int, 0, 4)

	for _, listing := range listings {
		if listing.BedroomCount == nil {
			continue
		}
		value := *listing.BedroomCount
		if _, known := votes[value]; !known {
			order = append(order, value)
		}
		votes[value]++
	}

	if len(order) == 0 {
		return nil
	}

	best := order[0]
	for _, value := range order[1:] {
		if votes[value] > votes[best] {
			best = value
		}
	}

	// Cópia: o ponteiro não pode ser compartilhado com o anúncio lido, que o
	// chamador pode reaproveitar.
	return &best
}
