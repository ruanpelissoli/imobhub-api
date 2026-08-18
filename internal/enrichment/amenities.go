package enrichment

import (
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v4"
)

// amenitiesFileDoc é o formato de configs/amenities.yaml. A chave de topo
// `amenities` existe para que o arquivo possa ganhar outras seções (tipos de
// imóvel, termos de transação) sem virar um formato novo.
type amenitiesFileDoc struct {
	Amenities []amenityEntry `yaml:"amenities"`
}

type amenityEntry struct {
	// Canonical é o valor gravado em listings.amenities. Mudá-lo muda o dado.
	Canonical string `yaml:"canonical"`
	// Synonyms são as grafias reconhecidas no texto do anúncio.
	Synonyms []string `yaml:"synonyms"`
}

// termSeparator quebra um sinônimo composto nas suas palavras. Espaço e hífen
// são tratados igual porque o anúncio escreve os dois ("ar condicionado",
// "ar-condicionado") e nenhuma das grafias é mais correta que a outra.
var termSeparator = regexp.MustCompile(`[\s-]+`)

// NewTermExtractor lê o vocabulário de comodidades do arquivo indicado e devolve
// um extrator com todos os regexes já compilados.
//
// O caminho chega por parâmetro, nunca do ambiente: internal/config é a única
// fronteira do projeto com os.Getenv, e é de lá (config.AmenitiesFile,
// AMENITIES_FILE) que o chamador tira o valor. O caminho é usado como veio —
// relativo ao working directory do processo, igual a SOURCES_FILE e
// MIGRATIONS_DIR.
//
// O vocabulário é carregado **uma vez, pelo chamador, e injetado**: o extrator
// resultante é imutável e seguro para uso concorrente, e o teste consegue
// construir um extrator com um vocabulário próprio. Um init() ou uma global
// mutável dariam o mesmo "carrega uma vez" às custas disso.
//
// Diferente de sources.ReadSources — onde uma linha ruim só avisa —, aqui
// praticamente tudo é erro: uma comodidade faltando não se manifesta como falha,
// e sim como milhares de anúncios enriquecidos pela metade, sem nada no log
// depois do startup. Só o sinônimo individual inutilizável (vazio, ou sem
// nenhuma letra/dígito) é ignorado com aviso. Todos os erros citam o caminho.
func NewTermExtractor(amenitiesFile string) (*TermExtractor, error) {
	raw, err := os.ReadFile(amenitiesFile)
	if err != nil {
		return nil, fmt.Errorf("enrichment: could not read amenities file %q: %w", amenitiesFile, err)
	}

	var parsed amenitiesFileDoc
	if err := yaml.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("enrichment: could not parse amenities file %q: %w", amenitiesFile, err)
	}

	if len(parsed.Amenities) == 0 {
		return nil, fmt.Errorf("enrichment: amenities file %q defines no amenities", amenitiesFile)
	}

	var (
		matchers = make([]amenityMatcher, 0, len(parsed.Amenities))
		seen     = make(map[string]int, len(parsed.Amenities))
		terms    int
	)

	for i, entry := range parsed.Amenities {
		canonical := strings.TrimSpace(entry.Canonical)
		if canonical == "" {
			return nil, fmt.Errorf("enrichment: amenities file %q: amenity #%d has no canonical name", amenitiesFile, i+1)
		}

		// A duplicata é comparada pela mesma normalização usada no matching
		// (caixa, acento e hífen/espaço): "Ar-Condicionado" e "ar condicionado"
		// são a mesma comodidade escrita de dois jeitos, e mantê-las separadas
		// faria o mesmo texto gravar duas entradas equivalentes no banco.
		key := termSeparator.ReplaceAllString(fold(canonical), " ")
		if first, exists := seen[key]; exists {
			return nil, fmt.Errorf("enrichment: amenities file %q: amenity %q (#%d) duplicates #%d", amenitiesFile, canonical, i+1, first)
		}
		seen[key] = i + 1

		compiled := make([]*regexp.Regexp, 0, len(entry.Synonyms))
		for _, synonym := range entry.Synonyms {
			term, err := compileTerm(synonym)
			if err != nil {
				slog.Warn("enrichment: skipping unusable amenity synonym",
					"file", amenitiesFile,
					"canonical", canonical,
					"synonym", synonym,
					"error", err,
				)
				continue
			}
			compiled = append(compiled, term)
		}

		if len(compiled) == 0 {
			return nil, fmt.Errorf("enrichment: amenities file %q: amenity %q (#%d) has no usable synonyms", amenitiesFile, canonical, i+1)
		}

		terms += len(compiled)
		matchers = append(matchers, amenityMatcher{canonical: canonical, terms: compiled})
	}

	slog.Info("enrichment: amenities loaded",
		"file", amenitiesFile,
		"amenities", len(matchers),
		"terms", terms,
	)

	return &TermExtractor{amenities: matchers}, nil
}

// compileTerm transforma um sinônimo no regex que o reconhece no texto dobrado.
//
// O sinônimo é dobrado (caixa/acento), quebrado em palavras e remontado com
// `[\s-]+` entre elas, ancorado em `\b` nas duas pontas. Daí saem as três
// propriedades exigidas: matching insensível a caixa e acento, tolerância a
// hífen vs espaço em termos compostos, e limite de palavra — sem o `\b`,
// "ar condicionado" casaria dentro de "bar condicionado".
//
// Cada palavra passa por QuoteMeta: o vocabulário é dado de configuração, não
// código, e um "24h+" no arquivo não pode virar quantificador de regex.
func compileTerm(synonym string) (*regexp.Regexp, error) {
	folded := fold(strings.TrimSpace(synonym))
	if folded == "" {
		return nil, fmt.Errorf("synonym is empty")
	}

	words := termSeparator.Split(folded, -1)
	quoted := make([]string, 0, len(words))
	for _, word := range words {
		if word == "" {
			continue
		}
		quoted = append(quoted, regexp.QuoteMeta(word))
	}
	if len(quoted) == 0 {
		return nil, fmt.Errorf("synonym has no words")
	}

	// `\b` exige caractere de palavra ASCII na borda. Um sinônimo que comece ou
	// termine em pontuação ("24h.") produziria um regex que nunca casa: melhor
	// recusá-lo aqui, com aviso, do que deixá-lo mudo dentro do arquivo.
	first, _ := utf8.DecodeRuneInString(folded)
	last, _ := utf8.DecodeLastRuneInString(folded)
	if !isWordChar(first) || !isWordChar(last) {
		return nil, fmt.Errorf("synonym must start and end with an ASCII letter or digit")
	}

	pattern := `\b` + strings.Join(quoted, `[\s-]+`) + `\b`
	compiled, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("could not compile pattern %q: %w", pattern, err)
	}

	return compiled, nil
}

// isWordChar espelha a definição de `\w` do regexp (ASCII): é dela que `\b`
// depende, não da categoria Unicode.
func isWordChar(r rune) bool {
	return r == '_' ||
		(r >= '0' && r <= '9') ||
		(r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z')
}
