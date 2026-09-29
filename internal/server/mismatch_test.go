package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/alert"
	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/fpdetect"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// Le signal « language_mismatch » sur le chemin de réponse : question sans CJK,
// réponse en CJK. Deux formes de reroutage, parce que la sortie n'est pas disponible
// quand le détecteur de requête s'exécute :
//   - non-flux : la réponse incohérente est écartée, la requête rejouée sur l'externe ;
//   - flux : un préfixe est lu avant la première écriture, puis rejoué à l'identique.

const (
	zhAnswer    = "法国的首都是巴黎。这座城市位于塞纳河畔，是法国最大的城市，也是政治与文化的中心。"
	zhQuestion  = "法国的首都是哪里？请简要回答。"
	enQuestion  = "What is the capital of France?"
	externalTxt = "je suis le vrai modele"
)

// sseWithContent construit un flux SSE OpenAI dont le contenu est le texte donné.
func sseWithContent(parts ...string) string {
	var b strings.Builder
	for _, p := range parts {
		chunk, _ := json.Marshal(map[string]any{
			"id": "chatcmpl-up", "object": "chat.completion.chunk", "created": 1,
			"model": "deepseek-v4.1-flash",
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"content": p},
			}},
		})
		b.WriteString("data: " + string(chunk) + "\n\n")
	}
	b.WriteString("data: {\"id\":\"chatcmpl-up\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n")
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

// roleOnlyStream : un flux qui commence par une trame sans contenu, comme tout vrai
// flux (rôle puis texte).
const roleOnlyStream = "data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
	"data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n" +
	"data: [DONE]\n\n"

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

func mismatchHandler(t *testing.T, ext *fakeExternal, upstreamBody string) (*Handler, *int64) {
	t.Helper()
	var wbCalls int64
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		atomic.AddInt64(&wbCalls, 1)
		return 200, upstreamBody, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	stats := NewFPCounters(20, false, fpdetect.DefaultConfig().CorroborationSignals, 2, 100, 15*time.Minute)
	rr := &Rerouter{
		Enabled:        true,
		DryRun:         false,
		Target:         upstream.ExternalTarget{BaseURL: ext.srv.URL, Path: "/v1/chat/completions", APIKey: "cle-externe"},
		MessagesPath:   "/v1/messages",
		ClientIDHeader: "X-Client-Id",
		HealthPath:     "/v1/models",
		ModelDefault:   "claude-opus-5",
		ModelPrefixes:  []string{"claude-"},
		Upstream:       &upstream.Client{},
		Health:         upstream.NewExternalHealth(time.Minute),
		Stats:          stats,
		Alerts:         alert.New("jeton-de-test", "canal-de-test"),
	}
	h := NewHandler(Config{
		Pool: p, Upstream: up,
		FPDetect: fpdetect.New(fpdetect.DefaultConfig()),
		FPStats:  stats,
		Rerouter: rr,
	})
	return h, &wbCalls
}

