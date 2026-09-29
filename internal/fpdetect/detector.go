package fpdetect

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Noms des signaux. Référencés dans la configuration (poids, signaux forts) : ce sont
// des identifiants stables, ne pas les renommer sans migrer les configurations.
const (
	SigGlitchToken       = "glitch_token"
	SigModelQuestion     = "model_question"
	SigCutoffExplicit    = "cutoff_explicit"
	SigMassRepetition    = "mass_repetition"
	SigRepetitionPattern = "repetition_pattern"
	SigToolSuspicious    = "tool_suspicious"
	SigToolCountExtreme  = "tool_count_extreme"
	SigPDFContent        = "pdf_content"
	SigRepeatRequest     = "repeat_request"
	SigVeryShort         = "very_short"
	SigRecentFact        = "recent_fact"
	SigToolCount         = "tool_count"
	SigThinkingRequest   = "thinking_request"
	SigSSEShape          = "sse_shape"
	SigCodingLike        = "coding_like"
	SigLongConversation  = "long_conversation"
)

// defaultWeights : signaux forts >= seuil (4.0) par construction, signaux faibles
// nettement en dessous. Les pénalités ne peuvent annuler un signal fort — c'est
// volontaire, sinon un test PDF contenant un bloc de code ne serait jamais routé.
var defaultWeights = map[string]float64{
	SigGlitchToken:       5.0,
	SigModelQuestion:     5.0,
	SigCutoffExplicit:    5.0,
	SigMassRepetition:    4.0,
	SigRepetitionPattern: 4.0,
	SigToolSuspicious:    4.0,
	SigToolCountExtreme:  4.0,
	SigPDFContent:        4.0,
	SigRepeatRequest:     4.0,

	SigVeryShort:       1.5,
	SigRecentFact:      2.0,
	SigToolCount:       1.0,
	SigThinkingRequest: 1.0,
	SigSSEShape:        1.5,

	SigCodingLike:       -3.0,
	SigLongConversation: -2.0,
}

// defaultStrong : un seul de ces signaux suffit à router. Ce sont les signaux
// spécifiques — un jeton piège, une question d'identité, une consigne de répétition —
// qui n'apparaissent pas dans un usage normal. Les autres sont FAIBLES parce que le
// trafic réel les contient en masse : 65 % des requêtes mesurées en production
// dépassent 50 000 tokens (profil agent), envoient couramment plus de 5 outils et
// activent souvent le raisonnement étendu.
var defaultStrong = []string{
	SigGlitchToken, SigModelQuestion, SigCutoffExplicit, SigMassRepetition,
	SigRepetitionPattern, SigToolSuspicious, SigToolCountExtreme, SigPDFContent,
	SigRepeatRequest,
}

// Config règle le détecteur.
type Config struct {
	Threshold     float64            // seuil du score cumulé
	Weights       map[string]float64 // surcharge des poids par nom de signal
	StrongSignals []string           // signaux suffisants à eux seuls
	GlitchTokens  []string           // liste effective (défaut : paquet + littérature)
	GlitchMeta    map[string]string  // jeton -> famille de modèles (journalisation)
	RepeatWindow  time.Duration      // fenêtre de détection de répétition (défaut 5 min)
	RepeatCount   int                // nb de répétitions identiques déclenchant (défaut 4)
	State         *State             // état partagé ; nil => état interne
}

// DefaultConfig retourne la configuration par défaut, glitch tokens inclus.
func DefaultConfig() Config {
	return Config{
		Threshold:     4.0,
		Weights:       map[string]float64{},
		StrongSignals: append([]string(nil), defaultStrong...),
		GlitchTokens:  defaultGlitchStrings(),
		GlitchMeta:    defaultGlitchMeta(),
		RepeatWindow:  5 * time.Minute,
		RepeatCount:   4,
	}
}

func defaultGlitchStrings() []string {
	toks := DefaultGlitchTokens()
	out := make([]string, 0, len(toks))
	for _, t := range toks {
		out = append(out, t.Token)
	}
	return out
}

func defaultGlitchMeta() map[string]string {
	m := map[string]string{}
	for _, t := range DefaultGlitchTokens() {
		if t.ModelSeries != "" {
			m[t.Token] = t.ModelSeries
		}
	}
	return m
}

