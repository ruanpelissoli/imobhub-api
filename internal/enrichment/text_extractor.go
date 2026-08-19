// Package enrichment extrai dados estruturados do texto livre dos anúncios já
// coletados (listings.description_raw, listings.bedrooms_raw), alimentando as
// colunas de enriquecimento de `listings` e, mais adiante, o `properties`
// canônico.
//
// O pacote é puro: não acessa banco, não faz rede e não lê variáveis de
// ambiente. O único I/O é a leitura, no construtor, do arquivo de vocabulário de
// comodidades, cujo caminho chega por parâmetro (default em
// config.DefaultAmenitiesFile). Quem persiste o resultado e quem escolhe os
// anúncios a processar é o pipeline de enriquecimento.
//
// A extração é feita com expressões regulares e comparação de termos, não com
// IA: são dezenas de milhares de anúncios por passe, o vocabulário imobiliário
// brasileiro é pequeno e fechado, e uma chamada de rede por anúncio custaria
// tempo e dinheiro sem ganho de precisão perceptível.
package enrichment

import (
	"regexp"
	"strconv"
	"strings"
)

// ExtractedData é o resultado da análise de um texto de anúncio.
type ExtractedData struct {
	// BedroomCount é o número de quartos. **nil significa "o anúncio não
	// informa"**, e 0 significa "imóvel sem quarto separado" (studio, kitnet).
	// São situações diferentes: a coluna listings.bedroom_count é NULL no
	// primeiro caso (ver migrations/004_enrich_listings.sql) e um `int` simples
	// tornaria as duas indistinguíveis, transformando todo anúncio silencioso em
	// studio.
	BedroomCount *int
	// Amenities são os nomes canônicos das comodidades encontradas, sem
	// duplicatas e na ordem em que aparecem no arquivo de vocabulário — nunca na
	// ordem do texto nem na de iteração de um mapa. Ordem estável é o que torna o
	// resultado comparável entre passes e testável sem sort.
	Amenities []string
}

// TextExtractor analisa texto livre de anúncio e devolve os dados estruturados
// que consegue reconhecer.
//
// Extract **nunca** devolve erro: texto de anúncio é sempre "válido", o que
// varia é quanto dele é reconhecível. Não reconhecer nada é um resultado
// legítimo (ExtractedData zerado), não uma falha — o chamador processa milhares
// de anúncios em lote e não tem o que fazer com um erro por anúncio ilegível.
//
// A interface existe porque um extrator baseado em IA é uma alternativa prevista
// para os anúncios que o vocabulário não cobre; TermExtractor é a implementação
// por regex/termos.
type TextExtractor interface {
	Extract(text string) ExtractedData
}

// TermExtractor implementa TextExtractor com expressões regulares e um
// vocabulário de comodidades carregado de arquivo. Construa-o com
// NewTermExtractor; o zero value não reconhece nenhuma comodidade.
//
// É seguro para uso concorrente após a construção: só há leitura de campos, e
// *regexp.Regexp é seguro para uso simultâneo.
type TermExtractor struct {
	amenities []amenityMatcher
}

var _ TextExtractor = (*TermExtractor)(nil)

// amenityMatcher é uma comodidade já compilada: o nome canônico que vai para o
// banco e os regexes de cada sinônimo.
type amenityMatcher struct {
	canonical string
	terms     []*regexp.Regexp
}

// bedroomTerms são as palavras que, precedidas de um número, indicam quartos.
//
// "suíte" **não** está na lista de propósito: "3 quartos, sendo 1 suíte" descreve
// 3 quartos, não 4. Contar suítes como quartos extras inflaria a maior parte dos
// anúncios de médio padrão.
const bedroomTerms = `(?:quartos?|dormitorios?|dorms?|qtos?|qts?)`

// Todos os regexes de quartos são compilados uma única vez, na carga do pacote.
// Extract roda por anúncio, em lote: compilar dentro dele custaria uma
// compilação por anúncio por padrão. Os padrões são escritos sobre o texto já
// passado por fold (minúsculo e sem acento), por isso "dormitorios" aparece aqui
// sem acento e sem alternativa acentuada.
var (
	// Faixa: "2 a 3 quartos", "2 ate 3 quartos", "2-3 quartos". Vale o primeiro
	// número — é o piso do que o anúncio oferece.
	// "ate" precede "a" na alternância para que o separador mais longo seja
	// tentado primeiro.
	bedroomRangeRE = regexp.MustCompile(`\b(\d{1,2})\s*(?:ate|a|-)\s*\d{1,2}\s*` + bedroomTerms + `\b`)
	// Notação de mercado brasileira: "3/4" lê-se "três quartos". O denominador é
	// sempre 4 nessa notação (vem de "quartos"), e exigi-lo reduz a chance de
	// capturar uma fração legítima do texto.
	bedroomMarketRE = regexp.MustCompile(`\b([1-9])\s*/\s*4\b`)
	// Numérico + termo: "3 quartos", "2 qtos", "4 dormitorios", "2 dorm.".
	bedroomNumericRE = regexp.MustCompile(`\b(\d{1,2})\s*` + bedroomTerms + `\b`)
	// Por extenso: "três quartos", "dois dormitórios".
	bedroomSpelledRE = regexp.MustCompile(`\b(um|uma|dois|duas|tres|quatro|cinco|seis|sete|oito|nove|dez)\s+` + bedroomTerms + `\b`)
	// Imóvel sem quarto separado. Só é consultado quando nenhum padrão numérico
	// casou — é assim que "studio de 1 quarto" devolve 1 sem regra extra de
	// desempate.
	bedroomStudioRE = regexp.MustCompile(`\b(?:studios?|kitnets?|kitinetes?|kitchenettes?|quitinetes?|conjugados?)\b`)
)

