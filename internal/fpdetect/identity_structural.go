package fpdetect

import (
	"strconv"
	"strings"
	"unicode"
)

// Détection STRUCTURELLE des questions d'identité, toutes langues.
//
// POURQUOI. Une liste de phrases ne tient pas : elle a déjà laissé passer
// « Which AI model are you exactly? » (anglais) puis « Tu es qui ? » (français), deux
// tournures parmi les plus ordinaires. Lumia Router sert des développeurs dans le monde
// entier, avec une infinité de formulations par langue — on ne peut pas les énumérer.
//
// On décrit donc la STRUCTURE d'une question d'identité, pas ses mots :
//
//	(1) le client s'adresse à l'assistant   « tu/toi/you/你/ты/sen/usted … »
//	    (pronom de 2e personne ou forme verbale de 2e personne : « es-tu », « eres », « sei »)
//	(2) ET parle de son identité            « modèle/IA/LLM/version/éditeur/entraînement/
//	                                          nom/claude/gpt/模型/модель … »
//	(3) OU demande « qui / quoi »           « qui/who/谁/누구/quem/wer … »
//	(4) dans une question COURTE            (≤ 160 caractères)
//
// Les conditions (1), (2)/(3) doivent tomber DANS UNE FENÊTRE DE PROXIMITÉ : c'est la
// proximité qui sépare « tu es quel modèle ? » (une sonde) de « peux-tu m'expliquer ce
// modèle de données ? » (du travail).
//
// ARBITRAGE ASSUMÉ. La règle privilégie la couverture (toutes langues, toutes tournures)
// sur la pureté : une question courte qui parle à l'assistant de son modèle part chez le
// vrai Claude même si c'était une question légitime sur un modèle produit. Le coût est
// borné — un appel, et un plafond de 20 par heure et par client — alors qu'un test
// d'authenticité non détecté laisse le client croire à un vrai Claude. Les listes de
// phrases exactes (patterns.go) restent en première ligne, sans faux positif.

const (
	// identityMaxRunes : au-delà, c'est un prompt de travail, pas une sonde.
	identityMaxRunes = 160
	// Trois paliers de proximité, parce que tous les mots d'identité ne se valent pas :
	//
	//   - « modèle » est un mot de TRAVAIL (« ce modèle de données », « quel modèle
	//     choisir ») : il ne compte que COLLÉ à la 2e personne (« tu es quel modèle »,
	//     « ¿qué modelo eres? »). Mesuré le 2026-10-03 : avec un écart de 3 mots, le
	//     détecteur partait sur 8 % du trafic et le compte CrazyToken se vidait ;
	//   - une marque ou un terme sans ambiguïté (« claude », « llm », « version »,
	//     « éditeur », « entraînement ») tolère un petit écart : on parle bien de
	//     l'assistant ;
	//   - un interrogatif nu (« qui », « who », « 谁 ») reste serré lui aussi.
	weakIdentityProximity   = 2
	strongIdentityProximity = 3
	interrogativeProximity  = 2
	// cjkProximity : écart maximal en CARACTÈRES pour les écritures sans espaces
	// (« 你是谁 », « あなたは誰 ») — élargie car les signes diacritiques du thaï et du
	// bengali consomment des caractères à eux seuls.
	cjkProximity = 8
	// cjkWeakProximity : fenêtre du mot « modèle » en écriture sans espaces : il doit
	// être collé à la 2e personne (« 你是什么模型 »), pas seulement présent.
	cjkWeakProximity = 3
)

// cjkCopulas : le verbe « être » des écritures sans espaces. Il est INDISPENSABLE au
// palier faible : « 你是什么模型 » (tu es quel modèle) est une sonde, « 你能解释这个模型吗 »
// (peux-tu expliquer ce modèle) ne l'est pas — la seule différence est ce verbe.
var cjkCopulas = []string{
	"是", "係", "が", "です", "だ", "である", "でござ", "이다", "이야", "입니다", "야",
	"เป็น", "คือ",
}

