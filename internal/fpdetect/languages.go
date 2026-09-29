package fpdetect

import "sort"

// Language est une langue déclarée comme couverte par les tables de motifs.
//
// Le champ Sample est un fragment distinctif qui DOIT se retrouver dans
// modelIdentityPhrases. Il sert de preuve de couverture : sans lui, une table
// « 17 langues » pourrait n'être que de l'anglais dupliqué, ou une langue déclarée
// dans la configuration pourrait n'avoir aucun motif. TestCouvertureDesLanguesDeclarees
// vérifie l'appartenance des 17 fragments, et la configuration refuse un code inconnu.
type Language struct {
	Code   string // étiquette BCP-47 simplifiée : en, fr, zh-Hans, zh-Hant…
	Label  string // nom lisible, pour les journaux
	Sample string // fragment présent dans modelIdentityPhrases
}

// languages est la liste des 17 langues couvertes. L'ordre est celui du produit, mais
// toute sortie publique passe par SupportedLanguageCodes (trié) pour rester stable.
var languages = []Language{
	{Code: "en", Label: "anglais", Sample: "what model"},
	{Code: "fr", Label: "français", Sample: "quel modèle"},
	{Code: "zh-Hans", Label: "chinois simplifié", Sample: "什么模型"},
	{Code: "zh-Hant", Label: "chinois traditionnel", Sample: "什麼模型"},
	{Code: "es", Label: "espagnol", Sample: "qué modelo"},
	{Code: "pt", Label: "portugais", Sample: "seu modelo"},
	{Code: "de", Label: "allemand", Sample: "welches modell"},
	{Code: "it", Label: "italien", Sample: "quale modello"},
	{Code: "ru", Label: "russe", Sample: "какая модель"},
	{Code: "ar", Label: "arabe", Sample: "نموذج"},
	{Code: "hi", Label: "hindi", Sample: "मॉडल"},
	{Code: "ja", Label: "japonais", Sample: "モデル"},
	{Code: "ko", Label: "coréen", Sample: "모델"},
	{Code: "tr", Label: "turc", Sample: "hangi model"},
	{Code: "nl", Label: "néerlandais", Sample: "welk model"},
	{Code: "pl", Label: "polonais", Sample: "który model"},
	{Code: "vi", Label: "vietnamien", Sample: "mô hình"},
}

// Languages retourne la liste déclarée (copie).
func Languages() []Language {
	out := make([]Language, len(languages))
	copy(out, languages)
	return out
}

// SupportedLanguageCodes retourne les codes triés : c'est la valeur par défaut de la
// clé de configuration `languages` et la référence de validation.
func SupportedLanguageCodes() []string {
	out := make([]string, 0, len(languages))
	for _, l := range languages {
		out = append(out, l.Code)
	}
	sort.Strings(out)
	return out
}

// IsSupportedLanguage : vrai si le code est déclaré.
func IsSupportedLanguage(code string) bool {
	for _, l := range languages {
		if l.Code == code {
			return true
		}
	}
	return false
}

// LanguageLabel retourne le nom lisible d'un code (« » si inconnu).
func LanguageLabel(code string) string {
	for _, l := range languages {
		if l.Code == code {
			return l.Label
		}
	}
	return ""
}