// spelledNumbers mapeia os numerais por extenso usados em anúncios. Vai só até
// dez: acima disso ninguém escreve por extenso.
var spelledNumbers = map[string]int{
	"um": 1, "uma": 1,
	"dois": 2, "duas": 2,
	"tres":   3,
	"quatro": 4,
	"cinco":  5,
	"seis":   6,
	"sete":   7,
	"oito":   8,
	"nove":   9,
	"dez":    10,
}

// bedroomPatterns são avaliados todos, e vence o que casar **mais cedo** no
// texto — descrições brasileiras abrem com o número principal ("3 quartos, sendo
// 1 suíte, e 2 vagas"). A ordem desta lista só desempata quando dois padrões
// casam na mesma posição, caso em que o mais específico (faixa) precisa vir
// primeiro: em "2 a 3 quartos", o padrão numérico também casa, mas em "3
// quartos", depois.
var bedroomPatterns = []struct {
	re    *regexp.Regexp
	parse func(string) (int, bool)
}{
	{bedroomRangeRE, parseDigits},
	{bedroomMarketRE, parseDigits},
	{bedroomNumericRE, parseDigits},
	{bedroomSpelledRE, parseSpelled},
}

// Extract analisa o texto e devolve os dados reconhecidos. Aceita qualquer texto
// livre — a descrição, o bedrooms_raw, ou os dois concatenados pelo chamador —
// e nunca devolve erro.
//
// Texto vazio ou só com espaços devolve ExtractedData zerado, sem tocar em
// nenhum regex.
func (e *TermExtractor) Extract(text string) ExtractedData {
	folded := fold(strings.TrimSpace(text))
	if folded == "" {
		return ExtractedData{}
	}

	return ExtractedData{
		BedroomCount: extractBedroomCount(folded),
		Amenities:    e.extractAmenities(folded),
	}
}

// extractBedroomCount recebe o texto **já normalizado por fold** e devolve o
// número de quartos, ou nil quando o anúncio não informa.
func extractBedroomCount(folded string) *int {
	bestStart := -1
	bestValue := 0

	for _, pattern := range bedroomPatterns {
		loc := pattern.re.FindStringSubmatchIndex(folded)
		if loc == nil {
			continue
		}
		// loc[0] é o início do match completo; loc[2]:loc[3], o grupo 1.
		if bestStart >= 0 && loc[0] >= bestStart {
			continue
		}
		value, ok := pattern.parse(folded[loc[2]:loc[3]])
		if !ok {
			continue
		}
		bestStart = loc[0]
		bestValue = value
	}

	if bestStart >= 0 {
		return &bestValue
	}

	// Nenhum número reconhecido: só agora "studio"/"kitnet" valem como zero
	// quarto. Um número explícito no texto sempre tem prioridade.
	if bedroomStudioRE.MatchString(folded) {
		zero := 0
		return &zero
	}

	return nil
}

// extractAmenities percorre o vocabulário na ordem do arquivo e para no primeiro
// sinônimo que casar de cada comodidade. Isso dá deduplicação e ordem estável de
// graça, sem passar por mapa nem por sort.
func (e *TermExtractor) extractAmenities(folded string) []string {
	if e == nil || len(e.amenities) == 0 {
		return nil
	}

	var found []string
	for _, amenity := range e.amenities {
		for _, term := range amenity.terms {
			if term.MatchString(folded) {
				found = append(found, amenity.canonical)
				break
			}
		}
	}
	return found
}

// parseDigits converte o grupo capturado por um padrão numérico. O regex já
// garante que são no máximo dois dígitos, então o erro de Atoi é impossível na
// prática — tratá-lo mesmo assim evita um panic caso um padrão mude.
func parseDigits(match string) (int, bool) {
	value, err := strconv.Atoi(match)
	if err != nil {
		return 0, false
	}
	return value, true
}

// parseSpelled converte um numeral por extenso.
func parseSpelled(match string) (int, bool) {
	value, ok := spelledNumbers[match]
	return value, ok
}