func clientBody(t *testing.T, question string, stream bool) []byte {
	t.Helper()
	m := map[string]any{
		"model":      "claude-opus-5",
		"max_tokens": 64,
		"messages":   []any{map[string]any{"role": "user", "content": question}},
	}
	if stream {
		m["stream"] = true
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mismatchCounters(h *Handler) map[string]int64 {
	return h.cfg.FPStats.Snapshot()["language_mismatch"].(map[string]int64)
}

// --- non-flux --------------------------------------------------------------------

func TestMismatchNonFluxReroute(t *testing.T) {
	ext := newFakeExternal(t, nil)
	h, _ := mismatchHandler(t, ext, sseWithContent(zhAnswer))

	rec := postJSONClient(h, "/v1/chat/completions", clientBody(t, enQuestion, false), "u42")
	if rec.Code != 200 {
		t.Fatalf("code=%d corps=%s", rec.Code, rec.Body.String())
	}
	if n := ext.callCount(); n != 1 {
		t.Fatalf("l'upstream externe doit être appelé une fois, %d", n)
	}
	if !strings.Contains(rec.Body.String(), externalTxt) {
		t.Fatalf("le client doit recevoir la réponse de l'externe : %s", rec.Body.String()[:200])
	}
	if strings.Contains(rec.Body.String(), "巴黎") {
		t.Fatalf("la réponse incohérente ne doit pas atteindre le client : %s", rec.Body.String()[:200])
	}
	lm := mismatchCounters(h)
	if lm["detected"] != 1 || lm["rerouted"] != 1 || lm["kept"] != 0 {
		t.Fatalf("compteurs : %v", lm)
	}
	if by := h.cfg.FPStats.Snapshot()["by_signal"].(map[string]int64); by[fpdetect.SigLanguageMismatch] != 1 {
		t.Fatalf("le signal doit apparaître dans la ventilation : %v", by)
	}
}

func TestMismatchUtilisateurChinoisNonReroute(t *testing.T) {
	ext := newFakeExternal(t, nil)
	h, wbCalls := mismatchHandler(t, ext, sseWithContent(zhAnswer))

	rec := postJSONClient(h, "/v1/chat/completions", clientBody(t, zhQuestion, false), "u42")
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	if n := ext.callCount(); n != 0 {
		t.Fatalf("aucun reroutage attendu pour un utilisateur sinophone (%d)", n)
	}
	if n := atomic.LoadInt64(wbCalls); n != 1 {
		t.Fatalf("la route normale doit servir (%d)", n)
	}
	if !strings.Contains(rec.Body.String(), "巴黎") {
		t.Fatalf("la réponse d'origine doit être servie : %s", rec.Body.String()[:200])
	}
	if lm := mismatchCounters(h); lm["detected"] != 0 {
		t.Fatalf("aucun signal attendu : %v", lm)
	}
}

func TestMismatchFailOpenConserveLaReponse(t *testing.T) {
	ext := newFakeExternal(t, func(w http.ResponseWriter, r *http.Request, body string) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	h, _ := mismatchHandler(t, ext, sseWithContent(zhAnswer))

	rec := postJSONClient(h, "/v1/chat/completions", clientBody(t, enQuestion, false), "u42")
	if rec.Code != 200 {
		t.Fatalf("le client doit être servi malgré la panne : code=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "巴黎") {
		t.Fatalf("la réponse d'origine doit être servie : %s", rec.Body.String()[:200])
	}
	lm := mismatchCounters(h)
	if lm["detected"] != 1 || lm["rerouted"] != 0 || lm["kept"] != 1 {
		t.Fatalf("compteurs : %v", lm)
	}
}

// --- flux ------------------------------------------------------------------------

func TestMismatchFluxReroute(t *testing.T) {
	ext := newFakeExternal(t, func(w http.ResponseWriter, r *http.Request, body string) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sseExternal))
	})
	h, _ := mismatchHandler(t, ext, sseWithContent(zhAnswer))

	rec := postJSONClient(h, "/v1/chat/completions", clientBody(t, enQuestion, true), "u42")
	body := rec.Body.String()
	if n := ext.callCount(); n != 1 {
		t.Fatalf("l'upstream externe doit être appelé (%d)", n)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("le flux externe doit être relayé : %s", body[:200])
	}
	if strings.Contains(body, "巴黎") {
		t.Fatalf("les trames chinoises ne doivent pas atteindre le client : %s", body[:200])
	}
	if lm := mismatchCounters(h); lm["rerouted"] != 1 {
		t.Fatalf("compteurs : %v", lm)
	}
}

// TestMismatchFluxPrefixeRejoueAlIdentique : le test critique du chemin flux. Le
// préfixe consommé pour décider doit être rejoué sans perte ni doublon — une erreur
// corromprait toutes les réponses en flux.
func TestMismatchFluxPrefixeRejoueAlIdentique(t *testing.T) {
	ext := newFakeExternal(t, nil)
	h, _ := mismatchHandler(t, ext, sseWithContent("The capital of France is Paris."))

	rec := postJSONClient(h, "/v1/chat/completions", clientBody(t, enQuestion, true), "u42")
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	body := rec.Body.String()
	if n := ext.callCount(); n != 0 {
		t.Fatalf("aucun reroutage attendu (%d)", n)
	}
	if c := strings.Count(body, "The capital of France is Paris."); c != 1 {
		t.Fatalf("contenu attendu exactement une fois, trouvé %d :\n%s", c, body)
	}
	if c := strings.Count(body, `"role":"assistant"`); c > 1 {
		t.Fatalf("trame de rôle dupliquée (%d fois) :\n%s", c, body)
	}
	for _, want := range []string{`"object":"chat.completion.chunk"`, `"finish_reason":"stop"`, "data: [DONE]"} {
		if !strings.Contains(body, want) {
			t.Fatalf("le flux relayé a perdu %q :\n%s", want, body)
		}
	}
}

func TestMismatchFluxQuestionChinoiseNonDecalee(t *testing.T) {
	ext := newFakeExternal(t, nil)
	h, _ := mismatchHandler(t, ext, sseWithContent(zhAnswer))

	rec := postJSONClient(h, "/v1/chat/completions", clientBody(t, zhQuestion, true), "u42")
	body := rec.Body.String()
	if n := ext.callCount(); n != 0 {
		t.Fatalf("aucun reroutage attendu (%d)", n)
	}
	if !strings.Contains(body, "巴黎") {
		t.Fatalf("la réponse chinoise d'origine doit être relayée : %s", body[:200])
	}
	if lm := mismatchCounters(h); lm["detected"] != 0 {
		t.Fatalf("aucun signal attendu : %v", lm)
	}
}

// TestFluxSansSignalResteIntact : reroutage désactivé → aucun préfixe lu, flux relayé
// tel quel, aucun appel externe. Garantie de non-régression pour les déploiements qui
// n'utilisent pas cette fonction.
func TestFluxSansSignalResteIntact(t *testing.T) {
	ext := newFakeExternal(t, nil)
	h, _ := mismatchHandler(t, ext, sseWithContent(zhAnswer))
	h.cfg.Rerouter.Enabled = false

	rec := postJSONClient(h, "/v1/chat/completions", clientBody(t, enQuestion, true), "u42")
	body := rec.Body.String()
	if n := ext.callCount(); n != 0 {
		t.Fatalf("reroutage désactivé : aucun appel externe (%d)", n)
	}
	if !strings.Contains(body, "巴黎") || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("le flux doit être relayé intégralement : %s", body[:200])
	}
}

