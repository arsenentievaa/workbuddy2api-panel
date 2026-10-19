package fpdetect

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Signal « language_mismatch » : l'utilisateur écrit sans caractère CJK et le modèle
// répond en CJK.
//
// POURQUOI CE SIGNAL VIT CÔTÉ RÉPONSE. C'est une incohérence entre l'entrée et la
// sortie : la seconde n'existe qu'après l'appel amont. Le détecteur de requête, qui
// tourne avant toute sélection de compte, ne peut donc pas le produire. Il est calculé
// séparément (AnalyzeResponse) et l'appelant décide quoi en faire — le seul endroit où
// « router vers le vrai modèle » a encore un sens est la réponse pas encore envoyée.
//
// Un modèle chinois répond en chinois à une question anglaise : c'est un aveu direct
// du backend, plus net qu'un jeton piège. La contrepartie est un faux positif évident —
// « traduis ceci en chinois » produit exactement cette forme — d'où le garde-fou
// asksForCJKOutput ci-dessous.

// CJK : idéogrammes han (y compris extensions A et compatibilité), kana, hangûl.
//
// Les plages sont volontairement larges (pas seulement 4E00-9FFF) : un modèle chinois
// qui répond en japonais ou en coréen trahit le même backend, et les caractères
// traditionnels comme les variantes de compatibilité en font partie.
var (
	reHan     = regexp.MustCompile(`[\x{3400}-\x{4DBF}\x{4E00}-\x{9FFF}\x{F900}-\x{FAFF}\x{20000}-\x{2FA1F}]`)
	reKana    = regexp.MustCompile(`[\x{3040}-\x{30FF}\x{31F0}-\x{31FF}]`)
	reHangul  = regexp.MustCompile(`[\x{AC00}-\x{D7AF}\x{1100}-\x{11FF}\x{3130}-\x{318F}]`)
	reLetters = regexp.MustCompile(`[A-Za-zÀ-ÖØ-öø-ÿ]`)
)

// hasCJK : vrai si le texte contient au moins un caractère CJK.
func hasCJK(s string) bool {
	if s == "" {
		return false
	}
	return reHan.MatchString(s) || reKana.MatchString(s) || reHangul.MatchString(s)
}

// countCJK : nombre de caractères CJK.
func countCJK(s string) int {
	if s == "" {
		return 0
	}
	n := 0
	for _, r := range s {
		if isCJKRune(r) {
			n++
		}
	}
	return n
}

// cjkShare : part des caractères CJK parmi les caractères « porteurs de sens »
// (CJK + lettres latines accentuées comprises). Les chiffres, la ponctuation et le code
// sont ignorés : une réponse majoritairement anglaise qui contient un mot chinois
// glissé dans un commentaire ne doit pas peser comme une réponse chinoise.
func cjkShare(s string) float64 {
	if s == "" {
		return 0
	}
	cjk := countCJK(s)
	if cjk == 0 {
		return 0
	}
	letters := len(reLetters.FindAllString(s, -1))
	total := cjk + letters
	if total == 0 {
		return 0
	}
	return float64(cjk) / float64(total)
}

// cjkMinRunes / cjkMinShare : seuils de déclenchement. Un caractère isolé ne suffit
// pas (un nom propre, un terme technique, une unité) ; il faut une réponse réellement
// écrite en CJK. Valeurs volontairement basses : le garde-fou contre le faux positif
// « traduis en chinois » est asksForCJKOutput, pas un seuil élevé qui laisserait passer
// les vraies réponses chinoises courtes.
const (
	cjkMinRunes = 8
	cjkMinShare = 0.20

	// userCJKMinShare : part de CJK au-delà de laquelle on considère que l'UTILISATEUR
	// écrit dans cette écriture. Avant, la moindre citation chinoise (« 你好 » glissé
	// dans une question française) désarmait la détection : le client recevait ensuite
	// une réponse entièrement chinoise sans que rien ne réagisse (trou constaté le
	// 2026-10-20). Une citation courte ne fait pas de l'utilisateur un sinophone.
	userCJKMinShare = 0.50

	// Réponse courte : « 好的 », « 你好 » — deux caractères suffisent quand la réponse
	// n'est presque que du CJK. Sans cette règle, les réponses brèves en chinois
	// passaient sous le seuil de huit caractères.
	shortCJKMinRunes   = 2
	shortCJKLettersMax = 4
)

