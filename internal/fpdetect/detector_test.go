package fpdetect

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Tests du détecteur. Portage des tests Python (5 critères d'origine + faux positifs)
// et couverture des méthodes ajoutées (connaissance, outils, thinking, PDF, flux,
// répétition temporelle) et du support multilingue.
//
// Les tests de faux positifs sont aussi importants que les autres : une règle qui
// route trop large envoie du trafic client légitime vers un modèle payant, et rend le
// modèle non déterministe pour ce client.

const testThreshold = 4.0

func det() *Detector {
	cfg := DefaultConfig()
	cfg.Threshold = testThreshold
	return New(cfg)
}

// --- constructeurs de requêtes --------------------------------------------------

func body(t *testing.T, text string, extra map[string]any) []byte {
	t.Helper()
	m := map[string]any{
		"model":      "claude-opus-5",
		"max_tokens": 1024,
		"messages":   []any{map[string]any{"role": "user", "content": text}},
	}
	for k, v := range extra {
		m[k] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func bodySystem(t *testing.T, user, system string) []byte {
	t.Helper()
	b, _ := json.Marshal(map[string]any{
		"model":    "claude-opus-5",
		"system":   system,
		"messages": []any{map[string]any{"role": "user", "content": user}},
	})
	return b
}

func bodyOpenAI(t *testing.T, user, system string) []byte {
	t.Helper()
	msgs := []any{}
	if system != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": system})
	}
	msgs = append(msgs, map[string]any{"role": "user", "content": user})
	b, _ := json.Marshal(map[string]any{"model": "claude-opus-5", "messages": msgs})
	return b
}

func mustRoute(t *testing.T, b []byte) Result {
	t.Helper()
	r := det().Analyze(b, "")
	if !r.Route {
		t.Fatalf("devrait router : %s", r.Explain())
	}
	return r
}

func mustNotRoute(t *testing.T, b []byte) Result {
	t.Helper()
	r := det().Analyze(b, "")
	if r.Route {
		t.Fatalf("ne devrait PAS router : %s", r.Explain())
	}
	return r
}

// --- critère 1 : requêtes très courtes ------------------------------------------

func TestRequeteCourteEstUnSignalMaisNeRoutePasSeule(t *testing.T) {
	r := mustNotRoute(t, body(t, "corrige cette faute", nil))
	if !r.has(SigVeryShort) {
		t.Fatalf("very_short attendu : %s", r.Explain())
	}
	if r.EstimatedTokens >= 10 {
		t.Fatalf("estimation=%d, attendu <10", r.EstimatedTokens)
	}
}

func TestRequeteCourtePlusTokenPiegeRoute(t *testing.T) {
	mustRoute(t, body(t, " SolidGoldMagikarp", nil))
}

// --- critère 2 : répétitions massives -------------------------------------------

func TestSerieDeCaracteresRoute(t *testing.T) {
	r := mustRoute(t, body(t, strings.Repeat("a", 200), nil))
	if !r.has(SigMassRepetition) {
		t.Fatalf("mass_repetition attendu : %s", r.Explain())
	}
}

func TestMotRepeteDominantRoute(t *testing.T) {
	mustRoute(t, body(t, strings.Repeat("token ", 120), nil))
}

func TestUnicodeRepeteRoute(t *testing.T) {
	mustRoute(t, body(t, strings.Repeat("龍", 150), nil))
}

// --- critère 3 : jetons pièges --------------------------------------------------

func TestJetonsPiegesDuPaquetglitchLens(t *testing.T) {
	for _, g := range GlitchTokensFromPackage {
		t.Run(g.ModelSeries, func(t *testing.T) {
			r := mustRoute(t, body(t, "que penses-tu de "+g.Token+" ?", nil))
			if !r.has(SigGlitchToken) {
				t.Fatalf("glitch_token attendu pour %s", g.ModelSeries)
			}
		})
	}
}

func TestJetonsPiegesDeLaLitterature(t *testing.T) {
	for _, g := range GlitchTokensFromLiterature {
		t.Run(g.Token, func(t *testing.T) {
			mustRoute(t, body(t, "explique "+g.Token+" simplement", nil))
		})
	}
}

func TestExplicationNeRecopieJamaisLeJeton(t *testing.T) {
	r := mustRoute(t, body(t, " SolidGoldMagikarp", nil))
	if strings.Contains(r.Explain(), "SolidGoldMagikarp") {
		t.Fatalf("le journal ne doit pas recopier la charge utile : %s", r.Explain())
	}
}

// --- critère 4 : question d'identité --------------------------------------------

func TestQuestionIdentiteAnglais(t *testing.T) {
	for _, q := range []string{
		"What model are you?", "which model are you", "Who are you?",
		"are you claude?", "what version of claude are you", "identify yourself",
	} {
		t.Run(q, func(t *testing.T) { mustRoute(t, body(t, q, nil)) })
	}
}

func TestQuestionIdentiteFrancais(t *testing.T) {
	for _, q := range []string{
		"Quel modèle es-tu ?", "tu es quel modèle", "quel est ton modèle",
		"votre modèle est quoi", "qui es-tu", "tu es Claude ?",
	} {
		t.Run(q, func(t *testing.T) { mustRoute(t, body(t, q, nil)) })
	}
}

// TestMotModeleArabeEtHindi : la classification ambiguë doit s'appuyer sur le mot
// réellement employé pour « modèle » dans la langue (मॉडल, نموذج), pas sur une
// translittération littérale (नमूना = spécimen, طراز = type). Régression : avec les
// mauvais termes, aucune tournure hindi/arabe n'était classable comme ambiguë.
func TestMotModeleArabeEtHindi(t *testing.T) {
	if !looseIdentityModelRe.MatchString("कौन सा मॉडल") {
		t.Error("« मॉडल » (hindi) doit être reconnu comme le mot « modèle »")
	}
	if !looseIdentityModelRe.MatchString("ما هو نموذج") {
		t.Error("« نموذج » (arabe) doit être reconnu comme le mot « modèle »")
	}
	for _, faux := range []string{"नमूना", "طراز"} {
		if looseIdentityModelRe.MatchString(faux) {
			t.Errorf("%q n'est pas le mot « modèle » en usage : il ne doit plus servir de marqueur", faux)
		}
	}
}

// TestFormeArabeAmbigueNeRoutePasDansUnTexteLong : « ما هو النموذج » (« quel est le
// modèle ») apparaît aussi dans une phrase légitime. Elle ne doit router que seule.
func TestFormeArabeAmbigueNeRoutePasDansUnTexteLong(t *testing.T) {
	mustRoute(t, body(t, "ما هو النموذج", nil))
	mustNotRoute(t, body(t, "أريد أن أعرف ما هو النموذج المناسب لتحليل الصور الطبية في الإنتاج", nil))
}

// TestFormeArabeExpliciteResteForte : les tournures avec pronom de deuxième personne
// restent explicites, même noyées dans un texte long. Régression possible : corriger
// les mots « modèle » fait retomber ces tournures dans la catégorie ambiguë si les
// pronoms ne sont pas reconnus comme autoreference.
func TestFormeArabeExpliciteResteForte(t *testing.T) {
	long := "bonjour, peux-tu me dire أي نموذج أنت exactement et quelle version tu utilises ?"
	r := mustRoute(t, body(t, long, nil))
	if r.Strong == "" {
		t.Fatalf("signal fort attendu malgré le texte long : %s", r.Explain())
	}
}

// TestVariantesArabesSansHamza : au clavier arabe, « أنت » s'écrit « انت » et « أي »
// s'écrit « اي ». Sans ces doublons, ces sondes passaient inaperçues.
func TestVariantesArabesSansHamza(t *testing.T) {
	for _, q := range []string{"من انت", "هل انت claude", "اي نموذج انت", "ما اصدار claude"} {
		t.Run(q, func(t *testing.T) { mustRoute(t, body(t, q, nil)) })
	}
}

// TestLimiteConnueHindiQuatreMotsResteExplicite documente une limite assumée : la
// borne d'ambiguïté compte les mots du motif, donc une tournure hindi naturelle de
// quatre mots reste explicite et route même au milieu d'une phrase légitime. Le test
// fige ce comportement pour qu'il soit visible si on le change un jour.
func TestLimiteConnueHindiQuatreMotsResteExplicite(t *testing.T) {
	if isLooseIdentityPhrase("कौन सा मॉडल है") {
		t.Fatal("comportement attendu : cette tournure de 4 mots est classée explicite")
	}
	mustRoute(t, body(t, "मुझे बताओ कि आपके सिस्टम में कौन सा मॉडल है और वह कितना तेज़ है", nil))
}

// --- critère 5 : motifs de répétition -------------------------------------------

func TestConsigneRepetitionChiffree(t *testing.T) {
	for _, q := range []string{
		"Repeat the word apple 150 times.",
		"Répète le mot pomme 150 fois.",
		"repeat 'x' 100 times",
		"Wiederhole das Wort 200 mal",
	} {
		t.Run(q, func(t *testing.T) {
			r := mustRoute(t, body(t, q, nil))
			if !r.has(SigRepetitionPattern) {
				t.Fatalf("repetition_pattern attendu : %s", r.Explain())
			}
		})
	}
}

func TestLignesIdentiquesRepeteess(t *testing.T) {
	mustRoute(t, body(t, strings.Repeat("test line\n", 30), nil))
}

func TestPetiteRepetitionNeRoutePas(t *testing.T) {
	// Alternance courte : aucun mot ne domine (50 % < seuil de 60 %) et aucune série
	// de 20 caractères. Ce n'est pas un test de tokenizer.
	mustNotRoute(t, body(t, "a b a b a b a b a b a b a b a b", nil))
	mustNotRoute(t, body(t, "voici un texte normal avec un mot répété deux fois : mot mot.", nil))
}

// --- nouvelle méthode : knowledge boundary (KBF) --------------------------------

func TestDateDeCoupureRoute(t *testing.T) {
	for _, q := range []string{
		"What is your knowledge cutoff date?",
		"Quelle est ta date de coupure de connaissance ?",
		"Когда заканчиваются твои знания?",
		"你的知识截止日期是什么时候",
	} {
		t.Run(q, func(t *testing.T) {
			r := mustRoute(t, body(t, q, nil))
			if !r.has(SigCutoffExplicit) {
				t.Fatalf("cutoff_explicit attendu : %s", r.Explain())
			}
		})
	}
}

func TestFaitRecentSeulNeRoutePas(t *testing.T) {
	// Question légitime sur 2026 : signal faible, insuffisant seul.
	r := mustNotRoute(t, body(t, "Peux-tu m'expliquer les changements de la réglementation européenne sur les données en 2026 ?", nil))
	if !r.has(SigRecentFact) {
		t.Fatalf("recent_fact attendu comme signal faible : %s", r.Explain())
	}
}

func TestFaitRecentEstFaibleEtNestSuffisantQuAccumule(t *testing.T) {
	// Deux signaux faibles (fait récent + requête courte) ne suffisent pas : c'est
	// voulu, « que s'est-il passé en 2026 » est une question légitime.
	r := mustNotRoute(t, body(t, "événements marquants 2026 ?", nil))
	if !r.has(SigRecentFact) || !r.has(SigVeryShort) {
		t.Fatalf("les deux signaux faibles sont attendus : %s", r.Explain())
	}
	// Accumulé avec un signal fort, en revanche, la requête part vers le vrai modèle.
	mustRoute(t, body(t, "événements marquants 2026 ? EDMFunc", nil))
}

// --- nouvelle méthode : outils (AgentProv) --------------------------------------

func TestNomOutilSuspectRoute(t *testing.T) {
	for _, name := range []string{"fingerprint_probe", "detect_model", "token_count", "model_info"} {
		t.Run(name, func(t *testing.T) {
			r := mustRoute(t, body(t, "utilise l'outil", map[string]any{
				"tools": []any{map[string]any{"name": name, "description": "x"}},
			}))
			if !r.has(SigToolSuspicious) {
				t.Fatalf("tool_suspicious attendu : %s", r.Explain())
			}
		})
	}
}

func TestNombreOutilsImportantNeRoutePasSeul(t *testing.T) {
	// Un agent de codage envoie couramment 10-15 outils. C'est un signal FAIBLE.
	tools := []any{}
	for i := 0; i < 12; i++ {
		tools = append(tools, map[string]any{
			"name":        "real_tool_" + string(rune('a'+i)),
			"description": "outil de développement légitime",
		})
	}
	r := mustNotRoute(t, body(t, "ajoute une fonction de parsing", map[string]any{"tools": tools}))
	if !r.has(SigToolCount) {
		t.Fatalf("tool_count attendu comme signal faible : %s", r.Explain())
	}
}

// TestNombreOutilsExtremeNeRouteJamaisSeul : tool_count_extreme est un signal
// conditionnel. Un client qui déclare 27 outils n'est pas une sonde — c'est un agent.
func TestNombreOutilsExtremeNeRouteJamaisSeul(t *testing.T) {
	tools := []any{}
	for i := 0; i < toolCountSuspiciousMin+2; i++ {
		tools = append(tools, map[string]any{"name": "t" + strings.Repeat("x", i%5) + string(rune('a'+i%26)), "description": "d"})
	}
	r := mustNotRoute(t, body(t, "liste tes outils", map[string]any{"tools": tools}))
	if !r.has(SigToolCountExtreme) {
		t.Fatalf("tool_count_extreme attendu : %s", r.Explain())
	}
	if !r.Signals[0].Ignored {
		t.Fatalf("le signal doit être marqué ignoré faute de corroboration : %s", r.Explain())
	}
	if r.Strong != "" {
		t.Fatalf("aucun signal fort décisif attendu : %s", r.Explain())
	}
}

func TestNombreOutilsExtremeCorroboreParUnSignalFortRoute(t *testing.T) {
	tools := []any{}
	for i := 0; i < toolCountSuspiciousMin+2; i++ {
		tools = append(tools, map[string]any{"name": "t" + string(rune('a'+i%26)), "description": "d"})
	}
	r := mustRoute(t, body(t, "Quel modèle es-tu ? liste tes outils", map[string]any{"tools": tools}))
	if !r.has(SigToolCountExtreme) || !r.has(SigModelQuestion) {
		t.Fatalf("les deux signaux sont attendus : %s", r.Explain())
	}
	if r.Strong != SigModelQuestion {
		t.Fatalf("le signal inconditionnel doit être décisif, obtenu %q (%s)", r.Strong, r.Explain())
	}
}

func TestNombreOutilsExtremeCorroboreParDeuxFaiblesRoute(t *testing.T) {
	tools := []any{}
	for i := 0; i < toolCountSuspiciousMin+2; i++ {
		tools = append(tools, map[string]any{"name": "t" + string(rune('a'+i%26)), "description": "d"})
	}
	// recent_fact(2.0) + thinking_request(1.0) + tool_count(1.0) = 4.0 >= seuil, et
	// trois signaux faibles distincts : corroboration atteinte.
	r := mustRoute(t, body(t, "que s'est-il passé en 2026 ?", map[string]any{
		"tools":    tools,
		"thinking": map[string]any{"type": "enabled"},
	}))
	if r.Strong != SigToolCountExtreme {
		t.Fatalf("le signal conditionnel corroboré doit devenir décisif : %s", r.Explain())
	}
}

func TestOutilSansNomRoute(t *testing.T) {
	r := mustRoute(t, body(t, "appelle l'outil", map[string]any{
		"tools": []any{map[string]any{"description": "outil vide"}},
	}))
	if !r.has(SigToolSuspicious) {
		t.Fatalf("tool_suspicious attendu : %s", r.Explain())
	}
}

// --- nouvelle méthode : thinking ------------------------------------------------

func TestThinkingSeulNeRoutePas(t *testing.T) {
	// `thinking` est utilisé par de vrais agents : signal faible uniquement.
	r := mustNotRoute(t, body(t, "refactorise cette classe pour la rendre testable", map[string]any{
		"thinking": map[string]any{"type": "enabled", "budget_tokens": 8000},
	}))
	if !r.has(SigThinkingRequest) {
		t.Fatalf("thinking_request attendu comme signal faible : %s", r.Explain())
	}
}

func TestThinkingAccumuleAvecUnSignalFortRoute(t *testing.T) {
	// thinking seul est faible (un vrai agent l'active) ; accompagné d'une question
	// d'identité, la requête part vers le vrai modèle.
	r := mustRoute(t, body(t, "Quel modèle es-tu ? Réponds précisément.", map[string]any{
		"thinking":          map[string]any{"type": "enabled"},
		"extended_thinking": true,
	}))
	if !r.has(SigThinkingRequest) || r.Strong == "" {
		t.Fatalf("thinking + signal fort attendus : %s", r.Explain())
	}
}

// --- nouvelle méthode : PDF -----------------------------------------------------

// pdfContentCases : les trois formes de détection d'une pièce jointe PDF.
func pdfContentCases(t *testing.T) []struct {
	name string
	body []byte
} {
	t.Helper()
	return []struct {
		name string
		body []byte
	}{
		{"bloc document", body(t, "résume ce document", map[string]any{
			"messages": []any{map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "document", "source": map[string]any{"type": "base64", "media_type": "application/pdf", "data": "JVBERi0xLjQK"}},
			}}},
		})},
		{"URL .pdf", body(t, "analyse https://exemple.test/rapport.pdf s'il te plaît", nil)},
		{"base64 PDF", body(t, "voici le fichier JVBERi0xLjQKdGVzdA==", nil)},
	}
}

