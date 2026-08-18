package robots

import "testing"

const sample = `User-agent: ImobHubBot
Disallow: /admin/

User-agent: *
Disallow: /
`

func TestAllowedAppliesAgentSpecificRules(t *testing.T) {
	rules, err := Parse([]byte(sample))
	if err != nil {
		t.Fatalf("Parse() error = %v, want nil", err)
	}

	if !rules.Allowed("ImobHubBot", "/imoveis/123") {
		t.Error("Allowed(ImobHubBot, /imoveis/123) = false, want true")
	}
	if rules.Allowed("ImobHubBot", "/admin/painel") {
		t.Error("Allowed(ImobHubBot, /admin/painel) = true, want false")
	}
	// Outro agente cai no bloco "*", que proíbe tudo.
	if rules.Allowed("OutroBot", "/imoveis/123") {
		t.Error("Allowed(OutroBot, /imoveis/123) = true, want false")
	}
}

func TestAllowAllPermitsEverything(t *testing.T) {
	if !AllowAll().Allowed("ImobHubBot", "/qualquer/caminho") {
		t.Error("AllowAll().Allowed() = false, want true")
	}
}

func TestNilRulesBlock(t *testing.T) {
	// Sem regras avaliáveis, o padrão seguro é não raspar.
	var rules *Rules
	if rules.Allowed("ImobHubBot", "/") {
		t.Error("(*Rules)(nil).Allowed() = true, want false")
	}
}
