package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/alert"
	"github.com/linguo2625469/workbuddy2api-panel/internal/fpdetect"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// Reroutage d'une sonde d'empreinte vers un upstream externe (CrazyToken).
//
// PRINCIPE : le reroutage est un chemin PARALLÈLE, pas une branche du chemin normal.
// Il ne touche ni le pool de comptes, ni la facturation, ni la session collante. Si
// quoi que ce soit échoue — configuration, santé, plafond, transport, réponse non-2xx —
// la requête repart sur le chemin normal (fail-open) sans qu'aucun compteur de compte
// n'ait bougé. C'est la seule façon d'ajouter une dépendance externe sans ajouter un
// mode de panne.
//
// Conséquence volontaire : un reroutage n'appelle jamais recordAttempt, applyErrorPolicy,
// fail, NoteSuccess ni Session.Bind. Ces fonctions existent pour arbitrer entre
// COMPTES ; sans compte, elles n'ont pas de sens et pourraient empoisonner le pool
// (un 401 du fournisseur externe ne doit pas refroidir un compte CodeBuddy).

// Motifs de non-reroutage, journalisés tels quels (jamais renvoyés au client).
const (
	reasonDisabled  = "désactivé"
	reasonDryRun    = "dry_run"
	reasonHealth    = "upstream externe malsain"
	reasonCapClient = "plafond par client atteint"
	reasonCapGlobal = "plafond global atteint"
	reasonTransport = "échec d'appel externe"
	reasonBadStatus = "réponse externe non-2xx"
	reasonBadBody   = "corps de réponse externe illisible"
)

// Rerouter porte l'état du reroutage : cible, santé mise en cache, compteurs, alertes.
type Rerouter struct {
	Enabled bool
	DryRun  bool

	// Target : base URL, chemin et clé du fournisseur externe.
	Target upstream.ExternalTarget
	// MessagesPath : endpoint Messages (Anthropic) du même fournisseur.
	MessagesPath string
	// ClientIDHeader : en-tête portant l'identité du client final, injecté par
	// l'amont (NewAPI : `header_override` = x-client-id -> {client_id}). Vide =
	// repli sur la conversation puis sur l'autorisation.
	ClientIDHeader string
	// HealthPath : endpoint interrogé par la sonde de santé.
	HealthPath string
	// ModelDefault : modèle demandé au fournisseur externe quand la requête n'en
	// nomme pas un qu'il connaît.
	ModelDefault string
	// ModelPrefixes : préfixes de modèles que l'on transmet tels quels (le client a
	// demandé un modèle réel du fournisseur). Hors de ces préfixes, on substitue
	// ModelDefault — un nom de modèle CodeBuddy n'existe pas chez le fournisseur.
	ModelPrefixes []string
	// MaxTokensCeiling : borne haute des max_tokens d'une requête reroutée. Une sonde
	// qui demande 200 000 jetons de sortie se facture au prix du vrai modèle : la
	// sonde est un test, pas une commande.
	MaxTokensCeiling int64
	// RateAlertPercent / RateMinSample : surveillance du taux de reroutage.
	RateAlertPercent float64
	RateMinSample    int64

	Upstream *upstream.Client
	Health   *upstream.ExternalHealth
	Stats    *FPCounters
	Alerts   *alert.Notifier
}

