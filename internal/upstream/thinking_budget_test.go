package upstream

import (
	"encoding/json"
	"testing"
)

// Un classificateur de securite envoie max_tokens=64 : le raisonnement ne doit pas etre
// active, sinon il consomme tout le budget et le client ne recoit aucun verdict.
func TestBudgetCourtDesactiveLeRaisonnement(t *testing.T) {
	corps := `{"model":"deepseek-v4.1-flash","max_tokens":64,"messages":[{"role":"user","content":"classe"}]}`
	out := PrepareBodyOpt([]byte(corps), false)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("json: %v", err)
	}
	th, _ := obj["thinking"].(map[string]any)
	if th == nil || th["type"] != "disabled" {
		t.Fatalf("raisonnement attendu desactive pour max_tokens=64 : %v", obj["thinking"])
	}
	if _, present := obj["reasoning_effort"]; present {
		t.Fatalf("aucun effort ne doit accompagner un raisonnement desactive")
	}
}

// Une reponse normale garde le raisonnement actif.
func TestBudgetNormalGardeLeRaisonnement(t *testing.T) {
	corps := `{"model":"deepseek-v4.1-flash","max_tokens":2048,"messages":[{"role":"user","content":"explique"}]}`
	out := PrepareBodyOpt([]byte(corps), false)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("json: %v", err)
	}
	th, _ := obj["thinking"].(map[string]any)
	if th == nil || th["type"] != "enabled" {
		t.Fatalf("raisonnement attendu actif pour max_tokens=2048 : %v", obj["thinking"])
	}
}
