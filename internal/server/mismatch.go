package server

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/alert"
	"github.com/linguo2625469/workbuddy2api-panel/internal/fpdetect"
	"github.com/linguo2625469/workbuddy2api-panel/internal/prompt"
)

// Signal « language_mismatch » : la réponse trahit le backend.
//
// Question posée sans un seul caractère CJK, réponse écrite en chinois : c'est l'aveu
// le plus direct qu'un modèle chinois répond à la place du modèle annoncé — plus direct
// qu'un jeton piège, qui demande d'être interprété. Le signal est donc FORT et route
// seul vers l'upstream externe.
//
// LA DIFFICULTÉ STRUCTURELLE, à énoncer : ce signal compare une entrée et une SORTIE.
// Au moment où le détecteur de requête s'exécute (avant toute sélection de compte), la
// sortie n'existe pas. Le signal vit donc sur le chemin de RÉPONSE, et « router vers
// CrazyToken » y prend deux formes différentes :
//
//   - non-flux : la réponse est complète et rien n'a encore été écrit au client (le
//     handler agrège puis écrit à la fin). On peut donc JETER la réponse incohérente et
//     rejouer la même requête sur l'upstream externe : le client reçoit la bonne.
//   - flux : les trames partent au fur et à mesure. On lit donc un PRÉFIXE avant
//     d'écrire quoi que ce soit, on décide, puis on relaie — soit l'externe, soit le
//     préfixe déjà lu suivi du reste du flux. Le préfixe est rejoué à l'identique, donc
//     le client ne voit aucune différence quand il n'y a pas de désaccord.
//
// Coût assumé du préfixe en flux : la première trame porteuse de contenu doit arriver
// avant la première écriture. C'est exactement l'attente que le client subit de toute
// façon pour voir du texte ; on n'ajoute pas de latence perceptible, mais on renonce à
// transmettre la trame « role: assistant » seule.
const (
	streamSniffMaxBytes  = 16 << 10
	streamSniffMaxFrames = 8
)

// sniffStreamPrefix lit le début d'un flux SSE et retourne le préfixe consommé ainsi
// qu'un lecteur qui reprend exactement à la suite (préfixe compris). Le lecteur retourné
// est indispensable : un bufio.Reader a pu lire au-delà de ce qui a été consommé, et
// repartir du ReadCloser d'origine perdrait ces octets.
//
// La lecture s'arrête dès qu'une trame porteuse de contenu est vue : c'est le premier
// instant où l'on peut trancher, et cela borne le délai ajouté à la première écriture.
func sniffStreamPrefix(rc io.Reader) ([]byte, io.Reader) {
	br := bufio.NewReaderSize(rc, 64*1024)
	var buf bytes.Buffer
	frames := 0
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			buf.Write(line)
			if bytes.HasPrefix(line, []byte("data:")) {
				frames++
				if carriesContent(line) {
					break
				}
			}
		}
		if err != nil {
			break
		}
		if frames >= streamSniffMaxFrames || buf.Len() >= streamSniffMaxBytes {
			break
		}
	}
	return buf.Bytes(), br
}

// carriesContent : vrai si une trame SSE transporte du texte d'assistant. Deux formes
// suffisent à couvrir les protocoles servis ici : OpenAI (`choices[].delta.content`) et
// Messages (`content_block_delta` → `text`). On ne décode pas le JSON : une trame peut
// être volumineuse et l'on veut uniquement savoir si du texte est présent.
func carriesContent(line []byte) bool {
	s := string(line)
	for _, key := range []string{`"content":"`, `"text":"`} {
		i := strings.Index(s, key)
		if i < 0 {
			continue
		}
		if !strings.HasPrefix(s[i+len(key):], `"`) {
			return true
		}
	}
	return false
}

// languageMismatchEnabled : vrai si le signal peut avoir un effet. Sert à ne PAS payer
// le coût du préfixe (une écriture retardée d'une trame) quand la fonction est éteinte :
// un déploiement sans reroutage actif ne doit rien changer à son comportement.
func (h *Handler) languageMismatchEnabled() bool {
	rr := h.cfg.Rerouter
	if rr == nil || !rr.Enabled || rr.DryRun || h.cfg.FPDetect == nil {
		return false
	}
	return h.cfg.FPDetect.WantsLanguageMismatch()
}

