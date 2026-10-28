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

// L'amont refuse « stop » en chaine : la forme tableau doit etre imposee.
func TestStopChaineDevientTableau(t *testing.T) {
	obj := corpsApresTransformation(t, `{"model":"m","stop":"</severity>","messages":[{"role":"user","content":"x"}]}`)
	arr, ok := obj["stop"].([]any)
	if !ok || len(arr) != 1 || arr[0] != "</severity>" {
		t.Fatalf("stop doit etre un tableau d'une entree, obtenu %v", obj["stop"])
	}
	// Un tableau deja correct n'est pas altere.
	obj2 := corpsApresTransformation(t, `{"model":"m","stop":["a","b"],"messages":[{"role":"user","content":"x"}]}`)
	if arr2, _ := obj2["stop"].([]any); len(arr2) != 2 {
		t.Fatalf("tableau altere: %v", obj2["stop"])
	}
}

// REGRESSION DU CACHE (2026-10-26) : Claude Code insere des blocs systeme dynamiques
// (avis de fichier modifie, rappels horodates) AU MILIEU de la conversation. Ils doivent
// rester a leur place et rejoindre le message utilisateur suivant, jamais remonter en tete :
// sinon les premiers jetons changent a chaque tour et tout le cache de prefixe est perdu.
func TestBlocSystemeMilieuNeRemontePasEnTete(t *testing.T) {
	brut := `{"model":"m","messages":[
		{"role":"user","content":"premier"},
		{"role":"assistant","content":"ok"},
		{"role":"system","content":"Le fichier main.go a change a 18:55."},
		{"role":"user","content":"deuxieme"}]}`
	msgs := corpsApresTransformation(t, brut)["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("%d messages, attendu 3 (le bloc systeme rejoint le message suivant)", len(msgs))
	}
	premier := msgs[0].(map[string]any)
	if txt, _ := premier["content"].(string); txt != "premier" {
		t.Fatalf("le premier message ne doit pas etre modifie, obtenu %q", txt)
	}
	dernier := msgs[2].(map[string]any)
	txt, _ := dernier["content"].(string)
	if !strings.Contains(txt, "18:55") || !strings.Contains(txt, "deuxieme") {
		t.Fatalf("le bloc systeme doit accompagner le message suivant, obtenu %q", txt)
	}
	if strings.Index(txt, "18:55") > strings.Index(txt, "deuxieme") {
		t.Fatalf("le systeme doit preceder la demande: %q", txt)
	}
}

// Un bloc systeme place apres le dernier message utilisateur ne doit pas disparaitre.
func TestBlocSystemeFinalConserve(t *testing.T) {
	brut := `{"model":"m","messages":[
		{"role":"user","content":"demande"},
		{"role":"system","content":"consigne finale"}]}`
	msgs := corpsApresTransformation(t, brut)["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("%d messages, attendu 2", len(msgs))
	}
	dernier := msgs[1].(map[string]any)
	if txt, _ := dernier["content"].(string); !strings.Contains(txt, "consigne finale") {
		t.Fatalf("consigne finale perdue: %q", txt)
	}
}

// Claude Code place une ligne d'attribution dont les valeurs changent a chaque requete en
// tete de son prompt systeme. Si elle part vers un amont a cache de prefixe implicite, tout
// le cache est invalide a chaque tour (mesure du 2026-10-28). Elle doit disparaitre.
func TestEnteteAttributionRetire(t *testing.T) {
	obj := corpsApresTransformation(t, `{"model":"m","messages":[
		{"role":"system","content":"x-anthropic-billing-header: cc_version=2.1.311; cc_entrypoint=cli; cc_turn_index=7;\nTu es Claude Code."},
		{"role":"user","content":"Bonjour"}]}`)
	msgs := obj["messages"].([]any)
	txt := msgs[0].(map[string]any)["content"].(string)
	if strings.Contains(txt, "x-anthropic-billing-header") {
		t.Fatalf("entete d'attribution encore presente: %q", txt)
	}
	if !strings.Contains(txt, "Tu es Claude Code") {
		t.Fatalf("le reste du prompt systeme doit etre conserve: %q", txt)
	}
}
