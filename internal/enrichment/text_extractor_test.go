package enrichment

import (
	"reflect"
	"testing"
)

// testExtractor constrói um extrator a partir do vocabulário versionado no
// repositório. Usar o arquivo real (e não um vocabulário de mentira) é
// proposital: os testes de comodidade valem como verificação de que os termos
// que o pipeline vai usar de fato reconhecem o texto que aparece nos anúncios.
func testExtractor(t *testing.T) *TermExtractor {
	t.Helper()
	extractor, err := NewTermExtractor(repoAmenitiesFile)
	if err != nil {
		t.Fatalf("NewTermExtractor(%q) error = %v, want nil", repoAmenitiesFile, err)
	}
	return extractor
}

func intPtr(v int) *int { return &v }

func TestExtractBedroomCount(t *testing.T) {
	tests := []struct {
		name string
		text string
		want *int
	}{
		// Numérico + termo, nas grafias que aparecem nos portais.
		{"algarismo e quartos", "Apartamento com 3 quartos no centro", intPtr(3)},
		{"singular", "Kitnet reformada com 1 quarto", intPtr(1)},
		{"qtos abreviado", "Casa 2 qtos, 1 vaga", intPtr(2)},
		{"qts abreviado", "Sobrado 3 qts", intPtr(3)},
		{"qto singular", "Apto 1 qto", intPtr(1)},
		{"dormitorios por extenso do termo", "Excelente casa com 4 dormitórios", intPtr(4)},
		{"dorm abreviado com ponto", "Apartamento 2 dorm., 1 vaga", intPtr(2)},
		{"dorms plural abreviado", "Cobertura 5 dorms", intPtr(5)},
		{"sem espaco entre numero e termo", "AP 2QUARTOS PRONTO PARA MORAR", intPtr(2)},

		// Numeral por extenso.
		{"tres quartos por extenso", "Imóvel com três quartos amplos", intPtr(3)},
		{"dois dormitorios por extenso", "Alugo apartamento de dois dormitórios", intPtr(2)},
		{"um quarto por extenso", "Studio: um quarto integrado", intPtr(1)},
		{"dez quartos por extenso", "Pousada com dez quartos", intPtr(10)},

		// Notação de mercado.
		{"notacao 3/4", "Apartamento 3/4 no Costa Azul", intPtr(3)},
		{"notacao 2/4", "Casa 2/4 com quintal", intPtr(2)},
		{"notacao com espacos", "Apto 2 / 4 nascente", intPtr(2)},

		// Faixa: vale o primeiro número.
		{"faixa com a", "Unidades de 2 a 3 quartos", intPtr(2)},
		{"faixa com hifen", "Lançamento 2-3 quartos", intPtr(2)},
		{"faixa com ate", "De 1 até 4 dormitórios", intPtr(1)},

		// Studio / kitnet valem zero quarto.
		{"studio", "Studio mobiliado próximo ao metrô", intPtr(0)},
		{"studio com acento", "Stúdio novo para alugar", intPtr(0)},
		{"kitnet", "Kitnet reformada, ótima localização", intPtr(0)},
		{"kitchenette", "Charmosa kitchenette no centro", intPtr(0)},
		{"quitinete", "Quitinete com armários", intPtr(0)},
		{"caixa alta", "KITNET PARA ALUGAR", intPtr(0)},

		// Número explícito tem prioridade sobre studio.
		{"studio com numero explicito", "Studio de 1 quarto e varanda", intPtr(1)},
		{"kitnet com numero explicito", "Kitnet com 1 dormitório separado", intPtr(1)},

		// Não informado.
		{"sem mencao", "Imóvel bem localizado, aceita financiamento", nil},
		{"suite isolada nao conta", "Imóvel com 3 suítes e piscina", nil},
		{"texto vazio", "", nil},
		{"apenas espacos", "   \n\t ", nil},
		{"numero solto", "Apartamento de 90m² por R$ 450.000", nil},
	}

	extractor := testExtractor(t)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractor.Extract(tt.text).BedroomCount

			switch {
			case tt.want == nil && got != nil:
				t.Fatalf("BedroomCount = %d, want nil", *got)
			case tt.want != nil && got == nil:
				t.Fatalf("BedroomCount = nil, want %d", *tt.want)
			case tt.want != nil && *got != *tt.want:
				t.Fatalf("BedroomCount = %d, want %d", *got, *tt.want)
			}
		})
	}
}

