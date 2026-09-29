package fpdetect

import (
	"encoding/json"
	"strings"
	"testing"
)

// Le signal « language_mismatch » : entrée sans CJK + réponse en CJK.
// Les tests couvrent les deux cas exigés (anglais → chinois déclenche, chinois →
// chinois ne déclenche pas) et les faux positifs prévisibles.

func withPrompt(t *testing.T, text string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"model":    "claude-opus-5",
		"messages": []any{map[string]any{"role": "user", "content": text}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestLanguageMismatchAnglaisVersChinois(t *testing.T) {
	// Cas exigé : l'utilisateur écrit en anglais, le modèle répond en chinois.
	req := withPrompt(t, "What is the capital of France? Answer briefly.")
	answer := "法国的首都是巴黎。这座城市位于塞纳河畔，是法国最大的城市，也是政治与文化的中心。"
	res := New(DefaultConfig()).AnalyzeResponse(req, answer)
	if !res.Route {
		t.Fatalf("le signal doit déclencher : %s", res.Explain())
	}
	if !res.has(SigLanguageMismatch) {
		t.Fatalf("language_mismatch attendu : %s", res.Explain())
	}
	if res.Strong != SigLanguageMismatch {
		t.Fatalf("signal fort attendu : %s", res.Explain())
	}
}

func TestLanguageMismatchUtilisateurChinoisNeDeclenchePas(t *testing.T) {
	// Cas exigé : l'utilisateur écrit en chinois — une réponse chinoise est normale.
	req := withPrompt(t, "法国的首都是哪里？请简要回答。")
	answer := "法国的首都是巴黎。这座城市位于塞纳河畔，是法国最大的城市。"
	res := New(DefaultConfig()).AnalyzeResponse(req, answer)
	if res.Route || res.has(SigLanguageMismatch) {
		t.Fatalf("aucun signal attendu pour un utilisateur sinophone : %s", res.Explain())
	}
}

func TestLanguageMismatchReponseAnglaiseNeDeclenchePas(t *testing.T) {
	req := withPrompt(t, "What is the capital of France?")
	res := New(DefaultConfig()).AnalyzeResponse(req, "The capital of France is Paris.")
	if res.Route {
		t.Fatalf("aucun signal attendu : %s", res.Explain())
	}
}

func TestLanguageMismatchDemandeExpliciteDeChinois(t *testing.T) {
	// Faux positif prévisible : « traduis en chinois ». L'entrée ne contient aucun
	// caractère CJK, mais la réponse chinoise est exactement ce qui est demandé.
	for _, prompt := range []string{
		"Translate this paragraph into Chinese: Hello, how are you?",
		"Traduis ce paragraphe en chinois s'il te plaît : bonjour, comment vas-tu ?",
		"Donne-moi la traduction en chinois de cette phrase.",
		"Answer in Chinese: what is the capital of France?",
		"Übersetze das auf Chinesisch.",
		"请用中文回答：法国的首都是哪里？", // entrée déjà en CJK
	} {
		res := New(DefaultConfig()).AnalyzeResponse(withPrompt(t, prompt),
			"法国的首都是巴黎。这座城市位于塞纳河畔，是法国最大的城市。")
		if res.Route {
			t.Errorf("demande explicite de chinois : aucun signal attendu pour %q (%s)", prompt, res.Explain())
		}
	}
}

func TestLanguageMismatchQuelquesCaracteresIsoles(t *testing.T) {
	// Un terme technique ou un nom propre glissé dans une réponse anglaise ne fait pas
	// une réponse chinoise.
	cases := []string{
		"The Chinese word 你好 means hello.",
		"Use the flag `--lang=中文` to select the language.",
		"See 北京 in the documentation.",
	}
	for _, answer := range cases {
		res := New(DefaultConfig()).AnalyzeResponse(withPrompt(t, "How do I configure this?"), answer)
		if res.Route {
			t.Errorf("réponse majoritairement anglaise : aucun signal attendu pour %q", answer)
		}
	}
}

func TestLanguageMismatchJaponaisEtCoreen(t *testing.T) {
	// Un backend chinois qui répond en japonais ou en coréen trahit le même backend.
	for name, answer := range map[string]string{
		"japonais": "フランスの首都はパリです。セーヌ川のほとりにあり、フランス最大の都市です。",
		"coréen":   "프랑스의 수도는 파리입니다. 이 도시는 센 강변에 위치한 프랑스 최대 도시입니다.",
	} {
		t.Run(name, func(t *testing.T) {
			res := New(DefaultConfig()).AnalyzeResponse(withPrompt(t, "What is the capital of France?"), answer)
			if !res.Route {
				t.Fatalf("le signal doit déclencher pour une réponse en %s : %s", name, res.Explain())
			}
		})
	}
}

func TestLanguageMismatchCorpsVideOuIllisible(t *testing.T) {
	d := New(DefaultConfig())
	if r := d.AnalyzeResponse(nil, "中国的首都是北京。这是一座历史悠久的城市。"); r.Route {
		t.Fatalf("corps absent : aucun signal (%s)", r.Explain())
	}
	if r := d.AnalyzeResponse([]byte("{pas du json"), "中国的首都是北京。"); r.Route {
		t.Fatalf("corps illisible : aucun signal (%s)", r.Explain())
	}
	if r := d.AnalyzeResponse(withPrompt(t, "hello"), ""); r.Route {
		t.Fatalf("réponse vide : aucun signal (%s)", r.Explain())
	}
}

func TestLanguageMismatchPoidsConfigurable(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Weights = map[string]float64{SigLanguageMismatch: 0}
	d := New(cfg)
	req := withPrompt(t, "What is the capital of France?")
	if r := d.AnalyzeResponse(req, "法国的首都是巴黎。这座城市位于塞纳河畔。"); r.Route {
		t.Fatalf("poids nul : le signal doit être désactivé (%s)", r.Explain())
	}
}

func TestHasCJK(t *testing.T) {
	cases := map[string]bool{
		"hello world":   false,
		"bonjour":       false,
		"你好":            true,
		"こんにちは":         true,
		"안녕하세요":         true,
		"mixed 你好 text": true,
		"emoji 🙂":       false,
		"accentué éàü":  false,
		"繁體字":           true,
		"":              false,
	}
	for in, want := range cases {
		if got := hasCJK(in); got != want {
			t.Errorf("hasCJK(%q)=%v want %v", in, got, want)
		}
	}
}

func TestCountCJK(t *testing.T) {
	if n := countCJK("你好世界"); n != 4 {
		t.Errorf("countCJK=%d want 4", n)
	}
	if n := countCJK("hello 你好"); n != 2 {
		t.Errorf("countCJK=%d want 2", n)
	}
	if n := countCJK("hello"); n != 0 {
		t.Errorf("countCJK=%d want 0", n)
	}
}

// --- préfixe de flux ------------------------------------------------------------

func TestAnalyzeStreamPrefix(t *testing.T) {
	req := withPrompt(t, "What is the capital of France?")

	// Trame SSE dont le contenu est en chinois : détecté.
	zh := []byte("data: {\"id\":\"x\",\"choices\":[{\"delta\":{\"content\":\"法国的首都是巴黎，位于塞纳河畔。\"}}]}\n\n")
	if v := New(DefaultConfig()).AnalyzeStreamPrefix(req, zh); !v.Route {
		t.Fatal("un préfixe de flux en chinois doit être détecté")
	}
	// Trame en anglais : rien.
	en := []byte("data: {\"id\":\"x\",\"choices\":[{\"delta\":{\"content\":\"The capital of France is Paris.\"}}]}\n\n")
	if v := New(DefaultConfig()).AnalyzeStreamPrefix(req, en); v.Route {
		t.Fatal("un préfixe de flux en anglais ne doit rien déclencher")
	}
	// Entrée en chinois : on ne conclut rien, et surtout on ne retarde pas le flux.
	if v := New(DefaultConfig()).AnalyzeStreamPrefix(withPrompt(t, "法国的首都是哪里？"), zh); v.Route {
		t.Fatal("une entrée en chinois ne doit rien déclencher")
	}
	// Demande explicite de chinois : rien.
	if v := New(DefaultConfig()).AnalyzeStreamPrefix(withPrompt(t, "Translate into Chinese: hello"), zh); v.Route {
		t.Fatal("une demande explicite de chinois ne doit rien déclencher")
	}
	// Préfixe vide : rien.
	if v := New(DefaultConfig()).AnalyzeStreamPrefix(req, nil); v.Route {
		t.Fatal("préfixe vide : rien à conclure")
	}
}

// TestAnalyzeStreamPrefixEscapes : certains amonts encodent le non-ASCII en \uXXXX.
// Sans cette prise en charge, le signal ne marcherait que sur la moitié des amonts.
func TestAnalyzeStreamPrefixEscapes(t *testing.T) {
	req := withPrompt(t, "What is the capital of France?")
	escaped := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"\\u6cd5\\u56fd\\u7684\\u9996\\u90fd\\u662f\\u5df4\\u9ece\\u3002\"}}]}\n\n")
	if v := New(DefaultConfig()).AnalyzeStreamPrefix(req, escaped); !v.Route {
		t.Fatal("les caractères CJK échappés en \\uXXXX doivent être détectés")
	}
	// 9 séquences \uXXXX, dont \u3002 = « 。 » (ponctuation idéographique, plage
	// 3000-303F) volontairement NON comptée : la ponctuation ne fait pas qu'une réponse
	// est écrite en chinois. D'où 8.
	if n := countCJKInStreamFragment(escaped); n != 8 {
		t.Fatalf("comptage des échappements : %d (attendu 8)", n)
	}
}

// TestAskForCJKOutputNeCassePasLesAutresLangues : le garde-fou ne doit pas se déclencher
// sur une simple mention du mot « chinese » hors demande de traduction.
func TestAskForCJKOutputNeCassePasLesDemandesNormales(t *testing.T) {
	positive := []string{
		"translate to chinese", "en chinois", "answer in Chinese", "auf chinesisch",
		"traduire en chinois", "用中文回答",
	}
	for _, p := range positive {
		if !asksForCJKOutput(strings.ToLower(p)) {
			t.Errorf("demande de chinois non reconnue : %q", p)
		}
	}
	negative := []string{
		"who developed the chinese language model?",
		"what is the capital of france",
		"le chinois est une langue",
	}
	for _, p := range negative {
		if asksForCJKOutput(p) {
			t.Errorf("faux positif du garde-fou : %q", p)
		}
	}
}
