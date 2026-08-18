// Package selectors decide, para cada domínio, quais seletores CSS usar na
// extração dos anúncios: reaproveita os que já estão em site_selectors enquanto
// forem válidos e aciona a descoberta assistida por IA (pacote ai) quando a
// fonte é nova ou os seletores pararam de funcionar.
//
// A porta de entrada é SelectorService: EnsureSelectors no caminho normal da
// coleta e RecoverSelectors quando o extrator não encontra mais nenhum anúncio.
package selectors