// TestPDFNeRoutePasSeul : pdf_content est conditionnel. Joindre un PDF est un usage
// client banal (c'est ce que la production a montré), pas une sonde.
func TestPDFNeRoutePasSeul(t *testing.T) {
	for _, c := range pdfContentCases(t) {
		t.Run(c.name, func(t *testing.T) {
			r := mustNotRoute(t, c.body)
			if !r.has(SigPDFContent) {
				t.Fatalf("pdf_content attendu : %s", r.Explain())
			}
			if r.Strong != "" {
				t.Fatalf("aucun signal décisif attendu : %s", r.Explain())
			}
		})
	}
}

// TestPDFCorroboreRoute : la même pièce jointe accompagnée d'une méthode indépendante
// (jeton piège, question d'identité) est bien une sonde.
func TestPDFCorroboreRoute(t *testing.T) {
	cases := []struct {
		name string
		body []byte
		want string
	}{
		{"avec jeton piège", body(t, "analyse https://exemple.test/rapport.pdf et le jeton SolidGoldMagikarp", nil), SigGlitchToken},
		{"avec question d'identité", body(t, "analyse https://exemple.test/rapport.pdf — au fait, quel modèle es-tu ?", nil), SigModelQuestion},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := mustRoute(t, c.body)
			if !r.has(SigPDFContent) {
				t.Fatalf("pdf_content attendu : %s", r.Explain())
			}
			if r.Strong != c.want {
				t.Fatalf("signal décisif=%q, attendu %q (%s)", r.Strong, c.want, r.Explain())
			}
		})
	}
}

