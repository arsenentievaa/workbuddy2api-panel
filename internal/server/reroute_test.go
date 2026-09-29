package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/fpdetect"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// Tests du reroutage des sondes vers un upstream externe (phase 2).
//
// Le contrat à prouver est double : quand tout va bien la sonde est servie par
// l'extérieur SANS toucher au pool de comptes, et dès qu'un maillon manque (santé,
// plafond, dry_run, transport, réponse illisible) la requête repart sur la route
// normale sans que rien n'ait été consommé.

// fakeExternal : faux fournisseur externe. Il compte ses appels et son dernier corps
// reçu, ce qui permet d'affirmer « aucun appel » ou « ce corps précis ».
type fakeExternal struct {
	srv   *httptest.Server
	posts int64
	last  atomic.Value // string
	// probes compte les GET de la sonde de santé : ce ne sont PAS des reroutages, et
	// les confondre rendrait tout comptage ininterprétable.
	probes  int64
	handler func(w http.ResponseWriter, r *http.Request, body string)
}

func newFakeExternal(t *testing.T, h func(w http.ResponseWriter, r *http.Request, body string)) *fakeExternal {
	t.Helper()
	f := &fakeExternal{handler: h}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			atomic.AddInt64(&f.probes, 1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[]}`))
			return
		}
		atomic.AddInt64(&f.posts, 1)
		b, _ := io.ReadAll(r.Body)
		f.last.Store(string(b))
		if f.handler != nil {
			f.handler(w, r, string(b))
			return
		}
		// Le faux fournisseur répond la forme que le client a demandée : JSON en
		// non-flux, SSE en flux. Un faux qui répondrait toujours la même forme
		// laisserait passer un bug de sélection de chemin (c'est exactement ce qui
		// s'était produit : la réponse JSON était analysée comme un flux SSE).
		if upstream.IsStreamingBody(b) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(sseExternal))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_upstream","object":"chat.completion","model":"claude-opus-5","choices":[{"index":0,"message":{"role":"assistant","content":"je suis le vrai modele"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7,"prompt_cache_hit_tokens":2,"credit":0.5}}`))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// callCount : nombre de REROUTAGES tentés (POST uniquement).
func (f *fakeExternal) callCount() int64 { return atomic.LoadInt64(&f.posts) }

// sseExternal : flux SSE minimal, forme OpenAI.
const sseExternal = "data: {\"id\":\"msg_x\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"claude-opus-5\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"vrai\"}}]}\n\n" +
	"data: {\"id\":\"msg_x\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"claude-opus-5\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2,\"total_tokens\":3}}\n\n" +
	"data: [DONE]\n\n"

func newRerouter(t *testing.T, ext *fakeExternal, dryRun bool, capPerHour, globalCap int, notifier bool) (*Rerouter, *FPCounters) {
	t.Helper()
	stats := NewFPCounters(capPerHour, dryRun, fpdetect.DefaultConfig().CorroborationSignals, 2, globalCap)
	rr := &Rerouter{
		Enabled:          true,
		DryRun:           dryRun,
		Target:           upstream.ExternalTarget{BaseURL: ext.srv.URL, Path: "/v1/chat/completions", APIKey: "cle-externe", Timeout: 5 * time.Second},
		MessagesPath:     "/v1/messages",
		HealthPath:       "/v1/models",
		ModelDefault:     "claude-opus-5",
		ModelPrefixes:    []string{"claude-"},
		MaxTokensCeiling: 8192,
		RateAlertPercent: 5,
		RateMinSample:    50,
		Upstream:         &upstream.Client{},
		Health:           upstream.NewExternalHealth(time.Minute),
		Stats:            stats,
	}
	_ = notifier
	return rr, stats
}

// probeHTTP : handler avec détecteur actif + reroutage, sur un pool d'un compte dont
// l'upstream WorkBuddy est un faux qui compte ses appels.
func probeHTTP(t *testing.T, rr *Rerouter) (*Handler, *int64) {
	t.Helper()
	var wbCalls int64
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		atomic.AddInt64(&wbCalls, 1)
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{
		Pool: p, Upstream: up,
		FPDetect: fpdetect.New(fpdetect.DefaultConfig()),
		FPStats:  rr.Stats,
		Rerouter: rr,
	})
	return h, &wbCalls
}

