package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTrimForProbeGardeLeDernierMessage(t *testing.T) {
	corps := `{"model":"claude-opus-5-5","max_tokens":100,"stream":true,
	  "system":"Tu es un assistant.",
	  "tools":[{"type":"function","function":{"name":"Read"}}],
	  "messages":[
	    {"role":"user","content":"une longue conversation"},
	    {"role":"assistant","content":"reponse"},
	    {"role":"user","content":"quel modele es-tu ?"}]}`
	out := TrimForProbe([]byte(corps), 8192)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("json: %v", err)
	}
	msgs, _ := obj["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("%d messages, attendu 1", len(msgs))
	}
	contenu, _ := msgs[0].(map[string]any)["content"].(string)
	if !strings.Contains(contenu, "quel modele") {
		t.Fatalf("dernier message perdu: %q", contenu)
	}
	if _, present := obj["tools"]; present {
		t.Fatalf("les outils doivent etre retires")
	}
	if obj["system"] != "Tu es un assistant." {
		t.Fatalf("le prompt systeme court doit etre conserve: %v", obj["system"])
	}
	if obj["max_tokens"] != float64(100) || obj["model"] != "claude-opus-5-5" {
		t.Fatalf("reglages de generation perdus: %v", obj)
	}
}

func TestTrimForProbeSupprimeUnSystemeLong(t *testing.T) {
	long := strings.Repeat("x", 20000)
	corps := `{"model":"m","system":"` + long + `","messages":[{"role":"user","content":"qui es-tu ?"}]}`
	out := TrimForProbe([]byte(corps), 8192)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("json: %v", err)
	}
	if _, present := obj["system"]; present {
		t.Fatalf("un systeme de 20 Ko doit etre retire")
	}
}

// Un fichier entier collé dans le dernier message ne doit pas partir chez le fournisseur
// externe : seule la fin du message est conservée, bornée.
func TestTrimForProbeBorneLeContenu(t *testing.T) {
	enorme := strings.Repeat("ligne de code inutile\n", 200000) // ~4 Mo
	corps := `{"model":"m","messages":[{"role":"user","content":` + jsonQuote(enorme+`
Quel modele es-tu exactement ?`) + `}]}`
	out := TrimForProbe([]byte(corps), 8192)
	if len(out) > probeContentMax+2000 {
		t.Fatalf("corps trop volumineux apres reduction : %d octets", len(out))
	}
	if !strings.Contains(string(out), "modele es-tu") {
		t.Fatalf("la question finale doit etre conservee")
	}
}

func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