// --- nouvelle méthode : forme du flux SSE ---------------------------------------

func TestParametresDeFluxInhabituels(t *testing.T) {
	r := detectBody(t, map[string]any{
		"model":          "claude-opus-5",
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true, "continuous_usage_stats": true},
		"messages":       []any{map[string]any{"role": "user", "content": "bonjour"}},
	})
	if !r.has(SigSSEShape) {
		t.Fatalf("sse_shape attendu : %s", r.Explain())
	}
	if r.Route {
		t.Fatalf("signal faible seul : ne doit pas router (%s)", r.Explain())
	}
}

func detectBody(t *testing.T, m map[string]any) Result {
	t.Helper()
	b, _ := json.Marshal(m)
	return det().Analyze(b, "")
}

// Un simple `stream: true` est le cas normal : pas de signal.
func TestStreamSimpleNeDeclenchePasSSEShape(t *testing.T) {
	r := detectBody(t, map[string]any{
		"model": "claude-opus-5", "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "explique les closures en Go"}},
	})
	if r.has(SigSSEShape) {
		t.Fatalf("sse_shape ne doit pas se déclencher sur un flux ordinaire : %s", r.Explain())
	}
}

// --- nouvelle méthode : répétition temporelle (RUT) -----------------------------

func TestRequetesIdentiquesRepeteessRoute(t *testing.T) {
	d := det()
	b := body(t, "quelle heure est-il", nil)
	now := time.Now()
	for i := 1; i <= 3; i++ {
		r := d.AnalyzeAt(b, "client-A", now.Add(time.Duration(i)*time.Second))
		if r.has(SigRepeatRequest) {
			t.Fatalf("itération %d : répétition détectée trop tôt (%s)", i, r.Explain())
		}
	}
	r := d.AnalyzeAt(b, "client-A", now.Add(4*time.Second))
	if !r.has(SigRepeatRequest) {
		t.Fatalf("4e requête identique : repeat_request attendu (%s)", r.Explain())
	}
	if !r.Route {
		t.Fatalf("repeat_request est un signal fort : doit router (%s)", r.Explain())
	}
}

