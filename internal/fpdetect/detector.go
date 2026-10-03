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
	SigRepeatedLines     = "repeated_lines"
	SigToolSuspicious    = "tool_suspicious"
	SigToolGenericName   = "tool_generic_name"
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
	// SigLanguageMismatch est le seul signal calculé sur la RÉPONSE (voir
	// AnalyzeResponse) : il compare l'entrée à la sortie, et la sortie n'existe
	// qu'après l'appel amont.
	SigLanguageMismatch = "language_mismatch"
)

// defaultWeights : signaux forts >= seuil (4.0) par construction, signaux faibles
// nettement en dessous. Les pénalités ne peuvent annuler un signal fort — c'est
// volontaire, sinon un test PDF contenant un bloc de code ne serait jamais routé.
var defaultWeights = map[string]float64{
	SigGlitchToken:    5.0,
	SigModelQuestion:  5.0,
	SigCutoffExplicit: 5.0,
	// repetition_pattern reste FORT : il ne réagit qu'à une consigne chiffrée
	// explicite (« répète le mot pomme 150 fois »), qu'on ne rencontre pas dans un
	// usage normal.
	SigRepetitionPattern: 4.0,
	SigRepeatRequest:     4.0,
	// Une réponse en chinois à une question qui ne l'est pas est un aveu direct du
	// backend : signal fort, qui route seul (non conditionnel — la forme mesurée en
	// production est sans ambiguïté, et le faux positif « traduis en chinois » est
	// écarté en amont par asksForCJKOutput).
	SigLanguageMismatch: 5.0,

	// Sous le seuil À DESSEIN (4.0) : signaux « forts mais sujets à corroboration »
	// (voir Config.CorroborationSignals). Le relevé de production du 2026-09-29 a
	// montré, en deux vagues, que ces méthodes réagissent au trafic client légitime
	// d'un agent :
	//   pdf_content + tool_count_extreme : pièce jointe et déclaration d'outils ;
	//   mass_repetition + tool_suspicious + repeated_lines : une série de caractères
	//   identiques, un nom d'outil contenant « test »/« probe »/« detect », quinze
	//   lignes identiques — trois choses qu'on trouve dans un prompt de travail
	//   ordinaire (tableau Markdown, outil run_tests, journal collé).
	// Un poids sous le seuil garantit qu'aucun d'eux ne peut router par simple cumul
	// de score : il faut la corroboration ci-dessous.
	SigToolCountExtreme: 2.0,
	SigPDFContent:       2.0,
	SigMassRepetition:   2.0,
	SigToolSuspicious:   2.0,
	SigToolGenericName:  2.0,
	SigRepeatedLines:    2.0,

	SigVeryShort:       1.5,
	SigRecentFact:      2.0,
	SigToolCount:       1.0,
	SigThinkingRequest: 1.0,
	SigSSEShape:        1.5,

	// coding_like passe de -3.0 à -4.0 avec le même relevé : c'est le seul signal qui
	// distingue un agent de codage ordinaire d'une sonde, il doit donc pouvoir
	// neutraliser à lui seul l'accumulation de faibles d'une vraie session de travail.
	SigCodingLike:       -4.0,
	SigLongConversation: -2.0,
}

// defaultCorroboration : signaux forts qui ne suffisent PAS seuls. Ces méthodes font
// partie du fonctionnement normal d'un agent (pièce jointe, déclaration d'outils,
// contenu collé, outil dont le nom contient « test ») : elles n'ont de valeur que
// corroborées par une méthode indépendante. Chaque entrée vient d'une observation de
// production, pas d'une intuition.
var defaultCorroboration = []string{
	SigPDFContent, SigToolCountExtreme,
	SigMassRepetition, SigToolGenericName, SigRepeatedLines, SigToolSuspicious,
}

// DefaultCorroborationSignals retourne la liste par défaut des signaux conditionnels.
func DefaultCorroborationSignals() []string {
	return append([]string(nil), defaultCorroboration...)
}

// StrongSignalNames retourne les signaux forts par défaut, triés (aide au diagnostic
// de configuration et messages d'erreur).
func StrongSignalNames() []string {
	out := append([]string(nil), defaultStrong...)
	sort.Strings(out)
	return out
}

// IsStrongSignal : vrai si le nom est un signal fort par défaut.
func IsStrongSignal(name string) bool {
	for _, s := range defaultStrong {
		if s == name {
			return true
		}
	}
	return false
}

