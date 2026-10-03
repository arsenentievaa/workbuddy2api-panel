package fpdetect

import (
	"encoding/json"
	"testing"
)

// Sondes d'identité, toutes langues : doivent router.
func TestSondesIdentiteToutesLangues(t *testing.T) {
	sondes := []string{
		"Tu es qui ?", "tu es qui", "Who are you?", "What model are you?",
		"¿Quién eres?", "¿Qué modelo eres?", "Quem é você?", "Qual é o seu modelo?",
		"Wer bist du?", "Welches Modell bist du?", "Chi sei?", "Quale modello sei?",
		"Wie ben je?", "Welk model ben jij?", "Kim jesteś?", "Jaki to model?",
		"Кто ты?", "Какая ты модель?", "Хто ти?", "Sen kimsin?", "Hangi modelsin?",
		"من أنت؟", "ما هو نموذجك؟", "تو کی هستی؟", "אתה מי?", "מי אתה?",
		"तुम कौन हो?", "आप कौन हैं?", "আপনি কে?", "คุณเป็นใคร", "คุณคือโมเดลอะไร",
		"你是谁", "你是什么模型", "你是什麼模型", "あなたは誰ですか", "あなたは何のモデルですか",
		"너는 누구야", "무슨 모델이야", "Bạn là ai?", "Bạn là model gì?", "Kamu siapa?", "Anda model apa?",
		"Ποιος είσαι;", "Hvem er du?", "Vem är du?", "Wewe ni nani?", "Ty jsi kdo?",
		"ești cine?", "tu ești ce model?", "cu ce model vorbești?",
	}
	for _, s := range sondes {
		r := analyzeForTest(t, s)
		if !r.Route {
			t.Errorf("SONDE NON DETECTEE : %q (%s)", s, r.Explain())
		}
	}
}

// Travail ordinaire : ne doit PAS router, même en parlant de « modèle ».
func TestTravailOrdinaireNeRoutePas(t *testing.T) {
	ordinaires := []string{
		"Peux-tu m'expliquer ce modèle de données ?",
		"Je veux utiliser ce modèle pour la production.",
		"Quel modèle choisir pour un projet de résumé automatique en production ?",
		"Explique-moi comment fonctionne ce modèle de régression linéaire.",
		"Peux-tu refactoriser cette classe pour la rendre testable ?",
		"Ajoute un index sur la colonne user_id de la table commandes.",
		"Résume en deux phrases l'intérêt des tests unitaires.",
		"Le modèle de données doit gérer les commandes et les clients.",
		"Corrige le bug dans la fonction compute_total et lance les tests.",
		"¿Puedes explicar cómo funciona este modelo de datos?",
		"Kannst du mir dieses Datenmodell erklären?",
		"请解释这个数据模型如何工作",
		"Bu veri modelini açıklar mısın?",
	}
	for _, s := range ordinaires {
		r := analyzeForTest(t, s)
		if r.Route {
			t.Errorf("FAUX POSITIF : %q route (%s)", s, r.Explain())
		}
	}
}

// analyzeForTest : raccourci de test — corps minimal, aucune clé client.
func analyzeForTest(t *testing.T, user string) Result {
	t.Helper()
	d := New(DefaultConfig())
	b, err := json.Marshal(map[string]any{
		"model":    "claude-sonnet-5",
		"messages": []any{map[string]any{"role": "user", "content": user}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return d.Analyze(b, "")
}