func postJSON(h *Handler, path string, body []byte) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-data")
	h.ServeHTTP(rec, req)
	return rec
}

// TestRerouteNonFlux : une sonde est servie par l'upstream externe, sans qu'aucun
// compte ni l'upstream WorkBuddy ne soit sollicité.
func TestRerouteNonFlux(t *testing.T) {
	ext := newFakeExternal(t, nil)
	rr, stats := newRerouter(t, ext, false, 20, 100, false)
	h, wbCalls := probeHTTP(t, rr)

	rec := postJSON(h, "/v1/chat/completions", mustJSON(t, map[string]any{
		"model":      "claude-opus-5",
		"max_tokens": 16,
		"messages":   []any{map[string]any{"role": "user", "content": "What model are you?"}},
	}))
	if rec.Code != 200 {
		t.Fatalf("code=%d corps=%s", rec.Code, rec.Body.String())
	}
	if ext.callCount() != 1 {
		t.Fatalf("un appel externe attendu, %d observés", ext.callCount())
	}
	if n := atomic.LoadInt64(wbCalls); n != 0 {
		t.Fatalf("l'upstream WorkBuddy ne doit pas être appelé (%d appels)", n)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["model"] != "claude-opus-5" {
		t.Fatalf("le modèle du client doit être réaffiché: %v", out["model"])
	}
	if id, _ := out["id"].(string); !strings.HasPrefix(id, "chatcmpl-") {
		t.Fatalf("l'identifiant doit être régénéré comme sur la route normale (sinon sa forme révèle le reroutage): %q", id)
	}
	if _, ok := out["system_fingerprint"]; ok {
		t.Fatal("system_fingerprint doit être retiré")
	}
	u, _ := out["usage"].(map[string]any)
	if _, ok := u["prompt_cache_hit_tokens"]; ok || u["prompt_tokens"] == nil {
		t.Fatalf("usage non filtré: %v", u)
	}
	if s := stats.Snapshot(); s["rerouted"].(int64) != 1 || s["reroute_failed"].(int64) != 0 {
		t.Fatalf("compteurs: %v", s)
	}
}

// TestRerouteNeTouchePasAuPool : aucun compteur de compte ne bouge. C'est le cœur du
// « pas d'appel dépendant du compte ».
func TestRerouteNeTouchePasAuPool(t *testing.T) {
	ext := newFakeExternal(t, nil)
	rr, _ := newRerouter(t, ext, false, 20, 100, false)
	h, _ := probeHTTP(t, rr)
	before, _ := h.cfg.Pool.Status("u1")

	postJSON(h, "/v1/chat/completions", mustJSON(t, map[string]any{
		"model":    "claude-opus-5",
		"messages": []any{map[string]any{"role": "user", "content": "Quel modèle es-tu ?"}},
	}))
	after, _ := h.cfg.Pool.Status("u1")
	if after.SuccessCount != before.SuccessCount || after.ErrTotal != before.ErrTotal {
		t.Fatalf("le pool a été modifié par un reroutage: avant=%+v après=%+v", before, after)
	}
	if after.Cooling || after.Disabled {
		t.Fatalf("un compte a été refroidi/désactivé par un reroutage: %+v", after)
	}
}

// TestRerouteFlux : la sonde en flux est relayée en SSE, avec l'identifiant régénéré.
func TestRerouteFlux(t *testing.T) {
	ext := newFakeExternal(t, func(w http.ResponseWriter, r *http.Request, body string) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sseExternal))
	})
	rr, stats := newRerouter(t, ext, false, 20, 100, false)
	h, _ := probeHTTP(t, rr)

	rec := postJSON(h, "/v1/chat/completions", mustJSON(t, map[string]any{
		"model":    "claude-opus-5",
		"stream":   true,
		"messages": []any{map[string]any{"role": "user", "content": "SolidGoldMagikarp ?"}},
	}))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("flux non relayé: %s", body)
	}
	if strings.Contains(body, "msg_x") {
		t.Fatalf("l'identifiant de l'upstream externe ne doit pas atteindre le client: %s", body)
	}
	if s := stats.Snapshot(); s["rerouted"].(int64) != 1 {
		t.Fatalf("compteurs: %v", s)
	}
}