func TestRepetitionNeSeDeclenchePasEntreClients(t *testing.T) {
	d := det()
	b := body(t, "quelle heure est-il", nil)
	now := time.Now()
	for i := 0; i < 5; i++ {
		if r := d.AnalyzeAt(b, "client-"+string(rune('A'+i)), now); r.Route {
			t.Fatalf("des clients distincts ne doivent pas se cumuler : %s", r.Explain())
		}
	}
}

func TestRepetitionExpireAvecLaFenetre(t *testing.T) {
	d := det()
	b := body(t, "question identique", nil)
	now := time.Now()
	for i := 0; i < 3; i++ {
		d.AnalyzeAt(b, "client-C", now)
	}
	// Au-delà de la fenêtre de 5 minutes, l'historique est purgé.
	r := d.AnalyzeAt(b, "client-C", now.Add(6*time.Minute))
	if r.has(SigRepeatRequest) {
		t.Fatalf("la fenêtre aurait dû expirer : %s", r.Explain())
	}
}

func TestRequetesDifferentesNeSeCumulentPas(t *testing.T) {
	d := det()
	now := time.Now()
	for i := 0; i < 6; i++ {
		r := d.AnalyzeAt(body(t, "question numéro "+string(rune('a'+i))+" sur le code", nil), "client-D", now)
		if r.has(SigRepeatRequest) {
			t.Fatalf("des requêtes différentes ne sont pas une répétition : %s", r.Explain())
		}
	}
}

