package server

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/prompt"
)

// Remède aux fuites de langue PAR LE PARC.
//
// Objet : quand une réponse part dans la mauvaise langue (question sans caractère chinois,
// réponse en chinois), il faut la remplacer. Le remède historique passe par le fournisseur
// externe (vrai Claude) — coûteux, plafonné, et inopérant quand son compte est à sec : mesuré
// le 2026-10-05, 46 désaccords détectés, 46 subis par le client, parce que ce compte répondait
// 403 « Insufficient balance ».
//
// Ce remède-ci rejoue la requête SUR LE PARC, avec la consigne de langue du chemin de remède.
// Il ne dépend d'aucun fournisseur externe. Ordre d'essai dans mismatch.go :
//   1. rejeu sur le parc (ce fichier) — rapide, sans plafond de fournisseur ;
//   2. reroutage vers le fournisseur externe (existant), si le rejeu n'a rien donné ;
//   3. repli : la réponse d'origine (existante).
//
// Coût assumé : un appel amont supplémentaire sur le parc (le compte CodeBuddy est débité une
// seconde fois). C'est le prix du remède, et le budget existant (plafond par client et global)
// le borne.

// leakRetryHeader : marqueur du rejeu. La valeur est un secret tiré au démarrage, jamais
// exposé : un client ne peut donc pas fabriquer l'en-tête pour se soustraire à la sonde
// d'authenticité ni au remède.
const leakRetryHeader = "X-Wb2a-Leak-Retry"

var leakRetrySecret = func() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "wb2a"
	}
	return hex.EncodeToString(b)
}()

// leakRetryDo est la fonction d'envoi ; variable pour que les tests l'injectent.
var leakRetryDo = func(req *http.Request) (*http.Response, error) {
	return http.DefaultClient.Do(req)
}

// isLeakRetry : la requête est-elle un rejeu de remède (et non une requête client) ?
func (h *Handler) isLeakRetry(r *http.Request) bool {
	return r != nil && r.Header.Get(leakRetryHeader) == leakRetrySecret
}

// internalAddr : adresse à composer pour le rejeu interne.
//
// On dial 127.0.0.1 et NON le nom public : le passage par l'ingresse serait un aller-retour
// externe, et surtout il peut rediriger ou retirer des en-têtes — le marqueur ci-dessus ne
// survivrait pas, et le rejeu pourrait se rejouer lui-même.
func internalAddr(listen string) string {
	listen = strings.TrimSpace(listen)
	if listen == "" {
		return "127.0.0.1:7863"
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// hopByHop : en-têtes qui ne se transmettent pas dans un rejeu.
var hopByHop = map[string]bool{
	"connection": true, "keep-alive": true, "proxy-authenticate": true,
	"proxy-authorization": true, "te": true, "trailer": true,
	"transfer-encoding": true, "upgrade": true, "content-length": true,
}

// replayOnPool rejoue la requête sur le parc avec la consigne de langue et sert la réponse au
// client. Retourne true si le client a été servi (l'appelant doit alors retourner sans rien
// écrire de plus).
//
// Toute issue false signifie « garder la réponse d'origine » : c'est le fail-open, et c'est le
// cas par défaut dès qu'un doute existe (transport, statut non-2xx).
func (h *Handler) replayOnPool(w http.ResponseWriter, r *http.Request, body []byte, clientModel string,
	stream bool, st *chatStat) bool {

	addr := internalAddr(h.internalAddr)
	url := "http://" + addr + r.URL.Path
	outbound := prompt.Append(body, prompt.LanguageDirective)

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(outbound))
	if err != nil {
		log.Printf("[fp] remède par le parc : requête interne impossible (%v)", err)
		return false
	}
	for k, vs := range r.Header {
		if hopByHop[strings.ToLower(k)] || strings.EqualFold(k, leakRetryHeader) {
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(leakRetryHeader, leakRetrySecret)
	// Le rejeu doit rester identifiable dans le journal du handler interne.
	if clientModel != "" {
		req.Header.Set("X-Origin-Model", clientModel)
	}

	resp, err := leakRetryDo(req)
	if err != nil {
		log.Printf("[fp] remède par le parc : échec de transport (%v) -> repli", err)
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Le parc n'a pas su répondre : on garde la réponse d'origine plutôt que de servir
		// une erreur à la place d'une réponse (fût-elle dans la mauvaise langue).
		log.Printf("[fp] remède par le parc : statut %d -> repli sur la réponse d'origine", resp.StatusCode)
		return false
	}

	// VERIFICATION DU REJEU : rien n'a encore été écrit au client. Une réponse rejouée qui
	// serait ENCORE dans la mauvaise langue ne vaut pas mieux que l'originale — la servir
	// priverait le chemin payant (fournisseur externe) de sa chance tout en comptant un
	// remède qui n'en est pas un. On rend donc la main sans rien écrire : le client garde
	// sa réponse d'origine et l'appelant poursuit vers l'étape suivante.
	//
	// Constaté le 2026-10-20 : sans cette vérification, le compteur `rerouted` comptait des
	// rejeux restés en chinois comme des remèdes réussis (527 désaccords, 10 « remèdes »,
	// aucun moyen de savoir combien avaient réellement changé la langue).
	var prefix []byte
	var rest io.Reader
	if stream {
		prefix, rest = sniffStreamPrefix(resp.Body)
		if h.cfg.FPDetect != nil {
			if res := h.cfg.FPDetect.AnalyzeStreamPrefix(body, prefix); res.Route {
				log.Printf("[fp] remède par le parc : le rejeu repart en CJK (%s) -> repli", res.Explain())
				st.route = ""
				return false
			}
		}
	} else {
		data, rerr := io.ReadAll(resp.Body)
		if rerr != nil {
			log.Printf("[fp] remède par le parc : lecture interrompue (%v) -> repli", rerr)
			return false
		}
		if h.cfg.FPDetect != nil {
			var parsed map[string]any
			if json.Unmarshal(data, &parsed) == nil {
				if res := h.cfg.FPDetect.AnalyzeResponse(body, completionText(parsed)); res.Route {
					log.Printf("[fp] remède par le parc : le rejeu repart en CJK (%s) -> repli", res.Explain())
					st.route = ""
					return false
				}
			}
		}
		prefix = data
	}

	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)

	var copyErr error
	if stream {
		if _, err := w.Write(prefix); err != nil {
			copyErr = err
		} else if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
			_, copyErr = io.Copy(&flushWriter{w: w, fl: fl}, rest)
		} else {
			_, copyErr = io.Copy(w, rest)
		}
	} else {
		_, copyErr = w.Write(prefix)
	}
	if copyErr != nil {
		// Le client a déjà reçu des octets : on ne peut plus rien basculer, on le signale.
		log.Printf("[fp] remède par le parc : copie interrompue (%v)", copyErr)
		st.status = resp.StatusCode
		return true
	}
	st.status = resp.StatusCode
	st.route = "parc"
	log.Printf("[fp] désaccord de langue : remède par le PARC (rejeu avec consigne de langue) model=%s stream=%v", clientModel, stream)
	return true
}
