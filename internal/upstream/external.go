package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Appels vers un upstream EXTERNE, hors pool de comptes.
//
// Cas d'usage : une requête reconnue comme sonde d'empreinte doit être servie par un
// vrai modèle, sur un second fournisseur, sans consommer d'account CodeBuddy. Il n'y a
// donc AUCUN compte, AUCUN quota de pool, AUCUNE rotation : juste un base URL, une clé
// et un corps à transmettre.
//
// Deux différences délibérées avec ChatStreamContext :
//   - la réponse d'erreur n'est jamais renvoyée telle quelle. Chez le fournisseur de
//     comptes, le corps d'erreur est le texte que le client doit voir (il décrit SA
//     requête) ; ici il décrit un second fournisseur, dont l'existence même est ce que
//     la sonde cherche à découvrir. L'appelant ne reçoit qu'une erreur locale et
//     retombe sur la route normale.
//   - le corps n'est pas préparé (pas de réécriture de prompt, pas de modèle imposé) :
//     c'est à l'appelant de décider du modèle sortant.

// ExternalTarget décrit un upstream externe.
type ExternalTarget struct {
	BaseURL string        // ex. https://api.exemple.test (sans slash final)
	Path    string        // ex. /v1/chat/completions
	APIKey  string        // clé du fournisseur (jamais journalisée)
	Timeout time.Duration // 0 => défaut
	// Anthropic : true pour le protocole Messages (x-api-key + anthropic-version),
	// false pour le protocole OpenAI (Authorization: Bearer).
	Anthropic bool
}

// anthropicVersion : version d'API envoyée sur le protocole Messages. CrazyToken
// l'accepte ; l'omettre fait échouer certaines implémentations, l'inventer est pire.
const anthropicVersion = "2023-06-01"

// externalTimeoutDefault borne un appel externe. La sonde d'empreinte doit échouer
// VITE et retomber sur la route normale : un client qui attend 120 s pour un repli
// n'a pas de service.
const externalTimeoutDefault = 60 * time.Second

// ExternalHTTPClient : client HTTP dédié aux appels externes. Séparé du client du pool
// pour ne pas hériter de ses particularités (proxy, désactivation HTTP/2 côté
// CodeBuddy) et pour pouvoir régler ses délais indépendamment.
type externalHTTP struct {
	once sync.Once
	c    *http.Client
}

var externalClient externalHTTP

func (e *externalHTTP) get() *http.Client {
	e.once.Do(func() {
		e.c = &http.Client{
			Transport: &http.Transport{
				MaxIdleConns:        16,
				MaxIdleConnsPerHost: 8,
				IdleConnTimeout:     90 * time.Second,
				ForceAttemptHTTP2:   true,
			},
		}
	})
	return e.c
}

// endpoint construit l'URL finale. BaseURL peut porter un chemin (ex.
// https://exemple.test/api) : on ne coupe que le slash final.
func (t ExternalTarget) endpoint() string {
	return strings.TrimRight(strings.TrimSpace(t.BaseURL), "/") + t.Path
}

// targetError : échec local d'un appel externe. Jamais exposée au client — sert à
// journaliser et à déclencher le repli.
type targetError struct {
	reason string
	status int
}

func (e *targetError) Error() string {
	if e.status > 0 {
		return fmt.Sprintf("external upstream: %s (http %d)", e.reason, e.status)
	}
	return "external upstream: " + e.reason
}

// Status expose le code HTTP reçu (0 si l'échec est antérieur à la réponse).
func (e *targetError) Status() int { return e.status }

// ExternalResult : réponse d'un upstream externe, corps non consommé en cas de succès.
type ExternalResult struct {
	Body       io.ReadCloser
	StatusCode int
	// Stream indique que le corps est un flux SSE à relayer ligne à ligne.
	Stream bool
}

