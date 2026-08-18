// Package enrichment deriva campos canônicos a partir do texto bruto coletado
// pelo scraper. O primeiro deles é o bairro: cada portal escreve o mesmo lugar
// de um jeito ("Jd. Botânico", "JARDIM BOTANICO", "jardim botanico") e o
// agrupamento por região depende de todas essas grafias colapsarem em uma só.
//
// O pacote é uma folha do grafo de importação, junto com config: só depende da
// stdlib e de golang.org/x/text. Nada aqui toca banco, rede ou IA — as funções
// são puras e determinísticas, e a tabela de aliases vem embutida no binário
// via //go:embed. Persistir o resultado em listings.normalized_neighborhood é
// responsabilidade do chamador (ver internal/enrichment/CLAUDE.md).
package enrichment

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// aliasesJSON é a tabela de aliases padrão, embutida no binário. Ela mora
// dentro do pacote (e não em configs/) porque //go:embed não alcança caminhos
// acima do diretório do pacote; embutir também evita uma variável de ambiente
// nova e o gotcha de migrations/ e sources.txt, que precisam de COPY no
// Dockerfile e falham em runtime quando alguém esquece.
//
//go:embed neighborhood_aliases.json
var aliasesJSON []byte

// Erros de construção. São sentinelas para que o chamador distinga "o arquivo
// de aliases está errado" (bug de configuração, falha de boot) de qualquer
// outra falha de I/O. Sempre chegam embrulhados com o prefixo "enrichment:".
var (
	// ErrDuplicateAlias indica duas entradas com a mesma chave — seja
	// literalmente repetida no JSON, seja porque duas grafias diferentes
	// colapsam na mesma chave de busca ("Asa Norte" e "asa  norte"). Nos dois
	// casos uma delas seria descartada em silêncio, e o autor do arquivo
	// nunca saberia qual valor canônico está valendo.
	ErrDuplicateAlias = errors.New("enrichment: duplicate alias key")
	// ErrEmptyAliasKey indica uma chave que some ao ser normalizada (vazia, só
	// espaços ou só pontuação). Ela nunca casaria com entrada nenhuma.
	ErrEmptyAliasKey = errors.New("enrichment: alias key is empty after normalization")
	// ErrEmptyCanonicalValue indica um valor canônico vazio. Como o alias é
	// devolvido verbatim, isso transformaria um bairro conhecido em "".
	ErrEmptyCanonicalValue = errors.New("enrichment: alias canonical value is empty")
	// ErrMalformedAliasTable indica JSON que não tem o formato esperado
	// (objeto com "global" e "cities", valores string).
	ErrMalformedAliasTable = errors.New("enrichment: malformed alias table")
)

// NeighborhoodNormalizer converte o bairro bruto de um anúncio na forma
// canônica usada para agrupar região.
type NeighborhoodNormalizer interface {
	// Normalize devolve a forma canônica de raw.
	//
	// city delimita o escopo dos aliases: bairros homônimos são a regra no
	// Brasil ("Centro" existe em toda cidade), então a tabela tem uma seção
	// por cidade além da global. Passar city == "" consulta apenas a seção
	// global — que é o caso de hoje, porque nenhum chamador tem cidade
	// disponível: listings não tem coluna city (só properties tem). O
	// parâmetro existe agora para que a assinatura não mude quando o parsing
	// de endereço passar a extrair a cidade.
	//
	// Entrada vazia, só com espaços ou só com pontuação devolve "" — cabe ao
	// chamador gravar NULL, nunca string vazia.
	Normalize(raw, city string) string
}

// AliasMap mapeia a grafia bruta de um bairro no seu nome canônico. As chaves
// passam pela mesma normalização aplicada à entrada do usuário, então o arquivo
// pode ser escrito em forma legível ("Jd. Botânico") e continuar casando.
type AliasMap map[string]string

// CityAliasMap agrupa tabelas de alias por cidade. A chave (o nome da cidade)
// também é normalizada, então "Brasília", "BRASILIA" e "brasilia" apontam para
// a mesma seção.
type CityAliasMap map[string]AliasMap