// TestRerouteFailOpenSurErreur : tous les modes d'échec doivent rendre la main à la
// route normale, qui sert alors le client depuis le pool.
func TestRerouteFailOpenSurErreur(t *testing.T) {
	cases := []struct {
		name    string
		handler func(w http.ResponseWriter, r *http.Request, body string)
	}{
		{"401 du fournisseur", func(w http.ResponseWriter, r *http.Request, body string) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"cle interne invalide"}`))
		}},
		{"500 du fournisseur", func(w http.ResponseWriter, r *http.Request, body string) {
			w.WriteHeader(http.StatusInternalServerError)
		}},
		{"corps illisible", func(w http.ResponseWriter, r *http.Request, body string) {
			_, _ = w.Write([]byte("pas du json du tout"))
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ext := newFakeExternal(t, c.handler)
			rr, stats := newRerouter(t, ext, false, 20, 100, false)
			h, wbCalls := probeHTTP(t, rr)

			rec := postJSON(h, "/v1/chat/completions", mustJSON(t, map[string]any{
				"model":    "claude-opus-5",
				"messages": []any{map[string]any{"role": "user", "content": "What model are you?"}},
			}))
			if rec.Code != 200 {
				t.Fatalf("le repli doit servir le client: code=%d", rec.Code)
			}
			if n := atomic.LoadInt64(wbCalls); n != 1 {
				t.Fatalf("le repli doit passer par WorkBuddy (appels=%d)", n)
			}
			if s := stats.Snapshot(); s["reroute_failed"].(int64) != 1 || s["rerouted"].(int64) != 0 {
				t.Fatalf("l'échec doit être compté: %v", s)
			}
			// Le corps d'erreur du fournisseur ne doit jamais apparaître côté client.
			if strings.Contains(rec.Body.String(), "interne invalide") {
				t.Fatalf("fuite du corps d'erreur externe: %s", rec.Body.String())
			}
		})
	}
}

// TestRerouteSanteMalsaine : sonde malsaine => aucune tentative externe, route normale.
func TestRerouteSanteMalsaine(t *testing.T) {
	ext := newFakeExternal(t, nil)
	rr, stats := newRerouter(t, ext, false, 20, 100, false)
	// La sonde de santé est branchée sur un chemin qui répond 500.
	rr.Target.Path = "/v1/chat/completions"
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	bad := httptest.NewServer(mux)
	defer bad.Close()
	rr.Health = upstream.NewExternalHealth(time.Minute)
	rr.Target = upstream.ExternalTarget{BaseURL: bad.URL, Path: "/v1/chat/completions", APIKey: "k", Timeout: 3 * time.Second}

	h, wbCalls := probeHTTP(t, rr)
	rec := postJSON(h, "/v1/chat/completions", mustJSON(t, map[string]any{
		"model":    "claude-opus-5",
		"stream":   true,
		"messages": []any{map[string]any{"role": "user", "content": "What model are you?"}},
	}))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	if n := atomic.LoadInt64(wbCalls); n != 1 {
		t.Fatalf("route normale attendue (appels WorkBuddy=%d)", n)
	}
	if s := stats.Snapshot(); s["health_blocked"].(int64) == 0 && s["rerouted"].(int64) != 0 {
		t.Fatalf("un reroutage a eu lieu malgré une sonde malsaine: %v", s)
	}
}

// TestRerouteDryRun : dry_run interdit tout envoi réel.
func TestRerouteDryRun(t *testing.T) {
	ext := newFakeExternal(t, nil)
	rr, stats := newRerouter(t, ext, true, 20, 100, false)
	h, wbCalls := probeHTTP(t, rr)

	rec := postJSON(h, "/v1/chat/completions", mustJSON(t, map[string]any{
		"model":    "claude-opus-5",
		"stream":   true,
		"messages": []any{map[string]any{"role": "user", "content": "What model are you?"}},
	}))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	if n := ext.callCount(); n != 0 {
		t.Fatalf("dry_run : aucun reroutage ne doit partir (%d)", n)
	}
	if n := atomic.LoadInt64(&ext.probes); n != 0 {
		t.Fatalf("dry_run : le fournisseur ne doit même pas être sondé (%d)", n)
	}
	if n := atomic.LoadInt64(wbCalls); n != 1 {
		t.Fatalf("la route normale doit servir (%d appels)", n)
	}
	if s := stats.Snapshot(); s["rerouted"].(int64) != 0 {
		t.Fatalf("dry_run ne doit pas compter de reroutage réussi: %v", s)
	}
}

// TestReroutePlafond : au-delà du quota, la sonde repart sur la route normale.
func TestReroutePlafond(t *testing.T) {
	ext := newFakeExternal(t, nil)
	rr, stats := newRerouter(t, ext, false, 2, 100, false)
	h, wbCalls := probeHTTP(t, rr)

	body := mustJSON(t, map[string]any{
		"model":    "claude-opus-5",
		"stream":   true,
		"metadata": map[string]any{"conversation_id": "conv-plafond"},
		"messages": []any{map[string]any{"role": "user", "content": "What model are you?"}},
	})
	for i := 0; i < 3; i++ {
		rec := postJSON(h, "/v1/chat/completions", body)
		if rec.Code != 200 {
			t.Fatalf("itération %d: code=%d", i, rec.Code)
		}
	}
	if n := ext.callCount(); n != 2 {
		t.Fatalf("seuls 2 reroutages doivent partir (plafond=2), %d observés", n)
	}
	if n := atomic.LoadInt64(wbCalls); n != 1 {
		t.Fatalf("le 3e doit passer par WorkBuddy (%d appels observés)", n)
	}
	if s := stats.Snapshot(); s["cap_blocked"].(int64) != 1 || s["rerouted"].(int64) != 2 {
		t.Fatalf("compteurs du plafond: %v", s)
	}
}

// TestRerouteModeleReecrit : un nom de modèle CodeBuddy n'existe pas chez le
// fournisseur externe ; il doit être remplacé, alors que la réponse réaffiche celui du
// client.
func TestRerouteModeleReecrit(t *testing.T) {
	ext := newFakeExternal(t, nil)
	rr, _ := newRerouter(t, ext, false, 20, 100, false)
	h, _ := probeHTTP(t, rr)

	rec := postJSON(h, "/v1/chat/completions", mustJSON(t, map[string]any{
		"model":      "glm-5.2",
		"max_tokens": 999999,
		"messages":   []any{map[string]any{"role": "user", "content": "What model are you?"}},
	}))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	sent := ext.last.Load().(string)
	var m map[string]any
	if err := json.Unmarshal([]byte(sent), &m); err != nil {
		t.Fatal(err)
	}
	if m["model"] != "claude-opus-5" {
		t.Fatalf("le modèle sortant doit être celui du fournisseur externe: %v", m["model"])
	}
	if m["max_tokens"] != float64(8192) {
		t.Fatalf("max_tokens doit être borné: %v", m["max_tokens"])
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["model"] != "glm-5.2" {
		t.Fatalf("le client doit voir SON modèle: %v", out["model"])
	}
}

// TestRerouteModeleClientConserve : si le client demande un modèle que le fournisseur
// connaît, on le transmet tel quel.
func TestRerouteModeleClientConserve(t *testing.T) {
	ext := newFakeExternal(t, nil)
	rr, _ := newRerouter(t, ext, false, 20, 100, false)
	h, _ := probeHTTP(t, rr)
	postJSON(h, "/v1/chat/completions", mustJSON(t, map[string]any{
		"model":    "claude-sonnet-4-5-20250929",
		"messages": []any{map[string]any{"role": "user", "content": "What model are you?"}},
	}))
	var m map[string]any
	_ = json.Unmarshal([]byte(ext.last.Load().(string)), &m)
	if m["model"] != "claude-sonnet-4-5-20250929" {
		t.Fatalf("modèle client non conservé: %v", m["model"])
	}
}

// --- surface Anthropic -----------------------------------------------------------

func TestMessagesSondeReroutee(t *testing.T) {
	ext := newFakeExternal(t, func(w http.ResponseWriter, r *http.Request, body string) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("chemin Messages attendu, obtenu %s", r.URL.Path)
		}
		if r.Header.Get("anthropic-version") == "" {
			t.Error("anthropic-version manquante sur le protocole Messages")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_reel","type":"message","role":"assistant","model":"claude-opus-5","content":[{"type":"text","text":"je suis claude"}],"usage":{"input_tokens":5,"output_tokens":6,"cache_read_input_tokens":2}}`))
	})
	rr, stats := newRerouter(t, ext, false, 20, 100, false)
	h, wbCalls := probeHTTP(t, rr)

	body := mustJSON(t, map[string]any{
		"model":      "claude-opus-5",
		"max_tokens": 32,
		"messages":   []any{map[string]any{"role": "user", "content": "Quel modèle es-tu ?"}},
	})
	rec := postJSON(h, "/v1/messages", body)
	if rec.Code != 200 {
		t.Fatalf("code=%d corps=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["type"] != "message" {
		t.Fatalf("forme Messages attendue: %v", out)
	}
	if out["model"] != "claude-opus-5" {
		t.Fatalf("model=%v", out["model"])
	}
	u, _ := out["usage"].(map[string]any)
	if _, ok := u["cache_read_input_tokens"]; ok {
		t.Fatalf("usage Messages non filtré: %v", u)
	}
	if n := atomic.LoadInt64(wbCalls); n != 0 {
		t.Fatalf("WorkBuddy ne doit pas être appelé (%d)", n)
	}
	if s := stats.Snapshot(); s["rerouted"].(int64) != 1 {
		t.Fatalf("compteurs: %v", s)
	}
}