// TestExtractBedroomCountPrefersFirstOccurrence fixa a regra "a primeira
// ocorrência vence": descrições brasileiras abrem com o número principal e só
// depois citam outras plantas do empreendimento.
func TestExtractBedroomCountPrefersFirstOccurrence(t *testing.T) {
	extractor := testExtractor(t)

	got := extractor.Extract("Apartamento 2 quartos. Também temos unidades de 4 quartos.").BedroomCount
	if got == nil || *got != 2 {
		t.Fatalf("BedroomCount = %v, want 2", got)
	}
}

func TestExtractAmenities(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []string
	}{
		{"piscina", "Condomínio com piscina", []string{"piscina"}},
		{"academia", "Prédio com academia equipada", []string{"academia"}},
		// A saída segue a ordem do arquivo (churrasqueira vem antes de varanda),
		// não a ordem em que os termos aparecem no texto.
		{"churrasqueira", "Varanda com churrasqueira", []string{"churrasqueira", "varanda"}},
		{"portaria 24h", "Portaria 24h e câmeras", []string{"portaria 24h"}},
		{"portaria por extenso", "Conta com portaria 24 horas", []string{"portaria 24h"}},
		{"elevador", "Edifício com elevador", []string{"elevador"}},
		{"pet friendly", "Condomínio pet friendly", []string{"pet friendly"}},
		{"playground", "Área de lazer com playground", []string{"playground"}},
		{"sauna", "Sauna e espaço zen", []string{"sauna"}},
		{"vaga coberta", "Duas vagas cobertas na garagem", []string{"vaga coberta"}},
		{"mobiliado", "Apartamento totalmente mobiliado", []string{"mobiliado"}},
		{"sinonimo regional", "Apartamento com sacada", []string{"varanda"}},

		// Acento no termo do YAML e no texto, em qualquer combinação.
		{"acento nos dois lados", "Possui salão de festas", []string{"salão de festas"}},
		{"texto sem acento", "Possui salao de festas", []string{"salão de festas"}},
		{"area de servico acentuada", "Ampla Área de Serviço independente", []string{"área de serviço"}},
		{"area de servico sem acento", "ampla area de servico", []string{"área de serviço"}},

		// Termo composto com variação de hífen/espaço e de caixa.
		{"composto com espaco", "Sala com ar condicionado", []string{"ar-condicionado"}},
		{"composto com hifen", "Sala com Ar-Condicionado", []string{"ar-condicionado"}},
		{"composto em caixa alta", "SALA COM AR CONDICIONADO", []string{"ar-condicionado"}},

		{"sem comodidade", "Imóvel bem localizado", nil},
		{"texto vazio", "", nil},
	}

	extractor := testExtractor(t)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractor.Extract(tt.text).Amenities
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Amenities = %#v, want %#v", got, tt.want)
			}
		})
	}
}

// TestExtractAmenitiesDeduplicates garante que a mesma comodidade citada por
// dois sinônimos diferentes (e repetida) sai uma única vez.
func TestExtractAmenitiesDeduplicates(t *testing.T) {
	extractor := testExtractor(t)

	got := extractor.Extract("Tem piscina, piscina aquecida e outra piscina coberta.").Amenities
	want := []string{"piscina"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Amenities = %#v, want %#v", got, want)
	}
}

