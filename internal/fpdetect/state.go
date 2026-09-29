package fpdetect

import (
	"sync"
	"time"
)

// State porte les signaux **temporels**, impossibles à voir sur une seule requête :
// la répétition de la même requête par un même client. C'est la seule façon
// raisonnable de détecter un test de latence/régularité (RUT) : mesurer le temps de
// réponse ne se lit pas dans la requête, mais « la même requête 4 fois en 5 minutes »
// est une signature forte, et rare dans un usage réel (un agent varie ses tours).
//
// Mémoire bornée : nombre de clients plafonné (éviction FIFO) et nombre d'empreintes
// par client plafonné. Un détecteur qui fuit la mémoire sur un service en production
// serait pire que le problème qu'il résout.
type State struct {
	mu         sync.Mutex
	window     time.Duration
	maxClients int
	perClient  int
	seen       map[string]*clientWindow
	order      []string
}

type clientWindow struct {
	hashes []string
	times  []time.Time
}

// NewState construit l'état. window<=0 ou perClient<=0 retombe sur des valeurs sûres.
func NewState(window time.Duration, maxClients, perClient int) *State {
	if window <= 0 {
		window = 5 * time.Minute
	}
	if maxClients <= 0 {
		maxClients = 4096
	}
	if perClient <= 0 {
		perClient = 64
	}
	return &State{
		window:     window,
		maxClients: maxClients,
		perClient:  perClient,
		seen:       make(map[string]*clientWindow),
	}
}

// Observe enregistre une requête et retourne le nombre d'occurrences de la même
// empreinte dans la fenêtre glissante (celle-ci comprise).
func (s *State) Observe(client, hash string, now time.Time) int {
	if client == "" || hash == "" {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	w, ok := s.seen[client]
	if !ok {
		if len(s.seen) >= s.maxClients {
			// Éviction du plus ancien client enregistré.
			if len(s.order) > 0 {
				old := s.order[0]
				s.order = s.order[1:]
				delete(s.seen, old)
			}
		}
		w = &clientWindow{}
		s.seen[client] = w
		s.order = append(s.order, client)
	}

	// Purge de la fenêtre.
	cut := now.Add(-s.window)
	keep := 0
	for i, t := range w.times {
		if t.After(cut) {
			w.hashes[keep] = w.hashes[i]
			w.times[keep] = t
			keep++
		}
	}
	w.hashes = w.hashes[:keep]
	w.times = w.times[:keep]

	n := 0
	for _, h := range w.hashes {
		if h == hash {
			n++
		}
	}
	n++ // la requête courante

	if len(w.hashes) >= s.perClient {
		w.hashes = append(w.hashes[1:], hash)
		w.times = append(w.times[1:], now)
	} else {
		w.hashes = append(w.hashes, hash)
		w.times = append(w.times, now)
	}
	return n
}

// Clients retourne le nombre de clients suivis (observabilité / tests).
func (s *State) Clients() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.seen)
}

// Prune supprime les clients dont la fenêtre est entièrement expirée. À appeler
// périodiquement si le trafic est irrégulier, pour ne pas garder de clients inactifs.
func (s *State) Prune(now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	cut := now.Add(-s.window)
	removed := 0
	for c, w := range s.seen {
		alive := false
		for _, t := range w.times {
			if t.After(cut) {
				alive = true
				break
			}
		}
		if !alive {
			delete(s.seen, c)
			removed++
		}
	}
	if removed > 0 {
		kept := s.order[:0]
		for _, c := range s.order {
			if _, ok := s.seen[c]; ok {
				kept = append(kept, c)
			}
		}
		s.order = kept
	}
	return removed
}
