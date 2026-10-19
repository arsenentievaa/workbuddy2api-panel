package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/prompt"
)

// Chaîne de remède complète, vérifiée à chaque étage (2026-10-20).
//
// Rien ne doit atteindre un client qui n'écrit pas en chinois : ni la réponse du parc,
// ni celle du fournisseur externe.

// Le remède EXTERNE est désormais vérifié : une réponse encore en chinois n'est pas
// écrite, le client garde la réponse d'origine et la fuite est comptée.
func TestRemedeExterneEncoreChinoisNEstPasServi(t *testing.T) {
	ext := newFakeExternal(t, func(w http.ResponseWriter, r *http.Request, body string) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"` + zhAnswer + `"}}]}`))
	})
	h, _ := mismatchHandler(t, ext, sseWithContent(zhAnswer))

	rec := postJSONClient(h, "/v1/chat/completions", clientBody(t, enQuestion, false), "u42")
	if strings.Contains(rec.Body.String(), "巴黎") == false {
		t.Fatalf("le remède ayant échoué, la réponse d'origine doit être servie : %s", rec.Body.String()[:200])
	}
	lm := mismatchCounters(h)
	if lm["kept"] != 1 {
		t.Fatalf("une fuite subie doit être comptée : %v", lm)
	}
}

// Quand le fournisseur externe répond correctement, il est servi — et vérifié.
func TestRemedeExterneVerifieSertLaReponseCorrecte(t *testing.T) {
	ext := newFakeExternal(t, func(w http.ResponseWriter, r *http.Request, body string) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"Paris is the capital of France."}}]}`))
	})
	h, _ := mismatchHandler(t, ext, sseWithContent(zhAnswer))

	rec := postJSONClient(h, "/v1/chat/completions", clientBody(t, enQuestion, false), "u42")
	if !strings.Contains(rec.Body.String(), "Paris") {
		t.Fatalf("la réponse vérifiée du fournisseur doit être servie : %s", rec.Body.String()[:200])
	}
	if lm := mismatchCounters(h); lm["rerouted"] != 1 || lm["kept"] != 0 {
		t.Fatalf("compteurs : %v", lm)
	}
}

// Le second rejeu (consigne stricte) est utilisé quand le premier est revenu en chinois,
// et son résultat est vérifié : le client reçoit la version anglaise.
func TestSecondRejeuConsigneStricte(t *testing.T) {
	ancien := leakRetryDo
	appels := 0
	leakRetryDo = func(req *http.Request) (*http.Response, error) {
		appels++
		corps, _ := io.ReadAll(req.Body)
		contenu := "Paris is the capital of France."
		if !strings.Contains(string(corps), "Do NOT output Chinese characters") {
			contenu = zhAnswer // premier rejeu : la consigne normale ne suffit pas
		}
		body := `{"choices":[{"message":{"content":"` + contenu + `"}}]}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	}
	defer func() { leakRetryDo = ancien }()

	ext := newFakeExternal(t, nil)
	h, _ := mismatchHandler(t, ext, sseWithContent(zhAnswer))
	rec := postJSONClient(h, "/v1/chat/completions", clientBody(t, enQuestion, false), "u42")

	if appels != 2 {
		t.Fatalf("deux rejeux attendus (normal puis strict), %d observés", appels)
	}
	if !strings.Contains(rec.Body.String(), "Paris") {
		t.Fatalf("le client doit recevoir le second rejeu : %s", rec.Body.String()[:200])
	}
	if n := ext.callCount(); n != 0 {
		t.Fatalf("le fournisseur payant ne doit pas être sollicité : %d", n)
	}
	if lm := mismatchCounters(h); lm["rerouted"] != 1 || lm["kept"] != 0 {
		t.Fatalf("compteurs : %v", lm)
	}
}

// Le rejeu strict porte bien l'en-tête interne qui fait appliquer la consigne stricte au
// prompt système réécrit (mode custom), et le marqueur de rejeu.
func TestRejeuStrictPorteLEnTeteDeConsigne(t *testing.T) {
	var vu *http.Request
	ancien := leakRetryDo
	leakRetryDo = func(req *http.Request) (*http.Response, error) {
		vu = req
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"hello"}}]}`)),
		}, nil
	}
	defer func() { leakRetryDo = ancien }()

	h := &Handler{internalAddr: ":7863"}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	h.replayOnPool(httptest.NewRecorder(), req, []byte(`{"messages":[{"role":"user","content":"[1,2,3]"}]}`),
		"consigne-stricte-de-test", "claude-opus-5", false, &chatStat{start: time.Now()})

	if vu.Header.Get(leakRetryHeader) != leakRetrySecret {
		t.Fatal("le rejeu doit rester identifiable")
	}
	// Une consigne quelconque ne doit PAS poser l'en-tête strict : seul le texte exact de
	// la consigne stricte le fait.
	if got := vu.Header.Get(forceLangHeader); got != "" {
		t.Fatalf("en-tête strict posé à tort : %q", got)
	}
}

// La réécriture du prompt (mode custom) applique la consigne stricte sur un rejeu qui la
// demande, et la consigne normale partout ailleurs.
func TestConsigneStricteAppliqueeAuPromptReecrit(t *testing.T) {
	h := &Handler{}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if got := h.languageDirectiveFor(r); got != prompt.LanguageDirective {
		t.Fatalf("consigne normale attendue, obtenu %q", got)
	}
	r.Header.Set(leakRetryHeader, leakRetrySecret)
	r.Header.Set(forceLangHeader, forceLangStrict)
	if got := h.languageDirectiveFor(r); got != prompt.LanguageStrict {
		t.Fatalf("consigne stricte attendue, obtenu %q", got)
	}
}
