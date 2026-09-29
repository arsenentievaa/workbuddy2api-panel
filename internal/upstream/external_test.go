package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- appel externe ---------------------------------------------------------------

func TestChatStreamExternalEnvoieLaBonneRequete(t *testing.T) {
	type seen struct {
		path    string
		auth    string
		xapikey string
		version string
		accept  string
		body    string
		xff     string
	}
	var got seen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = seen{
			path:    r.URL.Path,
			auth:    r.Header.Get("Authorization"),
			xapikey: r.Header.Get("x-api-key"),
			version: r.Header.Get("anthropic-version"),
			accept:  r.Header.Get("Accept"),
			body:    string(b),
			xff:     r.Header.Get("X-Forwarded-For"),
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := &Client{}
	res, err := c.ChatStreamExternal(context.Background(), ExternalTarget{
		BaseURL: srv.URL, Path: "/v1/chat/completions", APIKey: "cle-secrete",
	}, []byte(`{"model":"claude-opus-5"}`), "203.0.113.7", false)
	if err != nil {
		t.Fatalf("appel: %v", err)
	}
	defer res.Body.Close()
	if got.path != "/v1/chat/completions" {
		t.Errorf("chemin=%s", got.path)
	}
	if got.auth != "Bearer cle-secrete" || got.xapikey != "cle-secrete" {
		t.Errorf("x-api-key doit être posé aussi sur le protocole OpenAI: %q / %q", got.auth, got.xapikey)
	}
	if got.version != "" {
		t.Errorf("anthropic-version ne doit pas être envoyée hors protocole Messages: %q", got.version)
	}
	if got.accept != "application/json" {
		t.Errorf("accept=%q", got.accept)
	}
	if got.body != `{"model":"claude-opus-5"}` {
		t.Errorf("corps altéré: %s", got.body)
	}
	if got.xff != "203.0.113.7" {
		t.Errorf("X-Forwarded-For=%q", got.xff)
	}
}

// TestChatStreamExternalAnthropic : le protocole Messages exige x-api-key et la version.
func TestChatStreamExternalAnthropic(t *testing.T) {
	var version, accept, xkey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		version = r.Header.Get("anthropic-version")
		accept = r.Header.Get("Accept")
		xkey = r.Header.Get("x-api-key")
		_, _ = w.Write([]byte(`{"type":"message"}`))
	}))
	defer srv.Close()

	c := &Client{}
	res, err := c.ChatStreamExternal(context.Background(), ExternalTarget{
		BaseURL: srv.URL, Path: "/v1/messages", APIKey: "k", Anthropic: true,
	}, []byte(`{}`), "", true)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if version != anthropicVersion {
		t.Errorf("anthropic-version=%q", version)
	}
	if accept != "text/event-stream" {
		t.Errorf("accept=%q", accept)
	}
	if xkey != "k" {
		t.Errorf("x-api-key=%q", xkey)
	}
}

