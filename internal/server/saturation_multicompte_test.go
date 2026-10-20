package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// TestBurstRespecteDeuxParCompteEtFile est la preuve de bout en bout des trois garanties
// demandees par l'exploitant (protection de l'infrastructure) :
//  1. aucun compte ne depasse 2 requetes simultanees (limite de debit en amont) ;
//  2. les requetes en trop ne sont pas perdues : elles sont servies tout de suite par les
//     autres comptes (capacite = comptes x 2) ;
//  3. quand TOUS les comptes sont saturent, les requetes attendent dans la file (pas de
//     503 immediat, pas de debordement vers un compte en refroidissement) puis sont servies.
//
// Mise en scene : 4 comptes en bonne sante, plafond 2 par compte (capacite 8), 24 requetes
// simultanees, amont artificiellement lent (25 ms) pour rendre la concurrence observable.
// Un echantillonneur releve l'in-flight maximum de chaque compte pendant toute la rafale.
func TestBurstRespecteDeuxParCompteEtFile(t *testing.T) {
	const nbComptes = 4
	const plafond = 2
	const nbRequetes = 24

	comptes := make([]*auth.Auth, 0, nbComptes)
	for i := 0; i < nbComptes; i++ {
		comptes = append(comptes, &auth.Auth{
			UID:         fmt.Sprintf("u%d", i),
			AccessToken: fmt.Sprintf("at%d", i),
			ExpiresAt:   9999999999,
		})
	}
	p := testPoolWith(comptes...)
	p.SetMaxInFlight(plafond)
	p.SetQueue(pool.QueueConfig{MaxWaiters: 100, MaxWait: 10 * time.Second})

	fake := newFakeUpstream(t, func(string) (int, string, bool) {
		time.Sleep(25 * time.Millisecond) // fenetre d'observation
		return 200, sseOK, true
	})
	h := NewHandler(Config{Pool: p, Upstream: fake})

	// Echantillonneur : in-flight maximum observe par compte.
	stop := make(chan struct{})
	done := make(chan struct{})
	maxVus := make(map[string]int, nbComptes)
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			for _, a := range comptes {
				if st, ok := p.Status(a.UID); ok && st.InFlight > maxVus[a.UID] {
					maxVus[a.UID] = st.InFlight
				}
			}
			time.Sleep(time.Millisecond)
		}
	}()

	ch := make(chan qResult, nbRequetes)
	for i := 0; i < nbRequetes; i++ {
		go func() {
			req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(qStreamBody))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			ch <- qResult{rec.Code, rec.Body.String()}
		}()
	}

	codes := make(map[int]int, 2)
	for i := 0; i < nbRequetes; i++ {
		r := <-ch
		codes[r.code]++
		if r.code != http.StatusOK {
			t.Errorf("requete %d : code %d (%s), attendu 200", i, r.code, r.body)
		}
	}
	close(stop)
	<-done

	// 1. plafond par compte jamais depasse.
	for _, a := range comptes {
		if maxVus[a.UID] > plafond {
			t.Errorf("compte %s : %d requetes simultanees vues, plafond %d", a.UID, maxVus[a.UID], plafond)
		}
		if maxVus[a.UID] == 0 {
			t.Errorf("compte %s : jamais sollicite alors que la rafale depassait la capacite d'un compte", a.UID)
		}
	}

	// Le plafond doit avoir ete reellement atteint, sinon la rafale ne prouve rien.
	atteint := false
	for _, v := range maxVus {
		if v >= plafond {
			atteint = true
		}
	}
	if !atteint {
		t.Fatalf("plafond jamais atteint (%v) : rafale trop faible, test non concluant", maxVus)
	}

	// 2. prise en charge immediate par les autres comptes : les 4 comptes ont servi.
	for _, a := range comptes {
		st, ok := p.Status(a.UID)
		if !ok {
			t.Fatalf("compte %s absent du pool", a.UID)
		}
		if st.SuccessCount == 0 {
			t.Errorf("compte %s : aucune requete servie (repartition defaillante)", a.UID)
		}
	}

	// 3. file d'attente reellement utilisee, sans perte.
	qs := p.QueueStats()
	if qs.Admitted == 0 {
		t.Errorf("file d'attente jamais utilisee : episodes=%d admitted=%d timed_out=%d", qs.Episodes, qs.Admitted, qs.TimedOut)
	}
	if qs.Rejected != 0 || qs.TimedOut != 0 {
		t.Errorf("file : rejets=%d depassements=%d, attendu 0/0", qs.Rejected, qs.TimedOut)
	}
	t.Logf("in-flight max par compte : %v | file : episodes=%d admitted=%d timed_out=%d rejected=%d avg_wait=%dms",
		maxVus, qs.Episodes, qs.Admitted, qs.TimedOut, qs.Rejected, qs.AvgWaitMS)
	var servies int64
	for _, a := range comptes {
		if st, ok := p.Status(a.UID); ok {
			servies += st.SuccessCount
		}
	}
	if servies != int64(nbRequetes) {
		t.Errorf("requetes servies = %d, attendu %d", servies, nbRequetes)
	}
}