// Signal est un critère déclenché.
type Signal struct {
	Name   string
	Weight float64
	Detail string
}

// Result est le verdict d'analyse.
type Result struct {
	Score           float64
	Signals         []Signal
	Strong          string // nom du premier signal fort rencontré ("" si aucun)
	Route           bool   // décision : envoyer vers le vrai modèle
	UserChars       int
	EstimatedTokens int
	CodingLike      bool
}

// SignalNames liste les signaux déclenchés (tests et journalisation).
func (r Result) SignalNames() []string {
	out := make([]string, 0, len(r.Signals))
	for _, s := range r.Signals {
		out = append(out, s.Name)
	}
	return out
}

func (r Result) has(name string) bool {
	for _, s := range r.Signals {
		if s.Name == name {
			return true
		}
	}
	return false
}

// Explain produit une ligne de journal **sans jamais recopier le contenu** de la
// requête : seuls les noms de signaux, leurs poids et des mesures agrégées.
func (r Result) Explain() string {
	if len(r.Signals) == 0 {
		return fmt.Sprintf("score=%.1f aucun signal", r.Score)
	}
	parts := make([]string, 0, len(r.Signals))
	for _, s := range r.Signals {
		parts = append(parts, fmt.Sprintf("%s(%+.1f)", s.Name, s.Weight))
	}
	tag := ""
	if r.Strong != "" {
		tag = " fort=" + r.Strong
	}
	return fmt.Sprintf("score=%.1f [%s]%s", r.Score, strings.Join(parts, ", "), tag)
}

// Detector porte la configuration et l'état temporel.
type Detector struct {
	cfg    Config
	strong map[string]bool
	state  *State
}

// New construit un détecteur. Les champs de Config absents retombent sur les défauts.
func New(cfg Config) *Detector {
	def := DefaultConfig()
	if cfg.Threshold <= 0 {
		cfg.Threshold = def.Threshold
	}
	if cfg.RepeatWindow <= 0 {
		cfg.RepeatWindow = def.RepeatWindow
	}
	if cfg.RepeatCount <= 0 {
		cfg.RepeatCount = def.RepeatCount
	}
	if len(cfg.StrongSignals) == 0 {
		cfg.StrongSignals = def.StrongSignals
	}
	if len(cfg.GlitchTokens) == 0 {
		cfg.GlitchTokens = def.GlitchTokens
	}
	if cfg.GlitchMeta == nil {
		cfg.GlitchMeta = def.GlitchMeta
	}
	strong := make(map[string]bool, len(cfg.StrongSignals))
	for _, s := range cfg.StrongSignals {
		strong[s] = true
	}
	if cfg.State == nil {
		cfg.State = NewState(cfg.RepeatWindow, 0, 0)
	}
	return &Detector{cfg: cfg, strong: strong, state: cfg.State}
}

func (d *Detector) weight(name string) float64 {
	if v, ok := d.cfg.Weights[name]; ok {
		return v
	}
	if v, ok := defaultWeights[name]; ok {
		return v
	}
	return 0
}

// State expose l'état temporel (observabilité / tests).
func (d *Detector) State() *State { return d.state }

