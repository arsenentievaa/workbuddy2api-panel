package prompt

import "testing"

func TestNomAffiche(t *testing.T) {
	cas := map[string]string{
		"claude-opus-5-5":   "Claude Opus 5.5",
		"claude-sonnet-5-5": "Claude Sonnet 5.5",
		"claude-haiku-4-5":  "Claude Haiku 4.5",
		"gpt-6":             "GPT-6",
		"gpt-6.1-sol":       "GPT-6.1 Sol",
		"gemini-3.8-flash":  "Gemini 3.8 Flash",
		"grok-4":            "Grok 4",
	}
	for id, attendu := range cas {
		if got := NomAffiche(id); got != attendu {
			t.Errorf("NomAffiche(%q) = %q, attendu %q", id, got, attendu)
		}
	}
}

func TestIdentityDirectiveParFamille(t *testing.T) {
	cas := map[string]string{
		"claude-opus-5-5":  "Anthropic",
		"gpt-6":            "OpenAI",
		"gemini-3.8-flash": "Google",
		"grok-4":           "xAI",
	}
	for id, editeur := range cas {
		d := IdentityDirective(id)
		if d == "" || !contains(d, editeur) {
			t.Errorf("directive pour %q doit nommer %s : %q", id, editeur, d)
		}
	}
	if IdentityDirective("mistral-large") != "" {
		t.Error("aucune identité ne doit être inventée pour une famille inconnue")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
