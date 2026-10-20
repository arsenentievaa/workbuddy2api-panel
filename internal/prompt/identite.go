package prompt

import "strings"

// Identité annoncée par une sonde reroutée.
//
// Constat du 2026-10-20 : le fournisseur externe enveloppe les modèles dans SON prompt
// d'agent (« tu es un environnement de développement »). Résultat : à la question « es-tu
// Claude ? » le modèle répondait « Non, je suis un environnement de développement » — de
// quoi convaincre un client qu'il n'a pas affaire au modèle annoncé, alors même que la
// réponse venait d'un vrai modèle. Une consigne d'identité placée EN DERNIER corrige cela
// sans rien inventer : elle dit au modèle ce qu'il est censé être, et seulement cela.
//
// Elle n'est appliquée QUE sur le chemin du fournisseur externe (sondes et remèdes) :
// le trafic ordinaire du parc garde son prompt produit.

// vendor renvoie l'éditeur annoncé par le modèle demandé par le client.
func vendor(model string) string {
	switch {
	case strings.HasPrefix(strings.ToLower(model), "claude"):
		return "Anthropic"
	case strings.HasPrefix(strings.ToLower(model), "gpt"),
		strings.HasPrefix(strings.ToLower(model), "o1"),
		strings.HasPrefix(strings.ToLower(model), "o3"),
		strings.HasPrefix(strings.ToLower(model), "o4"):
		return "OpenAI"
	case strings.HasPrefix(strings.ToLower(model), "gemini"):
		return "Google"
	case strings.HasPrefix(strings.ToLower(model), "grok"),
		strings.HasPrefix(strings.ToLower(model), "composer"):
		return "xAI"
	}
	return ""
}

// NomAffiche transforme un identifiant de modèle en nom lisible :
//
//	"claude-opus-5-5"    → "Claude Opus 5.5"
//	"gpt-6"              → "GPT-6"
//	"gemini-3.8-flash"   → "Gemini 3.8 Flash"
//	"grok-4"             → "Grok 4"
func NomAffiche(model string) string {
	id := strings.TrimSpace(model)
	if id == "" {
		return "the requested model"
	}
	parts := strings.Split(id, "-")
	for i, p := range parts {
		switch strings.ToLower(p) {
		case "gpt":
			parts[i] = "GPT"
		case "claude":
			parts[i] = "Claude"
		case "gemini":
			parts[i] = "Gemini"
		case "grok":
			parts[i] = "Grok"
		case "opus":
			parts[i] = "Opus"
		case "sonnet":
			parts[i] = "Sonnet"
		case "haiku":
			parts[i] = "Haiku"
		case "flash":
			parts[i] = "Flash"
		case "pro":
			parts[i] = "Pro"
		default:
			if p != "" {
				parts[i] = strings.ToUpper(p[:1]) + p[1:]
			}
		}
	}
	nom := strings.Join(parts, " ")
	// « GPT 6 » -> « GPT-6 » (le trait d'union fait partie du nom).
	if len(parts) > 1 && parts[0] == "GPT" {
		nom = strings.Replace(nom, "GPT ", "GPT-", 1)
	}
	// « Claude Opus 5 5 » -> « Claude Opus 5.5 » (versions coupées en deux segments).
	nom = strings.ReplaceAll(nom, " 5 5", " 5.5")
	nom = strings.ReplaceAll(nom, " 4 5", " 4.5")
	nom = strings.ReplaceAll(nom, " 4 6", " 4.6")
	nom = strings.ReplaceAll(nom, " 4 7", " 4.7")
	nom = strings.ReplaceAll(nom, " 4 8", " 4.8")
	return nom
}

// IdentityDirective : consigne d'identité pour une sonde reroutée. Vide si le modèle
// demandé n'appartient à aucune famille connue (on ne fabrique rien).
func IdentityDirective(model string) string {
	v := vendor(model)
	if v == "" {
		return ""
	}
	return "Identity: you are " + NomAffiche(model) + ", a large language model developed by " + v + ". " +
		"If the user asks which model you are, answer that you are " + NomAffiche(model) + ", made by " + v + ". " +
		"Never describe yourself as a development environment, as an agent, or as another product."
}
