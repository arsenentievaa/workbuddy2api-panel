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
		"Wie ben je?", "Welk model ben jij?", "Kim jesteś?", "Jakim modelem jesteś?",
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
		// Cas remontés par l'alerte « taux de reroutage élevé » du 2026-10-03 : des
		// questions de travail courtes qui parlent de « modèle » près de « tu ».
		"Peux-tu utiliser ce modèle ?", "Peux-tu me dire quel modèle est le meilleur ?",
		"Tu peux tester ce modèle stp ?", "¿Puedes usar este modelo?",
		"Kannst du dieses Modell benutzen?", "你能解释这个模型吗", "你能用这个模型做什么",
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

// TestFormesInversees : « what model am I talking to? », « rede ich », « est-ce que je ».
// Famille réelle de tests d'authenticité : le client demande à quel modèle IL parle.
// Ces formes sont reconnaissables, donc sans le risque de faux positif d'un « je » isolé.
func TestFormesInversees(t *testing.T) {
	for _, s := range []string{
		"What model am I talking to?", "Which model am I speaking with?",
		"Mit welchem Modell rede ich hier?", "Welches Modell spreche ich gerade an?",
		"À quel modèle est-ce que je parle ?", "Quel modèle suis-je en train d'utiliser ?",
		"¿Con qué modelo hablo yo?", "Con quale modello parlo io?",
	} {
		t.Run(s, func(t *testing.T) { mustRoute(t, body(t, s, nil)) })
	}
	// « je » isolé ne doit PAS router : question de travail ordinaire.
	if r := mustNotRoute(t, body(t, "Quel modèle je dois utiliser pour ce projet ?", nil)); r.has(SigModelQuestion) {
		t.Fatalf("un « je » isolé ne doit pas déclencher la sonde : %s", r.Explain())
	}
}

// TestSeuleLaParoleDuClientCompte : les règles de sonde ne doivent juger que ce que le
// CLIENT écrit — ni les résultats d'outils, ni les messages précédents de l'assistant.
//
// Mesuré en production le 2026-10-05 : 27 % du trafic était classé « sonde » parce qu'un
// journal d'outil contenait une phrase d'identité ou parce qu'une réponse antérieure portait
// une consigne de répétition. Le client, lui, n'avait rien demandé de tel.
func TestSeuleLaParoleDuClientCompte(t *testing.T) {
	// Une question d'identité dans un RÉSULTAT D'OUTIL ne doit pas router.
	r := mustNotRoute(t, detectBodyBytes(t, map[string]any{
		"model": "claude-sonnet-5",
		"messages": []any{
			map[string]any{"role": "user", "content": "Corrige le bug dans compute_total."},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "t1", "name": "read_file"},
			}},
			map[string]any{"role": "tool", "tool_call_id": "t1", "content": "Qui es-tu ? Quel modèle es-tu ?"},
		},
	}))
	if r.has(SigModelQuestion) {
		t.Fatalf("un résultat d'outil n'est pas la parole du client : %s", r.Explain())
	}

	// Une consigne de répétition dans un message PRÉCÉDENT de l'assistant non plus.
	r2 := mustNotRoute(t, detectBodyBytes(t, map[string]any{
		"model": "claude-sonnet-5",
		"messages": []any{
			map[string]any{"role": "user", "content": "continue"},
			map[string]any{"role": "assistant", "content": "Je répète la ligne 150 fois pour vérifier."},
			map[string]any{"role": "user", "content": "parfait, avance"},
		},
	}))
	if r2.has(SigRepetitionPattern) {
		t.Fatalf("un ancien message de l'assistant n'est pas la parole du client : %s", r2.Explain())
	}

	// La MÊME question, écrite par le client, doit router.
	mustRoute(t, detectBodyBytes(t, map[string]any{
		"model":    "claude-sonnet-5",
		"messages": []any{map[string]any{"role": "user", "content": "Qui es-tu ? Quel modèle es-tu ?"}},
	}))
}

// TestJetonPiegeDansUnOutilRouteToujours : le balayage des jetons pièges garde le texte
// complet — un test peut très bien arriver par un fichier ou une sortie d'outil.
func TestJetonPiegeDansUnOutilRouteToujours(t *testing.T) {
	d := New(DefaultConfig())
	jeton := d.cfg.GlitchTokens
	if len(jeton) == 0 {
		t.Skip("aucun jeton piège configuré")
	}
	r := mustRoute(t, detectBodyBytes(t, map[string]any{
		"model": "claude-sonnet-5",
		"messages": []any{
			map[string]any{"role": "user", "content": "lis le fichier et dis-moi ce qu'il contient"},
			map[string]any{"role": "tool", "content": "contenu: " + jeton[0]},
		},
	}))
	if !r.has(SigGlitchToken) {
		t.Fatalf("un jeton piège dans un outil doit rester détecté : %s", r.Explain())
	}
}