// asksForCJKOutputRe : l'utilisateur DEMANDE explicitement une sortie en chinois. Sans
// ce garde-fou, « traduis ce paragraphe en chinois » déclencherait le signal à chaque
// fois — un faux positif coûteux (il envoie une requête légitime vers le modèle payant)
// et parfaitement prévisible. Formulations en anglais, français, espagnol, allemand,
// plus les formes CJK elles-mêmes (au cas où l'entrée mêlerait déjà du chinois, ce que
// la première condition écarterait de toute façon).
var asksForCJKOutputRe = regexp.MustCompile(`(?i)` +
	`in\s+(?:simplified\s+|traditional\s+)?chin(?:ese|ois)|to\s+(?:simplified\s+|traditional\s+)?chin(?:ese|ois)|` +
	`en\s+chin(?:ois|ese)|auf\s+chinesisch|en\s+chino|` +
	`translat\w*\s+[^.\n]{0,40}?chin|tradu\w*\s+[^.\n]{0,40}?chin|` +
	// Autres écritures d'Asie de l'Est explicitement demandées : la réponse n'est pas une
	// fuite mais la traduction attendue (constat 2026-10-20 : élargir la règle courte aux
	// kana cassait « comment dit-on merci en japonais »).
	`(?:in|en|auf|into|vers|tradu\w*|translat\w*)\s+[^.\n]{0,20}?(?:mandarin|japanese|japonais|korean|cor[ée]en|sinogramme|id[ée]ogramme|kanji|hangul|kana)|` +
	`(?:回答|回复|输出|答案|翻译)[^。\n]{0,12}?(?:中文|汉语|简体|繁体)|(?:中文|汉语|简体|繁体)[^。\n]{0,6}?(?:回答|回复|输出|翻译)`)

// asksForCJKOutput : vrai si la demande porte explicitement sur une sortie en chinois.
func asksForCJKOutput(lower string) bool {
	return asksForCJKOutputRe.MatchString(lower)
}

// languageMismatch : la règle complète, isolée pour être testable seule.
//
//	input  : texte de l'utilisateur (système exclu)
//	output : texte de la réponse du modèle
func languageMismatch(input, output string) bool {
	if output == "" {
		return false
	}
	// 1) L'utilisateur écrit DANS cette écriture (et pas seulement la cite) : rien
	//    d'anormal à ce que la réponse le soit aussi.
	if userWritesCJK(input) {
		return false
	}
	// 2) Il a demandé du chinois : la réponse chinoise est attendue.
	if asksForCJKOutput(strings.ToLower(input)) {
		return false
	}
	return outputIsCJK(output)
}

// countHan : nombre de caractères han uniquement (sans kana ni hangul).
func countHan(s string) int {
	if s == "" {
		return 0
	}
	n := 0
	for _, r := range s {
		switch {
		case r >= 0x3400 && r <= 0x4DBF, r >= 0x4E00 && r <= 0x9FFF,
			r >= 0xF900 && r <= 0xFAFF, r >= 0x20000 && r <= 0x2FA1F:
			n++
		}
	}
	return n
}

// userWritesCJK : vrai si le message de l'utilisateur est réellement rédigé en CJK.
// Une citation noyée dans une question latine ne suffit pas (voir userCJKMinShare).
func userWritesCJK(input string) bool {
	if countCJK(input) == 0 {
		return false
	}
	return cjkShare(input) >= userCJKMinShare
}

// outputIsCJK : vrai si la réponse est écrite en CJK — soit longuement, soit
// brièvement mais alors presque sans lettres latines (« 好的 »).
func outputIsCJK(output string) bool {
	if output == "" {
		return false
	}
	cjk := countCJK(output)
	if cjk < shortCJKMinRunes {
		return false
	}
	letters := len(reLetters.FindAllString(output, -1))
	// Règle courte : uniquement sur les caractères HAN. Une réponse en kana ou en hangul
	// est presque toujours la traduction demandée par un client latin (« comment dit-on
	// merci en japonais ») : la remédier serait un faux positif coûteux.
	if han := countHan(output); han >= shortCJKMinRunes && han > letters && letters <= shortCJKLettersMax {
		return true
	}
	if cjk < cjkMinRunes {
		return false
	}
	return cjkShare(output) >= cjkMinShare
}

// --- détection sur flux ----------------------------------------------------------

// cjkEscapeRe : certains amonts encodent le non-ASCII en \uXXXX dans leurs trames SSE.
// Un balayage des seuls octets bruts raterait alors toute la réponse chinoise — le
// signal ne marcherait que sur les amonts qui émettent l'UTF-8 tel quel.
var cjkEscapeRe = regexp.MustCompile(`\\u([0-9a-fA-F]{4})`)

// isCJKRune : appartenance d'une rune aux plages CJK (mêmes plages que countCJK).
func isCJKRune(r rune) bool {
	switch {
	case r >= 0x3400 && r <= 0x4DBF, r >= 0x4E00 && r <= 0x9FFF,
		r >= 0xF900 && r <= 0xFAFF, r >= 0x20000 && r <= 0x2FA1F:
		return true
	case r >= 0x3040 && r <= 0x30FF, r >= 0x31F0 && r <= 0x31FF:
		return true
	case r >= 0xAC00 && r <= 0xD7AF, r >= 0x1100 && r <= 0x11FF, r >= 0x3130 && r <= 0x318F:
		return true
	}
	return false
}