// rerouteIdentity : clé d'isolation du plafond anti-abus.
//
// Ordre de préférence :
//
//  1. `X-Client-Id` (nom configurable) : l'identité du client final, injectée par la
//     passerelle amont (NewAPI) depuis SON contexte de relais. C'est la seule unité
//     qui corresponde à « un client » : elle permet de plafonner un client sans
//     pénaliser les autres.
//  2. La clé de conversation du corps : repli quand l'en-tête est absent (appel direct,
//     ancien amont), avec une granularité par conversation.
//  3. L'empreinte de l'en-tête d'autorisation : dernier recours. Derrière NewAPI cette
//     valeur est CONSTANTE pour tous les clients, donc ce repli équivaut à un seau
//     partagé — c'est précisément ce que l'en-tête (1) vient corriger.
//
// PORTÉE DE CONFIANCE : la passerelle ne peut pas vérifier elle-même la valeur de
// l'en-tête ; elle fait confiance à l'amont qui l'injecte. Deux conséquences assumées :
//   - NewAPI doit résoudre `X-Client-Id` CÔTÉ SERVEUR (`{client_id}`), jamais recopier
//     un en-tête fourni par le client : sinon un client obtiendrait un quota neuf en
//     changeant la valeur à chaque requête, et le plafond ne vaudrait rien ;
//   - un appel direct à la passerelle (hors amont) peut forger cet en-tête. Le risque
//     est borné par le fait que la clé de données de la passerelle est un secret
//     partagé avec l'amont, et par le plafond global, qui reste l'ultime garde-fou.
func rerouteIdentity(r *http.Request, body []byte, headerName string) string {
	if headerName != "" {
		if v := strings.TrimSpace(r.Header.Get(headerName)); v != "" {
			return "client:" + truncateIdentity(v)
		}
	}
	if k := session.ExtractKey(body); k != "" {
		return "conv:" + k
	}
	if a := r.Header.Get("Authorization"); a != "" {
		return "auth:" + shortHash(a)
	}
	if k := r.Header.Get("x-api-key"); k != "" {
		return "key:" + shortHash(k)
	}
	return ""
}

// truncateIdentity borne la longueur d'une valeur d'en-tête reprise comme clé de
// compteur. Une valeur non bornée laisserait un client gonfler la table des quotas
// (chaque valeur distincte crée une entrée) — un déni de service par la mémoire.
const identityMaxLen = 128

func truncateIdentity(v string) string {
	v = strings.Map(func(r rune) rune {
		// Les caractères de contrôle n'ont rien à faire dans une clé journalisée.
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, v)
	if len(v) > identityMaxLen {
		v = v[:identityMaxLen]
	}
	return v
}

// shortHash : identifiant journalisable, jamais le secret en clair.
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:6])
}

// Probe interroge la santé du fournisseur externe (mise en cache) et alerte sur
// bascule. Retourne (sain, viennent_de_tomber).
func (rr *Rerouter) Probe(ctx context.Context) (bool, bool) {
	if rr == nil || rr.Health == nil {
		return false, false
	}
	// La sonde interroge le chemin de SANTÉ, pas le chemin de complétion : interroger
	// /v1/chat/completions en GET renvoyait un 404 et l'upstream était déclaré malsain
	// en permanence — le reroutage ne partait jamais. HealthPath était configuré mais
	// jamais utilisé.
	probe := rr.Target
	if strings.TrimSpace(rr.HealthPath) != "" {
		probe.Path = rr.HealthPath
	}
	ok, transition, err := rr.Health.Healthy(ctx, func(ctx context.Context) error {
		return rr.Upstream.ProbeExternal(ctx, probe)
	})
	if transition && rr.Alerts != nil {
		if ok {
			rr.Alerts.Send(alert.KeyHealthUp, "✅ CrazyToken de nouveau sain (sonde /v1/models 2xx). Le reroutage des sondes reprend.")
		} else {
			rr.Alerts.Send(alert.KeyHealthDown, fmt.Sprintf(
				"🔴 CrazyToken malsain : sonde échouée (%v). Les sondes repartent sur WorkBuddy (fail-open). Aucun reroutage tant que la sonde ne repasse pas.",
				trimErr(err)))
		}
	}
	return ok, transition && !ok
}

