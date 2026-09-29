package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/fpdetect"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// Tests de l'observation des sondes (phase 1) : compteurs, plafond anti-abus horaire
// par client, et surtout caractère fail-open — l'observation ne doit jamais modifier
// la réponse servie au client.

func routeResult() fpdetect.Result {
	// Résultat minimal « router » : un signal fort suffit (Strong != "").
	return fpdetect.Result{Route: true, Score: 5, Strong: fpdetect.SigModelQuestion,
		Signals: []fpdetect.Signal{{Name: fpdetect.SigModelQuestion, Weight: 5, Detail: "test"}}}
}

func quietResult() fpdetect.Result {
	return fpdetect.Result{Signals: []fpdetect.Signal{{Name: fpdetect.SigVeryShort, Weight: 1}}}
}

func TestFPCountersEnregistreEtExpose(t *testing.T) {
	c := NewFPCounters(20, true, nil, 2)
	now := time.Now()
	c.Record("cli-1", routeResult(), now)
	c.Record("cli-1", quietResult(), now)

	s := c.Snapshot()
	if s["analyzed"].(int64) != 2 || s["would_route"].(int64) != 1 {
		t.Fatalf("compteurs inattendus : %v", s)
	}
	by := s["by_signal"].(map[string]int64)
	if by[fpdetect.SigModelQuestion] != 1 || by[fpdetect.SigVeryShort] != 1 {
		t.Fatalf("ventilation par signal inattendue : %v", by)
	}
	if s["dry_run"] != true || s["cap_per_hour_per_client"] != 20 {
		t.Fatalf("réglages absents du snapshot : %v", s)
	}
	if s["last_seen"] == "" {
		t.Fatal("last_seen non renseigné")
	}
}

// TestPlafondHoraireParClient : les 20 premières décisions « router » du client
// passent, la 21e est signalée comme plafonnée, et un autre client n'est pas affecté.
func TestPlafondHoraireParClient(t *testing.T) {
	c := NewFPCounters(20, true, nil, 2)
	now := time.Now()
	for i := 0; i < 20; i++ {
		if c.Record("cli-1", routeResult(), now) {
			t.Fatalf("décision %d : plafond atteint trop tôt", i+1)
		}
	}
	if !c.Record("cli-1", routeResult(), now) {
		t.Fatal("la 21e décision du client doit être signalée comme plafonnée")
	}
	if c.Record("cli-2", routeResult(), now) {
		t.Fatal("un autre client ne doit pas hériter du plafond")
	}
	// Une décision qui ne route pas ne consomme pas de quota.
	if c.Record("cli-1", quietResult(), now) {
		t.Fatal("une requête non routée ne doit jamais être plafonnée")
	}
	s := c.Snapshot()
	if s["over_cap"].(int64) != 1 || s["capped_clients"].(int) != 1 {
		t.Fatalf("plafond mal compté : %v", s)
	}
}

// TestPlafondFenetreGlissante : le quota se libère au bout d'une heure.
func TestPlafondFenetreGlissante(t *testing.T) {
	c := NewFPCounters(2, true, nil, 2)
	now := time.Now()
	c.Record("cli-1", routeResult(), now)
	c.Record("cli-1", routeResult(), now)
	if !c.Record("cli-1", routeResult(), now) {
		t.Fatal("3e décision attendue plafonnée")
	}
	if c.Record("cli-1", routeResult(), now.Add(time.Hour+time.Second)) {
		t.Fatal("après une heure, le quota doit être de nouveau disponible")
	}
}

// TestPlafondNonAttribuable : sans clé de session, le plafond est inapplicable. On
// compte ces décisions à part au lieu de les imputer à un seau global, qui ferait
// plafonner tous les clients à cause d'un seul bavard.
func TestPlafondNonAttribuable(t *testing.T) {
	c := NewFPCounters(1, true, nil, 2)
	now := time.Now()
	for i := 0; i < 5; i++ {
		if c.Record("", routeResult(), now) {
			t.Fatal("sans clé de client, aucune décision ne doit être plafonnée")
		}
	}
	s := c.Snapshot()
	if s["unattributed"].(int64) != 5 || s["over_cap"].(int64) != 0 {
		t.Fatalf("comptage non attribuable inattendu : %v", s)
	}
}