// selfRefForms : pronoms et formes verbales de 2e personne — le client parle À
// l'assistant. Les formes trop ambiguës d'une autre langue sont écartées : « je »
// (1re personne en français, 2e en néerlandais), « te » (clitique très fréquent),
// « is/are » seuls (une question sur des objets, pas sur l'assistant).
// selfPronounForms : pronoms de 2e personne — le client parle À l'assistant.
var selfPronounForms = []string{
	// --- pronoms ---
	// anglais
	"you", "yourself", "yourselves", "youre", "u",
	// français
	"tu", "toi", "vous", "votre", "vos",
	// espagnol / portugais
	"usted", "ustedes", "vosotros", "vos", "ti", "contigo", "voce", "voces", "vc", "você",
	"senhor", "senhora",
	// allemand / néerlandais
	"du", "dich", "dir", "ihr", "euch", "jij", "jou", "uw", "jullie",
	// italien / roumain
	"tu", "voi", "lei", "dumneavoastra",
	// slave
	"ty", "ciebie", "tobie", "pan", "pani", "wy", "ты", "тебя", "тебе", "тобой",
	"вы", "вас", "вам", "ти", "ви",
	// turc / hongrois / finnois
	"sen", "sana", "seni", "siz", "size", "ön", "sina", "sinä",
	// arabe / persan / hébreu
	"أنت", "انت", "أنتِ", "انتم", "أنتم", "إنت", "تو", "شما", "אתה", "את", "אתם",
	// inde / bengali
	"तुम", "आप", "तू", "तुम्हें", "آپ", "আপনি", "তুমি",
	// chinois / japonais / coréen
	"你", "您", "你们", "妳", "あなた", "君", "お前", "きみ", "あんた", "너", "당신",
	"네가", "너는",
	// vietnamien / indonésien / malais / thaï
	"bạn", "ban", "mày", "ngài", "cậu", "kamu", "anda", "kau", "kalian", "คุณ", "เธอ",
	// grec / suédois / danois / norvégien / swahili
	"εσύ", "εσείς", "dig", "wewe", "nyinyi", "wena",
}

// selfVerbForms : formes VERBALES de 2e personne (langues qui omettent le pronom).
// Elles comptent double : elles adressent la question à l'assistant ET elles portent la
// relation « être » — c'est ce qui distingue « tu es quel modèle ? » de
// « Kannst du dieses Modell benutzen ? » (peux-tu utiliser ce modèle), où « du » n'est
// qu'un pronom.
var selfVerbForms = []string{
	"eres", "estas", "estás", "es-tu", "etes", "êtes", "sei", "bist", "seid",
	"bent", "jestes", "jesteś", "misin", "musun", "ben", "εισαι", "jsi", "esti",
	// verbes de 2e personne des langues servies : « quel modèle utilises-tu ? » peut
	// arriver sans pronom séparé, surtout en espagnol, en italien et en turc.
	"hablas", "usas", "funcionas", "falas", "parli", "usi", "funzioni",
	"sprichst", "verwendest", "mówisz", "używasz", "działasz",
	"konuşuyorsun", "kullanıyorsun", "vorbesti", "folosesti", "rulezi",
	"говоришь", "используешь", "работаешь", "تستخدم", "تتكلم", "تحدث",
}

// selfRefForms : pronoms de 2e personne + formes verbales. « isSelfRef » couvre les deux.
var selfRefForms = append(append([]string(nil), selfPronounForms...), selfVerbForms...)

// isSelfVerb : la forme est un VERBE de 2e personne (pas un simple pronom). Les formes
// inversées recousues (« rede ich », « am I », « suis-je ») en contiennent toujours un.
func isSelfVerb(t string) bool {
	return containsForm(selfVerbForms, t) || containsForm(selfRefGlued, t)
}

// identityForms : mots qui parlent de l'identité, de la nature ou de l'origine du modèle.
// identityFormsWeak : le mot « modèle » dans toutes les langues. AMBIGU — c'est un mot
// de travail ordinaire — donc soumis au palier le plus serré (collé à la 2e personne).
var identityFormsWeak = []string{
	"model", "models", "modele", "modèle", "modelo", "modell", "modello", "modelu",
	"модель", "модели", "модел", "modeli", "modelis", "malli", "模型", "モデル", "모델",
	// déclinaisons slaves : « z modelem », « o modelu », « o modelach »
	"modelem", "modelowi", "modelom", "modelach",
	"نموذج", "मॉडल", "โมเดล", "μοντέλο", "mô", "hình",
}

