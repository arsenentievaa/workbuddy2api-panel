package fpdetect

import (
	"encoding/json"
	"testing"
)

// Règles resserrées le 2026-10-20 : rien ne doit passer sauf une conversation réellement
// écrite en chinois.

func corps(t *testing.T, user string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"model":    "claude-opus-5",
		"messages": []any{map[string]any{"role": "user", "content": user}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func detecteur() *Detector { return New(DefaultConfig()) }

// Réponse brève entièrement chinoise : « 好的 », « 你好 ». Sous l'ancien seuil de huit
// caractères, ces réponses passaient sans être vues.
func TestReponseCourteChinoiseDetectee(t *testing.T) {
	d := detecteur()
	for _, rep := range []string{"好的", "你好", "是的，可以。", "稍等"} {
		res := d.AnalyzeResponse(corps(t, "[1, 2, 3]"), rep)
		if !res.Route {
			t.Errorf("réponse courte %q non détectée", rep)
		}
	}
}

// Une réponse courte qui n'est pas du chinois (ou qui le cite au milieu d'une phrase
// latine) ne doit pas déclencher.
func TestReponseCourteLatineNonDetectee(t *testing.T) {
	d := detecteur()
	for _, rep := range []string{"ok", "7", "你好 (nǐ hǎo)", "Bonjour se dit 你好.", "Oui."} {
		if res := d.AnalyzeResponse(corps(t, "What is 3+4?"), rep); res.Route {
			t.Errorf("faux positif sur %q", rep)
		}
	}
}

// Une citation chinoise courte dans une question française ne doit plus désarmer la
// détection : le client attend une réponse française.
func TestCitationChinoiseNeDesarmePlus(t *testing.T) {
	d := detecteur()
	input := "Explique-moi ce que veut dire «光合作用» et donne un exemple."
	res := d.AnalyzeResponse(corps(t, input), "光合作用是植物把光能转化为化学能的过程，例子是绿色的叶子。")
	if !res.Route {
		t.Fatal("une citation courte ne doit plus blanchir une réponse entièrement chinoise")
	}
}

// Un utilisateur qui écrit réellement en chinois doit être servi en chinois : c'est la
// seule conversation légitime.
func TestUtilisateurSinophoneNonDetecte(t *testing.T) {
	d := detecteur()
	res := d.AnalyzeResponse(corps(t, "请用两句话解释光合作用。"), "光合作用是植物利用光能合成有机物的过程。")
	if res.Route {
		t.Fatal("une conversation en chinois ne doit pas être remédiée")
	}
}

func trame(content string) string {
	chunk, _ := json.Marshal(map[string]any{
		"id": "x", "object": "chat.completion.chunk", "created": 1, "model": "x",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": content}}},
	})
	return "data: " + string(chunk) + "\n\n"
}

func trameThinking(contenu string) string {
	chunk, _ := json.Marshal(map[string]any{
		"id": "x", "object": "chat.completion.chunk", "created": 1, "model": "x",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"reasoning_content": contenu}}},
	})
	return "data: " + string(chunk) + "\n\n"
}

// Le raisonnement interne en chinois avec une réponse latine n'est pas une fuite : le
// client ne voit que la réponse. Le compter déclenchait des remèdes inutiles.
func TestPenseeChinoiseReponseLatineNonDetectee(t *testing.T) {
	d := detecteur()
	prefix := []byte(trameThinking("用户问的是一个简单的问题，我应该用英语回答。") + trame("The answer is seven."))
	if res := d.AnalyzeStreamPrefix(corps(t, "What is 3+4?"), prefix); res.Route {
		t.Fatal("une pensée en chinois avec une réponse latine ne doit pas déclencher")
	}
}

// Réponse qui commence par un emoji puis bascule en chinois : elle doit être vue.
func TestFluxChinoisApresEmojiDetecte(t *testing.T) {
	d := detecteur()
	prefix := []byte(trame("👋") + trame("有什么可以帮你的？"))
	if res := d.AnalyzeStreamPrefix(corps(t, "👋"), prefix); !res.Route {
		t.Fatal("une réponse emoji puis chinoise doit être détectée")
	}
}

// Réponse brève en chinois sur le flux : deux caractères suffisent désormais.
func TestFluxCourtChinoisDetecte(t *testing.T) {
	d := detecteur()
	prefix := []byte(trame("好的"))
	if res := d.AnalyzeStreamPrefix(corps(t, "[1, 2, 3]"), prefix); !res.Route {
		t.Fatal("une réponse brève en chinois doit être détectée sur le flux")
	}
}

// Une demande de traduction vers une autre écriture d'Asie de l'Est n'est pas une fuite :
// la règle courte ne doit pas remédier « ありがとう » ni « 你好 » quand ils sont demandés.
func TestTraductionDemandeeNonRemediee(t *testing.T) {
	d := detecteur()
	cas := []struct{ question, reponse string }{
		{"Comment on dit merci en japonais ?", "ありがとう"},
		{"Dis-moi comment on dit bonjour en mandarin.", "你好"},
		{"Translate 'thank you' into Korean.", "감사합니다"},
		{"Comment dit-on « merci » en chinois ?", "谢谢"},
	}
	for _, c := range cas {
		if res := d.AnalyzeResponse(corps(t, c.question), c.reponse); res.Route {
			t.Errorf("traduction demandée remédiée à tort : %q -> %q", c.question, c.reponse)
		}
	}
}

// À l'inverse, une réponse en kana à une question qui ne demande rien de tel reste hors
// de la règle courte (elle ne concerne que le han) : pas de remède.
func TestKanaHorsRegleCourte(t *testing.T) {
	d := detecteur()
	if res := d.AnalyzeResponse(corps(t, "[1, 2, 3]"), "はい"); res.Route {
		t.Fatal("la règle courte ne doit viser que le han")
	}
}