// TestPlafondMemoireBornee : la table des clients ne grossit pas sans limite.
func TestPlafondMemoireBornee(t *testing.T) {
	c := NewFPCounters(20, true, nil, 2)
	now := time.Now()
	for i := 0; i < fpCapMaxClients+200; i++ {
		c.Record("cli-"+time.Duration(i).String(), routeResult(), now)
	}
	c.mu.Lock()
	n := len(c.windows)
	c.mu.Unlock()
	if n > fpCapMaxClients {
		t.Fatalf("table des clients non bornée : %d entrées", n)
	}
}

// sondeBody : corps de sonde minimal reconnu par le vrai détecteur, avec une clé de
// session pour que le plafond soit attribuable.
func sondeBody(key string) []byte {
	m := map[string]any{
		"model":    "glm-5.2",
		"messages": []any{map[string]any{"role": "user", "content": "What model are you?"}},
	}
	if key != "" {
		m["metadata"] = map[string]any{"conversation_id": key}
	}
	b, _ := json.Marshal(m)
	return b
}

// TestObservationFailOpen : une sonde détectée est journalisée et comptée, mais la
// requête suit exactement le chemin normal — même statut, un seul appel amont. C'est
// la garantie centrale de la phase 1.
func TestObservationFailOpen(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	counters := NewFPCounters(20, true, nil, 2)
	h := NewHandler(Config{Pool: p, Upstream: up, FPDetect: fpdetect.New(fpdetect.DefaultConfig()), FPStats: counters})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(sondeBody("conv-sonde-1")))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("statut inattendu : %d (%s)", rec.Code, rec.Body.String())
	}
	if calls != 1 {
		t.Fatalf("un seul appel amont attendu, %d observés", calls)
	}
	s := counters.Snapshot()
	if s["analyzed"].(int64) != 1 || s["would_route"].(int64) != 1 {
		t.Fatalf("la sonde devait être comptée : %v", s)
	}
	if s["over_cap"].(int64) != 0 {
		t.Fatalf("aucun plafond atteint attendu : %v", s)
	}
}

// TestSignauxIgnoresSontJournalises : un signal conditionnel écarté doit produire une
// trace distincte de la détection, sinon il est invisible dans le relevé.
func TestSignauxIgnoresSontJournalises(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	counters := NewFPCounters(20, true, fpdetect.DefaultConfig().CorroborationSignals, 2)
	h := NewHandler(Config{Pool: p, Upstream: up, FPDetect: fpdetect.New(fpdetect.DefaultConfig()), FPStats: counters})

	// PDF joint seul : signal conditionnel, non corroboré.
	body, _ := json.Marshal(map[string]any{
		"model": "glm-5.2",
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "document", "source": map[string]any{"type": "base64", "media_type": "application/pdf", "data": "JVBERi0xLjQK"}},
		}}},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("statut inattendu : %d", rec.Code)
	}
	s := counters.Snapshot()
	if s["would_route"].(int64) != 0 {
		t.Fatalf("un PDF seul ne doit pas compter comme reroutage : %v", s)
	}
	if s["ignored_signals"].(map[string]int64)[fpdetect.SigPDFContent] != 1 {
		t.Fatalf("le signal ignoré doit être compté : %v", s)
	}
}

// TestObservationPasseLeTraficNormalIntact : sur une requête ordinaire, le détecteur
// ne compte aucun reroutage et la requête atteint l'amont.
func TestObservationPasseLeTraficNormalIntact(t *testing.T) {
	var seen string
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	counters := NewFPCounters(20, true, nil, 2)
	h := NewHandler(Config{Pool: p, Upstream: up, FPDetect: fpdetect.New(fpdetect.DefaultConfig()), FPStats: counters})

	body, _ := json.Marshal(map[string]any{
		"model":    "glm-5.2",
		"stream":   true,
		"messages": []any{map[string]any{"role": "user", "content": "écris une fonction Go qui trie une slice"}},
	})
	seen = string(body)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("statut inattendu : %d", rec.Code)
	}
	if seen != string(body) {
		t.Fatal("le corps a été modifié")
	}
	if s := counters.Snapshot(); s["would_route"].(int64) != 0 {
		t.Fatalf("requête de codage : aucun reroutage attendu, obtenu %v", s)
	}
}