// identityFormsStrong : tout le reste — IA, LLM, marques, version, nom, éditeur,
// entraînement. Ces mots ne décrivent PAS un objet de travail : ils visent l'assistant.
var identityFormsStrong = []string{
	// IA / LLM / assistant
	"llm", "ai", "ia", "ki", "chatbot", "assistant", "asystent", "assistent",
	"ai模型", "大模型", "语言模型", "語言模型", "人工智能", "人工知能", "인공지능",
	"inteligencia", "inteligência", "intelligenza", "künstliche", "kunstliche",
	"искусственный", "интеллект", "штучний", "ذكاء", "اصطناعي", "कृत्रिम", "বুদ্ধিমত্তা",
	"trí", "tuệ", "kecerdasan", "buatan", "ปัญญาประดิษฐ์", "τεχνητή", "νοημοσύνη",
	"artificial", "intelligence",
	// marques de modèle (un client qui teste demande « are you claude ? »)
	"claude", "gpt", "chatgpt", "gemini", "llama", "deepseek", "qwen", "grok",
	"mistral", "opus", "sonnet", "haiku",
	// version
	"version", "versión", "versao", "versão", "versione", "wersja", "версия", "версія",
	"έκδοση", "版本", "バージョン", "버전", "phiên", "bản", "إصدار", "संस्करण", "เวอร์ชัน",
	// nom / identité
	"name", "nom", "nombre", "nome", "nazwa", "imię", "имя", "ім'я", "όνομα", "名前",
	"이름", "اسم", "नाम", "نام", "tên", "ชื่อ", "nama",
	// éditeur / fabricant
	"company", "entreprise", "societe", "société", "sociedad", "empresa", "companhia",
	"unternehmen", "firma", "azienda", "bedrijf", "компания", "компанія", "شركة",
	"कंपनी", "कम्पनी", "会社", "회사", "công", "ty", "perusahaan", "บริษัท",
	"εταιρεία", "anthropic", "openai", "google",
	// entraînement (origine du modèle)
	"trained", "training", "entraine", "entraîné", "entrainement", "entraînement",
	"entrenado", "entrenamiento", "treinado", "treinamento", "trainiert", "addestrato",
	"opgeleid", "trenowany", "обучен", "обучение", "обучена", "навчан", "تدريب",
	"训练", "訓練", "学習", "학습", "huấn", "luyện", "eğitildi", "प्रशिक्षित", "প্রশিক্ষণ",
}

// identityFormHit : mot d'identité, tous paliers confondus (le palier est appliqué par
// l'appelant, qui seul connaît la distance à la 2e personne).
func identityFormHit(word string) (string, bool) {
	if _, ok := weakIdentityHit(word); ok {
		return word, true
	}
	return strongIdentityHit(word)
}

func weakIdentityHit(word string) (string, bool) {
	if containsForm(identityFormsWeak, word) {
		return word, true
	}
	return "", false
}

func strongIdentityHit(word string) (string, bool) {
	if containsForm(identityFormsStrong, word) {
		return word, true
	}
	// Écriture non latine : l'arabe et le turc attachent le possessif et les désinences
	// au mot (« نموذج » + « ك » = ton modèle), une égalité stricte les raterait.
	rw := []rune(word)
	if len(rw) == 0 || (rw[0] >= 'a' && rw[0] <= 'z') {
		return "", false
	}
	for _, f := range identityFormsStrong {
		rf := []rune(f)
		if len(rf) >= 3 && len(rw) > len(rf) && strings.HasPrefix(word, f) {
			return f, true
		}
	}
	for _, f := range identityFormsWeak {
		rf := []rune(f)
		if len(rf) >= 3 && len(rw) > len(rf) && strings.HasPrefix(word, f) {
			return f, true
		}
	}
	return "", false
}

// possessiveIdentityForms : « TON modèle / TA version / TON nom » — langues qui attachent
// le possessif au mot (arabe, turc, hébreu). Ces formes portent la 2e personne en elles :
// les traiter comme de simples mots d'identité raterait « ما هو نموذجك ؟ » (quel est ton
// modèle), où aucun pronom séparé n'existe.
var possessiveIdentityForms = []string{
	"نموذجك", "اسمك", "شركتك", "نسختك", "إصدارك", "مطورك", "منشئك",
	"modelin", "modeliniz", "adın", "adınız", "sürümün", "sürümünüz",
	"המודל שלך", "השם שלך",
}