// --- faux positifs (ce qui coûte de l'argent) -----------------------------------

func TestRequeteDeCodageReelle(t *testing.T) {
	b := body(t, "Peux-tu corriger cette fonction Python qui plante ?\n```python\nimport os\nos.getcwd(1)\n```\nLe traceback dit: TypeError: getcwd() takes no arguments", nil)
	r := mustNotRoute(t, b)
	if !r.CodingLike {
		t.Fatalf("coding_like attendu : %s", r.Explain())
	}
}

func TestAgentAvecGrosPromptSysteme(t *testing.T) {
	mustNotRoute(t, bodySystem(t, "ajoute un test unitaire pour parser_config",
		"Tu es un assistant de développement. "+strings.Repeat("Contexte projet. ", 60)))
}

func TestConversationLongueEtNaturelle(t *testing.T) {
	mustNotRoute(t, body(t, strings.Repeat("Explique-moi en détail le fonctionnement d'un index B-tree, avec les compromis de conception. ", 8), nil))
}

func TestQuestionCommercialeSurLesModeles(t *testing.T) {
	// Le cas qui avait produit un faux positif côté Python.
	r := mustNotRoute(t, body(t, "Which model is best for summarisation tasks in production?", nil))
	if r.has(SigModelQuestion) {
		t.Fatalf("question commerciale : ne doit pas être prise pour une sonde d'identité (%s)", r.Explain())
	}
}

func TestFormeBreveAmbigueSurRequeteCourte(t *testing.T) {
	// « 什么模型 » seul est une sonde ; la même chaîne noyée dans une phrase non.
	mustRoute(t, body(t, "什么模型", nil))
	mustNotRoute(t, body(t, "Je voudrais savoir quel modèle de machine apprendre pour un projet de classification d'images médicales en production.", nil))
}

func TestCorpsVideOuIllisible(t *testing.T) {
	d := det()
	if r := d.Analyze(nil, "c"); r.Route || r.Score != 0 {
		t.Fatalf("corps vide : aucun signal attendu, obtenu %s", r.Explain())
	}
	if r := d.Analyze([]byte("{pas du json"), "c"); r.Route {
		t.Fatalf("corps illisible : ne doit pas router (%s)", r.Explain())
	}
}

