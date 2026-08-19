package enrichment

import (
	"context"
	"fmt"
)

// googleMapsGeocoder é um stub. Existe só para fixar a costura de troca de
// provider: com ele, migrar para a Geocoding API do Google é preencher um
// arquivo, não redesenhar a interface nem mexer em config.
//
// Deliberadamente sem HTTP e sem dependência nova no go.mod — o SDK do Google
// entra junto com a implementação real, não antes.
type googleMapsGeocoder struct {
	// apiKey é guardada para a implementação futura. NUNCA é logada nem
	// incluída em mensagem de erro.
	apiKey string
}

// NewGoogleMapsGeocoder cria o stub do provider Google Maps.
//
// Não erra com chave vazia de propósito: a obrigatoriedade de GEOCODING_API_KEY
// quando o provider é googlemaps é validada no boot, por internal/config, junto
// com as demais variáveis obrigatórias (o operador vê tudo o que falta de uma
// vez). Aqui a chave só é guardada.
func NewGoogleMapsGeocoder(apiKey string) (Geocoder, error) {
	return &googleMapsGeocoder{apiKey: apiKey}, nil
}

// Geocode implementa Geocoder devolvendo sempre ErrProviderNotSupported: o
// provider é reconhecido, mas ainda não implementado. É erro explícito e não
// fallback silencioso para o Nominatim — quem configurou googlemaps espera as
// cotas e a precisão do Google, e resolver com outro provider em silêncio
// entregaria coordenadas diferentes das esperadas.
func (g *googleMapsGeocoder) Geocode(_ context.Context, _, _, _ string) (float64, float64, error) {
	return 0, 0, fmt.Errorf("%w: %q is not implemented yet", ErrProviderNotSupported, ProviderGoogleMaps)
}
