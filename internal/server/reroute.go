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
	"sync/atomic"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/alert"
	"github.com/linguo2625469/workbuddy2api-panel/internal/fpdetect"
	"github.com/linguo2625469/workbuddy2api-panel/internal/prompt"
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
	// ProbeSystemMaxBytes : taille maximale du prompt systeme conserve lors d'une sonde
	// (au-dela, il est retire). 0 ou negatif : toujours retire.
	ProbeSystemMaxBytes int
	// HoneypotTurns : nombre de premiers échanges d'une conversation à servir par le
	// fournisseur externe (vrai Claude), pour que la première impression soit la
	// bonne. 0 désactive le stratagème. Les requêtes de code en sont exclues : un
	// client qui commence par une tâche de développement ne doit pas coûter cher.
	HoneypotTurns int

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

	// Compteurs d'usage du fournisseur externe (diagnostic) : nombre d'appels réellement
	// partis, octets de prompt envoyés et raison. Sans eux, la seule façon de savoir ce que
	// coûte l'anti-détection était de demander au tableau de bord du fournisseur.
	Usage RerouteUsage

	Upstream *upstream.Client
	Health   *upstream.ExternalHealth
	// Families : familles de modèles servies par le fournisseur externe (claude, openai,
	// gemini, grok…). Vide = comportement historique à cible unique.
	Families []*Family
	Stats    *FPCounters
	Alerts   *alert.Notifier
}

// Family : une famille de modèles servie par le fournisseur externe, avec SA clé.
//
// Pourquoi : un client qui teste « es-tu vraiment GPT ? » doit recevoir une réponse de
// GPT, pas de Claude. Une clé unique (celle de Claude) ne peut pas servir GPT, Gemini ou
// Grok : chaque famille a son compte chez le fournisseur, donc sa clé et ses modèles.
//
// La famille est choisie sur le modèle DEMANDÉ PAR LE CLIENT (X-Origin-Model, transmis
// par NewAPI lors d'un model_mapping), et non sur le modèle réécrit qui part vers le
// parc : c'est la seule information qui dise ce que le client croit utiliser.
type Family struct {
	Name     string
	Prefixes []string
	// Models : modèles réellement disponibles chez le fournisseur pour cette famille,
	// par ordre de préférence. Le premier est le défaut.
	Models []string
	Target upstream.ExternalTarget
	Health *upstream.ExternalHealth
	// Aliases : correspondance demandée -> modèle réel du fournisseur (facultatif).
	Aliases map[string]string
}

// familyFor : famille dont un préfixe correspond au modèle demandé (le plus long gagne).
// nil = aucune famille (repli sur la cible historique, comportement inchangé).
func (rr *Rerouter) familyFor(clientModel string) *Family {
	if rr == nil || len(rr.Families) == 0 {
		return nil
	}
	m := strings.ToLower(strings.TrimSpace(clientModel))
	var best *Family
	bestLen := 0
	for _, f := range rr.Families {
		for _, p := range f.Prefixes {
			if p == "" || !strings.HasPrefix(m, strings.ToLower(p)) {
				continue
			}
			if len(p) > bestLen {
				best, bestLen = f, len(p)
			}
		}
	}
	return best
}

// targetFor : cible externe à utiliser pour ce modèle demandé.
func (rr *Rerouter) targetFor(clientModel string) upstream.ExternalTarget {
	if f := rr.familyFor(clientModel); f != nil {
		return f.Target
	}
	return rr.Target
}

// modelesPour : modèles du fournisseur à essayer, dans l'ordre.
func (rr *Rerouter) modelesPour(clientModel string) []string {
	f := rr.familyFor(clientModel)
	if f == nil || len(f.Models) == 0 {
		if strings.TrimSpace(rr.ModelDefault) == "" {
			return nil
		}
		return []string{rr.ModelDefault}
	}
	demande := strings.TrimSpace(clientModel)
	if f.Aliases != nil {
		if v, ok := f.Aliases[demande]; ok && v != "" {
			return append([]string{v}, f.Models...)
		}
	}
	for _, m := range f.Models {
		if strings.EqualFold(m, demande) {
			// Le nom demandé existe chez le fournisseur : on le garde en tête.
			reste := make([]string, 0, len(f.Models))
			reste = append(reste, m)
			for _, autre := range f.Models {
				if !strings.EqualFold(autre, m) {
					reste = append(reste, autre)
				}
			}
			return reste
		}
	}
	return f.Models
}

