package server

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/prompt"
)

// L'adresse du rejeu reste DANS le conteneur : passer par le nom public ferait un aller-retour
// par l'ingresse, qui peut retirer des en-têtes — le marqueur ne survivrait pas et le rejeu
// pourrait se rejouer lui-même.
func TestInternalAddr(t *testing.T) {
	cas := map[string]string{
		":7863":          "127.0.0.1:7863",
		"0.0.0.0:7863":   "127.0.0.1:7863",
		"127.0.0.1:8080": "127.0.0.1:8080",
		"":               "127.0.0.1:7863",
		"localhost:9000": "localhost:9000",
	}
	for entree, attendu := range cas {
		if got := internalAddr(entree); got != attendu {
			t.Errorf("internalAddr(%q) = %q, attendu %q", entree, got, attendu)
		}
	}
}

// Le marqueur de rejeu n'est reconnu que par le secret tiré au démarrage : un client ne peut
// pas se soustraire à la sonde ni au remède en fabriquant l'en-tête.
func TestIsLeakRetryNeReconnaitQueLeSecret(t *testing.T) {
	h := &Handler{}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if h.isLeakRetry(r) {
		t.Fatal("une requête sans en-tête n'est pas un rejeu")
	}
	r.Header.Set(leakRetryHeader, "valeur-inventee")
	if h.isLeakRetry(r) {
		t.Fatal("une valeur inventée ne doit pas passer pour le secret")
	}
	r.Header.Set(leakRetryHeader, leakRetrySecret)
	if !h.isLeakRetry(r) {
		t.Fatal("le secret doit être reconnu")
	}
}

// Le rejeu porte la consigne de langue, le marqueur, et sert la réponse du parc au client.
func TestReplayOnPoolSertLaReponseEtMarqueLaRequete(t *testing.T) {
	var vu *http.Request
	var corps []byte
	ancien := leakRetryDo
	leakRetryDo = func(req *http.Request) (*http.Response, error) {
		vu = req
		corps, _ = io.ReadAll(req.Body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"Yaoundé"}}]}`)),
		}, nil
	}
	defer func() { leakRetryDo = ancien }()

	h := &Handler{internalAddr: ":7863"}
	corpsRequete := []byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"continue"}]}`)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(corpsRequete))
	r.Header.Set("Authorization", "Bearer cle-client")
	st := &chatStat{start: time.Now()}

	if !h.replayOnPool(rec, r, corpsRequete, "claude-opus-5", false, st) {
		t.Fatal("un 200 du parc doit être servi au client")
	}
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Yaoundé") {
		t.Fatalf("réponse mal recopiée : %d %s", rec.Code, rec.Body.String())
	}
	if vu.Header.Get(leakRetryHeader) != leakRetrySecret {
		t.Fatal("le rejeu doit porter le marqueur secret")
	}
	if vu.Header.Get("Authorization") != "Bearer cle-client" {
		t.Fatal("l'authentification du client doit être reprise")
	}
	if !strings.Contains(string(corps), prompt.LanguageDirective) {
		t.Fatalf("le rejeu doit porter la consigne de langue : %s", string(corps))
	}
	if st.route != "parc" {
		t.Fatalf("la route doit être signalée, obtenu %q", st.route)
	}
}

// Un parc en erreur ne doit PAS remplacer la réponse d'origine : on garde ce qu'on a.
func TestReplayOnPoolRefuseSurStatutNon2xx(t *testing.T) {
	ancien := leakRetryDo
	leakRetryDo = func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusBadGateway, Body: io.NopCloser(strings.NewReader("nope"))}, nil
	}
	defer func() { leakRetryDo = ancien }()

	h := &Handler{internalAddr: ":7863"}
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	if h.replayOnPool(rec, r, []byte(`{}`), "claude-opus-5", false, &chatStat{start: time.Now()}) {
		t.Fatal("un 502 ne doit pas être servi au client")
	}
	if rec.Code == http.StatusBadGateway {
		t.Fatal("rien ne doit avoir été écrit")
	}
}
