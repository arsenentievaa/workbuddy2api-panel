package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/alert"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// Alerte « échec de reroutage » : l'exploitant recevait un message par échec, y compris
// pour un 502 isolé du fournisseur (constaté sur téléphone le 2026-10-20). Il ne doit
// plus être notifié que si les échecs se répètent.

func rerouterPourAlerte(t *testing.T) *Rerouter {
	t.Helper()
	stats := NewFPCounters(20, false, nil, 2, 100, 15*time.Minute)
	return &Rerouter{
		Enabled: true,
		Stats:   stats,
		Health:  upstream.NewExternalHealth(time.Minute),
		Alerts:  alert.New("jeton-de-test", "canal-de-test"),
	}
}

func TestEchecIsoleNeDeclenchePasDAlerte(t *testing.T) {
	rr := rerouterPourAlerte(t)
	for i := 1; i <= 2; i++ {
		rr.Stats.NoteRerouteFailure(http.StatusBadGateway)
		if ok, n := rr.doitAlerterEchec(time.Now()); ok {
			t.Fatalf("après %d échec(s), aucune alerte ne doit partir (n=%d)", i, n)
		}
	}
}

func TestEchecsRepetesDeclenchentUneAlerte(t *testing.T) {
	rr := rerouterPourAlerte(t)
	for i := 0; i < rerouteAlertMinFailures; i++ {
		rr.Stats.NoteRerouteFailure(http.StatusBadGateway)
	}
	ok, n := rr.doitAlerterEchec(time.Now())
	if !ok || n < rerouteAlertMinFailures {
		t.Fatalf("une panne répétée doit alerter (n=%d, ok=%v)", n, ok)
	}
}

// Un vieil échec hors fenêtre ne doit plus compter : après un incident terminé, l'alerte
// ne doit pas se rallumer au premier échec suivant.
func TestEchecHorsFenetreNeComptePas(t *testing.T) {
	rr := rerouterPourAlerte(t)
	for i := 0; i < rerouteAlertMinFailures; i++ {
		rr.Stats.NoteRerouteFailure(http.StatusBadGateway)
	}
	if ok, _ := rr.doitAlerterEchec(time.Now().Add(rerouteAlertWindow + time.Minute)); ok {
		t.Fatal("des échecs sortis de la fenêtre ne doivent plus alerter")
	}
}

// Après un échec réel, le fournisseur est marqué malsain : les reroutages suivants sont
// refusés sans appel, donc sans nouvel échec ni nouvelle alerte.
func TestEchecReelMarqueLeFournisseurMalsain(t *testing.T) {
	rr := rerouterPourAlerte(t)
	ctx := context.Background()
	if ok, _, err := rr.Health.Healthy(ctx, func(context.Context) error { return nil }); !ok || err != nil {
		t.Fatal("première mesure : fournisseur sain")
	}
	rr.noteEchecExterne(http.StatusBadGateway, errTest("502"), "test")

	var appels int64
	ok, _, _ := rr.Health.Healthy(ctx, func(context.Context) error {
		atomic.AddInt64(&appels, 1)
		return nil
	})
	if ok {
		t.Fatal("après un échec réel, le fournisseur doit être considéré malsain")
	}
	if n := atomic.LoadInt64(&appels); n != 0 {
		t.Fatalf("le verdict basculé doit être servi depuis le cache (aucune sonde), %d appel(s)", n)
	}
}

// Un 5xx est rejoué une fois : une passerelle saturée répond souvent correctement à la
// seconde tentative, et le client n'a pas à subir un repli.
func TestCinqCentDeuxRejoueUneFois(t *testing.T) {
	var posts int64
	ext := newFakeExternal(t, func(w http.ResponseWriter, r *http.Request, body string) {
		n := atomic.AddInt64(&posts, 1)
		if n == 1 {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":"passager"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"message":{"content":"bonjour"}}]}`))
	})
	rr := rerouterPourAlerte(t)
	rr.Upstream = &upstream.Client{}
	rr.Target = upstream.ExternalTarget{BaseURL: ext.srv.URL, Path: "/v1/chat/completions", APIKey: "cle"}

	res, err := rr.sendExternal(context.Background(), rr.Target, []byte(`{"model":"claude-opus-5","messages":[]}`), "127.0.0.1", false)
	if err != nil {
		t.Fatalf("la seconde tentative doit réussir : %v", err)
	}
	if res == nil {
		t.Fatal("réponse attendue")
	}
	if n := atomic.LoadInt64(&posts); n != 2 {
		t.Fatalf("deux appels attendus (échec puis reprise), %d observés", n)
	}
}

// Un 4xx n'est pas rejoué : la requête ne passera pas davantage.
func TestQuatreCentNeRejouePas(t *testing.T) {
	var posts int64
	ext := newFakeExternal(t, func(w http.ResponseWriter, r *http.Request, body string) {
		atomic.AddInt64(&posts, 1)
		w.WriteHeader(http.StatusUnauthorized)
	})
	rr := rerouterPourAlerte(t)
	rr.Upstream = &upstream.Client{}
	rr.Target = upstream.ExternalTarget{BaseURL: ext.srv.URL, Path: "/v1/chat/completions", APIKey: "cle"}

	if _, err := rr.sendExternal(context.Background(), rr.Target, []byte(`{"model":"claude-opus-5","messages":[]}`), "", false); err == nil {
		t.Fatal("une erreur est attendue")
	}
	if n := atomic.LoadInt64(&posts); n != 1 {
		t.Fatalf("un seul appel attendu, %d observés", n)
	}
}

type errTest string

func (e errTest) Error() string { return string(e) }

var _ = httptest.NewServer