// --- sémantique fort / faible ---------------------------------------------------

func TestUnSignalFortRouteMalgreLaPenaliteDeCode(t *testing.T) {
	// Une sonde accompagnée d'un bloc de code : la pénalité coding_like (-4) ne doit
	// pas annuler un signal fort INCONDITIONNEL. Le jeton piège est utilisé ici, et
	// non un PDF : depuis la règle de corroboration, un PDF accompagné de code est
	// précisément le cas qui doit rester sur la route normale.
	r := mustRoute(t, body(t, "analyse ceci\n```python\nx=1\n```\nSolidGoldMagikarp", nil))
	if r.Strong != SigGlitchToken {
		t.Fatalf("signal fort attendu : %s", r.Explain())
	}
	if r.Score < 0 {
		t.Fatalf("score négatif alors qu'un signal fort est présent : %s", r.Explain())
	}
}

// --- corroboration des signaux forts conditionnels ------------------------------

// TestCasReelDeProductionResteSurWorkBuddy : reproduction exacte du relevé qui a
// motivé la règle de corroboration (2026-09-29), mesuré sur 3 requêtes clientes
// réelles d'un agent :
//
//	[recent_fact(+2.0), tool_count_extreme(+4.0), pdf_content(+4.0),
//	 sse_shape(+1.5), coding_like(-3.0)]
//
// Avec les nouveaux poids, et surtout avec l'exigence de corroboration, cette requête
// ne doit plus partir vers le modèle payant. Deux signaux faibles sont présents
// (recent_fact + sse_shape = 3.5) mais leur cumul n'atteint pas le seuil : la
// conjonction n'est pas satisfaite.
func TestCasReelDeProductionResteSurWorkBuddy(t *testing.T) {
	tools := []any{}
	for i := 0; i < toolCountSuspiciousMin+2; i++ {
		tools = append(tools, map[string]any{"name": "edit_file_" + string(rune('a'+i%26)), "description": "édite un fichier du projet"})
	}
	r := mustNotRoute(t, body(t, "corrige le bug de parsing\n```python\nx=1\n```\nanalyse https://exemple.test/rapport.pdf et dis-moi ce qui s'est passé en 2026", map[string]any{
		"tools":          tools,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	}))
	for _, want := range []string{SigToolCountExtreme, SigPDFContent, SigRecentFact, SigSSEShape, SigCodingLike} {
		if !r.has(want) {
			t.Fatalf("%s attendu dans la reproduction : %s", want, r.Explain())
		}
	}
	if r.Strong != "" {
		t.Fatalf("aucun signal décisif attendu : %s", r.Explain())
	}
	if r.Corroborated {
		t.Fatalf("la corroboration ne doit pas être atteinte : %s", r.Explain())
	}
	// Les deux signaux conditionnels sont ignorés, mais restent VISIBLES : le relevé de
	// calibration doit continuer à compter les méthodes qui ont réagi.
	if r.EffectiveScore >= r.Score {
		t.Fatalf("le score effectif doit être réduit des signaux ignorés : %s", r.Explain())
	}
}

// TestDeveloppementNormalAvecOutilsResteSurWorkBuddy : le pendant « pas de PDF ».
func TestDeveloppementNormalAvecOutilsResteSurWorkBuddy(t *testing.T) {
	tools := []any{}
	for i := 0; i < toolCountSuspiciousMin+2; i++ {
		tools = append(tools, map[string]any{"name": "grep_" + string(rune('a'+i%26)), "description": "cherche dans le dépôt"})
	}
	r := mustNotRoute(t, body(t, "refactorise cette fonction\n```go\nfunc main() {}\n```", map[string]any{"tools": tools}))
	if !r.has(SigToolCountExtreme) || !r.has(SigCodingLike) {
		t.Fatalf("tool_count_extreme et coding_like attendus : %s", r.Explain())
	}
}

// TestVraiTestJetonPiegeEtPDFRoute : un PDF n'est pas anodin dès lors qu'il accompagne
// une méthode de sondage — ici un jeton piège.
func TestVraiTestJetonPiegeEtPDFRoute(t *testing.T) {
	r := mustRoute(t, body(t, "compare https://exemple.test/a.pdf avec SolidGoldMagikarp", nil))
	if !r.has(SigPDFContent) || !r.has(SigGlitchToken) {
		t.Fatalf("les deux signaux attendus : %s", r.Explain())
	}
	if r.Strong != SigGlitchToken {
		t.Fatalf("le jeton piège doit être décisif : %s", r.Explain())
	}
}

// TestQuestionIdentiteSeuleRoute : les signaux inconditionnels restent décisifs seuls.
func TestQuestionIdentiteSeuleRoute(t *testing.T) {
	r := mustRoute(t, body(t, "What model are you?", nil))
	if r.Strong != SigModelQuestion {
		t.Fatalf("model_question attendu comme décisif : %s", r.Explain())
	}
	if r.Corroborated {
		t.Fatalf("aucun signal conditionnel ici : la corroboration ne s'applique pas (%s)", r.Explain())
	}
}