// AnalyzeAt analyse un corps de requête. now est injectable pour les tests.
func (d *Detector) AnalyzeAt(body []byte, clientKey string, now time.Time) Result {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return Result{} // corps illisible : aucun signal, l'appelant retombe en route normale
	}
	user, system := extractParts(raw)
	userLower := strings.ToLower(user)
	fullLower := userLower + "\n" + strings.ToLower(system)

	res := Result{UserChars: len(user), EstimatedTokens: estimateTokens(user)}
	add := func(name string, detail string) {
		w := d.weight(name)
		if w == 0 {
			return
		}
		res.Signals = append(res.Signals, Signal{Name: name, Weight: w, Detail: detail})
		res.Score += w
		if d.strong[name] && res.Strong == "" {
			res.Strong = name
		}
	}

	// 1) Jetons pièges. Le détail ne recopie jamais le jeton (charge utile).
	if n, series := d.matchGlitch(fullLower); n > 0 {
		add(SigGlitchToken, fmt.Sprintf("%d jeton(s) piège%s", n, series))
	}

	// 2) Question d'identité du modèle (multilingue), en deux niveaux.
	//    Les formes ambiguës (« what model », « 什么模型 ») apparaissent aussi dans une
	//    question commerciale légitime — « Which model is best for summarisation? ».
	//    Elles ne comptent donc que sur une requête très brève (une sonde), sinon
	//    elles enverraient du trafic client normal vers le modèle payant.
	if strict, loose := firstIdentityHit(userLower); strict != "" {
		add(SigModelQuestion, "question explicite sur l'identité")
	} else if loose != "" && len([]rune(user)) <= looseIdentityMaxRunes {
		add(SigModelQuestion, "forme brève « quel modèle »")
	}

	// 3) Question explicite sur la date de coupure de connaissance.
	if hitContains(userLower, cutoffPhrases) {
		add(SigCutoffExplicit, "question sur la date de coupure")
	}

	// 4) Fait récent — signal FAIBLE : « que s'est-il passé en 2026 » est une question
	//    légitime. Il ne route qu'accumulé avec autre chose.
	if y := recentYearMention(userLower); y != "" {
		add(SigRecentFact, "mention récente: "+y)
	} else if hitContains(userLower, recentFactPhrases) {
		add(SigRecentFact, "formulation « fait récent »")
	}

	// 5) Répétition massive (série de caractères, ou un mot qui domine le texte).
	if run := longestCharRun(user); run >= 20 {
		add(SigMassRepetition, fmt.Sprintf("série de %d caractères identiques", run))
	} else if share, w := maxTokenShare(user); share >= 0.6 && len(user) > 40 {
		add(SigMassRepetition, fmt.Sprintf("mot répété %.0f%% du texte (len=%d)", share*100, len(w)))
	}

	// 6) Motif de répétition structuré (lignes identiques, ou consigne chiffrée).
	if n, _ := maxLineRepeat(user); n >= 15 {
		add(SigRepetitionPattern, fmt.Sprintf("%d lignes identiques", n))
	} else if n := repetitionInstruction(user); n >= 20 {
		add(SigRepetitionPattern, fmt.Sprintf("consigne de %d répétitions", n))
	}

	// 7) Outils : noms suspects (faible spécificité) et compte anormal.
	tools := extractTools(raw)
	if len(tools) > 0 {
		if frag := suspiciousToolName(tools); frag != "" {
			add(SigToolSuspicious, "nom d'outil suspect: "+frag)
		}
		if hasEmptyTool(tools) {
			add(SigToolSuspicious, "outil sans nom/description")
		}
		if len(tools) >= toolCountSuspiciousMin {
			add(SigToolCountExtreme, fmt.Sprintf("%d outils", len(tools)))
		} else if len(tools) >= toolCountNotable {
			add(SigToolCount, fmt.Sprintf("%d outils", len(tools)))
		}
	}

	// 8) PDF : bloc document, URL .pdf, ou base64 d'en-tête %PDF (JVBERi).
	if kind := detectPDF(raw, user, fullLower); kind != "" {
		add(SigPDFContent, kind)
	}

	// 9) Réglages de streaming inhabituels.
	if keys := unusualStreamingKeys(raw); len(keys) > 0 {
		add(SigSSEShape, "paramètres de flux: "+strings.Join(keys, ","))
	}

	// 10) Requête explicitement « thinking ».
	if thinkingRequested(raw) {
		add(SigThinkingRequest, "raisonnement étendu demandé")
	}

	// 11) Requête très courte — FAIBLE : « corrige cette faute » est légitime.
	if res.EstimatedTokens > 0 && res.EstimatedTokens < 10 {
		add(SigVeryShort, fmt.Sprintf("~%d tokens", res.EstimatedTokens))
	}

	// 12) Répétition identique par le même client (test de latence / RUT).
	if clientKey != "" {
		h := bodyHash(body)
		if n := d.state.Observe(clientKey, h, now); n >= d.cfg.RepeatCount {
			add(SigRepeatRequest, fmt.Sprintf("%d requêtes identiques", n))
		}
	}

	// Pénalités : indices de développement courant. Elles ne peuvent pas annuler un
	// signal fort (voir le calcul de Route ci-dessous).
	if isCodingLike(raw, user) {
		res.CodingLike = true
		add(SigCodingLike, "requête de développement/outils")
	} else if res.EstimatedTokens >= 400 {
		add(SigLongConversation, "échange conversationnel long")
	}

	res.Route = res.Strong != "" || res.Score >= d.cfg.Threshold
	return res
}

