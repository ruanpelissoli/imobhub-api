// Package robots avalia o robots.txt das fontes antes de qualquer coleta.
// Respeitar essas regras é um requisito do projeto, não uma otimização.
package robots

import (
	"fmt"

	"github.com/temoto/robotstxt"
)

// Rules encapsula o robots.txt já parseado de um host.
type Rules struct {
	data *robotstxt.RobotsData
}

// Parse interpreta o conteúdo de um robots.txt. A biblioteca é tolerante a
// arquivos malformados, então o erro só ocorre em casos extremos.
func Parse(body []byte) (*Rules, error) {
	data, err := robotstxt.FromBytes(body)
	if err != nil {
		return nil, fmt.Errorf("robots: could not parse robots.txt: %w", err)
	}
	return &Rules{data: data}, nil
}

// AllowAll devolve regras permissivas, usadas quando não há robots.txt
// avaliável: HTTP 404 (ausência de robots.txt significa acesso liberado pela
// especificação) e também falhas de rede, por decisão de projeto — ver a falha
// permissiva documentada em Checker.IsAllowed.
func AllowAll() *Rules {
	data, _ := robotstxt.FromStatusAndBytes(404, nil)
	return &Rules{data: data}
}

// Allowed informa se o path pode ser acessado pelo userAgent informado.
// Rules nulo é tratado como bloqueio — rede de proteção contra um *Rules
// esquecido sem inicializar. Checker nunca devolve Rules nulo: falha de busca
// vira AllowAll, não nil.
func (r *Rules) Allowed(userAgent, path string) bool {
	if r == nil || r.data == nil {
		return false
	}
	return r.data.TestAgent(path, userAgent)
}
