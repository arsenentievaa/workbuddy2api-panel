package main

import (
	"strings"
	"testing"
)

func TestEtatParc(t *testing.T) {
	cas := []struct {
		total, healthy int
		attendu        string
	}{
		{50, 50, "ok"},
		{50, 49, "degrade"},
		{50, 40, "degrade"},
		{50, 24, "critique"},
		{50, 25, "degrade"},
		{50, 10, "critique"},
		{50, 26, "degrade"},
		{0, 0, "ok"},
	}
	for _, c := range cas {
		if got := etatParc(c.total, c.healthy); got != c.attendu {
			t.Errorf("etatParc(%d,%d) = %q, attendu %q", c.total, c.healthy, got, c.attendu)
		}
	}
}

// Les messages ne doivent contenir ni identifiant de compte, ni code d'erreur, ni nom
// de fichier : l'exploitant veut une information generale, comprehensible.
func TestMessageParcSansJargon(t *testing.T) {
	for _, etat := range []string{"ok", "critique"} {
		texte, cle, fenetre := messageParc(etat, 40, 50)
		if cle == "" || fenetre <= 0 {
			t.Errorf("%s : cle ou fenetre manquante", etat)
		}
		for _, interdit := range []string{"uid", "http", "rate", "6004", "pool", "fp_", "/status", "WorkBuddy"} {
			if strings.Contains(strings.ToLower(texte), strings.ToLower(interdit)) {
				t.Errorf("%s : terme technique %q dans le message : %q", etat, interdit, texte)
			}
		}
		if len([]rune(texte)) > 240 {
			t.Errorf("%s : message trop long (%d caracteres)", etat, len([]rune(texte)))
		}
	}
	// Un parc simplement ralenti ne produit AUCUN message.
	if texte, _, _ := messageParc("degrade", 40, 50); texte != "" {
		t.Errorf("l'etat intermediaire ne doit envoyer aucun message, obtenu %q", texte)
	}
}