// Analyze analyse avec l'horloge courante.
func (d *Detector) Analyze(body []byte, clientKey string) Result {
	return d.AnalyzeAt(body, clientKey, time.Now())
}

// looseIdentityMaxRunes : longueur maximale du texte utilisateur pour qu'une forme
// ambiguë (« what model ») soit considérée comme une sonde. Au-delà, c'est une
// question ordinaire.
const looseIdentityMaxRunes = 24

// looseIdentityModelRe repère les motifs qui ne parlent QUE de « modèle » — sans
// pronom de seconde personne ni verbe d'état, donc ambigu.
//
// Les formes hindi (मॉडल) et arabe (نموذج) doivent être celles réellement employées
// pour « modèle » : les translittérations littérales (« नमूना » = spécimen,
// « طراز » = type/patron) ne figurent dans aucune des tournures sondes, donc les
// lister ici ne servait à rien et laissait au contraire les vraies tournures
// hindi/arabe classées explicites même au milieu d'une question légitime.
var looseIdentityModelRe = regexp.MustCompile(`(?i)model|llm|mod[eè]le|modell|modelo|modelul|модель|模型|モデル|모델|mô hình|मॉडल|نموذج`)

// selfReferenceRe repère une autoreference : « are you », « es-tu », « 你是 », « あなた »…
//
// Les pronoms de deuxième personne y figurent parce qu'ils sont le seul indice sûr
// pour distinguer « quel modèle » (ambigu) de « quel modèle es-tu » (sonde). Sans
// « أنت », la correction des mots arabes/hindi ci-dessus aurait fait retomber
// « أي نموذج أنت » dans la catégorie ambiguë, donc affaibli la détection.
// « أنت » et « انت » (sans hamza, la graphie réelle au clavier arabe) : cette dernière
// n'est employée que sur les MOTIFS, jamais sur le texte client, donc la séquence
// « انت » qu'on trouve aussi dans « انتهاء » ne peut pas provoquer de faux positif ici.
var selfReferenceRe = regexp.MustCompile(`(?i)are you|es-tu|eres |bist du|sei tu|ты |sen |kamu |tu es|voc[eê] |你|あなた|너|bạn|siz|jij|jesteś|ty jesteś|هَلْ|أنت|انت|तुम|आप|identify|identifie|identifiez|identif[ií]cate|qui |who |何|哪个|哪個|什么|什麼`)

// isLooseIdentityPhrase : ambigu = parle de « modèle » SANS autoreference ET tient en
// trois mots ou moins. La borne de mots évite de classer ambiguë une vraie question
// d'identité formulée dans une langue dont je ne connais pas le verbe (« hangi model
// cevap veriyor », turc : 4 mots -> non ambigu). Sans cette borne, la phrase turque
// était rejetée parce que trop longue pour la condition de brièveté.
//
// Limite connue : la borne porte sur le NOMBRE DE MOTS du motif, ce qui pénalise les
// langues qui expriment la même question en plus de mots. « कौन सा मॉडल है » (hindi,
// 4 mots) reste donc classé explicite et déclenche le signal fort même s'il apparaît
// au milieu d'une phrase légitime. C'est volontairement prudent dans ce sens : mieux
// vaut sur-router une question sur les modèles que laisser passer une sonde.
func isLooseIdentityPhrase(p string) bool {
	if !looseIdentityModelRe.MatchString(p) {
		return false
	}
	if selfReferenceRe.MatchString(p) {
		return false
	}
	return len(strings.Fields(p)) <= 3
}