func TestSniffStreamPrefixNePerdRien(t *testing.T) {
	src := roleOnlyStream
	prefix, rest := sniffStreamPrefix(strings.NewReader(src))
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(readerFunc(func(p []byte) (int, error) { return rest.Read(p) })); err != nil {
		t.Fatal(err)
	}
	if string(prefix)+buf.String() != src {
		t.Fatalf("préfixe + reste doit reformer la source\npréfixe=%q\nreste=%q", prefix, buf.String())
	}
	if !bytes.Contains(prefix, []byte(`"role":"assistant"`)) {
		t.Fatalf("le préfixe doit contenir la première trame : %q", prefix)
	}
	if !bytes.Contains(prefix, []byte(`"content":"hello"`)) {
		t.Fatalf("le préfixe doit s'arrêter APRÈS la première trame de contenu : %q", prefix)
	}
}

func TestCompletionText(t *testing.T) {
	var openai map[string]any
	_ = json.Unmarshal([]byte(`{"choices":[{"message":{"role":"assistant","content":"bonjour"}}]}`), &openai)
	if got := completionText(openai); got != "bonjour" {
		t.Fatalf("completionText OpenAI = %q", got)
	}
	var anthropic map[string]any
	_ = json.Unmarshal([]byte(`{"content":[{"type":"text","text":"你好"},{"type":"text","text":"世界"}]}`), &anthropic)
	if got := completionText(anthropic); got != "你好世界" {
		t.Fatalf("completionText Messages = %q", got)
	}
	if got := completionText(map[string]any{}); got != "" {
		t.Fatalf("réponse vide = %q", got)
	}
}

