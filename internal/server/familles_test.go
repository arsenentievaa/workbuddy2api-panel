package server

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// Familles de modèles : un client qui teste « es-tu vraiment GPT ? » doit recevoir une
// réponse de GPT, avec la clé de GPT — pas de Claude.

func rerouterFamilles(t *testing.T) *Rerouter {
	t.Helper()
	cible := func(cle string) upstream.ExternalTarget {
		return upstream.ExternalTarget{BaseURL: "https://exemple.invalid", Path: "/v1/chat/completions", APIKey: cle}
	}
	stats := NewFPCounters(20, false, nil, 2, 100, 15*time.Minute)
	return &Rerouter{
		Enabled: true, Stats: stats, Upstream: &upstream.Client{},
		Health: upstream.NewExternalHealth(time.Minute),
		Families: []*Family{
			{Name: "claude", Prefixes: []string{"claude-"}, Models: []string{"claude-opus-5-5", "claude-opus-5"}, Target: cible("cle-claude")},
			{Name: "openai", Prefixes: []string{"gpt-", "o3-"}, Models: []string{"gpt-6.1-sol", "gpt-6-astra"}, Target: cible("cle-openai")},
			{Name: "gemini", Prefixes: []string{"gemini-"}, Models: []string{"gemini-3.8-flash"}, Target: cible("cle-gemini")},
			{Name: "grok", Prefixes: []string{"grok-"}, Models: []string{"grok-4.6"}, Target: cible("cle-grok")},
		},
	}
}

func TestFamilleChoisieParPrefixe(t *testing.T) {
	rr := rerouterFamilles(t)
	cas := map[string]string{
		"claude-opus-5-5":  "claude",
		"gpt-6":            "openai",
		"gpt-6.1-sol":      "openai",
		"o3-mini":          "openai",
		"gemini-3.8-flash": "gemini",
		"grok-4":           "grok",
	}
	for modele, attendu := range cas {
		f := rr.familyFor(modele)
		if f == nil || f.Name != attendu {
			t.Errorf("%s doit aller chez %s, obtenu %v", modele, attendu, f)
		}
	}
	if f := rr.familyFor("deepseek-v4.1-flash"); f != nil {
		t.Errorf("un modèle du parc ne doit pas avoir de famille : %v", f.Name)
	}
}

func TestModeleReelChoisiPourLaFamille(t *testing.T) {
	rr := rerouterFamilles(t)
	if ms := rr.modelesPour("gpt-6"); len(ms) == 0 || ms[0] != "gpt-6.1-sol" {
		t.Fatalf("gpt-6 doit viser un modèle réel de la famille, obtenu %v", ms)
	}
	// Un nom qui existe déjà chez le fournisseur est conservé en tête.
	if ms := rr.modelesPour("gpt-6-astra"); ms[0] != "gpt-6-astra" {
		t.Fatalf("le modèle demandé existant doit être conservé, obtenu %v", ms[0])
	}
}

// La clé utilisée est celle de la FAMILLE (vérifié sur l'en-tête d'autorisation réel).
func TestCleDeLaFamilleUtilisee(t *testing.T) {
	var vues []string
	ext := newFakeExternal(t, func(w http.ResponseWriter, r *http.Request, body string) {
		vues = append(vues, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"message":{"content":"ok"}}]}`))
	})
	rr := rerouterFamilles(t)
	for _, f := range rr.Families {
		f.Target.BaseURL = ext.srv.URL
	}
	if _, _, err := rr.appelExterne(context.Background(), []byte(`{"model":"x","messages":[]}`), "gpt-6", "", false, false); err != nil {
		t.Fatalf("appel : %v", err)
	}
	if len(vues) == 0 || vues[0] != "Bearer cle-openai" {
		t.Fatalf("la clé de la famille openai doit être utilisée, vu %v", vues)
	}
}

// Le fournisseur répond parfois « comptes épuisés » sur un modèle précis : le modèle
// suivant de la MÊME famille doit être essayé.
func TestModeleSuivantEssayeSiIndisponible(t *testing.T) {
	var modeles []string
	ext := newFakeExternal(t, func(w http.ResponseWriter, r *http.Request, body string) {
		if strings.Contains(body, "gpt-6.1-sol") {
			modeles = append(modeles, "gpt-6.1-sol")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":{"message":"All available accounts exhausted"}}`))
			return
		}
		modeles = append(modeles, "gpt-6-astra")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"message":{"content":"I am GPT-6."}}]}`))
	})
	rr := rerouterFamilles(t)
	for _, f := range rr.Families {
		f.Target.BaseURL = ext.srv.URL
	}
	if _, m, err := rr.appelExterne(context.Background(), []byte(`{"model":"x","messages":[]}`), "gpt-6", "", false, false); err != nil {
		t.Fatalf("le modèle suivant doit prendre le relais : %v", err)
	} else if m != "gpt-6-astra" {
		t.Fatalf("modèle utilisé = %s", m)
	}
	// gpt-6.1-sol est tenté deux fois (reprise sur 5xx) puis gpt-6-astra prend le relais.
	if len(modeles) != 3 || modeles[2] != "gpt-6-astra" {
		t.Fatalf("essais inattendus : %v", modeles)
	}
	_ = atomic.LoadInt64
}

// Si toute une famille est en panne (le fournisseur répond « comptes épuisés » par
// famille), la réponse vient d'une AUTRE famille de vrais modèles, jamais du parc : une
// réponse du parc trahirait le backend chinois, ce qui est exactement ce qu'on évite.
func TestRepliInterFamilles(t *testing.T) {
	var cles []string
	ext := newFakeExternal(t, func(w http.ResponseWriter, r *http.Request, body string) {
		cle := r.Header.Get("Authorization")
		cles = append(cles, cle)
		if cle == "Bearer cle-openai" {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":{"message":"All available accounts exhausted"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"message":{"content":"Je suis Claude."}}]}`))
	})
	rr := rerouterFamilles(t)
	for _, f := range rr.Families {
		f.Target.BaseURL = ext.srv.URL
	}
	res, m, err := rr.appelExterne(context.Background(), []byte(`{"model":"x","messages":[]}`), "gpt-6", "", false, false)
	if err != nil || res == nil {
		t.Fatalf("le repli doit servir une réponse : %v", err)
	}
	if m == "gpt-6.1-sol" || m == "gpt-6-astra" {
		t.Fatalf("le modèle servi doit venir de la famille de repli, obtenu %s", m)
	}
	if cles[len(cles)-1] != "Bearer cle-claude" {
		t.Fatalf("la dernière tentative doit utiliser la clé Claude, vu %v", cles)
	}
}