// identityInterrogatives : mots interrogatifs d'identité. Ils suffisent avec la
// 2e personne : « tu es qui ? » ne contient aucun mot « modèle ».
var identityInterrogatives = []string{
	"who", "whom", "whose", "what", "which",
	"qui", "que", "quoi", "quel", "quelle", "quels", "quelles",
	"quien", "quién", "quienes", "qué", "cual", "cuál", "quem", "qual", "quais",
	"wer", "was", "welcher", "welche", "welches", "welchem", "welchen", "wie", "wat", "welk", "welke",
	"chi", "che", "cosa", "quale", "cine",
	"kto", "kdo", "co", "jaki", "jaka", "jakie", "który", "ktora", "która",
	"кто", "что", "какой", "какая", "какие", "хто", "що", "який", "яка",
	"kim", "kimsin", "kimsiniz", "nesin", "hangi", "milyen", "ki", "mikä", "kuka",
	"ما", "ماذا", "من", "اي", "أي", "کی", "چه", "מי", "מה",
	"कौन", "क्या", "कौनसा", "कौन-सा", "کون", "کیا", "কে", "কি",
	"谁", "誰", "什么", "什麼", "哪", "哪位", "哪個", "哪个",
	"だれ", "何", "なに", "どの", "どれ", "누구", "무엇", "뭐", "어떤", "어느",
	"ai", "gì", "gi", "nào", "nao", "siapa", "apa", "mana", "ใคร", "อะไร",
	"ποιος", "ποια", "τι", "vem", "vad", "hvem", "hvad", "hvilken", "nani",
}

// foldAccents : retire les diacritiques latins courants, pour que « modèle » rencontre
// « modele » et « quién » rencontre « quien ». Pas de dépendance externe.
var foldAccents = map[rune]rune{
	'á': 'a', 'à': 'a', 'â': 'a', 'ä': 'a', 'ã': 'a', 'å': 'a', 'ā': 'a', 'ă': 'a', 'ą': 'a',
	'é': 'e', 'è': 'e', 'ê': 'e', 'ë': 'e', 'ē': 'e', 'ĕ': 'e', 'ė': 'e', 'ę': 'e', 'ě': 'e',
	'í': 'i', 'ì': 'i', 'î': 'i', 'ï': 'i', 'ī': 'i', 'į': 'i', 'ı': 'i',
	'ó': 'o', 'ò': 'o', 'ô': 'o', 'ö': 'o', 'õ': 'o', 'ø': 'o', 'ō': 'o', 'ő': 'o',
	'ú': 'u', 'ù': 'u', 'û': 'u', 'ü': 'u', 'ū': 'u', 'ů': 'u', 'ű': 'u',
	'ç': 'c', 'ć': 'c', 'č': 'c', 'ñ': 'n', 'ń': 'n', 'š': 's', 'ś': 's', 'ş': 's',
	'ž': 'z', 'ź': 'z', 'ż': 'z', 'ý': 'y', 'ÿ': 'y', 'ğ': 'g', 'ł': 'l', 'ř': 'r', 'ť': 't',
	// roumain (comma-below), croate/serbe, albanais
	'ș': 's', 'ț': 't', 'ţ': 't', 'đ': 'd', 'ð': 'd', 'ħ': 'h', 'ġ': 'g', 'ċ': 'c',
	// grec : tonos et diérèse — « είσαι » doit rencontrer « εισαι »
	'ά': 'α', 'έ': 'ε', 'ή': 'η', 'ί': 'ι', 'ϊ': 'ι', 'ΐ': 'ι', 'ό': 'ο', 'ύ': 'υ', 'ϋ': 'υ',
	'ΰ': 'υ', 'ώ': 'ω',
}