func trimErr(err error) string {
	if err == nil {
		return "inconnu"
	}
	s := err.Error()
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// Decide arbitre le reroutage : santé puis plafonds. Consomme le quota quand il
// autorise. Retourne (autorise, motif).
func (rr *Rerouter) Decide(ctx context.Context, key string, now time.Time) (bool, string) {
	if rr == nil || !rr.Enabled {
		return false, reasonDisabled
	}
	ok, justDown := rr.Probe(ctx)
	if !ok {
		if justDown && rr.Stats != nil {
			rr.Stats.NoteHealthBlocked()
		}
		return false, reasonHealth
	}
	allowed, reason := rr.Stats.AllowReroute(key, now)
	if !allowed {
		if rr.Alerts != nil {
			switch reason {
			case reasonCapClient:
				rr.Alerts.Send(alert.KeyCapReached, fmt.Sprintf(
					"⚠️ Plafond anti-abus atteint : %d reroutages/heure pour un client (%s). Les sondes de ce client repartent sur WorkBuddy jusqu'à libération du quota.",
					rr.Stats.CapPerHour(), key))
			case reasonCapGlobal:
				rr.Alerts.Send(alert.KeyGlobalCap, fmt.Sprintf(
					"⚠️ Plafond GLOBAL de reroutage atteint : %d/heure tous clients confondus. Toutes les sondes repartent sur WorkBuddy. Vérifier s'il s'agit d'un usage légitime ou d'un abus.",
					rr.Stats.GlobalCapPerHour()))
			}
		}
	}
	return allowed, reason
}

// DecideLeak : même arbitrage que Decide, mais sur le budget PROPRE au remède d'une
// fuite de langue (voir FPCounters.AllowLeakReroute).
//
// La sonde de santé reste exigée : appeler un fournisseur mort ne remplace rien. En
// revanche le plafond anti-abus des SONDES n'est pas consulté — une fuite de langue est
// un défaut déjà visible par le client, pas une dépense de confort. Le 2026-10-02, les
// quatre fuites détectées ont toutes été servies au client parce que ce quota partagé
// était épuisé par les sondes du même client.
func (rr *Rerouter) DecideLeak(ctx context.Context, key string, now time.Time) (bool, string) {
	if rr == nil || !rr.Enabled {
		return false, reasonDisabled
	}
	ok, justDown := rr.Probe(ctx)
	if !ok {
		if justDown && rr.Stats != nil {
			rr.Stats.NoteHealthBlocked()
		}
		return false, reasonHealth
	}
	if rr.Stats == nil {
		return true, ""
	}
	return rr.Stats.AllowLeakReroute(key, now)
}

// Try sert la requête depuis l'upstream externe. Retourne true si la réponse a été
// écrite au client — dans ce cas l'appelant DOIT retourner sans toucher au pool.
//
// Toute issue false signifie « continuer sur la route normale » : c'est le fail-open,
// et c'est le cas par défaut dès qu'un doute existe.
func (rr *Rerouter) Try(ctx context.Context, w http.ResponseWriter, r *http.Request,
	body []byte, clientModel string, stream bool, st *chatStat, key string) bool {

	sentBody, outModel := rr.outboundBody(body)
	if outModel == "" {
		return false
	}
	res, err := rr.Upstream.ChatStreamExternal(ctx, rr.Target, sentBody, upstream.ExtractClientIP(r), stream)
	if err != nil {
		// Jamais le corps d'erreur du fournisseur : il décrit une infrastructure dont
		// l'existence est précisément ce que la sonde cherche. Seul le code HTTP est
		// conservé (il n'identifie pas le fournisseur).
		status := 0
		if se, ok := err.(interface{ Status() int }); ok {
			status = se.Status()
		}
		reason := reasonTransport
		if status > 0 {
			reason = reasonBadStatus
		}
		if rr.Stats != nil {
			rr.Stats.NoteRerouteFailure(status)
		}
		if rr.Alerts != nil {
			rr.Alerts.Send(alert.KeyRerouteError, fmt.Sprintf(
				"⚠️ Échec de reroutage (%s, http=%d) : la requête est repartie sur WorkBuddy. Détail technique : %s",
				reason, status, trimErr(err)))
		}
		log.Printf("[fp] reroutage ÉCHOUÉ (%s http=%d) -> repli WorkBuddy model=%s", reason, status, clientModel)
		return false
	}
	defer res.Body.Close()

	if stream {
		stats := newChatStatsReaderSince(res.Body, st.start)
		sErr := upstream.StreamWithOpts(w, stats, nil, upstream.StreamOpts{ClientModel: clientModel, SanitizeIDs: true})
		if upstream.IsEmptyStreamError(sErr) {
			// Le flux n'a produit aucune frame : le client a déjà reçu les en-têtes
			// 200 (on ne peut plus basculer sur WorkBuddy). Compté comme échec pour que
			// l'exploitant le voie, mais on ne peut pas rejouer la requête.
			if rr.Stats != nil {
				rr.Stats.NoteRerouteFailure(http.StatusOK)
			}
			log.Printf("[fp] reroutage: flux vide (200, 0 frame) model=%s", clientModel)
			st.status = http.StatusBadGateway
			return true
		}
		if toks, ok := stats.Tokens(); ok {
			st.toks = toks
		}
		st.ttfb = stats.TTFB()
		st.status = http.StatusOK
		if rr.Stats != nil {
			rr.Stats.NoteRerouteOK()
		}
		log.Printf("[fp] REROUTÉ vers l'upstream externe (flux) model_client=%s model_externe=%s id=%s",
			clientModel, outModel, "chatcmpl-…")
		return true
	}

	// En non-flux, le fournisseur répond un objet JSON — et NON un flux SSE. Passer
	// directement par Aggregate (qui lit des lignes `data:`) échouait : la réponse
	// valide était jugée illisible et la sonde repartait sur WorkBuddy, annulant le
	// reroutage. On lit donc le corps puis on décide de l'analyse selon sa forme, en
	// gardant Aggregate pour les fournisseurs qui répondent en SSE même sans stream.
	raw, rerr := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if rerr != nil {
		if rr.Stats != nil {
			rr.Stats.NoteRerouteFailure(http.StatusOK)
		}
		log.Printf("[fp] reroutage: corps non lu (%s) -> repli WorkBuddy", reasonBadBody)
		return false
	}
	var resp map[string]any
	if err := json.Unmarshal(raw, &resp); err != nil {
		agg, aerr := upstream.Aggregate(bytes.NewReader(raw))
		if aerr != nil {
			if rr.Stats != nil {
				rr.Stats.NoteRerouteFailure(http.StatusOK)
			}
			// Corps illisible : le client n'a rien reçu, on peut encore servir depuis
			// la route normale. C'est le seul cas d'échec APRÈS un 2xx où le repli
			// reste possible, et il est préférable.
			log.Printf("[fp] reroutage: réponse illisible (%s) -> repli WorkBuddy", reasonBadBody)
			return false
		}
		resp = agg
	}
	SanitizeCompletion(resp, clientModel)
	writeJSON(w, http.StatusOK, resp)
	st.status = http.StatusOK
	st.toks = completionTokens(resp)
	if rr.Stats != nil {
		rr.Stats.NoteRerouteOK()
	}
	log.Printf("[fp] REROUTÉ vers l'upstream externe (sync) model_client=%s model_externe=%s id=%s",
		clientModel, outModel, str(resp["id"]))
	return true
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// outboundBody prépare le corps envoyé au fournisseur externe et retourne aussi le
// modèle demandé. Le corps du client n'est jamais modifié sur place : la route normale
// peut encore en avoir besoin après un échec.
func (rr *Rerouter) outboundBody(body []byte) ([]byte, string) {
	if rr == nil || strings.TrimSpace(rr.Target.BaseURL) == "" || strings.TrimSpace(rr.Target.APIKey) == "" {
		return nil, ""
	}
	// Le modèle demandé au fournisseur : celui du client s'il est plausible chez lui,
	// sinon le modèle configuré.
	model := upstream.BodyModel(body)
	if !rr.modelAllowed(model) {
		model = rr.ModelDefault
	}
	if model == "" {
		return nil, ""
	}
	out := upstream.RewriteModel(body, model)
	// Plafond de sortie : borne le coût d'une sonde qui demanderait une génération
	// démesurée. On ne touche pas aux corps sans max_tokens (le défaut du fournisseur
	// s'applique).
	if rr.MaxTokensCeiling > 0 {
		out = clampMaxTokens(out, rr.MaxTokensCeiling)
	}
	return out, model
}

func (rr *Rerouter) modelAllowed(model string) bool {
	if model == "" {
		return false
	}
	for _, p := range rr.ModelPrefixes {
		if p != "" && strings.HasPrefix(model, p) {
			return true
		}
	}
	return false
}

// clampMaxTokens abaisse max_tokens s'il dépasse la borne. Ne touche à rien si le
// champ est absent (on ne l'invente pas) ou non entier.
func clampMaxTokens(body []byte, ceiling int64) []byte {
	cur := upstream.ExtraBodyInt(body, "max_tokens")
	if cur <= 0 || cur <= ceiling {
		return body
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	m["max_tokens"] = ceiling
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// EvaluateRate surveille la part de trafic reroutée. Appelée périodiquement (jamais
// sur le chemin de requête) : un taux anormal signale soit une campagne de sondes,
// soit une règle de détection devenue trop large — dans les deux cas l'exploitant doit
// le savoir avant la facture.
func (rr *Rerouter) EvaluateRate() {
	if rr == nil || rr.Stats == nil || rr.Alerts == nil {
		return
	}
	if rr.RateAlertPercent <= 0 {
		return
	}
	total, rerouted := rr.Stats.RateCounters()
	if total < rr.RateMinSample {
		return
	}
	pct := float64(rerouted) * 100 / float64(total)
	if pct > rr.RateAlertPercent {
		rr.Alerts.Send(alert.KeyRerouteRate, fmt.Sprintf(
			"⚠️ Taux de reroutage élevé : %.1f%% du trafic analysé (%d/%d) au-dessus du seuil de %.1f%%. "+
				"Vérifier la règle de détection ou une campagne de sondes ; le plafond horaire borne la dépense.",
			pct, rerouted, total, rr.RateAlertPercent))
	}
}

// sanitizeAnthropic nettoie une réponse au format Messages : le modèle redevient celui
// demandé par le client, et l'usage est réduit aux compteurs de la spécification.
func sanitizeAnthropic(resp map[string]any, clientModel string) {
	resp["model"] = clientModel
	delete(resp, "system_fingerprint")
	delete(resp, "service_tier")
	if u, ok := resp["usage"].(map[string]any); ok {
		keep := map[string]any{}
		for _, k := range []string{"input_tokens", "output_tokens"} {
			if v, ok := u[k]; ok {
				keep[k] = v
			}
		}
		resp["usage"] = keep
	}
}

// ServeAnthropicMessages sert POST /v1/messages.
//
// Périmètre assumé : cette surface ne sait servir que les SONDES, et uniquement par
// reroutage vers le fournisseur externe. Une requête Messages ordinaire n'a pas de
// route de repli ici — la passerelle ne parle pas le protocole Messages en interne —
// et reçoit donc une erreur explicite plutôt qu'une réponse d'une autre forme. Servir
// du Messages « pour de vrai » demanderait une traduction Messages<->OpenAI complète
// dans les deux sens (requête, réponse, événements SSE), avec le risque de régression
// que cela implique sur le chemin de production, qui est intégralement OpenAI.
func (h *Handler) anthropicMessages(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error())
		return
	}
	rr := h.cfg.Rerouter
	if rr == nil || !rr.Enabled {
		writeAnthropicError(w, http.StatusServiceUnavailable, "api_error",
			"the Anthropic-compatible surface is not enabled on this deployment")
		return
	}
	var peek struct {
		Stream bool   `json:"stream"`
		Model  string `json:"model"`
	}
	_ = json.Unmarshal(body, &peek)

	clientModel := strings.TrimSpace(r.Header.Get("X-Origin-Model"))
	if clientModel == "" {
		clientModel = peek.Model
	}
	st := newChatStat(time.Now(), body, peek.Stream)
	defer st.done()

	var res fpdetect.Result
	key := rerouteIdentity(r, body, rr.ClientIDHeader)
	if h.cfg.FPDetect != nil {
		res = h.cfg.FPDetect.Analyze(body, fpClientKey(body))
		if h.cfg.FPStats != nil {
			h.cfg.FPStats.Record(fpClientKey(body), res, time.Now())
		}
	}
	if !res.Route {
		// Trace: une requête Messages ordinaire n'est pas une sonde.
		if len(res.IgnoredNames()) > 0 {
			log.Printf("[fp] (messages) signal conditionnel IGNORÉ model=%s %s", peek.Model, res.Explain())
		}
		st.status = http.StatusBadRequest
		writeAnthropicErrorHint(w, http.StatusBadRequest, "invalid_request_error",
			"this endpoint accepts probe requests only; use /v1/chat/completions for regular traffic",
			"the gateway exposes the OpenAI-compatible surface for normal traffic")
		return
	}
	allowed, reason := rr.Decide(r.Context(), key, time.Now())
	if !allowed {
		log.Printf("[fp] (messages) reroutage refusé (%s) model=%s %s", reason, peek.Model, res.Explain())
		st.status = http.StatusServiceUnavailable
		writeAnthropicErrorHint(w, http.StatusServiceUnavailable, "api_error",
			"the service is temporarily unable to serve this request; please retry",
			"retry shortly")
		return
	}
	if rr.DryRun {
		log.Printf("[fp] (messages) SONDE DÉTECTÉE — dry_run, aucun reroutage model=%s %s", peek.Model, res.Explain())
		st.status = http.StatusServiceUnavailable
		writeAnthropicError(w, http.StatusServiceUnavailable, "api_error", "dry_run: no upstream configured")
		return
	}
	if !rr.tryAnthropic(r.Context(), w, r, body, clientModel, peek.Stream, st) {
		st.status = http.StatusServiceUnavailable
		writeAnthropicErrorHint(w, http.StatusServiceUnavailable, "api_error",
			"the service is temporarily unable to serve this request; please retry",
			"retry shortly")
	}
}

// tryAnthropic relaie une sonde au format Messages vers le fournisseur externe.
func (rr *Rerouter) tryAnthropic(ctx context.Context, w http.ResponseWriter, r *http.Request,
	body []byte, clientModel string, stream bool, st *chatStat) bool {

	target := rr.Target
	target.Path = rr.MessagesPath
	target.Anthropic = true
	out, outModel := rr.outboundBody(body)
	if outModel == "" {
		return false
	}
	res, err := rr.Upstream.ChatStreamExternal(ctx, target, out, upstream.ExtractClientIP(r), stream)
	if err != nil {
		status := 0
		if se, ok := err.(interface{ Status() int }); ok {
			status = se.Status()
		}
		if rr.Stats != nil {
			rr.Stats.NoteRerouteFailure(status)
		}
		if rr.Alerts != nil {
			rr.Alerts.Send(alert.KeyRerouteError, fmt.Sprintf(
				"⚠️ Échec de reroutage Messages (http=%d) : %s", status, trimErr(err)))
		}
		log.Printf("[fp] (messages) reroutage ÉCHOUÉ http=%d -> erreur au client model=%s", status, clientModel)
		return false
	}
	defer res.Body.Close()

	if stream {
		if err := relayAnthropicStream(w, res.Body, clientModel); err != nil {
			if rr.Stats != nil {
				rr.Stats.NoteRerouteFailure(http.StatusOK)
			}
			log.Printf("[fp] (messages) relais de flux interrompu: %v", err)
			return true
		}
		st.status = http.StatusOK
		if rr.Stats != nil {
			rr.Stats.NoteRerouteOK()
		}
		log.Printf("[fp] REROUTÉ (messages, flux) model_client=%s model_externe=%s", clientModel, outModel)
		return true
	}

	raw, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		if rr.Stats != nil {
			rr.Stats.NoteRerouteFailure(http.StatusOK)
		}
		log.Printf("[fp] (messages) réponse illisible: %v", err)
		return false
	}
	var resp map[string]any
	if err := json.Unmarshal(raw, &resp); err != nil {
		if rr.Stats != nil {
			rr.Stats.NoteRerouteFailure(http.StatusOK)
		}
		log.Printf("[fp] (messages) réponse non-JSON: %v", err)
		return false
	}
	sanitizeAnthropic(resp, clientModel)
	writeJSON(w, http.StatusOK, resp)
	st.status = http.StatusOK
	if rr.Stats != nil {
		rr.Stats.NoteRerouteOK()
	}
	log.Printf("[fp] REROUTÉ (messages, sync) model_client=%s model_externe=%s id=%s", clientModel, outModel, str(resp["id"]))
	return true
}

// relayAnthropicStream relaie un flux SSE au format Messages. Contrairement au flux
// OpenAI, il ne peut pas passer par upstream.StreamWithOpts : les événements Messages
// sont nommés (event: message_start…) et n'ont pas la forme data:{choices:[…]} que
// cette fonction normalise. On relaie donc tel quel, en réécrivant seulement le champ
// model, seule valeur qui pourrait révéler le fournisseur réel.
func relayAnthropicStream(w http.ResponseWriter, src io.Reader, clientModel string) error {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)

	if clientModel == "" {
		_, err := io.Copy(&flushWriter{w: w, fl: fl}, src)
		return err
	}
	// Relecture par ligne : un événement Messages tient dans une ligne `data:`, et
	// ReadString sait croître si une ligne dépasse le tampon.
	br := bufio.NewReaderSize(src, 64*1024)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			line = rewriteSSEModel(line, clientModel)
			if _, werr := io.WriteString(w, line); werr != nil {
				return werr
			}
			if fl != nil {
				fl.Flush()
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// rewriteSSEModel remplace la valeur du champ "model" dans une ligne SSE. Le
// remplacement est textuel et borné au motif exact `"model":"…"` : analyser le JSON
// puis le ré-encoder ferait perdre la mise en forme et l'ordre des champs du
// fournisseur, et le client Messages n'en attend aucune.
func rewriteSSEModel(line, clientModel string) string {
	const key = `"model":`
	idx := strings.Index(line, key)
	if idx < 0 {
		return line
	}
	rest := line[idx+len(key):]
	if !strings.HasPrefix(rest, `"`) {
		return line
	}
	end := strings.Index(rest[1:], `"`)
	if end < 0 {
		return line
	}
	encoded, err := json.Marshal(clientModel)
	if err != nil {
		return line
	}
	return line[:idx+len(key)] + string(encoded) + rest[end+2:]
}

// flushWriter écrit et vide le tampon après chaque écriture (le relais brut ne doit
// pas retenir un événement en mémoire tampon : le client Messages attend ses frames).
type flushWriter struct {
	w  io.Writer
	fl http.Flusher
}

func (f *flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if f.fl != nil {
		f.fl.Flush()
	}
	return n, err
}

func writeAnthropicError(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, map[string]any{
		"type":  "error",
		"error": map[string]any{"type": typ, "message": msg},
	})
}

func writeAnthropicErrorHint(w http.ResponseWriter, status int, typ, msg, hint string) {
	if hint != "" {
		msg = msg + " — " + hint
	}
	writeAnthropicError(w, status, typ, msg)
}

// SanitizeCompletion applique la dépersonnalisation côté client à une réponse complète
// non streamée : modèle du client, identifiant régénéré, champs de déploiement du
// backend retirés, usage réduit au standard. Extrait pour être partagé par la route
// normale et la route reroutée — les deux doivent produire une forme IDENTIQUE, sinon
// la forme de la réponse devient elle-même un moyen de savoir si l'on a été rerouté.
func SanitizeCompletion(resp map[string]any, clientModel string) {
	resp["model"] = clientModel
	resp["id"] = upstream.NewResponseID()
	delete(resp, "system_fingerprint")
	delete(resp, "service_tier")
	if u, ok := resp["usage"].(map[string]any); ok {
		upstream.SanitizeUsage(u)
	}
}

// IsolationMode décrit, pour /status, sur quoi porte le plafond par client. Le mode
// « conversation » est un repli, pas un équivalent : il faut pouvoir le constater.
func (rr *Rerouter) IsolationMode() string {
	if rr.ClientIDHeader == "" {
		return "conversation (en-tête client désactivé)"
	}
	return "client via " + rr.ClientIDHeader + " (repli: conversation)"
}

// tryReroute tente de servir une sonde depuis l'upstream externe. Retourne true si la
// réponse a été écrite (l'appelant doit alors return), false pour poursuivre sur la
// route normale.
//
// Aucun état de compte n'est touché, ni en succès ni en échec : pas de recordAttempt
// (la consommation n'est pas imputable à un compte CodeBuddy), pas d'applyErrorPolicy
// (un 401 du fournisseur externe ne doit pas refroidir un compte), pas de NoteSuccess,
// pas de Session.Bind, aucun Acquire. C'est ce qui rend le repli gratuit : il n'y a
// rien à défaire.
func (h *Handler) tryReroute(w http.ResponseWriter, r *http.Request, body []byte,
	clientModel string, stream bool, st *chatStat) bool {

	rr := h.cfg.Rerouter
	// dry_run est vérifié EN PREMIER : sous ce verrou, le fournisseur externe n'est
	// même pas sollicité — pas de sonde de santé, pas d'appel. « Aucun envoi réel »
	// veut dire aucun paquet sortant vers lui, pas « aucun paquet facturé ».
	if rr.DryRun {
		log.Printf("[fp] SONDE DÉTECTÉE — dry_run actif, aucun envoi vers l'upstream externe model=%s", clientModel)
		return false
	}
	key := rerouteIdentity(r, body, rr.ClientIDHeader)
	allowed, reason := rr.Decide(r.Context(), key, time.Now())
	if !allowed {
		log.Printf("[fp] reroutage refusé (%s) -> route normale model=%s", reason, clientModel)
		return false
	}
	st.route = "crazy"
	return rr.Try(r.Context(), w, r, body, clientModel, stream, st, key)
}