// firstIdentityHit retourne (forme explicite, forme ambiguë) trouvées dans le texte.
//
// La table peut contenir les deux : « hangi model » (ambigu, 2 mots) et « hangi model
// cevap veriyor » (explicite, 4 mots). S'arrêter au premier motif de la table ferait
// gagner l'ambigu et perdre la détection — d'où la préférence explicite.
func firstIdentityHit(lower string) (strict, loose string) {
	for _, p := range modelIdentityPhrases {
		if p == "" || !strings.Contains(lower, p) {
			continue
		}
		if !isLooseIdentityPhrase(p) {
			return p, loose
		}
		if loose == "" {
			loose = p
		}
	}
	return "", loose
}

// --- correspondance multilingue -------------------------------------------------

// hitContains teste des sous-chaînes minuscules. Pas d'expression régulière ni de \b :
// les langues sans séparateurs de mots (chinois, japonais, coréen, thaï) ne s'y prêtent
// pas, et la comparaison par sous-chaîne est prévisible donc testable.
func hitContains(lower string, phrases []string) bool {
	for _, p := range phrases {
		if p != "" && strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

func (d *Detector) matchGlitch(lower string) (int, string) {
	n := 0
	series := map[string]bool{}
	for _, tok := range d.cfg.GlitchTokens {
		if tok == "" {
			continue
		}
		if strings.Contains(lower, strings.ToLower(tok)) {
			n++
			if s, ok := d.cfg.GlitchMeta[tok]; ok && s != "" {
				series[s] = true
			}
		}
	}
	if n == 0 {
		return 0, ""
	}
	if len(series) == 0 {
		return n, ""
	}
	keys := make([]string, 0, len(series))
	for s := range series {
		keys = append(keys, s)
	}
	sort.Strings(keys)
	return n, " (" + strings.Join(keys, ",") + ")"
}

// --- extraction -----------------------------------------------------------------

func textOf(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var b strings.Builder
		for _, item := range t {
			switch it := item.(type) {
			case string:
				b.WriteString(it)
				b.WriteByte('\n')
			case map[string]any:
				if s, ok := it["text"].(string); ok {
					b.WriteString(s)
					b.WriteByte('\n')
				} else if s, ok := it["content"].(string); ok {
					b.WriteString(s)
					b.WriteByte('\n')
				}
			}
		}
		return b.String()
	}
	return ""
}

// extractParts retourne (texte utilisateur, texte système). Le système est isolé :
// un long prompt système signale un agent de codage, pas une sonde.
func extractParts(raw map[string]any) (string, string) {
	var user, sys strings.Builder
	if s := textOf(raw["system"]); s != "" {
		sys.WriteString(s)
	}
	if msgs, ok := raw["messages"].([]any); ok {
		for _, m := range msgs {
			mm, ok := m.(map[string]any)
			if !ok {
				continue
			}
			role, _ := mm["role"].(string)
			txt := textOf(mm["content"])
			switch role {
			case "system", "developer":
				sys.WriteString(txt)
			default:
				user.WriteString(txt)
			}
		}
	}
	return user.String(), sys.String()
}

func estimateTokens(s string) int { return len(s) / 4 }

func bodyHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// --- détecteurs élémentaires ----------------------------------------------------

func longestCharRun(s string) int {
	best, run := 0, 0
	var prev rune
	for i, r := range s {
		if i > 0 && r == prev {
			run++
		} else {
			run = 1
		}
		prev = r
		if run > best {
			best = run
		}
	}
	return best
}

func maxTokenShare(s string) (float64, string) {
	fields := strings.Fields(s)
	if len(fields) < 10 {
		return 0, ""
	}
	counts := map[string]int{}
	for _, f := range fields {
		counts[f]++
	}
	top, n := "", 0
	for w, c := range counts {
		if c > n || (c == n && w < top) {
			top, n = w, c
		}
	}
	return float64(n) / float64(len(fields)), top
}

func maxLineRepeat(s string) (int, string) {
	counts := map[string]int{}
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(l)
		if l != "" {
			counts[l]++
		}
	}
	top, n := "", 0
	for l, c := range counts {
		if c > n {
			top, n = l, c
		}
	}
	return n, top
}

func extractTools(raw map[string]any) []map[string]any {
	var out []map[string]any
	for _, key := range []string{"tools", "functions"} {
		if arr, ok := raw[key].([]any); ok {
			for _, t := range arr {
				if m, ok := t.(map[string]any); ok {
					out = append(out, m)
				}
			}
		}
	}
	return out
}