// rerouteOnMismatch arbitre puis exécute le reroutage pour un désaccord de langue déjà
// détecté. Retourne true si la réponse du client a été écrite (l'appelant doit return).
//
// Le compte du compte CodeBuddy a DÉJÀ été débité pour l'appel qui a produit la réponse
// incohérente : c'est honnête, ces jetons ont réellement été consommés. Ce qu'on refuse,
// c'est de laisser la réponse trompeuse partir.
func (h *Handler) rerouteOnMismatch(w http.ResponseWriter, r *http.Request, body []byte, res fpdetect.Result,
	clientModel string, stream bool, st *chatStat) bool {

	rr := h.cfg.Rerouter
	if h.cfg.FPStats != nil {
		h.cfg.FPStats.NoteMismatch(res)
	}
	log.Printf("[fp] DÉSACCORD DE LANGUE détecté model=%s %s", clientModel, res.ExplainDetailed())
	if rr == nil || !rr.Enabled || rr.DryRun {
		return false
	}
	now := time.Now()
	key := rerouteIdentity(r, body, rr.ClientIDHeader)
	allowed, reason := rr.Decide(r.Context(), key, now)
	if !allowed {
		// Refus (plafond atteint, upstream malsain) : le client garde bel et bien la
		// réponse incohérente. C'est une fuite subie, donc elle compte au même titre
		// qu'un échec d'appel — sinon l'alerte « kept > 0 » raterait précisément les
		// pannes de fournisseur et les plafonds, c'est-à-dire les cas les plus probables.
		if h.cfg.FPStats != nil {
			h.cfg.FPStats.NoteMismatchKept(now)
		}
		log.Printf("[fp] désaccord de langue : reroutage refusé (%s) -> FUITE SUBIE par le client", reason)
		return false
	}
	st.route = "crazy"
	// Le prompt système de la passerelle est en chinois : sans cette consigne, le
	// modèle de remplacement répond en chinois à son tour et le client ne voit aucune
	// différence — on aurait déplacé le symptôme au lieu de le corriger. La directive
	// n'est ajoutée QUE sur ce chemin.
	outbound := prompt.Append(body, prompt.LanguageDirective)
	if rr.Try(r.Context(), w, r, outbound, clientModel, stream, st, key) {
		if h.cfg.FPStats != nil {
			h.cfg.FPStats.NoteMismatchRerouted()
		}
		return true
	}
	// L'upstream externe n'a pas répondu : on sert la réponse d'origine plutôt que rien.
	// Elle est incohérente, mais c'est ce que le client serait de toute façon destiné à
	// recevoir en cas de panne — et le signal reste compté et journalisé.
	if h.cfg.FPStats != nil {
		h.cfg.FPStats.NoteMismatchKept(now)
	}
	st.route = ""
	log.Printf("[fp] désaccord de langue : reroutage impossible -> FUITE SUBIE par le client (réponse d'origine servie)")
	return false
}

// rerouteOnStreamMismatch : version flux, à partir du préfixe déjà lu.
func (h *Handler) rerouteOnStreamMismatch(w http.ResponseWriter, r *http.Request, body, prefix []byte,
	clientModel string, st *chatStat) bool {

	res := h.cfg.FPDetect.AnalyzeStreamPrefix(body, prefix)
	if !res.Route {
		return false
	}
	return h.rerouteOnMismatch(w, r, body, res, clientModel, true, st)
}

// completionText extrait le texte de l'assistant d'une réponse non streamée, quel que
// soit le protocole : `choices[].message.content` (OpenAI) ou `content[].text`
// (Messages). Une réponse sans texte (appel d'outil pur) donne "".
func completionText(resp map[string]any) string {
	var b strings.Builder
	if choices, ok := resp["choices"].([]any); ok {
		for _, c := range choices {
			m, ok := c.(map[string]any)
			if !ok {
				continue
			}
			msg, ok := m["message"].(map[string]any)
			if !ok {
				continue
			}
			switch v := msg["content"].(type) {
			case string:
				b.WriteString(v)
			case []any:
				for _, part := range v {
					if p, ok := part.(map[string]any); ok {
						if t, ok := p["text"].(string); ok {
							b.WriteString(t)
						}
					}
				}
			}
		}
	}
	if content, ok := resp["content"].([]any); ok {
		for _, part := range content {
			if p, ok := part.(map[string]any); ok {
				if t, ok := p["text"].(string); ok {
					b.WriteString(t)
				}
			}
		}
	}
	return b.String()
}

// EvaluateLanguageLeak surveille la fuite de langue sur une fenêtre GLISSANTE.
//
// Objet : `kept` compte les réponses incohérentes qui ont été servies au client alors
// que le reroutage devait les remplacer — parce qu'il a échoué (transport, réponse
// illisible, flux déjà ouvert) OU parce qu'il a été refusé (plafond anti-abus atteint,
// upstream externe malsain). Dans tous ces cas le client subit la fuite : c'est
// exactement ce qu'il faut savoir tout de suite, parce que le remède dépend de la cause
// (rétablir le fournisseur, relever un plafond, corriger la configuration).
//
// Appelée périodiquement (jamais sur le chemin de requête). Retourne true si une alerte
// a été émise, ce qui rend la décision testable sans réseau.
func (rr *Rerouter) EvaluateLanguageLeak(now time.Time) bool {
	if rr == nil || rr.Stats == nil || rr.Alerts == nil {
		return false
	}
	window := rr.Stats.LanguageLeakWindow()
	n := rr.Stats.MismatchKeptInWindow(now)
	if n <= 0 {
		return false
	}
	sante := "inconnue"
	if h := rr.Health.Snapshot(); h.Measured {
		if h.Healthy {
			sante = "saine"
		} else {
			sante = "MALSAINE (" + h.LastError + ")"
		}
	}
	total, rerouted := rr.Stats.RateCounters()
	// La fenêtre de déduplication est celle de la surveillance : tant que la fuite
	// dure, on le rappelle à chaque fenêtre, plutôt que de laisser l'incident se
	// prolonger derrière une alerte déjà envoyée il y a longtemps.
	return rr.Alerts.SendWindow(alert.KeyLanguageLeak, fmt.Sprintf(
		"🔴 FUITE DE LANGUE : %d réponse(s) incohérente(s) servie(s) au client sur les %d dernières minutes "+
			"(question sans caractère chinois, réponse en chinois qui a atteint le client). "+
			"Le reroutage devait la remplacer : il a échoué ou a été refusé. "+
			"Upstream externe : %s. %d reroutage(s) réussis au total sur %d requêtes analysées. "+
			"À vérifier : santé du fournisseur, plafond anti-abus, /status (fp_observe.language_leak).",
		n, int(window.Minutes()), sante, rerouted, total), window)
}