// countCJKInStreamFragment : nombre de caractères CJK d'un fragment BRUT de flux SSE,
// en clair ou sous forme d'échappement \uXXXX.
func countCJKInStreamFragment(raw []byte) int {
	s := string(raw)
	n := 0
	for _, r := range s {
		if isCJKRune(r) {
			n++
		}
	}
	for _, m := range cjkEscapeRe.FindAllStringSubmatch(s, -1) {
		if len(m) < 2 {
			continue
		}
		code := 0
		for _, c := range m[1] {
			code <<= 4
			switch {
			case c >= '0' && c <= '9':
				code += int(c - '0')
			case c >= 'a' && c <= 'f':
				code += int(c-'a') + 10
			case c >= 'A' && c <= 'F':
				code += int(c-'A') + 10
			}
		}
		if isCJKRune(rune(code)) {
			n++
		}
	}
	return n
}

// visibleStreamText : texte d'ASSISTANT contenu dans un préfixe de flux SSE, quel que
// soit le protocole (OpenAI `choices[].delta.content`, Anthropic `delta.text`).
//
// Le raisonnement interne (reasoning_content / thinking) est volontairement EXCLU : un
// modèle qui réfléchit en chinois mais répond dans la langue du client ne livre aucune
// fuite au client. Le compter déclenchait des remèdes inutiles (un appel de parc en plus)
// pour des réponses parfaitement correctes.
//
// Le JSON est décodé, ce qui règle du même coup les amonts qui échappent le non-ASCII en
// \uXXXX : après décodage, les caractères CJK sont comptés comme les autres.
func visibleStreamText(prefix []byte) string {
	if len(prefix) == 0 {
		return ""
	}
	var b strings.Builder
	for _, line := range bytes.Split(prefix, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal(payload, &obj); err != nil {
			continue // ligne incomplète ou non JSON : ignorée
		}
		if choices, ok := obj["choices"].([]any); ok {
			for _, ch := range choices {
				m, ok := ch.(map[string]any)
				if !ok {
					continue
				}
				if d, ok := m["delta"].(map[string]any); ok {
					if c, ok := d["content"].(string); ok {
						b.WriteString(c)
					}
				}
				if msg, ok := m["message"].(map[string]any); ok {
					if c, ok := msg["content"].(string); ok {
						b.WriteString(c)
					}
				}
			}
		}
		if d, ok := obj["delta"].(map[string]any); ok {
			if t, ok := d["text"].(string); ok {
				b.WriteString(t)
			}
		}
	}
	return b.String()
}

// StreamFeatures : comptages CJK / lettres latines du texte visible d'un préfixe.
// Exporté pour que le sniffing du flux (côté serveur) sache QUAND il en sait assez pour
// trancher, sans dupliquer la règle de langue.
func StreamFeatures(prefix []byte) (cjk, letters int) {
	txt := visibleStreamText(prefix)
	if txt == "" {
		return 0, 0
	}
	return countCJK(txt), len(reLetters.FindAllString(txt, -1))
}

// WantsLanguageMismatch : vrai si le signal a un poids non nul dans cette configuration.
// L'appelant s'en sert pour NE PAS payer le coût du préfixe de flux quand la fonction
// est éteinte.
func (d *Detector) WantsLanguageMismatch() bool {
	return d != nil && d.wants(SigLanguageMismatch)
}

// AnalyzeStreamPrefix applique la règle à un préfixe brut de flux SSE et retourne le
// même Result que AnalyzeResponse, pour que le comptage et la journalisation soient
// identiques sur les deux chemins.
//
// On ne cherche PAS à extraire proprement le contenu : sur ce chemin il faut trancher
// en quelques trames sans rien avoir écrit au client, et un comptage CJK du fragment
// brut (échappements \uXXXX compris) suffit — une trame `data:` transporte du JSON,
// donc le texte chinois y figure soit tel quel, soit échappé.
//
// Le seuil cjkMinRunes s'applique comme en non-flux : quelques caractères isolés ne
// déclenchent rien. La part (cjkMinShare) n'est pas calculable sur un préfixe — mais un
// premier jet contenant déjà cjkMinRunes caractères chinois est sans ambiguïté : aucune
// réponse anglaise ne commence ainsi.
func (d *Detector) AnalyzeStreamPrefix(reqBody, prefix []byte) Result {
	var res Result
	if !d.wants(SigLanguageMismatch) || len(prefix) == 0 {
		return res
	}
	var input string
	if len(reqBody) > 0 {
		var raw map[string]any
		if err := json.Unmarshal(reqBody, &raw); err == nil {
			input, _, _ = extractParts(raw)
		}
	}
	if userWritesCJK(input) || asksForCJKOutput(strings.ToLower(input)) {
		return res
	}
	n := countCJKInStreamFragment(prefix)
	if n < shortCJKMinRunes {
		return res
	}
	// Mêmes règles qu'en non-flux, appliquées au texte visible du préfixe : une réponse
	// brève entièrement chinoise déclenche, une réponse longue aussi.
	visible := visibleStreamText(prefix)
	if !outputIsCJK(visible) {
		return res
	}
	w := d.weight(SigLanguageMismatch)
	res.Signals = append(res.Signals, Signal{
		Name:   SigLanguageMismatch,
		Weight: w,
		Detail: fmt.Sprintf("entrée sans CJK, début de réponse à %d caractères CJK", n),
	})
	res.Score, res.EffectiveScore, res.Strong, res.Route = w, w, SigLanguageMismatch, true
	return res
}
