package enrichment

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// fold devolve s em caixa baixa e sem marcas de acentuação ("Ar-Condicionado" e
// "área de serviço" viram "ar-condicionado" e "area de servico").
//
// É aplicado dos **dois lados** do matching — no sinônimo lido do YAML e no
// texto do anúncio — de modo que qualquer combinação de caixa e acento entre os
// dois casa. Anúncios brasileiros são escritos à mão e chegam em CAIXA ALTA, sem
// acento, ou com acentuação parcial; comparar as formas cruas perderia a maioria
// das ocorrências.
//
// A implementação é deliberadamente **stateless**: NFD + descarte das runas da
// categoria Mn (marcas não espaçantes, que é onde a decomposição joga os
// acentos) + recomposição. A alternativa idiomática (transform.Chain com
// runes.Remove) produz um transform.Transformer com estado interno, que não pode
// ser guardado no extrator e reusado por várias goroutines — e o extrator existe
// justamente para ser reusado sobre dezenas de milhares de anúncios.
//
// O comprimento em bytes pode mudar; fold não serve para calcular offsets sobre
// o texto original. Nada aqui depende disso: só usamos o resultado para casar
// expressões regulares.
func fold(s string) string {
	decomposed := norm.NFD.String(strings.ToLower(s))

	var b strings.Builder
	b.Grow(len(decomposed))
	for _, r := range decomposed {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		b.WriteRune(r)
	}

	// Recompõe: caracteres sem acento (ex.: "ß", ligaturas) voltam à forma
	// canônica em vez de ficarem numa decomposição parcial.
	return norm.NFC.String(b.String())
}