// TestCorroborationParCumulDeFaiblesSousLeSeuilNeSuffitPas documente la conjonction :
// deux signaux faibles ne corroborent pas un signal conditionnel si leur cumul reste
// sous le seuil. Le texte est volontairement long pour ne pas ajouter `very_short`.
func TestCorroborationParCumulDeFaiblesSousLeSeuilNeSuffitPas(t *testing.T) {
	// recent_fact(2.0) + sse_shape(1.5) = 3.5 < 4.0, deux signaux faibles distincts.
	question := "peux-tu me dire ce qui s'est passé dans le monde en 2026 ?"
	r := mustNotRoute(t, detectBodyBytes(t, map[string]any{
		"model":          "claude-opus-5",
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
		"messages":       []any{map[string]any{"role": "user", "content": question}},
	}))
	if r.Corroborated {
		t.Fatalf("aucun signal conditionnel : rien à corroborer (%s)", r.Explain())
	}
	// Le même cumul PLUS un signal conditionnel ne doit pas router par le score brut.
	r = mustNotRoute(t, detectBodyBytes(t, map[string]any{
		"model":          "claude-opus-5",
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "document", "source": map[string]any{"type": "base64", "media_type": "application/pdf", "data": "JVBERi0xLjQK"}},
			map[string]any{"type": "text", "text": "analyse ceci — " + question},
		}}},
	}))
	if r.Route {
		t.Fatalf("un conditionnel non corroboré ne doit pas router même si le score brut dépasse le seuil : %s", r.Explain())
	}
}

// TestCumulDeFaiblesSansSignalConditionnelRouteToujours : la règle de corroboration ne
// touche pas le chemin « accumulation de signaux faibles » quand aucune méthode
// conditionnelle n'est en jeu — ce serait un recul de détection.
func TestCumulDeFaiblesSansSignalConditionnelRouteToujours(t *testing.T) {
	// very_short(1.5) + recent_fact(2.0) + sse_shape(1.5) = 5.0 >= 4.0, sans
	// conditionnel : la détection par accumulation reste intacte.
	r := mustRoute(t, detectBodyBytes(t, map[string]any{
		"model":          "claude-opus-5",
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
		"messages":       []any{map[string]any{"role": "user", "content": "2026 : que s'est-il passé ?"}},
	}))
	if r.Strong != "" {
		t.Fatalf("aucun signal fort attendu : %s", r.Explain())
	}
	if r.EffectiveScore < 4.0 {
		t.Fatalf("le cumul de faibles doit router par le score : %s", r.Explain())
	}
}

// TestTraficAvecOutilsExigeUnSignalInconditionnel : conséquence directe de
// coding_like(-4) — toute requête qui DÉCLARE des outils porte cette pénalité (c'est
// ainsi que la production a été classée « développement »). L'accumulation de faibles
// ne peut donc plus faire router un agent : il faut une méthode de sondage
// inconditionnelle. C'est le comportement voulu, pas un effet de bord.
func TestTraficAvecOutilsExigeUnSignalInconditionnel(t *testing.T) {
	tools := []any{}
	for i := 0; i < toolCountNotable+1; i++ {
		tools = append(tools, map[string]any{"name": "outil_" + string(rune('a'+i)), "description": "outil réel"})
	}
	r := mustNotRoute(t, detectBodyBytes(t, map[string]any{
		"model":          "claude-opus-5",
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
		"tools":          tools,
		"messages":       []any{map[string]any{"role": "user", "content": "peux-tu me dire ce qui s'est passé dans le monde en 2026 ?"}},
	}))
	if !r.has(SigCodingLike) {
		t.Fatalf("coding_like attendu (outils déclarés) : %s", r.Explain())
	}
	if r.EffectiveScore >= 4.0 {
		t.Fatalf("la pénalité doit maintenir le cumul sous le seuil : %s", r.Explain())
	}
}

// TestCorroborationConfigurable : le jeu de signaux conditionnels et le nombre de
// faibles requis sont des réglages, pas des constantes cachées.
func TestCorroborationConfigurable(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CorroborationWeakMin = 1
	cfg.Threshold = 3.0
	d := New(cfg)
	// recent_fact(2.0) + sse_shape(1.5) = 3.5 >= 3.0 avec 2 faibles : corroboré.
	r := d.Analyze(detectBodyBytes(t, map[string]any{
		"model":          "claude-opus-5",
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "document", "source": map[string]any{"type": "base64", "media_type": "application/pdf", "data": "JVBERi0xLjQK"}},
			map[string]any{"type": "text", "text": "que s'est-il passé en 2026 ?"},
		}}},
	}), "")
	if !r.Corroborated || !r.Route {
		t.Fatalf("avec un seuil abaissé, la corroboration doit être atteinte : %s", r.Explain())
	}

	// Un signal conditionnel retiré de la liste redevient un fort ordinaire.
	cfg2 := DefaultConfig()
	cfg2.StrongSignals = []string{SigPDFContent}
	cfg2.CorroborationSignals = []string{SigToolCountExtreme}
	d2 := New(cfg2)
	r2 := d2.Analyze(body(t, "analyse https://exemple.test/rapport.pdf", nil), "")
	if !r2.Route || r2.Strong != SigPDFContent {
		t.Fatalf("pdf_content hors liste de corroboration doit router seul : %s", r2.Explain())
	}
}