// TestChatStreamExternalNeDivulguePasLErreur : un corps d'erreur du fournisseur
// externe ne doit jamais remonter — il décrit une infrastructure dont l'existence est
// précisément ce que la sonde cherche à découvrir.
func TestChatStreamExternalNeDivulguePasLErreur(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"compte interne fournisseur XYZ en sursis, contactez ops@exemple"}`))
	}))
	defer srv.Close()

	c := &Client{}
	_, err := c.ChatStreamExternal(context.Background(), ExternalTarget{
		BaseURL: srv.URL, Path: "/v1/chat/completions", APIKey: "k",
	}, []byte(`{}`), "", false)
	if err == nil {
		t.Fatal("une réponse 401 doit produire une erreur")
	}
	if strings.Contains(err.Error(), "XYZ") || strings.Contains(err.Error(), "ops@exemple") {
		t.Fatalf("le corps du fournisseur externe fuit dans l'erreur: %v", err)
	}
	var se interface{ Status() int }
	if !errors.As(err, &se) || se.Status() != http.StatusUnauthorized {
		t.Fatalf("le code HTTP doit rester disponible sans le corps: %v", err)
	}
}

func TestChatStreamExternalConfigurationIncomplete(t *testing.T) {
	c := &Client{}
	for _, tc := range []ExternalTarget{
		{BaseURL: "", Path: "/x", APIKey: "k"},
		{BaseURL: "https://exemple.test", Path: "/x", APIKey: ""},
	} {
		if _, err := c.ChatStreamExternal(context.Background(), tc, []byte(`{}`), "", false); err == nil {
			t.Fatalf("configuration incomplète acceptée: %+v", tc)
		}
	}
}

func TestChatStreamExternalTransportKO(t *testing.T) {
	c := &Client{}
	// Port fermé : l'échec doit être une erreur locale, pas une panique ni un succès.
	_, err := c.ChatStreamExternal(context.Background(), ExternalTarget{
		BaseURL: "http://127.0.0.1:1", Path: "/x", APIKey: "k", Timeout: 2 * time.Second,
	}, []byte(`{}`), "", false)
	if err == nil {
		t.Fatal("un transport injoignable doit échouer")
	}
}

func TestProbeExternal(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		if r.Header.Get("Authorization") != "Bearer k" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	c := &Client{}
	if err := c.ProbeExternal(context.Background(), ExternalTarget{BaseURL: srv.URL, Path: "/v1/models", APIKey: "k"}); err != nil {
		t.Fatalf("sonde: %v", err)
	}
	if path != "/v1/models" {
		t.Errorf("chemin de sonde=%s", path)
	}
	if err := c.ProbeExternal(context.Background(), ExternalTarget{BaseURL: srv.URL, Path: "/v1/models", APIKey: "mauvaise"}); err == nil {
		t.Fatal("une sonde 401 doit échouer")
	}
}

// --- santé mise en cache ---------------------------------------------------------

func TestExternalHealthCache(t *testing.T) {
	h := NewExternalHealth(time.Hour)
	var calls int64
	probe := func(context.Context) error { atomic.AddInt64(&calls, 1); return nil }

	for i := 0; i < 5; i++ {
		ok, transition, err := h.Healthy(context.Background(), probe)
		if !ok || transition || err != nil {
			t.Fatalf("itération %d: ok=%v transition=%v err=%v", i, ok, transition, err)
		}
	}
	if calls != 1 {
		t.Fatalf("la sonde doit être mise en cache : %d appels", calls)
	}
}

func TestExternalHealthBascule(t *testing.T) {
	h := NewExternalHealth(time.Millisecond)
	var fail atomic.Bool
	probe := func(context.Context) error {
		if fail.Load() {
			return errors.New("401")
		}
		return nil
	}
	if ok, _, _ := h.Healthy(context.Background(), probe); !ok {
		t.Fatal("première mesure attendue saine")
	}
	fail.Store(true)
	time.Sleep(2 * time.Millisecond)
	ok, transition, err := h.Healthy(context.Background(), probe)
	if ok || !transition || err == nil {
		t.Fatalf("bascule vers malsain attendue: ok=%v transition=%v err=%v", ok, transition, err)
	}
	// Une mesure suivante, toujours malsaine, ne re-déclenche pas de transition : sans
	// cela, chaque requête enverrait une alerte.
	if _, transition, _ := h.Healthy(context.Background(), probe); transition {
		t.Fatal("l'état malsain durable ne doit pas re-déclencher de transition")
	}
	fail.Store(false)
	time.Sleep(2 * time.Millisecond)
	ok, transition, _ = h.Healthy(context.Background(), probe)
	if !ok || !transition {
		t.Fatalf("retour à sain attendu avec transition: ok=%v transition=%v", ok, transition)
	}
	// 3 sondes seulement : la 3e lecture est servie par le cache (TTL non expiré),
	// donc elle n'ajoute ni mesure ni échec — c'est le comportement voulu.
	s := h.Snapshot()
	if s.Checks != 3 || s.Failures != 1 || s.Transitions != 2 {
		t.Fatalf("compteurs de santé: %+v", s)
	}
}

// TestExternalHealthSondeEnVolNeBloquePas : une seule sonde à la fois, et les appels
// concurrents ne s'empilent pas derrière elle.
func TestExternalHealthSondeEnVolNeBloquePas(t *testing.T) {
	h := NewExternalHealth(time.Nanosecond)
	release := make(chan struct{})
	var calls int64
	probe := func(context.Context) error {
		atomic.AddInt64(&calls, 1)
		<-release
		return nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _ = h.Healthy(context.Background(), probe)
		}()
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	if n := atomic.LoadInt64(&calls); n > 2 {
		t.Fatalf("trop de sondes concurrentes (%d) : le cache doit sérialiser", n)
	}
}

func TestExternalHealthSnapshotInitial(t *testing.T) {
	h := NewExternalHealth(0)
	s := h.Snapshot()
	if s.Healthy || s.Checks != 0 || s.TTLSeconds != 300 {
		t.Fatalf("état initial inattendu: %+v", s)
	}
}

// --- clé -------------------------------------------------------------------------

func TestLoadExternalAPIKeyFichierEnv(t *testing.T) {
	dir := t.TempDir()
	// Format real-claude.env : commentaires + CLE=valeur.
	p := filepath.Join(dir, "real-claude.env")
	os.WriteFile(p, []byte("# commentaire\n\nCRAZYTOKEN_API_KEY=sk-test-123\nAUTRE=ignoré\n"), 0o600)
	k, err := LoadExternalAPIKey(p, "")
	if err != nil || k != "sk-test-123" {
		t.Fatalf("k=%q err=%v", k, err)
	}
}

func TestLoadExternalAPIKeyCleSeule(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "key")
	os.WriteFile(p, []byte("sk-nu\n"), 0o600)
	if k, err := LoadExternalAPIKey(p, ""); err != nil || k != "sk-nu" {
		t.Fatalf("k=%q err=%v", k, err)
	}
}

func TestLoadExternalAPIKeyInlineEtAbsence(t *testing.T) {
	if k, err := LoadExternalAPIKey("", "sk-inline"); err != nil || k != "sk-inline" {
		t.Fatalf("k=%q err=%v", k, err)
	}
	if _, err := LoadExternalAPIKey("", ""); err == nil {
		t.Fatal("sans clé, une erreur est attendue")
	}
	if _, err := LoadExternalAPIKey(filepath.Join(t.TempDir(), "absent"), ""); err == nil {
		t.Fatal("fichier absent : erreur attendue")
	}
}

func TestParseKeyFileVariantes(t *testing.T) {
	cases := map[string]string{
		"CRAZYTOKEN_API_KEY=sk-a":     "sk-a",
		"ANTHROPIC_API_KEY = 'sk-b' ": "sk-b",
		`API_KEY="sk-c"`:              "sk-c",
		"TELEGRAM_BOT_TOKEN=123:abc":  "123:abc",
		"# rien\n\n":                  "",
		"IGNORÉ=1\nAUTRE_KEY=sk-d":    "sk-d",
		"NOM_SANS_RAPPORT=zzz":        "",
	}
	for in, want := range cases {
		if got := parseKeyFile(in); got != want {
			t.Errorf("parseKeyFile(%q)=%q want %q", in, got, want)
		}
	}
}

// --- réécriture ------------------------------------------------------------------

func TestRewriteModel(t *testing.T) {
	out := RewriteModel([]byte(`{"model":"glm-5.2","max_tokens":8}`), "claude-opus-5")
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["model"] != "claude-opus-5" {
		t.Fatalf("model=%v", m["model"])
	}
	if m["max_tokens"] != float64(8) {
		t.Fatalf("les autres champs doivent survivre: %v", m)
	}
	// Corps illisible : rendu tel quel (l'appelant n'a rien corrompu).
	if got := RewriteModel([]byte("{pas du json"), "x"); string(got) != "{pas du json" {
		t.Fatalf("corps illisible altéré: %s", got)
	}
	if got := RewriteModel([]byte(`{"a":1}`), ""); string(got) != `{"a":1}` {
		t.Fatalf("modèle vide ne doit rien changer: %s", got)
	}
}

func TestBodyModelEtStreaming(t *testing.T) {
	if got := BodyModel([]byte(`{"model":"x"}`)); got != "x" {
		t.Errorf("BodyModel=%q", got)
	}
	if got := BodyModel([]byte("{cassé")); got != "" {
		t.Errorf("BodyModel illisible=%q", got)
	}
	if !IsStreamingBody([]byte(`{"stream":true}`)) || IsStreamingBody([]byte(`{"stream":false}`)) {
		t.Error("IsStreamingBody incohérent")
	}
}

func TestExtraBodyInt(t *testing.T) {
	if got := ExtraBodyInt([]byte(`{"max_tokens":123}`), "max_tokens"); got != 123 {
		t.Errorf("got=%d", got)
	}
	if got := ExtraBodyInt([]byte(`{"max_tokens":1.5e3}`), "max_tokens"); got != 1500 {
		t.Errorf("notation exponentielle: got=%d", got)
	}
	if got := ExtraBodyInt([]byte(`{"autre":1}`), "max_tokens"); got != 0 {
		t.Errorf("champ absent: got=%d", got)
	}
}

func TestExternalTargetEndpoint(t *testing.T) {
	cases := []struct{ base, path, want string }{
		{"https://exemple.test", "/v1/models", "https://exemple.test/v1/models"},
		{"https://exemple.test/", "/v1/models", "https://exemple.test/v1/models"},
		{" https://exemple.test/api ", "/v1/messages", "https://exemple.test/api/v1/messages"},
	}
	for _, c := range cases {
		got := (ExternalTarget{BaseURL: c.base, Path: c.path}).endpoint()
		if got != c.want {
			t.Errorf("endpoint(%q,%q)=%q want %q", c.base, c.path, got, c.want)
		}
	}
}