// AliasTable é a tabela como ela aparece no JSON, antes da normalização das
// chaves. Existe exportada para permitir override programático da tabela sem
// tocar no arquivo embutido.
type AliasTable struct {
	Global AliasMap     `json:"global"`
	Cities CityAliasMap `json:"cities"`
}

// neighborhoodNormalizer é a tabela já compilada: chaves normalizadas, valores
// canônicos verbatim. Imutável após a construção, o que a torna segura para uso
// concorrente.
type neighborhoodNormalizer struct {
	global map[string]string
	cities map[string]map[string]string
}

// NewNeighborhoodNormalizer monta o normalizador a partir da tabela de aliases
// embutida no binário. Erra quando o arquivo está malformado ou tem chaves
// duplicadas — um alias silenciosamente descartado corromperia o agrupamento
// por região sem deixar rastro.
func NewNeighborhoodNormalizer() (NeighborhoodNormalizer, error) {
	table, err := parseAliasTable(aliasesJSON)
	if err != nil {
		return nil, fmt.Errorf("enrichment: loading embedded alias table: %w", err)
	}

	normalizer, err := NewNeighborhoodNormalizerFromAliases(table)
	if err != nil {
		return nil, fmt.Errorf("enrichment: loading embedded alias table: %w", err)
	}

	return normalizer, nil
}

// NewNeighborhoodNormalizerFromAliases monta o normalizador a partir de uma
// tabela já em memória. É a costura de teste e o gancho para override futuro
// (por exemplo, uma tabela vinda do banco) sem mexer no arquivo embutido.
//
// As chaves da tabela e os nomes das cidades são normalizados aqui; colisões
// após a normalização são erro, não "a última vence".
func NewNeighborhoodNormalizerFromAliases(table AliasTable) (NeighborhoodNormalizer, error) {
	global, err := compileAliases(table.Global)
	if err != nil {
		return nil, fmt.Errorf("enrichment: global aliases: %w", err)
	}

	cities := make(map[string]map[string]string, len(table.Cities))
	for city, aliases := range table.Cities {
		cityKey := normalizeKey(city)
		if cityKey == "" {
			return nil, fmt.Errorf("enrichment: city aliases: %w: %q", ErrEmptyAliasKey, city)
		}
		if _, exists := cities[cityKey]; exists {
			return nil, fmt.Errorf("enrichment: city aliases: %w: %q", ErrDuplicateAlias, cityKey)
		}

		compiled, err := compileAliases(aliases)
		if err != nil {
			return nil, fmt.Errorf("enrichment: city %q aliases: %w", city, err)
		}
		cities[cityKey] = compiled
	}

	return &neighborhoodNormalizer{global: global, cities: cities}, nil
}

// Normalize implementa NeighborhoodNormalizer.
func (n *neighborhoodNormalizer) Normalize(raw, city string) string {
	clean := stripBairroToken(collapseSpaces(raw))
	if clean == "" {
		return ""
	}

	key := normalizeKey(clean)
	if key == "" {
		return ""
	}

	// A seção da cidade vence a global de propósito: "Sudoeste" é um bairro
	// qualquer em geral, mas em Brasília é "Setor Sudoeste".
	if cityKey := normalizeKey(city); cityKey != "" {
		if canonical, ok := n.cities[cityKey][key]; ok {
			return canonical
		}
	}
	if canonical, ok := n.global[key]; ok {
		return canonical
	}

	// Sem alias: devolve a string limpa em title-case, com os acentos
	// originais. A remoção de acentos serve só para a comparação — inventar
	// acento aqui ("Acacias" → "Acácias") exigiria um dicionário, e errar a
	// grafia é pior que preservá-la. Acentuação correta vem por alias.
	return titleCasePT(clean)
}

// parseAliasTable decodifica o JSON da tabela. DisallowUnknownFields é
// deliberado: um "globals" digitado errado produziria uma tabela vazia e o
// normalizador cairia no fallback para tudo, em silêncio.
func parseAliasTable(data []byte) (AliasTable, error) {
	var table AliasTable

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&table); err != nil {
		return AliasTable{}, fmt.Errorf("decoding alias table: %w", err)
	}

	return table, nil
}

