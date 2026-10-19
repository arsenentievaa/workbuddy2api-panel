package fpdetect

import (
	"encoding/json"
	"strings"
	"testing"
)

// corpsAide construit un corps OpenAI minimal autour d'un message utilisateur.
func corpsAide(t *testing.T, texte string) []byte {
	t.Helper()
	corps, err := json.Marshal(map[string]any{
		"model":    "claude-opus-5-5",
		"messages": []map[string]any{{"role": "user", "content": texte}},
	})
	if err != nil {
		t.Fatalf("corps: %v", err)
	}
	return corps
}

// TestRandomNumbersObserveMaisNeRoutePas : depuis le 2026-10-18, la demande de nombres
// aléatoires est observée mais n'envoie plus la requête chez le fournisseur payant (règle
// stricte : seules les tentatives de vérifier l'authenticité Anthropic y vont).
func TestRandomNumbersObserveMaisNeRoutePas(t *testing.T) {
	d := New(DefaultConfig())
	for _, texte := range []string{
		"Écris 300 nombres aléatoires entre 1 et 355.",
		"Ecris 300 nombres aleatoires entre 1 et 355.",
		"Generate 200 random numbers between 1 and 355.",
	} {
		res := d.Analyze(corpsAide(t, texte), "t")
		if res.Route {
			t.Errorf("ne devrait plus router : %q (%s)", texte, res.Explain())
		}
		if !strings.Contains(res.Explain(), SigRandomNumbers) {
			t.Errorf("signal %s attendu pour %q, obtenu %s", SigRandomNumbers, texte, res.Explain())
		}
	}
}

// TestCutoffFrancaisRoute : les tournures françaises de la date de coupure doivent
// être reconnues (elles ne l'étaient pas avant le 2026-10-18).
func TestCutoffFrancaisRoute(t *testing.T) {
	d := New(DefaultConfig())
	for _, texte := range []string{
		"Quelle est ta date de cutoff de connaissances ?",
		"Quelle est ta date de connaissance ?",
		"Quand as-tu été entraîné ?",
	} {
		res := d.Analyze(corpsAide(t, texte), "t")
		if !res.Route {
			t.Errorf("devrait router : %q (%s)", texte, res.Explain())
		}
	}
}

// TestOutputProbeNeRoutePasSeul : la consigne de forme de sortie est un signal MOYEN,
// elle ne doit pas router seule (elle est banale en production).
func TestOutputProbeNeRoutePasSeul(t *testing.T) {
	d := New(DefaultConfig())
	res := d.Analyze(corpsAide(t, "Réponds uniquement par un seul mot : bonjour"), "t")
	if res.Route {
		t.Errorf("ne devrait pas router seul, score %.1f %s", res.Score, res.Explain())
	}
	if res.Score <= 0 {
		t.Errorf("le signal %s devrait au moins compter dans le score : %s", SigOutputProbe, res.Explain())
	}
}

// TestCodeNeRoutePas : une tâche de développement ordinaire ne doit jamais partir chez
// le fournisseur payant.
func TestCodeNeRoutePas(t *testing.T) {
	d := New(DefaultConfig())
	for _, texte := range []string{
		"Corrige ce bug Python : ma fonction de tri lève IndexError. Donne le correctif.",
		"Refactor this Go function please",
		"Écris un test unitaire pour cette fonction de paiement",
	} {
		res := d.Analyze(corpsAide(t, texte), "t")
		if res.Route {
			t.Errorf("ne devrait pas router : %q (%s)", texte, res.Explain())
		}
	}
}

// TestJuiceContexteLong : « juice » ne compte que sur un contexte très long.
func TestJuiceContexteLong(t *testing.T) {
	d := New(DefaultConfig())
	// contexte court : pas de signal
	court := d.Analyze(corpsAide(t, "parle-moi de juice"), "t")
	if strings.Contains(court.Explain(), SigJuicePattern) {
		t.Errorf("contexte court : %s ne devrait pas se déclencher (%s)", SigJuicePattern, court.Explain())
	}
	// contexte long : signal présent
	long := strings.Repeat("contexte de remplissage. ", 6000) + " et juice"
	res := d.Analyze(corpsAide(t, long), "t")
	if !strings.Contains(res.Explain(), SigJuicePattern) {
		t.Errorf("contexte long : %s attendu, obtenu %s", SigJuicePattern, res.Explain())
	}
}