func toolName(t map[string]any) string {
	if s, ok := t["name"].(string); ok && s != "" {
		return s
	}
	if fn, ok := t["function"].(map[string]any); ok {
		if s, ok := fn["name"].(string); ok {
			return s
		}
	}
	return ""
}

func suspiciousToolName(tools []map[string]any) string {
	for _, t := range tools {
		name := strings.ToLower(toolName(t))
		if name == "" {
			continue
		}
		for _, frag := range suspiciousToolNameFragments {
			if frag != "" && strings.Contains(name, frag) {
				return frag
			}
		}
	}
	return ""
}

// hasEmptyTool : outil déclaré sans nom, ou sans description ni paramètres — un outil
// réel a toujours au moins un nom et une description (c'est ce que le modèle lit).
func hasEmptyTool(tools []map[string]any) bool {
	for _, t := range tools {
		if toolName(t) == "" {
			return true
		}
	}
	return false
}

func detectPDF(raw map[string]any, user, fullLower string) string {
	// Bloc de document explicite (formes Anthropic et OpenAI).
	if msgs, ok := raw["messages"].([]any); ok {
		for _, m := range msgs {
			mm, ok := m.(map[string]any)
			if !ok {
				continue
			}
			blocks, ok := mm["content"].([]any)
			if !ok {
				continue
			}
			for _, b := range blocks {
				bm, ok := b.(map[string]any)
				if !ok {
					continue
				}
				if t, _ := bm["type"].(string); t == "document" {
					return "bloc document"
				}
				if src, ok := bm["source"].(map[string]any); ok {
					if mt, _ := src["media_type"].(string); strings.Contains(mt, "pdf") {
						return "pièce jointe PDF (media_type)"
					}
				}
				if iu, ok := bm["image_url"].(map[string]any); ok {
					if u, _ := iu["url"].(string); strings.Contains(strings.ToLower(u), ".pdf") {
						return "URL .pdf"
					}
				}
			}
		}
	}
	// En-tête de fichier PDF encodé en base64 : « %PDF- » => JVBERi.
	if strings.Contains(user, "JVBERi") {
		return "PDF encodé en base64"
	}
	if strings.Contains(fullLower, ".pdf") {
		return "référence .pdf"
	}
	return ""
}

// unusualStreamingKeys : paramètres de flux qui sortent de l'ordinaire. Le simple
// `stream: true` est le cas normal de tout client conversationnel — l'inclure
// déclencherait sse_shape sur la totalité du trafic en flux.
func unusualStreamingKeys(raw map[string]any) []string {
	var found []string
	for _, k := range streamingParamKeys {
		if k == "stream" {
			continue
		}
		if _, ok := raw[k]; ok {
			found = append(found, k)
		}
	}
	if so, ok := raw["stream_options"].(map[string]any); ok {
		for _, k := range []string{"include_usage", "continuous_usage_stats"} {
			if _, ok := so[k]; ok {
				found = append(found, "stream_options."+k)
			}
		}
	}
	sort.Strings(found)
	return found
}

func thinkingRequested(raw map[string]any) bool {
	if v, ok := raw["thinking"]; ok {
		switch t := v.(type) {
		case map[string]any:
			if s, _ := t["type"].(string); strings.EqualFold(s, "enabled") {
				return true
			}
			if b, ok := t["enabled"].(bool); ok && b {
				return true
			}
		case bool:
			return t
		}
	}
	if b, ok := raw["extended_thinking"].(bool); ok && b {
		return true
	}
	if b, ok := raw["enable_thinking"].(bool); ok && b {
		return true
	}
	return false
}

// isCodingLike : indices forts qu'il s'agit de développement courant.
func isCodingLike(raw map[string]any, user string) bool {
	if len(extractTools(raw)) > 0 {
		return true
	}
	for _, k := range []string{"tool_choice", "tool_calls", "response_format", "parallel_tool_calls"} {
		if _, ok := raw[k]; ok {
			return true
		}
	}
	return codingHintRe.MatchString(user)
}