// compileAliases normaliza as chaves de um bloco de aliases e valida o
// conteúdo. O valor canônico é guardado verbatim: é a tabela que manda na
// grafia final, acentos e maiúsculas inclusive.
func compileAliases(aliases AliasMap) (map[string]string, error) {
	compiled := make(map[string]string, len(aliases))

	for raw, canonical := range aliases {
		key := normalizeKey(raw)
		if key == "" {
			return nil, fmt.Errorf("%w: %q", ErrEmptyAliasKey, raw)
		}
		if strings.TrimSpace(canonical) == "" {
			return nil, fmt.Errorf("%w: key %q", ErrEmptyCanonicalValue, raw)
		}
		if existing, exists := compiled[key]; exists {
			return nil, fmt.Errorf("%w: %q (already mapped to %q)", ErrDuplicateAlias, key, existing)
		}
		compiled[key] = canonical
	}

	return compiled, nil
}

// UnmarshalJSON decodifica um bloco de aliases recusando chaves repetidas.
// encoding/json aceita chave duplicada em silêncio (a última vence), o que
// esconderia exatamente o erro que mais importa num arquivo escrito à mão.
func (m *AliasMap) UnmarshalJSON(data []byte) error {
	out := make(AliasMap)

	err := forEachObjectKey(data, func(dec *json.Decoder, key string) error {
		var value string
		if err := dec.Decode(&value); err != nil {
			return fmt.Errorf("%w: value of key %q is not a string: %w", ErrMalformedAliasTable, key, err)
		}
		out[key] = value
		return nil
	})
	if err != nil {
		return err
	}

	*m = out
	return nil
}

// UnmarshalJSON decodifica as seções por cidade recusando cidades repetidas,
// pelo mesmo motivo de AliasMap.UnmarshalJSON.
func (m *CityAliasMap) UnmarshalJSON(data []byte) error {
	out := make(CityAliasMap)

	err := forEachObjectKey(data, func(dec *json.Decoder, key string) error {
		var aliases AliasMap
		if err := dec.Decode(&aliases); err != nil {
			return fmt.Errorf("city %q: %w", key, err)
		}
		out[key] = aliases
		return nil
	})
	if err != nil {
		return err
	}

	*m = out
	return nil
}

// forEachObjectKey percorre um objeto JSON token a token, chamando decodeValue
// para cada chave e rejeitando chaves literalmente repetidas.
func forEachObjectKey(data []byte, decodeValue func(dec *json.Decoder, key string) error) error {
	dec := json.NewDecoder(bytes.NewReader(data))

	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("%w: %w", ErrMalformedAliasTable, err)
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return fmt.Errorf("%w: expected a JSON object, got %v", ErrMalformedAliasTable, tok)
	}

	seen := make(map[string]struct{})
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return fmt.Errorf("%w: %w", ErrMalformedAliasTable, err)
		}
		key, ok := keyTok.(string)
		if !ok {
			return fmt.Errorf("%w: expected a string key, got %v", ErrMalformedAliasTable, keyTok)
		}
		if _, duplicated := seen[key]; duplicated {
			return fmt.Errorf("%w: %q", ErrDuplicateAlias, key)
		}
		seen[key] = struct{}{}

		if err := decodeValue(dec, key); err != nil {
			return err
		}
	}

	if _, err := dec.Token(); err != nil {
		return fmt.Errorf("%w: %w", ErrMalformedAliasTable, err)
	}
	return nil
}

// edgePunctuation é a pontuação descartada nas bordas de uma string ou de uma
// palavra. O apóstrofo fica de fora de propósito: ele faz parte de nomes como
// "Olho d'Água". Pontuação interna também é preservada — a chave de
// "Jd. Botânico" é "jd. botanico", e é assim que ela vai escrita na tabela.
const edgePunctuation = " \t\n\r,.;:!?-–—_/\\|()[]{}\"«»"

// bairroToken é a única palavra removida como redundante. Setor NÃO entra
// nesta lista: em Brasília e Goiânia "Setor Sudoeste", "Setor Bueno" e "Setor
// Oeste" são nomes reais de bairro, e removê-la corromperia o dado em silêncio.
// Casos em que "Setor" de fato sobra vão para a tabela de aliases.
const bairroToken = "bairro"