// TestObservationDesactivee : sans détecteur, /status ne présente pas de bloc
// fp_observe (le panneau distingue « pas de sonde » de « observation éteinte »).
func TestObservationDesactivee(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: upstream.New(),
		Live:     livecfg.New(livecfg.Snapshot{APIKey: dataKey, AdminAPIKey: adminKey}),
	})
	rec := get(h, "/status", adminKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("/status code=%d", rec.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("réponse /status illisible : %v", err)
	}
	if v, ok := out["fp_observe"]; !ok || v != nil {
		t.Fatalf("fp_observe devrait être null, obtenu %v", v)
	}
}

// TestObservationActiveDansStatus : quand l'observation est branchée, /status expose
// les compteurs et les réglages du plafond.
func TestObservationActiveDansStatus(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: upstream.New(),
		Live:     livecfg.New(livecfg.Snapshot{APIKey: dataKey, AdminAPIKey: adminKey}),
		FPDetect: fpdetect.New(fpdetect.DefaultConfig()),
		FPStats:  NewFPCounters(7, true, []string{fpdetect.SigPDFContent}, 3),
	})
	rec := get(h, "/status", adminKey)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("réponse /status illisible : %v", err)
	}
	fp, ok := out["fp_observe"].(map[string]any)
	if !ok {
		t.Fatalf("bloc fp_observe attendu, obtenu %v", out["fp_observe"])
	}
	if fp["cap_per_hour_per_client"] != float64(7) || fp["dry_run"] != true {
		t.Fatalf("réglages du plafond absents : %v", fp)
	}
	// La règle de corroboration doit être relisible depuis /status : sans elle, un
	// relevé de plusieurs semaines n'est plus interprétable.
	sigs, ok := fp["corroboration_signals"].([]any)
	if !ok || len(sigs) != 1 || sigs[0] != fpdetect.SigPDFContent {
		t.Fatalf("corroboration_signals absent ou inattendu : %v", fp["corroboration_signals"])
	}
	if fp["corroboration_weak_min"] != float64(3) {
		t.Fatalf("corroboration_weak_min attendu à 3 : %v", fp["corroboration_weak_min"])
	}
	if _, ok := fp["ignored_signals"].(map[string]any); !ok {
		t.Fatalf("ignored_signals attendu dans /status : %v", fp["ignored_signals"])
	}
}

// TestSignauxIgnoresComptes : la mesure directe de ce que la corroboration évite.
func TestSignauxIgnoresComptes(t *testing.T) {
	c := NewFPCounters(20, true, []string{fpdetect.SigPDFContent, fpdetect.SigToolCountExtreme}, 2)
	now := time.Now()
	ignored := fpdetect.Result{
		Score: 2, EffectiveScore: 0,
		Signals: []fpdetect.Signal{{Name: fpdetect.SigPDFContent, Weight: 2, Ignored: true}},
	}
	c.Record("cli-1", ignored, now)
	c.Record("cli-1", ignored, now)
	c.Record("cli-1", routeResult(), now)

	s := c.Snapshot()
	ign := s["ignored_signals"].(map[string]int64)
	if ign[fpdetect.SigPDFContent] != 2 {
		t.Fatalf("signaux ignorés mal comptés : %v", ign)
	}
	if s["analyzed"].(int64) != 3 || s["would_route"].(int64) != 1 {
		t.Fatalf("compteurs globaux inattendus : %v", s)
	}
}

// TestObservationConcurrente vérifie qu'il n'y a pas de course sur les compteurs
// (le chemin de requête est concurrent par nature).
func TestObservationConcurrente(t *testing.T) {
	c := NewFPCounters(1000000, true, nil, 2)
	now := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				c.Record("cli", routeResult(), now)
			}
		}()
	}
	wg.Wait()
	if s := c.Snapshot(); s["analyzed"].(int64) != 1600 {
		t.Fatalf("analyses perdues : %v", s)
	}
}