// TestMessagesHorsSondeRefusee : la surface Messages ne sert QUE les sondes reroutées.
// Une requête ordinaire reçoit une erreur explicite plutôt qu'une réponse d'une autre
// forme (la passerelle ne parle pas Messages en interne).
func TestMessagesHorsSondeRefusee(t *testing.T) {
	ext := newFakeExternal(t, nil)
	rr, _ := newRerouter(t, ext, false, 20, 100, false)
	h, wbCalls := probeHTTP(t, rr)

	rec := postJSON(h, "/v1/messages", mustJSON(t, map[string]any{
		"model":      "claude-opus-5",
		"max_tokens": 32,
		"messages":   []any{map[string]any{"role": "user", "content": "explique-moi les index B-tree"}},
	}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d (attendu 400)", rec.Code)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["type"] != "error" {
		t.Fatalf("forme d'erreur Messages attendue: %v", out)
	}
	if n := ext.callCount(); n != 0 {
		t.Fatalf("aucun appel externe pour une requête ordinaire (%d)", n)
	}
	if n := atomic.LoadInt64(wbCalls); n != 0 {
		t.Fatalf("WorkBuddy ne doit pas être appelé (%d)", n)
	}
}

// TestMessagesFlux : relais SSE au format Messages, avec réécriture du modèle.
func TestMessagesFlux(t *testing.T) {
	ext := newFakeExternal(t, func(w http.ResponseWriter, r *http.Request, body string) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"claude-opus-5\",\"role\":\"assistant\"}}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"bonjour\"}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	})
	rr, stats := newRerouter(t, ext, false, 20, 100, false)
	h, _ := probeHTTP(t, rr)

	rec := postJSON(h, "/v1/messages", mustJSON(t, map[string]any{
		"model":      "claude-opus-5",
		"max_tokens": 16,
		"stream":     true,
		"messages":   []any{map[string]any{"role": "user", "content": "Quel modèle es-tu ?"}},
	}))
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, "event: message_stop") {
		t.Fatalf("flux Messages non relayé: code=%d corps=%s", rec.Code, body)
	}
	if s := stats.Snapshot(); s["rerouted"].(int64) != 1 {
		t.Fatalf("compteurs: %v", s)
	}
}

func TestRewriteSSEModel(t *testing.T) {
	in := `data: {"type":"message_start","message":{"id":"msg_1","model":"claude-opus-5","role":"assistant"}}`
	got := rewriteSSEModel(in, "glm-5.2")
	if !strings.Contains(got, `"model":"glm-5.2"`) {
		t.Fatalf("modèle non réécrit: %s", got)
	}
	if !strings.Contains(got, `"id":"msg_1"`) {
		t.Fatalf("le reste de la ligne doit survivre: %s", got)
	}
	// Ligne sans champ model : inchangée.
	plain := "event: message_stop"
	if rewriteSSEModel(plain, "x") != plain {
		t.Fatal("une ligne sans model doit être inchangée")
	}
	// Ligne malformée : inchangée plutôt que corrompue.
	bad := `data: {"model":"sans fin`
	if rewriteSSEModel(bad, "x") != bad {
		t.Fatalf("ligne malformée altérée: %s", rewriteSSEModel(bad, "x"))
	}
}

// --- identité du plafond ---------------------------------------------------------

func TestRerouteIdentity(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	if got := rerouteIdentity(req, []byte(`{}`)); got != "" {
		t.Fatalf("sans clé ni autorisation, aucune identité attendue: %q", got)
	}
	req.Header.Set("Authorization", "Bearer sk-data-plane")
	got := rerouteIdentity(req, []byte(`{}`))
	if !strings.HasPrefix(got, "auth:") || strings.Contains(got, "sk-data-plane") {
		t.Fatalf("identité attendue sous forme d'empreinte: %q", got)
	}
	// La clé de conversation prime : c'est la seule unité d'isolation réellement
	// distinguable derrière une clé de données unique.
	withConv := []byte(`{"metadata":{"conversation_id":"abc"}}`)
	if got := rerouteIdentity(req, withConv); got != "conv:abc" {
		t.Fatalf("la conversation doit primer: %q", got)
	}
}

func TestSanitizeCompletionEtAnthropic(t *testing.T) {
	resp := map[string]any{
		"id": "msg_upstream", "model": "claude-opus-5", "system_fingerprint": "fp_1",
		"service_tier": "default", "usage": map[string]any{"prompt_tokens": 1, "credit": 2.0},
	}
	SanitizeCompletion(resp, "glm-5.2")
	if resp["model"] != "glm-5.2" {
		t.Fatal("modèle non remplacé")
	}
	if _, ok := resp["system_fingerprint"]; ok {
		t.Fatal("system_fingerprint non retiré")
	}
	if _, ok := resp["usage"].(map[string]any)["credit"]; ok {
		t.Fatal("credit non filtré")
	}
	if !strings.HasPrefix(resp["id"].(string), "chatcmpl-") {
		t.Fatal("id non régénéré")
	}

	a := map[string]any{"model": "claude-opus-5", "usage": map[string]any{"input_tokens": 1, "cache_read_input_tokens": 9}}
	sanitizeAnthropic(a, "glm-5.2")
	if a["model"] != "glm-5.2" {
		t.Fatal("modèle Messages non remplacé")
	}
	u := a["usage"].(map[string]any)
	if _, ok := u["cache_read_input_tokens"]; ok {
		t.Fatal("champ d'usage Messages non filtré")
	}
	if u["input_tokens"] != 1 {
		t.Fatal("les compteurs de la spécification doivent survivre")
	}
}

func TestClampMaxTokens(t *testing.T) {
	out := clampMaxTokens([]byte(`{"max_tokens":100000,"model":"x"}`), 8192)
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	if m["max_tokens"] != float64(8192) {
		t.Fatalf("max_tokens non borné: %v", m["max_tokens"])
	}
	// Absent : on ne l'invente pas.
	same := `{"model":"x"}`
	if string(clampMaxTokens([]byte(same), 8192)) != same {
		t.Fatal("max_tokens absent ne doit pas être ajouté")
	}
	// Sous la borne : inchangé.
	low := `{"max_tokens":10}`
	if string(clampMaxTokens([]byte(low), 8192)) != low {
		t.Fatal("une valeur sous la borne ne doit pas bouger")
	}
}

func TestEvaluateRateAlerte(t *testing.T) {
	ext := newFakeExternal(t, nil)
	rr, stats := newRerouter(t, ext, false, 20, 100, false)
	// Sous l'échantillon minimal : silence.
	stats.Record("c", routeResult(), time.Now())
	rr.EvaluateRate()
	// Au-dessus du seuil avec un échantillon suffisant : l'alerte part (notifieur inerte
	// ici, on vérifie seulement qu'aucune panique ni blocage ne survient).
	for i := 0; i < 60; i++ {
		stats.Record("c", routeResult(), time.Now())
		stats.NoteRerouteOK()
	}
	rr.EvaluateRate()
	total, rerouted := stats.RateCounters()
	if total != 61 || rerouted != 60 {
		t.Fatalf("compteurs de taux: %d/%d", rerouted, total)
	}
}

// mustJSON sérialise un corps de requête de test.
func mustJSON(t *testing.T, m map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestRerouteFluxIllisibleNePeutPasReplier documente une limite structurelle : une
// fois les en-têtes 200 envoyés (le flux a commencé), plus rien ne permet de basculer
// sur WorkBuddy. Le client reçoit la trame d'erreur du relais et l'incident est compté.
// C'est la contrepartie assumée du streaming — la seule alternative serait de tamponner
// tout le flux, c'est-à-dire de perdre l'intérêt du streaming.
func TestRerouteFluxIllisibleNePeutPasReplier(t *testing.T) {
	ext := newFakeExternal(t, func(w http.ResponseWriter, r *http.Request, body string) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("ceci n'est pas un flux SSE\n"))
	})
	rr, stats := newRerouter(t, ext, false, 20, 100, false)
	h, wbCalls := probeHTTP(t, rr)

	rec := postJSON(h, "/v1/chat/completions", mustJSON(t, map[string]any{
		"model":    "claude-opus-5",
		"stream":   true,
		"messages": []any{map[string]any{"role": "user", "content": "What model are you?"}},
	}))
	if rec.Code != 200 {
		t.Fatalf("les en-têtes étant déjà partis, la réponse reste 200: %d", rec.Code)
	}
	if n := atomic.LoadInt64(wbCalls); n != 0 {
		t.Fatalf("aucun repli n'est possible après l'ouverture du flux (%d appels)", n)
	}
	if s := stats.Snapshot(); s["reroute_failed"].(int64) != 1 {
		t.Fatalf("l'incident doit être visible dans les compteurs: %v", s)
	}
}

