package fpdetect

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Fichier séparé des tables de motifs (patterns.go) : ici la logique d'analyse
// élémentaire, testable indépendamment du contenu linguistique.

// codingHintRe : indices de développement courant. Volontairement large — ce n'est
// qu'une pénalité, et un faux positif ici ne coûte qu'un signal fort non détecté,
// alors qu'un faux négatif enverrait du code légitime vers le modèle payant.
var codingHintRe = regexp.MustCompile(`(?i)` +
	"```|~~~" +
	`|\b(?:def|class|func|function|import|package|SELECT|INSERT|const|let|var)\b` +
	`|\b(?:traceback|stack\s*trace|segfault|exception|erreur|error)\b` +
	`|\b(?:refactor|debug|compile|déploie|deploie|commit|merge|pull\s+request)\b` +
	`|[\w./-]+\.(?:py|js|ts|tsx|jsx|go|java|php|rb|rs|sql|json|ya?ml|md|sh|css|html)\b` +
	`|(?:^|\s)/(?:Users|home|var|etc|opt)/`)

// recentYearThreshold : première année considérée comme « récente ». Le détecteur
// compare à l'année courante pour ne pas dépendre d'une date figée dans le code.
const recentYearFloor = 2025

// recentYearMention retourne l'année récente trouvée (« 2026 », « 2026年 », « 2026年 »),
// ou une chaîne vide. Signal FAIBLE : demander « que s'est-il passé en 2026 » est
// parfaitement légitime pour un utilisateur.
func recentYearMention(lower string) string {
	floor := recentYearFloor
	if y := time.Now().Year(); y > floor {
		floor = y
	}
	for _, m := range yearRe.FindAllString(lower, -1) {
		n, err := strconv.Atoi(strings.TrimFunc(m, func(r rune) bool { return r < '0' || r > '9' }))
		if err == nil && n >= floor && n <= floor+1 {
			return m
		}
	}
	return ""
}

var yearRe = regexp.MustCompile(`20[0-9]{2}`)

// repetitionInstructionRe : consigne explicite « répète N fois ». Multilingue, avec
// les formes sans espaces pour le chinois et le japonais.
var repetitionInstructionRe = regexp.MustCompile(`(?i)` +
	`(?:repeat|répète|repete|repite|repita|wiederhole|ripeti|повтори|tekrarla|herhaal|powtórz|lặp lại|كرر|दोहराएं)` +
	`[^\n]{0,40}?(\d{2,5})` +
	`(?:\s*(?:times|fois|veces|mal|volte|раз|kez|keer|razy|lần|مرة|बार))?` +
	`|(?:重复|重複|繰り返|반복)[^\n]{0,10}?(\d{2,5})`)

// repetitionInstruction retourne N si une consigne de répétition chiffrée est présente,
// sinon 0.
func repetitionInstruction(s string) int {
	m := repetitionInstructionRe.FindStringSubmatch(s)
	if m == nil {
		return 0
	}
	for _, g := range m[1:] {
		if g != "" {
			if n, err := strconv.Atoi(g); err == nil {
				return n
			}
		}
	}
	return 0
}