// TestMismatchDemandeLaLangueDeLUtilisateur : le prompt système de la passerelle est en
// chinois, donc le modèle de remplacement répond en chinois à son tour si on ne lui dit
// rien. La requête rejouée doit porter une consigne de langue — et SEULEMENT celle-là :
// le trafic normal ne doit pas la recevoir.
func TestMismatchDemandeLaLangueDeLUtilisateur(t *testing.T) {
	ext := newFakeExternal(t, nil)
	h, _ := mismatchHandler(t, ext, sseWithContent(zhAnswer))

	postJSONClient(h, "/v1/chat/completions", clientBody(t, enQuestion, false), "u42")
	sent := ext.last.Load().(string)
	if !strings.Contains(sent, "same language as the user's last message") {
		t.Fatalf("la requête rejouée doit porter la consigne de langue : %s", sent[:400])
	}
	// Le corps d'origine du client n'est pas modifié (le test vérifie indirectement que
	// la consigne a été ajoutée par lesystem prompt, pas en écrasant la question).
	if !strings.Contains(sent, enQuestion) {
		t.Fatalf("la question du client doit être conservée : %s", sent[:400])
	}
}

// TestReroutageSondeNormaleSansConsigneDeLangue : le reroutage d'une sonde détectée sur
// la REQUÊTE (chemin normal) ne doit pas recevoir la consigne — elle est réservée au
// désaccord de langue.
func TestReroutageSondeNormaleSansConsigneDeLangue(t *testing.T) {
	ext := newFakeExternal(t, nil)
	rr, _ := newRerouter(t, ext, false, 20, 100, false)
	h, _ := probeHTTP(t, rr)

	postJSONClient(h, "/v1/chat/completions", mustJSON(t, map[string]any{
		"model":    "claude-opus-5",
		"messages": []any{map[string]any{"role": "user", "content": "What model are you?"}},
	}), "u42")
	sent := ext.last.Load().(string)
	if strings.Contains(sent, "same language as the user's last message") {
		t.Fatalf("la consigne ne doit pas s'appliquer au reroutage des sondes de requête : %s", sent[:300])
	}
}

// --- alerte « fuite de langue » --------------------------------------------------

