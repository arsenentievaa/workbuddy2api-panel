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

// « Es-tu Claude ? » doit donner « oui », et non « non, je suis un environnement ».
// La consigne doit donc énoncer l'identité (nom de famille) et l'éditeur : c'est ce que
// lit le modèle avant de répondre. Sans l'éditeur, « qui t'a créé ? » reste sans réponse
// attendue.
func TestDirectiveRepondOuiALaFamille(t *testing.T) {
	for modele, famille := range map[string]string{
		"claude-opus-5-5": "Claude", "gpt-6": "GPT", "gemini-3.8-flash": "Gemini", "grok-4": "Grok",
	} {
		d := IdentityDirective(modele)
		if !contains(d, "You are "+famille) {
			t.Errorf("la directive de %q doit énoncer l'identité %s : %q", modele, famille, d)
		}
	}
}

// La note utilisateur doit atterrir dans le DERNIER message utilisateur, sans toucher
// aux autres messages ni emporter l'historique.
func TestAppendUserNote(t *testing.T) {
	corps := []byte(`{"model":"gpt-6","messages":[{"role":"system","content":"s"},{"role":"user","content":"premier"},{"role":"assistant","content":"a"},{"role":"user","content":"dernier"}]}`)
	out := string(AppendUserNote(corps, "NOTE"))
	if !contains(out, `"dernier\n\nNOTE"`) {
		t.Fatalf("la note doit fermer le dernier message utilisateur : %s", out)
	}
	if !contains(out, `"premier"`) {
		t.Fatalf("le message précédent doit rester intact : %s", out)
	}
	// Contenu en blocs : la note devient un bloc de texte.
	corps2 := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"salut"}]}]}`)
	out2 := string(AppendUserNote(corps2, "NOTE"))
	if !contains(out2, `"text":"NOTE"`) {
		t.Fatalf("la note doit être ajoutée comme bloc : %s", out2)
	}
}