// TestRerouteDesactiveNEtouchePasALEcterieur : avec Enabled=false, la sonde reste sur
// la route normale et le fournisseur externe n'est jamais contacté.
func TestRerouteDesactiveNEtouchePasALEcterieur(t *testing.T) {
	ext := newFakeExternal(t, nil)
	rr, _ := newRerouter(t, ext, false, 20, 100, false)
	rr.Enabled = false
	h, wbCalls := probeHTTP(t, rr)

	postJSON(h, "/v1/chat/completions", mustJSON(t, map[string]any{
		"model":    "claude-opus-5",
		"stream":   true,
		"messages": []any{map[string]any{"role": "user", "content": "What model are you?"}},
	}))
	if n := ext.callCount(); n != 0 {
		t.Fatalf("aucun appel externe attendu (%d)", n)
	}
	if n := atomic.LoadInt64(&ext.probes); n != 0 {
		t.Fatalf("même la sonde ne doit pas partir (%d)", n)
	}
	if n := atomic.LoadInt64(wbCalls); n != 1 {
		t.Fatalf("la route normale doit servir (%d)", n)
	}
}

// TestRerouteTraficOrdinaireIntact : une requête qui n'est pas une sonde ne doit pas
// être reroutée, même avec le reroutage armé.
func TestRerouteTraficOrdinaireIntact(t *testing.T) {
	ext := newFakeExternal(t, nil)
	rr, stats := newRerouter(t, ext, false, 20, 100, false)
	h, wbCalls := probeHTTP(t, rr)

	rec := postJSON(h, "/v1/chat/completions", mustJSON(t, map[string]any{
		"model":    "claude-opus-5",
		"stream":   true,
		"messages": []any{map[string]any{"role": "user", "content": "refactorise cette fonction\n```python\ndef f(x):\n    return x\n```"}},
	}))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	if n := ext.callCount(); n != 0 {
		t.Fatalf("un développement ordinaire ne doit jamais être rerouté (%d)", n)
	}
	if n := atomic.LoadInt64(wbCalls); n != 1 {
		t.Fatalf("la route normale doit servir (%d)", n)
	}
	if s := stats.Snapshot(); s["rerouted"].(int64) != 0 {
		t.Fatalf("compteurs: %v", s)
	}
}