// healthFor : verdict de santé de la famille concernée.
// noteExternal : enregistre un depart vers le fournisseur externe.
//
// On compte les departs REUSSIS pour la facture (un refus 4xx n'est generalement pas facture) et
// tous les echecs a part, pour distinguer « le fournisseur coute cher » de « il est en panne ».
func (rr *Rerouter) noteExternal(reason string, promptBytes int, err error) {
	if err != nil {
		rr.Usage.FailCalls.Add(1)
		return
	}
	if reason == "leak" {
		rr.Usage.LeakCalls.Add(1)
		rr.Usage.LeakBytes.Add(int64(promptBytes))
		return
	}
	rr.Usage.ProbeCalls.Add(1)
	rr.Usage.ProbeBytes.Add(int64(promptBytes))
}

// RerouteUsage : compteurs d'appels au fournisseur externe, par raison.
//
// Deux raisons, et elles seules : une SONDE d'authenticité (le client demande si le modèle est
// bien celui qu'il a commandé) et le REMÈDE d'une fuite de langue (le parc a répondu en chinois).
// Les octets comptés sont ceux du prompt envoyé : à 4 octets par jeton environ, c'est l'ordre de
// grandeur de la facture, et il suffit à voir quelle raison coûte.
type RerouteUsage struct {
	ProbeCalls  atomic.Int64
	ProbeBytes  atomic.Int64
	LeakCalls   atomic.Int64
	LeakBytes   atomic.Int64
	FailCalls   atomic.Int64
	FallbackHit atomic.Int64
}

// Snapshot : lecture cohérente pour /status.
func (u *RerouteUsage) Snapshot() map[string]any {
	probe, leak := u.ProbeCalls.Load(), u.LeakCalls.Load()
	probeB, leakB := u.ProbeBytes.Load(), u.LeakBytes.Load()
	total := probe + leak
	// Estimation de jetons : 4 octets par jeton, arrondi au millier. C'est une estimation
	// assumée : le fournisseur ne renvoie pas toujours l'usage, et on veut un ordre de grandeur.
	estTok := func(b int64) int64 { return (b / 4 / 1000) * 1000 }
	moyenne := func(b, n int64) int64 {
		if n == 0 {
			return 0
		}
		return b / n
	}
	return map[string]any{
		"probe_calls":         probe,
		"probe_prompt_bytes":  probeB,
		"probe_avg_bytes":     moyenne(probeB, probe),
		"leak_calls":          leak,
		"leak_prompt_bytes":   leakB,
		"leak_avg_bytes":      moyenne(leakB, leak),
		"total_calls":         total,
		"total_prompt_tokens": estTok(probeB + leakB),
		"failed_calls":        u.FailCalls.Load(),
		"cross_family_hits":   u.FallbackHit.Load(),
		"probe_share_percent": map[bool]float64{true: 100 * float64(probe) / float64(total), false: 0}[total > 0],
	}
}

func (rr *Rerouter) healthFor(clientModel string) *upstream.ExternalHealth {
	if f := rr.familyFor(clientModel); f != nil && f.Health != nil {
		return f.Health
	}
	return rr.Health
}