// corroborationWeakMin : nombre minimal de signaux FAIBLES distincts pour corroborer
// un signal conditionnel. Voir Config.CorroborationWeakMin pour la règle complète.
const corroborationWeakMin = 2

// defaultStrong : un seul de ces signaux suffit à router — SAUF s'il figure aussi
// dans CorroborationSignals. Ce sont les signaux spécifiques — un jeton piège, une
// question d'identité, une consigne de répétition — qui n'apparaissent pas dans un
// usage normal. Les autres sont FAIBLES parce que le trafic réel les contient en
// masse : 65 % des requêtes mesurées en production dépassent 50 000 tokens (profil
// agent), envoient couramment plus de 5 outils et activent souvent le raisonnement
// étendu.
var defaultStrong = []string{
	SigGlitchToken, SigModelQuestion, SigCutoffExplicit, SigRepetitionPattern,
	SigRepeatRequest, SigMassRepetition, SigToolSuspicious, SigToolCountExtreme,
	SigPDFContent, SigRepeatedLines, SigToolGenericName, SigLanguageMismatch,
}

// defaultAuthenticity : les SEULS signaux qui envoient une requête chez le fournisseur
// externe (vrai Claude).
//
// Règle posée par l'exploitant (2026-10-02) : « le canal CrazyToken doit uniquement
// servir les requêtes des clients qui cherchent à savoir s'il s'agit d'un modèle Claude
// légitime, ou qui utilisent des outils d'authentification ; dans tous les autres cas,
// absolument tout doit passer par le canal du mapping Claude-DeepSeek ». Règle stricte,
// sans exception.
//
// Ce qui authentifie réellement un modèle :
//   - model_question     : question explicite sur l'identité (« qui es-tu ? ») ;
//   - glitch_token       : jeton piège — l'outil canonique de détection de modèle ;
//   - cutoff_explicit    : question sur la date ou l'auteur de l'entraînement ;
//   - repetition_pattern : consigne chiffrée du type « répète le mot pomme 150 fois ».
//
// Le désaccord de langue (réponse en chinois à une question qui ne l'est pas) est un
// cinquième cas, mais il vit sur le chemin de RÉPONSE et route par lui-même (voir
// fpdetect/cjk.go) : il n'est donc pas listé ici.
//
// Tout le reste — répétitions de caractères, lignes répétées, PDF joints, nombre
// d'outils, noms d'outils génériques, mention d'une année récente, requêtes identiques
// répétées — décrit du TRAVAIL ORDINAIRE : mesuré en production le 2026-10-02, ces
// signaux envoyaient chez le vrai Claude des clients qui écrivaient simplement du code.
// Ils restent OBSERVÉS (visibles dans /status) mais ne décident plus rien.
var defaultAuthenticity = []string{
	SigModelQuestion, SigGlitchToken, SigCutoffExplicit, SigRepetitionPattern,
}

// DefaultAuthenticitySignals : copie de la liste d'authenticité, pour l'exposer dans
// /status (l'exploitant doit pouvoir lire la règle de routage sans relire le code).
func DefaultAuthenticitySignals() []string {
	return append([]string(nil), defaultAuthenticity...)
}

// Config règle le détecteur.
type Config struct {
	Threshold     float64            // seuil du score cumulé
	Weights       map[string]float64 // surcharge des poids par nom de signal
	StrongSignals []string           // signaux forts (voir CorroborationSignals)
	GlitchTokens  []string           // liste effective (défaut : paquet + littérature)
	GlitchMeta    map[string]string  // jeton -> famille de modèles (journalisation)
	RepeatWindow  time.Duration      // fenêtre de détection de répétition (défaut 5 min)
	RepeatCount   int                // nb de répétitions identiques déclenchant (défaut 4)
	State         *State             // état partagé ; nil => état interne

	// CorroborationSignals : sous-ensemble de StrongSignals qui ne route JAMAIS seul.
	// Défaut : pdf_content, tool_count_extreme. Un tel signal est ignoré (ni score, ni
	// décision) tant qu'il n'est pas corroboré, c'est-à-dire tant que :
	//   - un signal fort NON conditionnel est présent (glitch_token, model_question,
	//     cutoff_explicit, mass_repetition, repetition_pattern, tool_suspicious,
	//     repeat_request) ; ou
	//   - au moins CorroborationWeakMin signaux FAIBLES distincts sont présents ET
	//     leur poids cumulé atteint le seuil.
	//
	// La seconde condition est une CONJONCTION, pas une disjonction. C'est ce qui
	// répare le faux positif de production : la requête cliente observée cumulait
	// recent_fact(2.0) + sse_shape(1.5) = 3.5, soit deux signaux faibles mais sous le
	// seuil de 4.0. Une disjonction (« 2 faibles OU cumul atteignant le seuil »)
	// l'aurait laissée router.
	CorroborationSignals []string
	// CorroborationWeakMin : nombre minimal de signaux faibles distincts requis pour
	// la seconde branche ci-dessus (défaut 2, borne basse 1).
	CorroborationWeakMin int
}

