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
	// identityProximity : écart maximal, en mots, entre la 2e personne et le terme
	// d'identité ou l'interrogatif.
	identityProximity = 3
	// cjkProximity : écart maximal en CARACTÈRES pour les écritures sans espaces
	// (« 你是谁 », « あなたは誰 ») — élargie car les signes diacritiques du thaï et du
	// bengali consomment des caractères à eux seuls.
	cjkProximity = 8
)

// selfRefForms : pronoms et formes verbales de 2e personne — le client parle À
// l'assistant. Les formes trop ambiguës d'une autre langue sont écartées : « je »
// (1re personne en français, 2e en néerlandais), « te » (clitique très fréquent),
// « is/are » seuls (une question sur des objets, pas sur l'assistant).
var selfRefForms = []string{
	// --- pronoms ---
	// anglais
	"you", "yourself", "yourselves", "youre", "u",
	// français
	"tu", "toi", "vous", "votre", "vos",
	// espagnol / portugais
	"usted", "ustedes", "vosotros", "vos", "ti", "contigo", "voce", "voces", "vc",
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

	// --- formes verbales de 2e personne (langues qui omettent le pronom) ---
	"eres", "estas", "estás", "es-tu", "etes", "êtes", "sei", "bist", "seid",
	"bent", "jestes", "jesteś", "misin", "musun", "ben", "εισαι", "jsi", "esti",
	"você",
	// verbes de 2e personne des langues servies : « quel modèle utilises-tu ? » peut
	// arriver sans pronom séparé, surtout en espagnol, en italien et en turc.
	"hablas", "usas", "funcionas", "falas", "parli", "usi", "funzioni",
	"sprichst", "verwendest", "mówisz", "używasz", "działasz",
	"konuşuyorsun", "kullanıyorsun", "vorbesti", "folosesti", "rulezi",
	"говоришь", "используешь", "работаешь", "تستخدم", "تتكلم", "تحدث",
}

// identityForms : mots qui parlent de l'identité, de la nature ou de l'origine du modèle.
var identityForms = []string{
	// modèle
	"model", "models", "modele", "modèle", "modelo", "modell", "modello", "modelu",
	"модель", "модели", "модел", "modeli", "modelis", "malli", "模型", "モデル", "모델",
	"نموذج", "मॉडल", "โมเดล", "μοντέλο", "mô", "hình",
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
	"wer", "was", "welcher", "welche", "welches", "wie", "wat", "welk", "welke",
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

// identityFormHit : correspondance d'un mot d'identité. Égalité exacte, ou — pour une
// écriture non latine — préfixe : l'arabe et le turc attachent le possessif et les
// désinences au mot (« نموذج » + « ك » = ton modèle), une égalité stricte les raterait.
func identityFormHit(word string) (string, bool) {
	if containsForm(identityForms, word) {
		return word, true
	}
	if containsForm(possessiveIdentityForms, word) {
		return word, true
	}
	rw := []rune(word)
	if len(rw) == 0 || (rw[0] >= 'a' && rw[0] <= 'z') {
		return "", false // écriture latine : pas de préfixe (trop de collisions)
	}
	for _, f := range identityForms {
		rf := []rune(f)
		if len(rf) >= 3 && len(rw) > len(rf) && strings.HasPrefix(word, f) {
			return f, true
		}
	}
	return "", false
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

	// (1) Écritures sans espaces : proximité en CARACTÈRES.
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

	tokens := identityTokens(user)
	lastSelf, lastIdent, lastKind := -1, -1, ""
	for i, t := range tokens {
		if _, ok := identityFormHit(t); ok {
			lastIdent, lastKind = i, "mot d'identité « "+t+" »"
			// « ton modèle » porte déjà la 2e personne : c'est aussi une adresse à
			// l'assistant.
			if containsForm(possessiveIdentityForms, t) {
				lastSelf = i
			}
		} else if containsForm(selfRefForms, t) {
			lastSelf = i
		} else if containsForm(identityInterrogatives, t) {
			lastIdent, lastKind = i, "interrogatif « "+t+" »"
		} else {
			continue
		}
		if lastSelf < 0 || lastIdent < 0 {
			continue
		}
		d := lastSelf - lastIdent
		if d < 0 {
			d = -d
		}
		if d <= identityProximity {
			return true, "2e personne + " + lastKind + " (écart " + strconv.Itoa(d) + " mot(s))"
		}
	}
	return false, ""
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
			if hasSubstringForm(identityForms, window) {
				return true, "2e personne « " + ref + " » + mot d'identité à moins de " + strconv.Itoa(cjkProximity) + " caractères"
			}
			if hasSubstringForm(identityInterrogatives, window) {
				return true, "2e personne « " + ref + " » + interrogatif à moins de " + strconv.Itoa(cjkProximity) + " caractères"
			}
		}
	}
	return false, ""
}
