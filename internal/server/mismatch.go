package server

import (
	"bufio"
	"bytes"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

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
	// Bornes larges : un modèle qui raisonne émet des centaines de trames internes avant
	// la première trame de texte. Avec 40 trames, on relâchait l'inspection avant d'avoir
	// vu le moindre texte visible — et la réponse chinoise passait (constaté le
	// 2026-10-20 sur le message « go » en flux).
	streamSniffMaxBytes  = 1 << 20
	streamSniffMaxFrames = 400

	// Seuils de décision du préfixe : deux caractères CJK visibles suffisent à conclure
	// (réponse chinoise, y compris brève), sinon 24 lettres latines — assez pour qu'une
	// réponse soit lisiblement dans la langue du client, assez tôt pour ne pas retarder
	// la première écriture de plusieurs secondes.
	// Avant, on s'arrêtait à la première trame porteuse de contenu : une réponse qui
	// commençait par un emoji puis basculait en chinois passait sans être vue (2026-10-20).
	streamDecideCJK     = 2
	streamDecideLetters = 24

	// Une attente bornée dans le temps : au-delà, on cesse d'inspecter et on relaie tel
	// quel. Le client ne voit de toute façon rien tant que le modèle n'a pas commencé à
	// répondre ; cette borne évite qu'un long raisonnement interne retarde la première
	// écriture.
	streamSniffMaxWait = 20 * time.Second
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
	deadline := time.Now().Add(streamSniffMaxWait)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			buf.Write(line)
			if bytes.HasPrefix(line, []byte("data:")) {
				frames++
				// Un appel d'outil n'a pas de langue : inutile d'attendre davantage.
				if fpdetect.HasToolCallFrame(line) {
					break
				}
				if cjk, letters := fpdetect.StreamFeatures(buf.Bytes()); cjk >= streamDecideCJK || letters >= streamDecideLetters {
					break
				}
			}
		}
		if err != nil {
			break
		}
		if frames >= streamSniffMaxFrames || buf.Len() >= streamSniffMaxBytes || time.Now().After(deadline) {
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
	// Un rejeu de remède ne se remédie pas lui-même (voir leak_retry.go) : sans cette
	// garde, une réponse rejouée qui fuit encore déclencherait un rejeu sans fin.
	if h.isLeakRetry(r) {
		return false
	}

	// 1) REMÈDE PAR LE PARC, HORS BUDGET : rejouer la requête avec la consigne de langue.
	//    Le budget anti-abus (5/h par client, 30/h au total) borne la dépense vers le
	//    FOURNISSEUR EXTERNE ; il ne doit pas s'appliquer à un rejeu interne, qui ne coûte
	//    qu'un appel de parc. Constaté le 2026-10-20 : il s'appliquait aux deux, si bien que
	//    527 désaccords détectés donnaient 10 remèdes et 511 fuites subies par le client
	//    (97 %) — le plafond était épuisé en permanence (over_cap 511).
	if h.replayOnPool(w, r, body, prompt.LanguageGuard, clientModel, stream, st) {
		if h.cfg.FPStats != nil {
			h.cfg.FPStats.NoteMismatchRerouted()
		}
		return true
	}

	// 2) SECOND REJEU, CONSIGNE STRICTE : quand la première reprise est revenue en chinois
	//    elle aussi, on redemande une fois avec une consigne qui interdit explicitement les
	//    caractères chinois. Cas visé : les messages sans langue (« 👋 ») où le modèle
	//    s'accroche au chinois ; la consigne normale n'y suffisait pas toujours.
	if h.replayOnPool(w, r, body, prompt.LanguageStrict, clientModel, stream, st) {
		if h.cfg.FPStats != nil {
			h.cfg.FPStats.NoteMismatchRerouted()
		}
		return true
	}

	// Le budget ne couvre donc plus que le remède payant, et il est consulté ICI.
	budgetOK, budgetReason := true, ""
	if rr.Stats != nil {
		budgetOK, budgetReason = rr.Stats.AllowLeakReroute(key, now)
	}

	// 2) REMÈDE PAR LE FOURNISSEUR EXTERNE (chemin historique). La santé de ce fournisseur est
	//    vérifiée ici et non avant le rejeu : elle ne conditionne que ce chemin. La logique de
	//    DecideLeak est reprise sur place pour ne pas consommer le budget une seconde fois.
	healthy, justDown := rr.probeFamily(r.Context(), clientModel)
	if !healthy && justDown && rr.Stats != nil {
		rr.Stats.NoteHealthBlocked()
	}
	allowed, reason := budgetOK, budgetReason
	if allowed && !healthy {
		allowed, reason = false, reasonHealth
	}
	if !allowed {
		// Refus (budget dédié atteint, upstream malsain) : plus aucune reprise n'est
		// possible. La réponse chinoise n'est PAS servie (règle du 2026-10-20 : rien ne
		// passe hors conversation réellement chinoise) — le client reçoit une erreur
		// claire, qu'il peut relancer, au lieu d'un texte inutilisable.
		if h.cfg.FPStats != nil {
			h.cfg.FPStats.NoteMismatchKept(now)
		}
		log.Printf("[fp] désaccord de langue : remède refusé (%s) -> réponse chinoise ÉCARTÉE, erreur renvoyée", reason)
		writeOpenAIError(w, http.StatusBadGateway, "language_guard",
			"the answer came back in an unexpected language; please retry")
		st.status = http.StatusBadGateway
		return true
	}
	st.route = "crazy"
	// Le prompt système de la passerelle est en chinois : sans cette consigne, le
	// modèle de remplacement répond en chinois à son tour et le client ne voit aucune
	// différence — on aurait déplacé le symptôme au lieu de le corriger. La directive
	// n'est ajoutée QUE sur ce chemin.
	outbound := prompt.Append(body, prompt.LanguageStrict)
	if h.tryExternalVerified(w, r, body, outbound, clientModel, stream, st) {
		if h.cfg.FPStats != nil {
			h.cfg.FPStats.NoteMismatchRerouted()
		}
		return true
	}
	// Toutes les reprises ont échoué (parc normal, parc strict, fournisseur externe
	// vérifié). La réponse chinoise n'est pas servie : une erreur claire vaut mieux qu'un
	// texte que le client ne peut pas lire, et elle est relançable.
	if h.cfg.FPStats != nil {
		h.cfg.FPStats.NoteMismatchKept(now)
	}
	st.route = ""
	log.Printf("[fp] désaccord de langue : toutes les reprises échoué -> réponse chinoise ÉCARTÉE, erreur renvoyée")
	writeOpenAIError(w, http.StatusBadGateway, "language_guard",
		"the answer came back in an unexpected language; please retry")
	st.status = http.StatusBadGateway
	return true
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
	// La fenêtre de déduplication est celle de la surveillance : tant que la fuite
	// dure, on le rappelle à chaque fenêtre, plutôt que de laisser l'incident se
	// prolonger derrière une alerte déjà envoyée il y a longtemps.
	// Notifications : l'exploitant ne veut que les evenements majeurs d'infrastructure.
	// Une fuite de langue est desormais journalisee, jamais notifiee.
	log.Printf("[fp] fuite de langue : %d reponse(s) incoherente(s) sur les %d dernieres minutes",
		n, int(window.Minutes()))
	return false
}