// DefaultConfig retourne la configuration par défaut, glitch tokens inclus.
func DefaultConfig() Config {
	return Config{
		Threshold:            4.0,
		Weights:              map[string]float64{},
		StrongSignals:        append([]string(nil), defaultStrong...),
		CorroborationSignals: append([]string(nil), defaultCorroboration...),
		CorroborationWeakMin: corroborationWeakMin,
		GlitchTokens:         defaultGlitchStrings(),
		GlitchMeta:           defaultGlitchMeta(),
		RepeatWindow:         5 * time.Minute,
		RepeatCount:          4,
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
	// Ignored : signal déclenché mais écarté de la décision faute de corroboration.
	// Il reste visible pour l'observation (le panneau compte toujours la méthode qui
	// a réagi), mais il ne pèse ni sur le score effectif ni sur le routage.
	Ignored bool
}

// Result est le verdict d'analyse.
type Result struct {
	Score           float64  // somme brute des poids de tous les signaux déclenchés
	EffectiveScore  float64  // Score moins les signaux ignorés faute de corroboration
	Signals         []Signal // tous les signaux déclenchés, y compris les ignorés
	Strong          string   // signal fort DÉCISIF ("" si aucun ne décide du routage)
	Route           bool     // décision : envoyer vers le vrai modèle
	Corroborated    bool     // un signal conditionnel a-t-il été corroboré ?
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

// IgnoredNames liste les signaux écartés faute de corroboration (ordre d'apparition).
func (r Result) IgnoredNames() []string {
	var out []string
	for _, s := range r.Signals {
		if s.Ignored {
			out = append(out, s.Name)
		}
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
//
// Un signal ignoré est marqué « ~ » : sans cette marque, un lecteur de journal
// croirait que la méthode a suffi, et le relevé de calibration serait faux.
func (r Result) Explain() string {
	if len(r.Signals) == 0 {
		return fmt.Sprintf("score=%.1f aucun signal", r.Score)
	}
	parts := make([]string, 0, len(r.Signals))
	for _, s := range r.Signals {
		mark := ""
		if s.Ignored {
			mark = "~"
		}
		parts = append(parts, fmt.Sprintf("%s%s(%+.1f)", mark, s.Name, s.Weight))
	}
	tag := ""
	if r.Strong != "" {
		tag = " fort=" + r.Strong
	}
	eff := ""
	if r.EffectiveScore != r.Score {
		eff = fmt.Sprintf(" effectif=%.1f", r.EffectiveScore)
	}
	return fmt.Sprintf("score=%.1f%s [%s]%s", r.Score, eff, strings.Join(parts, ", "), tag)
}

// AnalyzeResponse applique les signaux qui dépendent de la RÉPONSE à reqBody.
//
// Un seul signal vit ici aujourd'hui : language_mismatch (entrée sans CJK, réponse en
// CJK). Il est séparé de Analyze parce que le détecteur de requête tourne AVANT tout
// appel amont : au moment où l'on saurait, la réponse est déjà produite. L'appelant
// décide quoi faire du verdict — sur une réponse non encore envoyée, un reroutage est
// encore possible ; sur un flux déjà à moitié transmis, non.
//
// Le texte comparé est celui de l'UTILISATEUR, pas du système : un projet dont le
// prompt système contient du chinois ne doit pas blanchir une réponse chinoise à une
// question anglaise.
func (d *Detector) AnalyzeResponse(reqBody []byte, output string) Result {
	res := Result{}
	if !d.wants(SigLanguageMismatch) {
		return res
	}
	if len(reqBody) == 0 || output == "" {
		return res
	}
	var raw map[string]any
	if err := json.Unmarshal(reqBody, &raw); err != nil {
		return res
	}
	input, _ := extractParts(raw)
	if !languageMismatch(input, output) {
		return res
	}
	res.Signals = append(res.Signals, Signal{
		Name:   SigLanguageMismatch,
		Weight: d.weight(SigLanguageMismatch),
		Detail: fmt.Sprintf("entrée sans CJK, réponse à %d caractères CJK (%.0f%%)",
			countCJK(output), cjkShare(output)*100),
	})
	res.Score = d.weight(SigLanguageMismatch)
	res.Strong = SigLanguageMismatch
	res.EffectiveScore = res.Score
	res.Route = true
	return res
}

// wants : vrai si le signal a un poids non nul dans cette configuration.
func (d *Detector) wants(name string) bool { return d.weight(name) != 0 }

// ExplainDetailed ajoute le détail de chaque signal — compteurs, pourcentages, nom du
// FRAGMENT de notre propre liste qui a réagi. Aucun contenu de requête n'y figure : le
// détail est produit par le détecteur, jamais recopié du client.
//
// Sert aux journaux d'exploitation : sans lui, un relevé dit « tool_suspicious a
// réagi » sans permettre de savoir lequel des fragments est en cause, et il faut
// reproduire l'incident pour trancher.
func (r Result) ExplainDetailed() string {
	if len(r.Signals) == 0 {
		return r.Explain()
	}
	parts := make([]string, 0, len(r.Signals))
	for _, s := range r.Signals {
		mark := ""
		if s.Ignored {
			mark = "~"
		}
		if s.Detail == "" {
			parts = append(parts, fmt.Sprintf("%s%s(%+.1f)", mark, s.Name, s.Weight))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s%s(%+.1f: %s)", mark, s.Name, s.Weight, s.Detail))
	}
	tag := ""
	if r.Strong != "" {
		tag = " fort=" + r.Strong
	}
	return fmt.Sprintf("score=%.1f effectif=%.1f [%s]%s", r.Score, r.EffectiveScore, strings.Join(parts, ", "), tag)
}

// Detector porte la configuration et l'état temporel.
type Detector struct {
	cfg           Config
	strong        map[string]bool
	corroboration map[string]bool
	// authenticity : signaux qui, seuls, envoient la requête chez le fournisseur
	// externe. Voir defaultAuthenticity pour la règle et sa justification.
	authenticity map[string]bool
	state        *State
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
	if len(cfg.CorroborationSignals) == 0 {
		cfg.CorroborationSignals = def.CorroborationSignals
	}
	if cfg.CorroborationWeakMin < 1 {
		cfg.CorroborationWeakMin = def.CorroborationWeakMin
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
	corroboration := make(map[string]bool, len(cfg.CorroborationSignals))
	for _, s := range cfg.CorroborationSignals {
		// Un signal de corroboration qui n'est pas fort n'a aucun sens : il ne serait
		// de toute façon jamais décisif. On ne le retient que s'il est bien fort.
		if strong[s] {
			corroboration[s] = true
		}
	}
	if cfg.State == nil {
		cfg.State = NewState(cfg.RepeatWindow, 0, 0)
	}
	// Authenticity : liste FIXE (voir defaultAuthenticity). Elle n'est pas configurable
	// à dessein : c'est la règle produit « le vrai Claude ne sert qu'aux tests
	// d'authenticité », pas un réglage d'exploitation qui pourrait dériver.
	authenticity := make(map[string]bool, len(defaultAuthenticity))
	for _, s := range defaultAuthenticity {
		authenticity[s] = true
	}
	return &Detector{cfg: cfg, strong: strong, corroboration: corroboration, authenticity: authenticity, state: cfg.State}
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
	// add enregistre un signal déclenché. Il ne décide de rien : Strong et Route sont
	// posés une seule fois, à la fin, par decide() — deux sources de vérité pour la
	// décision finiraient par diverger.
	add := func(name string, detail string) {
		w := d.weight(name)
		if w == 0 {
			return
		}
		res.Signals = append(res.Signals, Signal{Name: name, Weight: w, Detail: detail})
		res.Score += w
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

	// 6) Motif de répétition structuré, en deux signaux DISTINCTS.
	//
	// La consigne chiffrée explicite (« répète le mot pomme 150 fois ») est une
	// méthode de sondage précise : elle reste un signal FORT qui route seul. Quinze
	// lignes identiques, en revanche, décrivent aussi bien un journal collé ou un
	// tableau Markdown : mesuré en production sur du trafic client réel, c'est donc un
	// signal CONDITIONNEL. Les confondre faisait router du travail ordinaire.
	if n := repetitionInstruction(user); n >= 20 {
		add(SigRepetitionPattern, fmt.Sprintf("consigne de %d répétitions", n))
	}
	if n, _ := maxLineRepeat(user); n >= 15 {
		add(SigRepeatedLines, fmt.Sprintf("%d lignes identiques", n))
	}

	// 7) Outils : noms suspects (faible spécificité) et compte anormal.
	tools := extractTools(raw)
	if len(tools) > 0 {
		// Deux niveaux : un nom d'outil qui DÉSIGNE une méthode d'empreinte
		// (fingerprint, count_tokens, model_info…) est spécifique et route seul ; un
		// nom qui contient « test », « probe », « detect », « ping » est banal dans du
		// code réel (run_tests, probe_health) — mesuré en production — et n'est donc
		// qu'un indice à corroborer.
		if frag := suspiciousToolName(tools); frag != "" {
			add(SigToolSuspicious, "nom d'outil de sondage: "+frag)
		}
		if frag := genericToolName(tools); frag != "" {
			add(SigToolGenericName, "nom d'outil banal: "+frag)
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
	// signal fort décisif (voir le calcul de Route ci-dessous).
	if isCodingLike(raw, user) {
		res.CodingLike = true
		add(SigCodingLike, "requête de développement/outils")
	} else if res.EstimatedTokens >= 400 {
		add(SigLongConversation, "échange conversationnel long")
	}

	d.decide(&res)
	return res
}

// decide applique la règle de corroboration puis la règle de score.
//
// Le principe : un signal fort CONDITIONNEL (aujourd'hui pdf_content et
// tool_count_extreme) est une méthode qui réagit aussi au trafic légitime. Tant qu'il
// n'est pas corroboré, il est IGNORÉ — retiré du score effectif et de la décision.
// Sans cette mise à l'écart, le cumul du score le ferait router de toute façon avec
// deux signaux faibles quelconques, et la corroboration ne serait qu'un commentaire.
//
//	faille évitée : pdf_content(2.0) + recent_fact(2.0) + sse_shape(1.5) = 5.5 >= 4.0
//	                 routait alors que le signal conditionnel n'était pas corroboré.
func (d *Detector) decide(res *Result) {
	// Séparation des signaux. Les pénalités (poids négatif) ne comptent jamais comme
	// corroboration : un indice de développement ne corrobore pas une sonde.
	var decisiveStrong string
	var conditional []int
	weakCount := 0
	weakSum := 0.0
	for i, s := range res.Signals {
		switch {
		case d.corroboration[s.Name]:
			conditional = append(conditional, i)
		case d.strong[s.Name]:
			// Signal fort inconditionnel : suffit seul, et corrobore les conditionnels.
			if decisiveStrong == "" {
				decisiveStrong = s.Name
			}
		case s.Weight > 0:
			weakCount++
			weakSum += s.Weight
		}
	}

	// condition : un signal fort inconditionnel est présent, OU la conjonction
	// « assez de signaux faibles distincts » ET « leur cumul atteint le seuil ».
	condition := decisiveStrong != "" ||
		(weakCount >= d.cfg.CorroborationWeakMin && weakSum >= d.cfg.Threshold)

	// Corroborated ne parle que des signaux conditionnels : il vaut false tant qu'aucun
	// n'est présent (une requête qui n'a que des signaux inconditionnels n'a rien à
	// corroborer, et l'appelant ne doit pas croire qu'un arbitrage a eu lieu).
	if len(conditional) == 0 {
		res.Corroborated = false
	} else {
		res.Corroborated = condition
		if condition {
			// Le premier conditionnel corroboré devient le signal décisif affiché, si
			// aucun signal inconditionnel n'était déjà décisif.
			if decisiveStrong == "" {
				decisiveStrong = res.Signals[conditional[0]].Name
			}
		} else {
			for _, i := range conditional {
				res.Signals[i].Ignored = true
				res.EffectiveScore -= res.Signals[i].Weight
			}
		}
	}

	res.EffectiveScore += res.Score
	res.Strong = decisiveStrong
	// DÉCISION (règle stricte de l'exploitant, 2026-10-02) : seuls les signaux
	// d'AUTHENTICITÉ routent. Ni un signal de travail ordinaire, ni un cumul de tels
	// signaux, ni même un signal conditionnel corroboré ne peut envoyer un client chez
	// le vrai Claude : tout ce qui n'est pas un test d'authenticité doit rester sur le
	// canal du mapping Claude-DeepSeek.
	//
	// Le score cumulé n'est donc plus une condition de routage : il ne sert plus qu'à
	// l'observation (/status). La corroboration garde son rôle d'affichage (elle
	// explique pourquoi un signal conditionnel est retenu ou écarté) mais ne décide plus.
	res.Route = d.authenticity[res.Strong]
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
