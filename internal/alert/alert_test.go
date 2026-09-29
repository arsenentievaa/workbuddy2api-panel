package alert

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestNotifierInerteSansIdentifiants(t *testing.T) {
	for _, n := range []*Notifier{New("", ""), New("token", ""), New("", "chat"), nil} {
		if n.Enabled() {
			t.Fatal("un notifieur sans identifiants doit être inerte")
		}
		if n.Send("k", "texte") {
			t.Fatal("un notifieur inerte ne doit rien mettre en file")
		}
	}
}

func TestNotifierEnvoieEtDeduplique(t *testing.T) {
	var calls int64
	var lastText string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		lastText, _ = m["text"].(string)
		if m["chat_id"] != "42" {
			t.Errorf("chat_id envoyé: %v", m["chat_id"])
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	n := New("jeton", "42")
	n.baseURL = srv.URL
	n.dedup = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n.Start(ctx)

	if !n.Send("sante", "🔴 fournisseur malsain") {
		t.Fatal("la première alerte doit partir")
	}
	// La même clé dans la fenêtre de déduplication : supprimée, pas envoyée.
	if n.Send("sante", "🔴 fournisseur malsain encore") {
		t.Fatal("une alerte répétée dans la fenêtre doit être supprimée")
	}
	if !n.Send("plafond", "⚠️ plafond atteint") {
		t.Fatal("une clé différente doit passer")
	}

	deadline := time.Now().Add(3 * time.Second)
	for atomic.LoadInt64(&calls) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := atomic.LoadInt64(&calls); got != 2 {
		t.Fatalf("2 envois attendus, %d observés", got)
	}
	if lastText != "⚠️ plafond atteint" {
		t.Fatalf("dernier texte envoyé: %q", lastText)
	}
	s := n.Stats()
	if !s.Enabled || s.Sent != 2 || s.Suppressed != 1 || s.Failed != 0 {
		t.Fatalf("compteurs: %+v", s)
	}
}

func TestNotifierFileBornee(t *testing.T) {
	// Aucun consommateur : Send ne doit jamais bloquer, et doit compter les pertes.
	n := New("jeton", "42")
	n.dedup = 0
	perdus := 0
	for i := 0; i < queueSize+20; i++ {
		if !n.Send(string(rune('a'+i%26))+time.Duration(i).String(), "x") {
			perdus++
		}
	}
	if perdus == 0 {
		t.Fatal("au-delà de la file, les alertes doivent être comptées comme perdues")
	}
	if s := n.Stats(); s.Dropped == 0 {
		t.Fatalf("compteur de pertes: %+v", s)
	}
}

func TestNotifierEchecHTTPCompte(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	n := New("jeton", "42")
	n.baseURL = srv.URL
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n.Start(ctx)
	n.Send("k", "texte")
	deadline := time.Now().Add(3 * time.Second)
	for n.Stats().Failed == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if s := n.Stats(); s.Failed != 1 || s.Sent != 0 {
		t.Fatalf("un 400 doit être compté en échec: %+v", s)
	}
}

func TestLoadFromEnv(t *testing.T) {
	t.Setenv("WB2A_TELEGRAM_BOT_TOKEN", "wb2a-token")
	t.Setenv("TELEGRAM_CHAT_ID", "999")
	tok, chat := LoadFromEnv()
	if tok != "wb2a-token" || chat != "999" {
		t.Fatalf("tok=%q chat=%q", tok, chat)
	}
	t.Setenv("WB2A_TELEGRAM_BOT_TOKEN", "")
	t.Setenv("TELEGRAM_BOT_TOKEN", "generique")
	if tok, _ := LoadFromEnv(); tok != "generique" {
		t.Fatalf("repli sur le nom générique attendu, obtenu %q", tok)
	}
}