// probeFamily : sonde la santé de la famille du modèle demandé.
func (rr *Rerouter) probeFamily(ctx context.Context, clientModel string) (bool, bool) {
	health := rr.healthFor(clientModel)
	if rr == nil || health == nil {
		return false, false
	}
	probe := rr.targetFor(clientModel)
	if strings.TrimSpace(rr.HealthPath) != "" {
		probe.Path = rr.HealthPath
	}
	ok, transition, _ := health.Healthy(ctx, func(ctx context.Context) error {
		return rr.Upstream.ProbeExternal(ctx, probe)
	})
	if transition && rr.Alerts != nil {
		if ok {
			log.Printf("[fp] service de secours de nouveau disponible : la protection anti-detection reprend")
		} else {
			log.Printf("[fp] service de secours indisponible : les reponses restent assurees par le service principal")
		}
	}
	return ok, transition && !ok
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
	ok, transition, _ := rr.Health.Healthy(ctx, func(ctx context.Context) error {
		return rr.Upstream.ProbeExternal(ctx, probe)
	})
	if transition && rr.Alerts != nil {
		if ok {
			log.Printf("[fp] service de secours de nouveau disponible : la protection anti-detection reprend")
		} else {
			log.Printf("[fp] service de secours indisponible : les reponses restent assurees par le service principal")
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
func (rr *Rerouter) Decide(ctx context.Context, key string, now time.Time, clientModel string) (bool, string) {
	if rr == nil || !rr.Enabled {
		return false, reasonDisabled
	}
	ok, justDown := rr.probeFamily(ctx, clientModel)
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
				log.Printf("[fp] plafond de securite atteint pour un client sur cette heure")
			case reasonCapGlobal:
				log.Printf("[fp] plafond de securite global atteint pour cette heure")
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
func (rr *Rerouter) DecideLeak(ctx context.Context, key string, now time.Time, clientModel string) (bool, string) {
	if rr == nil || !rr.Enabled {
		return false, reasonDisabled
	}
	ok, justDown := rr.probeFamily(ctx, clientModel)
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

// appelExterne appelle le fournisseur avec SA famille (donc sa clé) et le bon modèle.
//
// Les modèles de la famille sont essayés dans l'ordre : le fournisseur répond souvent
// « All available accounts exhausted » sur un modèle précis alors qu'un autre de la même
// famille répond. Trois essais au plus, pour borner le coût et la latence.
// messagesPath = true bascule sur l'endpoint Anthropic du même fournisseur.
func (rr *Rerouter) appelExterne(ctx context.Context, body []byte, clientModel, clientIP string,
	stream, messagesPath bool, reason string) (*upstream.ExternalResult, string, error) {

	target := rr.targetFor(clientModel)
	if messagesPath {
		target.Path = rr.MessagesPath
		target.Anthropic = true
	}
	// Aucune famille configurée : comportement historique à cible unique (le modèle du
	// client est transmis tel quel s'il est plausible chez le fournisseur).
	if rr.familyFor(clientModel) == nil {
		sentBody, outModel := rr.outboundBody(body, reason != "leak")
		if outModel == "" {
			return nil, "", fmt.Errorf("modèle indisponible")
		}
		res, err := rr.sendExternal(ctx, target, sentBody, clientIP, stream)
		rr.noteExternal(reason, len(sentBody), err)
		return res, outModel, err
	}
	models := rr.modelesPour(clientModel)
	if strings.TrimSpace(target.BaseURL) == "" || strings.TrimSpace(target.APIKey) == "" || len(models) == 0 {
		return nil, "", fmt.Errorf("cible externe incomplète")
	}
	// Budget de temps : une famille qui pend ne doit pas retenir la sonde du client.
	limite := time.Now().Add(25 * time.Second)
	var lastErr error
	for i, m := range models {
		if i >= 3 || time.Now().After(limite) {
			break
		}
		out := rr.outboundBodyFor(body, clientModel, m, reason != "leak")
		if out == nil {
			return nil, "", fmt.Errorf("corps vide")
		}
		out = avecIdentite(out, clientModel)
		res, err := rr.sendExternal(ctx, target, out, clientIP, stream)
		rr.noteExternal(reason, len(out), err)
		if err == nil {
			return res, m, nil
		}
		lastErr = err
		log.Printf("[fp] modèle %s indisponible chez le fournisseur (%v) -> modèle suivant", m, trimErr(err))
	}

	// DERNIÈRE TENTATIVE DANS LA FAMILLE DEMANDÉE : les 502 du fournisseur arrivent par
	// rafales (« comptes épuisés ») ; une seconde tentative 500 ms plus tard réussit
	// souvent. Mieux vaut cela qu'un repli inter-familles qui trahit l'identité.
	if len(models) > 0 && !time.Now().After(limite) {
		select {
		case <-ctx.Done():
		case <-time.After(500 * time.Millisecond):
		}
		if out := rr.outboundBodyFor(body, clientModel, models[0], reason != "leak"); out != nil && !time.Now().After(limite) {
			derniere := avecIdentite(out, clientModel)
			res, err := rr.sendExternal(ctx, target, derniere, clientIP, stream)
			rr.noteExternal(reason, len(derniere), err)
			if err == nil {
				return res, models[0], nil
			}
		}
	}

	// REPLI INTER-FAMILLES : la famille demandée est en panne (le fournisseur répond
	// « comptes épuisés » par famille). Une réponse d'un VRAI modèle vaut mieux que la
	// réponse du parc, qui trahirait le backend chinois : on réessaie chez la famille la
	// plus fiable (Claude), et seulement ensuite on rend la main.
	if fam := rr.familyParNom("claude"); fam != nil && !time.Now().After(limite) {
		cible := fam.Target
		if messagesPath {
			cible.Path = rr.MessagesPath
			cible.Anthropic = true
		}
		for i, m := range fam.Models {
			if i >= 2 || time.Now().After(limite) {
				break
			}
			out := rr.outboundBodyFor(body, "claude", m, reason != "leak")
			if out == nil {
				continue
			}
			out = avecIdentiteNeutre(out)
			res, err := rr.sendExternal(ctx, cible, out, clientIP, stream)
			rr.noteExternal(reason, len(out), err)
			if err == nil {
				rr.Usage.FallbackHit.Add(1)
				log.Printf("[fp] repli inter-familles : %s indisponible, réponse servie par %s", clientModel, m)
				return res, m, nil
			}
			lastErr = err
		}
	}
	return nil, "", lastErr
}

// avecIdentite ajoute la consigne d'identité de la famille au corps envoyé au
// fournisseur externe (voir prompt.IdentityDirective).
//
// Sans elle, le fournisseur enveloppe le modèle dans SON prompt d'agent et le modèle
// répond « non, je suis un environnement de développement » à la question « es-tu
// Claude ? » — constaté le 2026-10-20. La consigne est placée en dernier : ce qui est lu
// en dernier pèse le plus.
func avecIdentiteNeutre(body []byte) []byte {
	d := prompt.IdentityNeutre()
	body = prompt.Append(body, d)
	return prompt.AppendUserNote(body, d)
}

func avecIdentite(body []byte, clientModel string) []byte {
	if d := prompt.IdentityDirective(clientModel); d != "" {
		body = prompt.Append(body, d)
	}
	if n := prompt.IdentityNote(clientModel); n != "" {
		body = prompt.AppendUserNote(body, n)
	}
	return body
}

// familyParNom retrouve une famille par son nom (« claude », « openai »…).
func (rr *Rerouter) familyParNom(nom string) *Family {
	if rr == nil {
		return nil
	}
	for _, f := range rr.Families {
		if strings.EqualFold(f.Name, nom) {
			return f
		}
	}
	return nil
}

// Try sert la requête depuis l'upstream externe. Retourne true si la réponse a été
// écrite au client — dans ce cas l'appelant DOIT retourner sans toucher au pool.
//
// Toute issue false signifie « continuer sur la route normale » : c'est le fail-open,
// et c'est le cas par défaut dès qu'un doute existe.
func (rr *Rerouter) Try(ctx context.Context, w http.ResponseWriter, r *http.Request,
	body []byte, clientModel string, stream bool, st *chatStat, key string) bool {

	res, outModel, err := rr.appelExterne(ctx, body, clientModel, upstream.ExtractClientIP(r), stream, false, "probe")
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
		rr.noteEchecExterne(status, err, reason)
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

// rerouteAlertMinFailures : nombre d'échecs dans la fenêtre avant d'alerter.
const (
	rerouteAlertWindow      = 15 * time.Minute
	rerouteAlertMinFailures = 3
)

// sendExternalOnce : un seul appel, sans reprise. Utilisé quand d'autres modèles de la
// même famille restent à essayer : mieux vaut changer de modèle que réessayer celui qui
// vient de refuser (le fournisseur répond souvent « comptes épuisés » par modèle).
func (rr *Rerouter) sendExternalOnce(ctx context.Context, target upstream.ExternalTarget, body []byte,
	clientIP string, stream bool) (*upstream.ExternalResult, error) {
	return rr.Upstream.ChatStreamExternal(ctx, target, body, clientIP, stream)
}

// sendExternal appelle le fournisseur externe avec UNE reprise sur 5xx.
//
// Un 502 renvoyé par le fournisseur est presque toujours passager (passerelle saturée) :
// une seconde tentative immédiate suffit le plus souvent, et évite au client de recevoir
// un repli alors que le service était disponible. Les 4xx ne sont pas rejoués (la requête
// ne passera pas davantage).
func (rr *Rerouter) sendExternal(ctx context.Context, target upstream.ExternalTarget, body []byte,
	clientIP string, stream bool) (*upstream.ExternalResult, error) {

	res, err := rr.sendExternalOnce(ctx, target, body, clientIP, stream)
	if err == nil {
		return res, nil
	}
	status := 0
	if se, ok := err.(interface{ Status() int }); ok {
		status = se.Status()
	}
	if status < 500 {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, err
	case <-time.After(250 * time.Millisecond):
	}
	if res2, err2 := rr.Upstream.ChatStreamExternal(ctx, target, body, clientIP, stream); err2 == nil {
		return res2, nil
	}
	return nil, err
}

// doitAlerterEchec : faut-il envoyer l'alerte d'échec de reroutage ? Seulement quand
// les échecs se répètent dans la fenêtre — un 502 isolé du fournisseur ne mérite pas un
// message sur le téléphone de l'exploitant, la requête étant déjà repartie sur le parc.
// Fonction pure pour être testable sans réseau.
func (rr *Rerouter) doitAlerterEchec(now time.Time) (bool, int) {
	if rr == nil || rr.Stats == nil {
		return false, 0
	}
	n := rr.Stats.RerouteFailuresInWindow(now, rerouteAlertWindow)
	return n >= rerouteAlertMinFailures, n
}

// noteEchecExterne : enregistre un échec, invalide la santé et n'alerte que si les
// échecs se répètent. Un 502 isolé ne mérite pas un message sur le téléphone de
// l'exploitant : la requête est déjà repartie sur le parc et le client n'a rien vu.
func (rr *Rerouter) noteEchecExterne(status int, err error, libelle string) {
	if rr.Stats != nil {
		rr.Stats.NoteRerouteFailure(status)
	}
	if rr.Health != nil {
		rr.Health.MarkDown(fmt.Sprintf("%s http=%d : %v", libelle, status, trimErr(err)))
	}
	if rr.Alerts == nil {
		return
	}
	alerter, n := rr.doitAlerterEchec(time.Now())
	if !alerter {
		log.Printf("[fp] échec de reroutage isolé (%d/%d dans la fenêtre) : pas d'alerte", n, rerouteAlertMinFailures)
		return
	}
	log.Printf("[fp] %d echecs de verification sur les %d dernieres minutes (dernier : %s http=%d) : aucun message a l'exploitant",
		n, int(rerouteAlertWindow.Minutes()), libelle, status)
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// outboundBody prépare le corps envoyé au fournisseur externe et retourne aussi le
// modèle demandé. Le corps du client n'est jamais modifié sur place : la route normale
// peut encore en avoir besoin après un échec.
// outboundBodyFor prépare le corps pour une famille et un modèle de fournisseur donnés.
// Le corps du client n'est jamais modifié sur place : la route normale peut encore en
// avoir besoin après un échec.
func (rr *Rerouter) outboundBodyFor(body []byte, clientModel, model string, trim bool) []byte {
	if rr == nil {
		return nil
	}
	out := upstream.RewriteModel(body, model)
	if rr.MaxTokensCeiling > 0 {
		out = clampMaxTokens(out, rr.MaxTokensCeiling)
	}
	// Sonde : on n'envoie que le dernier message du client et un systeme court. Le reste
	// de la conversation (outils, historique, fichiers) ne sert pas a repondre « quel
	// modele es-tu ? » et coutait des centaines de milliers de jetons par appel.
	if trim {
		out = upstream.TrimForProbe(out, rr.ProbeSystemMaxBytes)
	}
	return out
}

func (rr *Rerouter) outboundBody(body []byte, trim bool) ([]byte, string) {
	target := rr.Target
	if strings.TrimSpace(target.BaseURL) == "" || strings.TrimSpace(target.APIKey) == "" {
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
	// Meme allegement que sur les autres chemins : une sonde n'emporte que le dernier
	// message du client et un systeme court. Le remede d'une fuite de langue, lui, garde
	// tout le contexte (il doit reconstruire une reponse correcte dans la bonne langue).
	if trim {
		out = upstream.TrimForProbe(out, rr.ProbeSystemMaxBytes)
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
		log.Printf("[fp] taux de verification eleve : %.1f%% du trafic analyse (%d/%d)", pct, rerouted, total)
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
	allowed, reason := rr.Decide(r.Context(), key, time.Now(), clientModel)
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

	res, outModel, err := rr.appelExterne(ctx, body, clientModel, upstream.ExtractClientIP(r), stream, true, "probe")
	if err != nil {
		status := 0
		if se, ok := err.(interface{ Status() int }); ok {
			status = se.Status()
		}
		rr.noteEchecExterne(status, err, "Messages")
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
	// Marqueurs Anthropic (identifiants d'outils toolu_, raisonnement masqué, contenu
	// non vide). Ici et pas seulement sur la route normale : la route reroutée et le
	// remède par l'upstream externe doivent produire la même forme, sinon la forme de
	// la réponse dit au client s'il a été servi en local ou non.
	upstream.MaskResponseAsAnthropic(resp)
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
	allowed, reason := rr.Decide(r.Context(), key, time.Now(), clientModel)
	if !allowed {
		log.Printf("[fp] reroutage refusé (%s) -> route normale model=%s", reason, clientModel)
		return false
	}
	st.route = "crazy"
	return rr.Try(r.Context(), w, r, body, clientModel, stream, st, key)
}

// userTurns compte les messages « user » du corps : c'est le numéro de l'échange en
// cours. Le corps est déjà en mémoire (peek), aucune lecture supplémentaire.
func userTurns(body []byte) int {
	var doc struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return 0
	}
	n := 0
	for _, m := range doc.Messages {
		if m.Role == "user" {
			n++
		}
	}
	return n
}