func foldLower(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		if f, ok := foldAccents[r]; ok {
			b.WriteRune(f)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// isNoSpaceScriptRune : écritures sans séparation de mots — la proximité s'y mesure en caractères.
func isNoSpaceScriptRune(r rune) bool {
	switch {
	case r >= 0x3040 && r <= 0x30FF: // kana
		return true
	case r >= 0x3400 && r <= 0x9FFF: // idéogrammes
		return true
	case r >= 0xAC00 && r <= 0xD7AF: // hangul
		return true
	case r >= 0x0E00 && r <= 0x0E7F: // thaï
		return true
	}
	return false
}

// identityTokens découpe un texte en mots (lettres, chiffres et SIGNES DIACRITIQUES,
// toutes écritures). Les diacritiques comptent : en bengali, en thaï ou en hindi, la
// voyelle est un signe combinant — les traiter comme des séparateurs coupait les mots en
// morceaux (« আপনি » devenait « আপন » + « ক ») et aucune forme n'était reconnue.
func identityTokens(s string) []string {
	return strings.FieldsFunc(foldLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && !unicode.IsMark(r)
	})
}

// selfRefBigrams : formes interrogatives inversées en DEUX mots — « am I », « suis-je »,
// « rede ich », « est-ce que je ». Le pronom y est séparé du verbe, donc absent de la
// liste de pronoms ; on recoud ces paires avant la comparaison. Elles sont
// reconnaissables et quasi absentes d'un prompt de travail, contrairement à un « je »
// ou un « ich » isolés, volontairement écartés (faux positifs sur « quel modèle je dois
// utiliser pour ce projet ? »).
var selfRefBigrams = [][2]string{
	{"am", "i"}, {"suis", "je"}, {"rede", "ich"}, {"spreche", "ich"},
	{"parle", "je"}, {"hablo", "yo"}, {"czy", "ja"}, {"czy", "ty"},
	{"parlo", "io"}, {"falo", "eu"}, {"говори", "я"},
}

// selfRefGlued : formes de deux mots recousues (« am i » → « ami »), comparées ensuite
// comme un mot ordinaire. Rempli au chargement à partir de selfRefBigrams.
var selfRefGlued = func() []string {
	out := make([]string, 0, len(selfRefBigrams))
	for _, b := range selfRefBigrams {
		out = append(out, b[0]+b[1])
	}
	out = append(out, "estcequeje", "estcequej")
	return out
}()

// glueSelfRefs recoud les paires de selfRefBigrams dans la liste de mots.
func glueSelfRefs(tokens []string) []string {
	if len(tokens) < 2 {
		return tokens
	}
	out := make([]string, 0, len(tokens))
	for i := 0; i < len(tokens); i++ {
		if i+1 < len(tokens) {
			joined := false
			for _, b := range selfRefBigrams {
				if tokens[i] == b[0] && tokens[i+1] == b[1] {
					out = append(out, b[0]+b[1])
					i++
					joined = true
					break
				}
			}
			if joined {
				continue
			}
		}
		out = append(out, tokens[i])
	}
	return out
}

// isSelfRef : pronom, verbe de 2e personne, ou forme inversée recousue.
func isSelfRef(t string) bool {
	return containsForm(selfRefForms, t) || containsForm(selfRefGlued, t)
}

func containsForm(forms []string, word string) bool {
	for _, f := range forms {
		if f == word {
			return true
		}
	}
	return false
}

func hasSubstringForm(forms []string, text string) bool {
	for _, f := range forms {
		if f != "" && strings.Contains(text, f) {
			return true
		}
	}
	return false
}

// structuralIdentityQuestion : vrai si le message ressemble à une question d'identité.
// Retourne aussi la preuve lisible, écrite dans le journal.
func structuralIdentityQuestion(user string) (bool, string) {
	runes := []rune(user)
	if len(runes) == 0 || len(runes) > identityMaxRunes {
		return false, ""
	}
	folded := foldLower(user)

	// (1) Écritures sans espaces : proximité en CARACTÈRES, mêmes paliers.
	if hasNoSpaceScript(folded) {
		if ok, why := cjkIdentityAdjacency([]rune(folded)); ok {
			return true, why
		}
	}

	// (2) Écritures à espaces : proximité en MOTS. Condition (4) : la forme doit être
	// interrogative — un point d'interrogation ou un mot interrogatif.
	if !strings.ContainsAny(folded, "?？؟") {
		hasInterrogative := false
		for _, t := range identityTokens(user) {
			if containsForm(identityInterrogatives, t) {
				hasInterrogative = true
				break
			}
		}
		if !hasInterrogative {
			return false, ""
		}
	}

	tokens := glueSelfRefs(identityTokens(user))
	// « est-ce que je » arrive aussi en quatre mots.
	glued := strings.ReplaceAll(strings.Join(tokens, " "), "est ce que je", "estcequeje")
	glued = strings.ReplaceAll(glued, "est ce que j", "estcequej")
	tokens = strings.Fields(glued)

	// Index de chaque famille, puis décision par palier.
	var selfs, interrs, weaks, strongs []int
	for i, t := range tokens {
		switch {
		case isSelfRef(t):
			selfs = append(selfs, i)
		case containsForm(identityInterrogatives, t):
			interrs = append(interrs, i)
		default:
			// « ton modèle » porte déjà la 2e personne : le mot est des deux côtés.
			if containsForm(possessiveIdentityForms, t) {
				selfs = append(selfs, i)
				strongs = append(strongs, i)
				continue
			}
			if _, ok := weakIdentityHit(t); ok {
				weaks = append(weaks, i)
				continue
			}
			if _, ok := strongIdentityHit(t); ok {
				strongs = append(strongs, i)
			}
		}
	}
	if len(selfs) == 0 {
		return false, ""
	}
	// Palier faible : le mot « modèle » est ambigu, il ne suffit pas d'être proche de la
	// 2e personne — la question doit porter sur l'identité, c'est-à-dire contenir un mot
	// interrogatif (« quel modèle es-tu ? ») ou une 2e personne qui est un VERBE
	// (« ¿qué modelo eres? », « ce model ești? »). Sans cette condition,
	// « Kannst du dieses Modell benutzen? » (peux-tu utiliser ce modèle) partait chez le
	// vrai Claude.
	weakOK := len(interrs) > 0
	if !weakOK {
		for _, sr := range selfs {
			if isSelfVerb(tokens[sr]) {
				weakOK = true
				break
			}
		}
	}
	for _, sr := range selfs {
		if weakOK {
			for _, w := range weaks {
				if absInt(sr-w) <= weakIdentityProximity {
					return true, "2e personne + « " + tokens[w] + " » collés (écart " + strconv.Itoa(absInt(sr-w)) + ")"
				}
			}
		}
		for _, st := range strongs {
			if absInt(sr-st) <= strongIdentityProximity {
				return true, "2e personne + terme d'identité « " + tokens[st] + " » (écart " + strconv.Itoa(absInt(sr-st)) + ")"
			}
		}
		for _, q := range interrs {
			if absInt(sr-q) <= interrogativeProximity {
				return true, "2e personne + interrogatif « " + tokens[q] + " » (écart " + strconv.Itoa(absInt(sr-q)) + ")"
			}
		}
	}
	return false, ""
}

// cjkCopulaBetween : une copule se trouve-t-elle entre la 2e personne (à l'index self)
// et le mot « modèle », ou juste après lui (japonais, coréen : le verbe vient en fin) ?
func cjkCopulaBetween(runes []rune, self, selfLen int) bool {
	lo := self
	if lo < 0 {
		lo = 0
	}
	hi := self + selfLen + 2*cjkProximity
	if hi > len(runes) {
		hi = len(runes)
	}
	if lo > hi {
		return false
	}
	return hasSubstringForm(cjkCopulas, string(runes[lo:hi]))
}

func absInt(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func hasNoSpaceScript(s string) bool {
	for _, r := range s {
		if isNoSpaceScriptRune(r) {
			return true
		}
	}
	return false
}

// cjkIdentityAdjacency : dans un texte sans espaces, cherche un mot de 2e personne et un
// terme d'identité (ou interrogatif) à moins de cjkProximity caractères l'un de l'autre.
func cjkIdentityAdjacency(runes []rune) (bool, string) {
	for i := 0; i < len(runes); i++ {
		for _, ref := range selfRefForms {
			rr := []rune(ref)
			if len(rr) == 0 || !isNoSpaceScriptRune(rr[0]) || i+len(rr) > len(runes) {
				continue
			}
			if string(runes[i:i+len(rr)]) != ref {
				continue
			}
			lo := i - cjkProximity
			if lo < 0 {
				lo = 0
			}
			hi := i + len(rr) + cjkProximity
			if hi > len(runes) {
				hi = len(runes)
			}
			window := string(runes[lo:hi])
			if hasSubstringForm(identityFormsStrong, window) {
				return true, "2e personne « " + ref + " » + terme d'identité à moins de " + strconv.Itoa(cjkProximity) + " caractères"
			}
			if hasSubstringForm(identityInterrogatives, window) {
				return true, "2e personne « " + ref + " » + interrogatif à moins de " + strconv.Itoa(cjkProximity) + " caractères"
			}
			// Mot « modèle » : il doit être COLLÉ à la 2e personne, et une copule
			// (verbe « être ») doit les relier. Sans cette copule, « 你能解释这个模型吗 »
			// (peux-tu expliquer ce modèle) partirait chez le vrai Claude.
			near := i - cjkWeakProximity
			if near < 0 {
				near = 0
			}
			far := i + len(rr) + cjkProximity
			if far > len(runes) {
				far = len(runes)
			}
			nearWindow := string(runes[near:far])
			if hasSubstringForm(identityFormsWeak, nearWindow) && cjkCopulaBetween(runes, i, len(rr)) {
				return true, "2e personne « " + ref + " » + copule + mot « modèle »"
			}
		}
	}
	return false, ""
}
