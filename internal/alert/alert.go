// Package alert envoie des notifications d'exploitation (Telegram) sans jamais
// pouvoir perturber le chemin de requête.
//
// Trois contraintes ont dicté la conception :
//
//  1. Le chemin de requête ne bloque JAMAIS sur une alerte. Un envoi se fait dans une
//     goroutine, avec une file bornée : si le réseau du destinataire est lent, les
//     alertes sont perdues (et comptées) plutôt que de retarder un client.
//  2. Une condition qui dure ne produit pas des milliers de messages. Chaque alerte
//     porte une clé de déduplication et n'est réémise qu'après un délai (30 min par
//     défaut). Sans cela, un fournisseur tombé enverrait une alerte par requête, et
//     l'exploitant couperait les notifications — c'est-à-dire perdrait l'alerte qui
//     compte.
//  3. Aucun secret dans les messages. Le texte ne contient que des faits et des
//     compteurs.
package alert

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

// DefaultDedupWindow : délai minimal entre deux émissions d'une même clé.
const DefaultDedupWindow = 30 * time.Minute

// queueSize : profondeur de la file d'envoi. Au-delà, l'alerte est comptée comme
// perdue : mieux vaut perdre une notification que consommer la mémoire du gateway.
const queueSize = 64

// Notifier envoie des messages via l'API Bot Telegram.
type Notifier struct {
	token  string
	chatID string
	http   *http.Client
	queue  chan string
	dedup  time.Duration

	// baseURL permet de diriger les envois vers un serveur local dans les tests. En
	// production il reste l'API publique : aucune configuration ne l'expose, pour
	// qu'aucun déploiement ne puisse détourner les alertes par erreur.
	baseURL string

	mu       sync.Mutex
	lastSent map[string]time.Time
	sent     int64
	dropped  int64
	failed   int64
	suppress int64
	started  bool
}

// New construit un notifieur. Sans token ou sans chat ID, le notifieur est inerte :
// Enabled() est faux et Send ne fait rien. C'est volontaire — un déploiement sans
// Telegram ne doit pas voir son comportement changer.
func New(token, chatID string) *Notifier {
	return &Notifier{
		token:    strings.TrimSpace(token),
		chatID:   strings.TrimSpace(chatID),
		baseURL:  "https://api.telegram.org",
		http:     &http.Client{Timeout: 10 * time.Second},
		queue:    make(chan string, queueSize),
		dedup:    DefaultDedupWindow,
		lastSent: map[string]time.Time{},
	}
}

// Enabled : vrai si les deux identifiants sont présents.
func (n *Notifier) Enabled() bool {
	return n != nil && n.token != "" && n.chatID != ""
}

// Start lance la goroutine d'envoi. Idempotent.
func (n *Notifier) Start(ctx context.Context) {
	if !n.Enabled() {
		return
	}
	n.mu.Lock()
	if n.started {
		n.mu.Unlock()
		return
	}
	n.started = true
	n.mu.Unlock()
	go n.loop(ctx)
}

func (n *Notifier) loop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-n.queue:
			n.deliver(ctx, msg)
		}
	}
}

// Send met une alerte en file, en respectant la fenêtre de déduplication globale.
// Retourne false si l'alerte a été supprimée (doublon récent) ou perdue (file pleine).
func (n *Notifier) Send(key, text string) bool {
	return n.SendWindow(key, text, 0)
}

// SendWindow : comme Send, avec une fenêtre de déduplication propre à cette alerte.
// window <= 0 => fenêtre globale.
//
// Nécessaire parce que toutes les alertes n'ont pas la même urgence : une fuite de
// langue signalée sur une fenêtre de 15 minutes doit pouvoir se rappeler toutes les
// 15 minutes tant qu'elle dure, sans pour autant raccourcir la fenêtre des autres
// (une panne de fournisseur qui dure ne doit pas spammer).
func (n *Notifier) SendWindow(key, text string, window time.Duration) bool {
	if !n.Enabled() {
		return false
	}
	if window <= 0 {
		window = n.dedup
	}
	now := time.Now()
	n.mu.Lock()
	if last, ok := n.lastSent[key]; ok && now.Sub(last) < window {
		n.suppress++
		n.mu.Unlock()
		return false
	}
	n.lastSent[key] = now
	n.mu.Unlock()

	msg := text
	select {
	case n.queue <- msg:
		return true
	default:
		n.mu.Lock()
		n.dropped++
		n.mu.Unlock()
		return false
	}
}

func (n *Notifier) deliver(ctx context.Context, text string) {
	payload, err := json.Marshal(map[string]any{
		"chat_id": n.chatID,
		"text":    text,
		// Pas de Markdown : un modèle, un nom de fichier ou un souligné dans un
		// compteur suffit à faire rejeter le message par Telegram (400 parse error),
		// ce qui perdrait l'alerte pour une raison cosmétique.
		"disable_web_page_preview": true,
	})
	if err != nil {
		return
	}
	base := n.baseURL
	if base == "" {
		base = "https://api.telegram.org"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		base+"/bot"+n.token+"/sendMessage", bytes.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.http.Do(req)
	if err != nil {
		n.mu.Lock()
		n.failed++
		n.mu.Unlock()
		fmt.Fprintf(os.Stderr, "alerte Telegram non envoyée: %v\n", err)
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 8<<10))
	n.mu.Lock()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		n.sent++
	} else {
		n.failed++
	}
	n.mu.Unlock()
}

// Stats : compteurs pour /status (aucun secret).
type Stats struct {
	Enabled    bool  `json:"enabled"`
	Sent       int64 `json:"sent"`
	Failed     int64 `json:"failed"`
	Dropped    int64 `json:"dropped"`
	Suppressed int64 `json:"suppressed"`
}

// Stats retourne les compteurs d'envoi.
func (n *Notifier) Stats() Stats {
	if n == nil {
		return Stats{Enabled: false}
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return Stats{Enabled: n.Enabled(), Sent: n.sent, Failed: n.failed, Dropped: n.dropped, Suppressed: n.suppress}
}

// --- clés d'alerte ---------------------------------------------------------------

// Clés stables : la déduplication porte sur ces valeurs, pas sur le texte.
const (
	KeyHealthDown   = "external_health_down"
	KeyHealthUp     = "external_health_up"
	KeyCapReached   = "reroute_cap_reached"
	KeyGlobalCap    = "reroute_global_cap_reached"
	KeyRerouteError = "reroute_error"
	KeyRerouteRate  = "reroute_rate_high"
	KeyLanguageLeak = "language_leak"
)

// LoadFromEnv lit les identifiants Telegram depuis l'environnement. Les noms
// génériques TELEGRAM_* sont acceptés en plus des WB2A_TELEGRAM_* pour rester
// compatible avec un fichier d'environnement existant.
func LoadFromEnv() (token, chat string) {
	token = firstEnv("WB2A_TELEGRAM_BOT_TOKEN", "TELEGRAM_BOT_TOKEN")
	chat = firstEnv("WB2A_TELEGRAM_CHAT_ID", "TELEGRAM_CHAT_ID")
	return token, chat
}

func firstEnv(names ...string) string {
	for _, n := range names {
		if v := strings.TrimSpace(os.Getenv(n)); v != "" {
			return v
		}
	}
	return ""
}