func detectBodyBytes(t *testing.T, m map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSeuilEtPoidsSontConfigurables(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Weights = map[string]float64{SigVeryShort: 9.0}
	d := New(cfg)
	if r := d.Analyze(body(t, "salut", nil), ""); !r.Route {
		t.Fatalf("un poids de 9 doit router malgré le seuil : %s", r.Explain())
	}

	cfg2 := DefaultConfig()
	cfg2.Threshold = 1.0
	if r := New(cfg2).Analyze(body(t, "salut", nil), ""); !r.Route {
		t.Fatalf("un seuil de 1.0 doit router la requête courte : %s", r.Explain())
	}
}

// TestSignauxFortsParDefautCoherents : l'invariant a changé avec la corroboration.
// Un signal fort n'a plus forcément un poids >= seuil : les signaux conditionnels
// doivent précisément être SOUS le seuil, sinon ils routeraient par simple cumul et
// la corroboration ne servirait à rien.
func TestSignauxFortsParDefautCoherents(t *testing.T) {
	cfg := DefaultConfig()
	d := New(cfg)
	if len(cfg.CorroborationSignals) == 0 {
		t.Fatal("une liste de corroboration par défaut est attendue")
	}
	for _, name := range defaultStrong {
		if !d.strong[name] {
			t.Fatalf("%s devrait être fort par défaut", name)
		}
		if cfg.CorroborationWeakMin < 1 {
			t.Fatalf("corroboration_weak_min=%d : au moins 1 signal faible est requis", cfg.CorroborationWeakMin)
		}
		if d.corroboration[name] {
			if defaultWeights[name] >= cfg.Threshold {
				t.Errorf("%s exige une corroboration mais pèse %v (>= seuil %v) : "+
					"il routerait par cumul sans être corroboré", name, defaultWeights[name], cfg.Threshold)
			}
			continue
		}
		if defaultWeights[name] < cfg.Threshold {
			t.Errorf("%s est déclaré fort sans corroboration mais son poids (%v) est sous le seuil",
				name, defaultWeights[name])
		}
	}
	// Chaque signal de corroboration doit exister dans la liste des forts.
	for _, name := range cfg.CorroborationSignals {
		if !d.strong[name] {
			t.Errorf("%s est dans corroboration_signals mais absent de strong_signals", name)
		}
	}
}

// --- extraction -----------------------------------------------------------------

func TestExtractionSystemeSepare(t *testing.T) {
	u, s := extractParts(mustMap(t, bodySystem(t, "bonjour", "tu es un assistant")))
	if u != "bonjour" || s != "tu es un assistant" {
		t.Fatalf("user=%q system=%q", u, s)
	}
	u2, s2 := extractParts(mustMap(t, bodyOpenAI(t, "bonjour", "tu es un assistant")))
	if u2 != "bonjour" || s2 != "tu es un assistant" {
		t.Fatalf("openai: user=%q system=%q", u2, s2)
	}
}

func TestBlocsDeContenu(t *testing.T) {
	b, _ := json.Marshal(map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": "première partie"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "http://x"}},
		map[string]any{"type": "text", "text": "puis quel modèle es-tu"},
	}}}})
	r := mustRoute(t, b)
	if !r.has(SigModelQuestion) {
		t.Fatalf("model_question attendu : %s", r.Explain())
	}
}

func mustMap(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestEstimationTokens(t *testing.T) {
	if estimateTokens("salut") >= 10 {
		t.Fatal("« salut » doit être estimé sous 10 tokens")
	}
	if estimateTokens(strings.Repeat("x", 4000)) <= 900 {
		t.Fatal("4000 caractères doivent dépasser 900 tokens")
	}
}

func TestLongestCharRun(t *testing.T) {
	if n := longestCharRun("aaabbbbbbbbbbbbcccc"); n != 12 {
		t.Fatalf("longestCharRun=%d attendu 12", n)
	}
	if n := longestCharRun(""); n != 0 {
		t.Fatalf("chaîne vide=%d attendu 0", n)
	}
}

func TestRepetitionInstructionMultilingue(t *testing.T) {
	cases := map[string]int{
		"repeat the word apple 150 times": 150,
		"répète le mot pomme 150 fois":    150,
		"повтори слово 200 раз":           200,
		"请重复 300 次":                       300,
		"tekrarla 120 kez":                120,
		"aucune consigne ici":             0,
	}
	for in, want := range cases {
		if got := repetitionInstruction(in); got != want {
			t.Errorf("repetitionInstruction(%q)=%d attendu %d", in, got, want)
		}
	}
}