// TestAlerteFuiteDeLangue : l'alerte se déclenche dès qu'une réponse incohérente a été
// servie au client dans la fenêtre, et se répète au plus une fois par fenêtre.
func TestAlerteFuiteDeLangue(t *testing.T) {
	ext := newFakeExternal(t, nil)
	h, _ := mismatchHandler(t, ext, sseWithContent(zhAnswer))
	rr := h.cfg.Rerouter

	// Sans aucune fuite : silence.
	if rr.EvaluateLanguageLeak(time.Now()) {
		t.Fatal("aucune alerte attendue sans fuite")
	}

	// Une réponse incohérente servie (l'upstream externe est en panne) → une fuite.
	extDown := newFakeExternal(t, func(w http.ResponseWriter, r *http.Request, body string) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	h2, _ := mismatchHandler(t, extDown, sseWithContent(zhAnswer))
	rr2 := h2.cfg.Rerouter
	postJSONClient(h2, "/v1/chat/completions", clientBody(t, enQuestion, false), "u42")
	if lm := mismatchCounters(h2); lm["kept"] != 1 {
		t.Fatalf("une fuite attendue : %v", lm)
	}
	if !rr2.EvaluateLanguageLeak(time.Now()) {
		t.Fatal("l'alerte doit partir dès qu'une fuite existe dans la fenêtre")
	}
	// Deuxième évaluation immédiate : supprimée par la fenêtre de déduplication.
	if rr2.EvaluateLanguageLeak(time.Now()) {
		t.Fatal("l'alerte ne doit pas être répétée dans la même fenêtre")
	}
	if s := rr2.Alerts.Stats(); s.Suppressed == 0 {
		t.Fatalf("la répétition doit être comptée comme supprimée : %+v", s)
	}
}

// TestFuiteDeLangueRefuseeCompte : un reroutage REFUSÉ (plafond atteint) est aussi une
// fuite subie par le client — sinon l'alerte raterait les cas les plus probables
// (plafond, fournisseur malsain).
func TestFuiteDeLangueRefuseeCompte(t *testing.T) {
	ext := newFakeExternal(t, nil)
	h, _ := mismatchHandler(t, ext, sseWithContent(zhAnswer))
	rr := h.cfg.Rerouter
	// Plafond global à 0 : aucun reroutage autorisé.
	rr.Stats = NewFPCounters(20, false, nil, 2, 1, 15*time.Minute)
	// Une unité déjà consommée par un autre client sature le plafond global.
	rr.Stats.AllowReroute("autre", time.Now())

	rec := postJSONClient(h, "/v1/chat/completions", clientBody(t, enQuestion, false), "u42")
	if !strings.Contains(rec.Body.String(), "巴黎") {
		t.Fatalf("la réponse incohérente doit être servie (reroutage refusé) : %s", rec.Body.String()[:200])
	}
	lm := mismatchCounters(h)
	if lm["kept"] != 1 || lm["rerouted"] != 0 {
		t.Fatalf("un refus de plafond doit compter comme fuite subie : %v", lm)
	}
	if n := ext.callCount(); n != 0 {
		t.Fatalf("aucun appel externe attendu (plafond) : %d", n)
	}
}

// TestFenetreFuiteGlissante : la fenêtre est glissante. Une fuite ancienne ne doit plus
// compter (sinon l'alerte se déclencherait indéfiniment après un incident terminé), et
// ce que /status expose doit être exactement ce que l'alerte évalue.
func TestFenetreFuiteGlissante(t *testing.T) {
	stats := NewFPCounters(20, false, nil, 2, 100, 15*time.Minute)
	now := time.Now()
	if stats.MismatchKeptInWindow(now) != 0 {
		t.Fatal("aucune fuite au départ")
	}
	// Une fuite il y a 20 minutes : hors fenêtre de 15.
	stats.NoteMismatchKept(now.Add(-20 * time.Minute))
	if n := stats.MismatchKeptInWindow(now); n != 0 {
		t.Fatalf("une fuite de 20 min ne doit pas compter dans 15 min : %d", n)
	}
	// Une fuite récente : dans la fenêtre.
	stats.NoteMismatchKept(now.Add(-5 * time.Minute))
	if n := stats.MismatchKeptInWindow(now); n != 1 {
		t.Fatalf("une fuite de 5 min doit compter : %d", n)
	}
	// /status doit exposer la même fenêtre et le même compte.
	if w := stats.LanguageLeakWindow(); w != 15*time.Minute {
		t.Fatalf("fenêtre exposée : %v", w)
	}
	snap := stats.Snapshot()["language_leak"].(map[string]any)
	if snap["window_seconds"] != 900 || snap["kept_in_window"] != 1 {
		t.Fatalf("bloc language_leak : %v", snap)
	}
}

// TestFuiteDeLangueBornageMemoire : une fuite prolongée ne doit pas faire croître la
// mémoire du compteur sans limite.
func TestFuiteDeLangueBornageMemoire(t *testing.T) {
	stats := NewFPCounters(20, false, nil, 2, 100, 15*time.Minute)
	now := time.Now()
	for i := 0; i < fpKeptMaxEntries+200; i++ {
		stats.NoteMismatchKept(now)
	}
	stats.mu.Lock()
	n := len(stats.mismatchKeptAt)
	stats.mu.Unlock()
	if n > fpKeptMaxEntries {
		t.Fatalf("horodatages non bornés : %d", n)
	}
	if got := stats.MismatchKeptInWindow(now); got > fpKeptMaxEntries {
		t.Fatalf("compte fenêtré incohérent : %d", got)
	}
}