// TestExtractAmenitiesOrderIsFileOrder fixa o contrato de determinismo: a saída
// segue a ordem do arquivo de vocabulário, nunca a ordem do texto nem a de
// iteração de um mapa. Sem isso, o teste passaria de forma intermitente e o dado
// gravado no banco mudaria de forma a cada passe.
func TestExtractAmenitiesOrderIsFileOrder(t *testing.T) {
	extractor := testExtractor(t)

	// No texto a ordem é sauna → elevador → piscina; no arquivo é piscina →
	// elevador → sauna.
	text := "Sauna, elevador social e piscina aquecida."
	want := []string{"piscina", "elevador", "sauna"}

	first := extractor.Extract(text).Amenities
	if !reflect.DeepEqual(first, want) {
		t.Fatalf("Amenities = %#v, want %#v", first, want)
	}

	// Repetir a extração precisa dar exatamente o mesmo slice.
	for i := 0; i < 5; i++ {
		if got := extractor.Extract(text).Amenities; !reflect.DeepEqual(got, first) {
			t.Fatalf("execução %d: Amenities = %#v, want %#v", i, got, first)
		}
	}
}

// TestExtractAmenitiesRespectsWordBoundaries protege contra o falso positivo
// mais provável: o termo casando no meio de outra palavra.
func TestExtractAmenitiesRespectsWordBoundaries(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{"termo composto colado a outra palavra", "Bar condicionado ao consumo no térreo"},
		{"termo simples como sufixo", "Prédio com hipersauna"},
		{"termo simples como prefixo", "Piscinapark fica ao lado"},
	}

	extractor := testExtractor(t)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractor.Extract(tt.text).Amenities; len(got) != 0 {
				t.Fatalf("Amenities = %#v, want nenhuma", got)
			}
		})
	}
}

// TestExtractEmptyTextIsZeroValue cobre o contrato do critério de aceite: texto
// vazio devolve ExtractedData zerado e nunca erro.
func TestExtractEmptyTextIsZeroValue(t *testing.T) {
	extractor := testExtractor(t)

	for _, text := range []string{"", "   ", "\n\t"} {
		got := extractor.Extract(text)
		if got.BedroomCount != nil {
			t.Errorf("Extract(%q).BedroomCount = %d, want nil", text, *got.BedroomCount)
		}
		if len(got.Amenities) != 0 {
			t.Errorf("Extract(%q).Amenities = %#v, want vazio", text, got.Amenities)
		}
	}
}

// TestExtractCombinesBedroomsAndAmenities exercita o uso real: descrição inteira
// de anúncio, com os dois campos preenchidos de uma vez.
func TestExtractCombinesBedroomsAndAmenities(t *testing.T) {
	extractor := testExtractor(t)

	text := `Excelente apartamento com 3 quartos, sendo 1 suíte, sala com
	ar-condicionado e varanda gourmet com churrasqueira. O condomínio oferece
	piscina aquecida, academia, salão de festas e portaria 24 horas. Vaga coberta.`

	got := extractor.Extract(text)

	if got.BedroomCount == nil || *got.BedroomCount != 3 {
		t.Fatalf("BedroomCount = %v, want 3", got.BedroomCount)
	}

	want := []string{
		"piscina", "academia", "churrasqueira", "portaria 24h",
		"ar-condicionado", "varanda", "salão de festas", "vaga coberta",
	}
	if !reflect.DeepEqual(got.Amenities, want) {
		t.Fatalf("Amenities = %#v, want %#v", got.Amenities, want)
	}
}

func TestFold(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Ar-Condicionado", "ar-condicionado"},
		{"ÁREA DE SERVIÇO", "area de servico"},
		{"salão de festas", "salao de festas"},
		{"Stúdio", "studio"},
		{"já sem acento", "ja sem acento"},
		{"", ""},
	}

	for _, tt := range tests {
		if got := fold(tt.in); got != tt.want {
			t.Errorf("fold(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
