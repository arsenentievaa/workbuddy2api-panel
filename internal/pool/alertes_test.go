package pool

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func authStub(uid, nick string) *auth.Auth {
	return &auth.Auth{UID: uid, Nickname: nick, AccessToken: "x", RefreshToken: "y"}
}

// Un compte qui tombe doit prevenir l'exploitant : c'est le trou qui a laisse 13 comptes
// se faire bannir sans que personne ne le sache (constat du 2026-10-20).
func TestAlerteQuandUnCompteTombe(t *testing.T) {
	p := New("")
	var alertes int64
	var kind, reason string
	p.SetOnDown(func(k, uid, nick, r string) {
		atomic.AddInt64(&alertes, 1)
		kind, reason = k, r
	})
	e := &entry{a: authStub("u1", "70294338")}
	p.byUID["u1"] = e

	p.Disable("u1", "account banned by upstream (11140 request illegal)")
	time.Sleep(50 * time.Millisecond) // le callback part en goroutine
	if atomic.LoadInt64(&alertes) != 1 || kind != "disabled" {
		t.Fatalf("une alerte attendue (kind=disabled), vu %d et %q", alertes, kind)
	}
	if reason == "" {
		t.Fatal("la cause doit etre transmise")
	}

	// Une deuxieme desactivation du meme compte n'alerte pas de nouveau.
	p.Disable("u1", "deja desactive")
	time.Sleep(50 * time.Millisecond)
	if n := atomic.LoadInt64(&alertes); n != 1 {
		t.Fatalf("pas de doublon attendu, %d alertes", n)
	}
}

// Une limite de debit (rate limit) est aussi un « hors service » a signaler.
func TestAlerteSurRateLimit(t *testing.T) {
	p := New("")
	var alertes int64
	p.SetOnDown(func(k, uid, nick, r string) { atomic.AddInt64(&alertes, 1) })
	p.byUID["u2"] = &entry{a: authStub("u2", "70358153")}

	p.Cooldown("u2", CoolSoft, time.Minute, "429 rate limit")
	time.Sleep(50 * time.Millisecond)
	if atomic.LoadInt64(&alertes) != 1 {
		t.Fatalf("une alerte attendue sur une limite de debit, %d", alertes)
	}
	// Un compte deja en limite ne re-alerte pas a chaque requete.
	p.Cooldown("u2", CoolSoft, time.Minute, "429 rate limit")
	time.Sleep(50 * time.Millisecond)
	if n := atomic.LoadInt64(&alertes); n != 1 {
		t.Fatalf("pas de doublon attendu, %d alertes", n)
	}
}
