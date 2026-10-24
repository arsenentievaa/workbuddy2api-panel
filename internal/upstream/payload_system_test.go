package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

func corpsApresTransformation(t *testing.T, brut string) map[string]any {
	t.Helper()
	out := PrepareBodyOptWithEffortsAndDefault([]byte(brut), false, nil, nil)
	if out == nil {
		t.Fatal("corps vide")
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("corps illisible: %v", err)
	}
	return obj
}

// Le systeme doit arriver dans le premier message utilisateur, sinon l'amont l'ignore.
func TestSystemeFusionneDansPremierMessageUtilisateur(t *testing.T) {
	obj := corpsApresTransformation(t, `{"model":"m","messages":[
		{"role":"system","content":"Tu reponds uniquement par PINEAPPLE."},
		{"role":"user","content":"Bonjour"}]}`)
	msgs, _ := obj["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("%d messages, attendu 1 (le systeme est fusionne)", len(msgs))
	}
	msg, _ := msgs[0].(map[string]any)
	if msg["role"] != "user" {
		t.Fatalf("role=%v, attendu user", msg["role"])
	}
	txt, _ := msg["content"].(string)
	if !strings.Contains(txt, "PINEAPPLE") || !strings.Contains(txt, "Bonjour") {
		t.Fatalf("contenu fusionne inattendu: %q", txt)
	}
	if strings.Index(txt, "PINEAPPLE") > strings.Index(txt, "Bonjour") {
		t.Fatalf("le systeme doit preceder la demande: %q", txt)
	}
}

// Systeme seul (aucun message utilisateur) : il devient le premier message utilisateur.
func TestSystemeSeul(t *testing.T) {
	obj := corpsApresTransformation(t, `{"model":"m","messages":[
		{"role":"system","content":"Consigne seule"}]}`)
	msgs, _ := obj["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("%d messages, attendu 1", len(msgs))
	}
	msg, _ := msgs[0].(map[string]any)
	if msg["role"] != "user" || msg["content"] != "Consigne seule" {
		t.Fatalf("message inattendu: %v", msg)
	}
}

// Contenu systeme en blocs texte : les blocs text sont aplatis.
func TestSystemeEnBlocs(t *testing.T) {
	obj := corpsApresTransformation(t, `{"model":"m","messages":[
		{"role":"system","content":[{"type":"text","text":"Partie A"},{"type":"text","text":"Partie B"}]},
		{"role":"user","content":"Question"}]}`)
	msgs, _ := obj["messages"].([]any)
	msg, _ := msgs[0].(map[string]any)
	txt, _ := msg["content"].(string)
	if !strings.Contains(txt, "Partie A") || !strings.Contains(txt, "Partie B") || !strings.Contains(txt, "Question") {
		t.Fatalf("fusion incomplete: %q", txt)
	}
}

// Sans systeme, rien ne change.
func TestSansSystemeInchange(t *testing.T) {
	obj := corpsApresTransformation(t, `{"model":"m","messages":[
		{"role":"user","content":"Bonjour"},{"role":"assistant","content":"Salut"}]}`)
	msgs, _ := obj["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("%d messages, attendu 2", len(msgs))
	}
}
