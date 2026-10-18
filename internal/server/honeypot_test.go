package server

import (
	"encoding/json"
	"testing"
)

// TestUserTurns : le numéro d'échange est le nombre de messages utilisateur.
func TestUserTurns(t *testing.T) {
	corps, _ := json.Marshal(map[string]any{
		"messages": []map[string]any{
			{"role": "system", "content": "consigne"},
			{"role": "user", "content": "un"},
			{"role": "assistant", "content": "réponse"},
			{"role": "user", "content": "deux"},
		},
	})
	if n := userTurns(corps); n != 2 {
		t.Fatalf("attendu 2 échanges, obtenu %d", n)
	}
	if n := userTurns([]byte("corps illisible")); n != 0 {
		t.Fatalf("corps illisible : attendu 0, obtenu %d", n)
	}
}