// ChatStreamExternal envoie body à l'upstream externe et retourne le corps de réponse
// en cas de succès (2xx). Toute autre issue retourne une *targetError : l'appelant
// DOIT retomber sur la route normale (fail-open), jamais relayer cette erreur.
func (c *Client) ChatStreamExternal(ctx context.Context, t ExternalTarget, body []byte, clientIP string, stream bool) (*ExternalResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(t.BaseURL) == "" || strings.TrimSpace(t.APIKey) == "" {
		return nil, &targetError{reason: "configuration incomplète (base_url ou clé absente)"}
	}
	timeout := t.Timeout
	if timeout <= 0 {
		timeout = externalTimeoutDefault
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, t.endpoint(), bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, &targetError{reason: "requête invalide: " + err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	// Les deux en-têtes d'authentification sont posés quel que soit le protocole :
	// selon le fournisseur, l'un ou l'autre est lu, et envoyer les deux évite un 401
	// dépendant d'un détail d'implémentation qu'on ne peut pas vérifier.
	req.Header.Set("Authorization", "Bearer "+t.APIKey)
	// x-api-key est posé MÊME sur le protocole OpenAI : certains fournisseurs
	// compatibles OpenAI sont derrière une façade Anthropic qui ne lit que cet
	// en-tête, et envoyer les deux évite un 401 dépendant d'un détail
	// d'implémentation qu'on ne peut pas vérifier depuis ici.
	req.Header.Set("x-api-key", t.APIKey)
	if t.Anthropic {
		req.Header.Set("anthropic-version", anthropicVersion)
	}
	if clientIP != "" {
		req.Header.Set("X-Forwarded-For", clientIP)
	}

	resp, err := externalClient.get().Do(req)
	if err != nil {
		cancel()
		return nil, &targetError{reason: "transport: " + err.Error()}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Corps d'erreur lu puis JETÉ : il n'est ni journalisé en clair (il peut
		// contenir des identifiants de compte fournisseur) ni transmis au client.
		n, _ := io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		cancel()
		return nil, &targetError{reason: fmt.Sprintf("réponse non-2xx (%d octets ignorés)", n), status: resp.StatusCode}
	}
	return &ExternalResult{Body: monitorBody(resp.Body, c.IdleTimeout, cancel), StatusCode: resp.StatusCode, Stream: stream}, nil
}

// ProbeExternal interroge la sonde de santé de l'upstream externe (GET). Retourne nil
// si le fournisseur répond 2xx.
func (c *Client) ProbeExternal(ctx context.Context, t ExternalTarget) error {
	if strings.TrimSpace(t.BaseURL) == "" || strings.TrimSpace(t.APIKey) == "" {
		return &targetError{reason: "configuration incomplète (base_url ou clé absente)"}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	timeout := t.Timeout
	if timeout <= 0 {
		timeout = externalTimeoutDefault
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	url := strings.TrimRight(strings.TrimSpace(t.BaseURL), "/") + t.Path
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return &targetError{reason: "requête invalide: " + err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+t.APIKey)
	if t.Anthropic {
		req.Header.Set("x-api-key", t.APIKey)
		req.Header.Set("anthropic-version", anthropicVersion)
	}
	resp, err := externalClient.get().Do(req)
	if err != nil {
		return &targetError{reason: "transport: " + err.Error()}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &targetError{reason: "sonde non-2xx", status: resp.StatusCode}
	}
	return nil
}

// --- santé mise en cache ---------------------------------------------------------

// ExternalHealth met en cache le verdict de la sonde. Objectif : ne pas interroger le
// fournisseur à chaque requête reroutée (une sonde par requête doublerait le trafic
// sortant et la latence), tout en détectant une panne en quelques minutes.
type ExternalHealth struct {
	mu        sync.Mutex
	ttl       time.Duration
	healthy   bool
	checkedAt time.Time
	lastErr   string
	checks    int64
	failures  int64
	// changed : nombre de bascules sain<->malsain. Sert à l'alerte « vient de tomber ».
	changed  int64
	inflight bool
}

// NewExternalHealth construit le cache de santé. ttl <= 0 => 5 minutes.
func NewExternalHealth(ttl time.Duration) *ExternalHealth {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &ExternalHealth{ttl: ttl}
}

// Healthy retourne (sain, vient_de_changer, erreur). probe n'est appelée que si le
// verdict en cache est expiré, ou si aucune mesure n'existe encore.
//
// En cas de mesure expirée ET de sonde déjà en vol, la valeur en cache est retournée
// telle quelle : on ne fait pas attendre toutes les requêtes derrière une seule sonde.
// Si aucune mesure n'existe (démarrage), l'appel BLOQUE le temps de la sonde — router
// à l'aveugle vers un fournisseur inconnu serait pire qu'attendre 60 s une fois.
func (h *ExternalHealth) Healthy(ctx context.Context, probe func(context.Context) error) (bool, bool, error) {
	h.mu.Lock()
	fresh := h.checks > 0 && time.Since(h.checkedAt) < h.ttl
	if fresh {
		ok, err := h.healthy, h.errLocked()
		h.mu.Unlock()
		return ok, false, err
	}
	if h.inflight {
		ok, err := h.healthy, h.errLocked()
		h.mu.Unlock()
		return ok, false, err
	}
	h.inflight = true
	h.mu.Unlock()

	err := probe(ctx)

	h.mu.Lock()
	defer h.mu.Unlock()
	h.inflight = false
	h.checks++
	h.checkedAt = time.Now()
	prev := h.healthy
	h.healthy = err == nil
	if err != nil {
		h.failures++
		h.lastErr = err.Error()
	} else {
		h.lastErr = ""
	}
	transition := h.checks > 1 && prev != h.healthy
	if transition {
		h.changed++
	}
	return h.healthy, transition, err
}

func (h *ExternalHealth) errLocked() error {
	if h.lastErr == "" {
		return nil
	}
	return &targetError{reason: h.lastErr}
}

// Snapshot : état de santé pour /status et pour les alertes.
type HealthSnapshot struct {
	Healthy bool `json:"healthy"`
	// Measured distingue « jamais mesuré » de « mesuré et malsain ». Sans lui, un
	// /status lu juste après un redémarrage annonce healthy=false alors que rien n'a
	// encore été testé — de quoi déclencher une fausse alerte de panne à chaque
	// déploiement. Healthy reste false tant qu'aucune mesure n'existe : on ne déclare
	// pas sain ce qu'on n'a pas vérifié.
	Measured    bool   `json:"measured"`
	LastError   string `json:"last_error"`
	CheckedAt   string `json:"checked_at"`
	TTLSeconds  int    `json:"ttl_seconds"`
	Checks      int64  `json:"checks"`
	Failures    int64  `json:"failures"`
	Transitions int64  `json:"transitions"`
}

// Snapshot retourne l'état courant (jamais de secret : seulement le verdict).
func (h *ExternalHealth) Snapshot() HealthSnapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	return HealthSnapshot{
		Healthy:     h.healthy,
		Measured:    h.checks > 0,
		LastError:   h.lastErr,
		CheckedAt:   h.checkedAt.UTC().Format(time.RFC3339),
		TTLSeconds:  int(h.ttl.Seconds()),
		Checks:      h.checks,
		Failures:    h.failures,
		Transitions: h.changed,
	}
}

// --- lecture de la clé -----------------------------------------------------------

// LoadExternalAPIKey résout la clé du fournisseur externe. Priorité : fichier, puis
// valeur en ligne. Le fichier accepte deux formes : la clé seule, ou un fichier
// d'environnement contenant « CLE=valeur » (c'est le format de real-claude.env).
//
// Le contrôle de permissions est un AVERTISSEMENT, pas un refus : en conteneur la
// clé vient d'une variable d'environnement, et refuser de démarrer parce qu'un
// fichier monté est en 644 transformerait une hygiène en panne de service.
func LoadExternalAPIKey(file, inline string) (string, error) {
	if strings.TrimSpace(file) != "" {
		info, err := os.Stat(file)
		if err != nil {
			return "", fmt.Errorf("clé externe illisible (%s): %w", file, err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			fmt.Fprintf(os.Stderr, "AVERTISSEMENT: %s est lisible par d'autres (mode %o) — préférer 600\n", file, info.Mode().Perm())
		}
		raw, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("clé externe illisible (%s): %w", file, err)
		}
		if k := parseKeyFile(string(raw)); k != "" {
			return k, nil
		}
		return "", fmt.Errorf("clé externe absente de %s", file)
	}
	inline = strings.TrimSpace(inline)
	if inline == "" {
		return "", fmt.Errorf("aucune clé externe configurée")
	}
	return inline, nil
}

// parseKeyFile extrait la clé d'un contenu de fichier. Reconnaît « CLE=valeur »
// (commentaires # et lignes vides ignorés) et la clé seule sur une ligne.
func parseKeyFile(content string) string {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if i := strings.IndexByte(line, '='); i > 0 {
			key := strings.TrimSpace(line[:i])
			val := strings.TrimSpace(line[i+1:])
			val = strings.Trim(val, `"'`)
			if val == "" {
				continue
			}
			// Toute variable dont le nom évoque une clé est acceptée : le nom exact
			// dépend du fournisseur (CRAZYTOKEN_API_KEY, API_KEY, ANTHROPIC_API_KEY…),
			// et exiger un nom précis ferait échouer silencieusement un fichier correct.
			if strings.Contains(strings.ToUpper(key), "KEY") || strings.Contains(strings.ToUpper(key), "TOKEN") {
				return val
			}
			continue
		}
		if strings.Contains(line, "=") {
			continue
		}
		return line
	}
	return ""
}

// --- réécriture du modèle sortant ------------------------------------------------

// RewriteModel remplace le champ model du corps JSON. Retourne le corps original si
// la réécriture échoue : mieux vaut transmettre le corps tel quel (l'upstream
// répondra son propre 4xx, traité en repli) que de corrompre la requête.
func RewriteModel(body []byte, model string) []byte {
	if model == "" {
		return body
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	enc, err := json.Marshal(model)
	if err != nil {
		return body
	}
	m["model"] = enc
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// BodyModel lit le champ model d'un corps JSON ("" si absent/illisible).
func BodyModel(body []byte) string {
	var m struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return ""
	}
	return m.Model
}

// IsStreamingBody : vrai si le corps demande un flux SSE.
func IsStreamingBody(body []byte) bool {
	var m struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return false
	}
	return m.Stream
}

// ExtraBodyInt lit un entier du corps (0 si absent). Utilisé pour borner les
// max_tokens d'une requête reroutée sans avoir à connaître tout le schéma.
func ExtraBodyInt(body []byte, field string) int64 {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return 0
	}
	raw, ok := m[field]
	if !ok {
		return 0
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil {
		var f float64
		if err := json.Unmarshal(raw, &f); err != nil {
			return 0
		}
		n = int64(f)
	}
	return n
}