// collapseSpaces apara as pontas e colapsa qualquer sequência de espaço em
// branco (inclusive tabs e quebras de linha) num único espaço.
func collapseSpaces(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// stripBairroToken remove a palavra "Bairro" quando ela aparece como prefixo ou
// sufixo redundante ("Bairro Jardim Botânico", "Jardim Botânico, Bairro"). A
// comparação é feita sem acento e sem caixa, tolerando pontuação colada.
//
// A remoção nunca esvazia a string: um bairro que se chame literalmente
// "Bairro" é preservado, porque devolver "" apagaria o único dado que havia.
func stripBairroToken(s string) string {
	words := strings.Fields(s)
	if len(words) < 2 {
		return trimEdges(s)
	}

	if normalizeKey(words[0]) == bairroToken {
		words = words[1:]
	}
	if len(words) > 1 && normalizeKey(words[len(words)-1]) == bairroToken {
		words = words[:len(words)-1]
	}

	return trimEdges(strings.Join(words, " "))
}

// trimEdges descarta pontuação e espaço das bordas, preservando o miolo.
func trimEdges(s string) string {
	return collapseSpaces(strings.Trim(s, edgePunctuation))
}

// normalizeKey produz a chave de busca: minúsculas, sem acento, com espaços
// colapsados e sem pontuação de borda. É só para comparação — nunca é devolvida
// ao chamador.
func normalizeKey(s string) string {
	return trimEdges(strings.ToLower(removeAccents(collapseSpaces(s))))
}

// removeAccents decompõe (NFD), descarta as marcas combinantes (unicode.Mn) e
// recompõe (NFC). Cobre ç, ã, õ, á/à/â, é/ê, í, ó/ô e ú.
//
// O transform.Transformer é montado por chamada de propósito: transformers do
// x/text guardam estado e não são seguros para uso concorrente. Guardá-lo num
// campo do struct daria saída corrompida quando a fila de enriquecimento
// (concorrente, por design) chamar Normalize de várias goroutines.
func removeAccents(s string) string {
	t := transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)

	folded, _, err := transform.String(t, s)
	if err != nil {
		// transform.String só falha em entrada não-UTF-8; nesse caso a string
		// original é melhor resposta que uma string vazia.
		return s
	}
	return folded
}

// lowercaseConnectives são as palavras que ficam em minúscula quando não são a
// primeira do nome ("Jardim das Acácias", "Vila de São Jorge"). A chave é a
// forma normalizada (sem acento, minúscula).
var lowercaseConnectives = map[string]struct{}{
	"de": {}, "da": {}, "do": {}, "das": {}, "dos": {}, "e": {},
}

// titleCasePT aplica title-case pt-BR preservando os acentos originais.
//
// strings.Title está deprecado; cases.Title(language.BrazilianPortuguese) faz o
// trabalho pesado (inclusive rebaixar "JARDIM" para "Jardim"). O ajuste manual
// depois cobre o que o caser não sabe: conectivos em minúscula e o prefixo
// "d'" ("Olho d'Água").
//
// Assim como o transformer de acentos, o cases.Caser é criado por chamada —
// ele guarda estado interno e não é seguro para uso concorrente.
func titleCasePT(s string) string {
	caser := cases.Title(language.BrazilianPortuguese)

	words := strings.Fields(s)
	for i, word := range words {
		key := normalizeKey(word)

		if rest, ok := apostropheDPrefix(word); ok {
			prefix := "d'"
			if i == 0 {
				prefix = "D'"
			}
			words[i] = prefix + caser.String(rest)
			continue
		}

		titled := caser.String(word)
		if i > 0 {
			if _, isConnective := lowercaseConnectives[key]; isConnective {
				titled = strings.ToLower(titled)
			}
		}
		words[i] = titled
	}

	return strings.Join(words, " ")
}

// apostropheDPrefix reconhece palavras iniciadas por "d'" e devolve o que vem
// depois do apóstrofo. Sem isso, cases.Title produziria "D'água" — o caser
// trata o apóstrofo como fim de palavra e rebaixa o radical.
func apostropheDPrefix(word string) (rest string, ok bool) {
	if len(word) <= 2 || word[1] != '\'' {
		return "", false
	}
	if word[0] != 'd' && word[0] != 'D' {
		return "", false
	}
	return word[2:], true
}
