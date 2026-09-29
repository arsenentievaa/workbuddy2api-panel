// Package fpdetect détecte les requêtes qui ressemblent à un test de fingerprinting
// de modèle : un client qui cherche à savoir quel modèle répond réellement derrière
// la passerelle, par la forme de sa requête (jetons pièges, répétitions, questions
// d'identité, PDF, outils suspects…).
//
// Ce que le paquet ne fait pas : masquer l'empreinte du tokenizer. Router une sonde
// détectée vers le vrai modèle supprime la détection *sur cette sonde* ; une sonde
// non détectée reçoit toujours la réponse du modèle de substitution.
//
// Conception : chaque méthode produit un Signal pondéré. Le routage se déclenche si
// un signal dit « fort » est présent, ou si le score cumulé atteint le seuil. Les
// signaux faibles existent parce que plusieurs critères demandés (nombre d'outils,
// thinking activé, question sur une date récente) sont le pain quotidien d'un agent
// de codage légitime : les rendre suffisants à eux seuls enverrait la majorité du
// trafic réel — 65 % des requêtes de production font plus de 50 000 tokens, profil
// agent — vers le modèle payant.
package fpdetect

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"strings"
)

// GlitchToken est un jeton piège avec sa provenance. La cible (ModelSeries) indique
// quelle famille de modèles il fait dérailler : les jetons visant DeepSeek/GLM/Kimi/
// Qwen sont particulièrement pertinents ici, puisque ce sont les modèles de
// substitution de WorkBuddy.
type GlitchToken struct {
	Token       string
	ModelSeries string
	ErrorRate   string
	Source      string
}

// GlitchTokensFromPackage liste les jetons du fichier glitch_tokens.csv du paquet npm
// `glitch-lens` (version 0.4.0), récupéré et lu le 2026-09-29. Colonnes d'origine :
// `glitch token, model series, error rate@5, isSpecific`.
//
// 8 des 16 jetons visent une famille servie par WorkBuddy (DeepSeek, GLM, Kimi,
// Qwen) : ce sont les sondes les plus probantes contre cette infrastructure.
var GlitchTokensFromPackage = []GlitchToken{
	{"日以上更新していないブログに表示しています", "Minimax", "100", "glitch-lens"},
	{"锅内倒入植物油烧热", "GLM", "100", "glitch-lens"},
	{"开通天眼生意通银牌及以上会员", "Qwen", "100", "glitch-lens"},
	{"百度百科内容由网友共同编辑", "kimi", "100", "glitch-lens"},
	{"EDMFunc", "Deepseek", "100", "glitch-lens"},
	{"StarSrvGroupBody", "Gemini", "100", "glitch-lens"},
	{"给主人留下些什么吧", "GPT", "100", "glitch-lens"},
	{"不代表新浪看点观点或立场", "kimi", "100", "glitch-lens"},
	{"豫冠薰衣草疤痕精华素", "kimi", "100", "glitch-lens"},
	{"请问 everydaycalculation是什么意思", "Deepseek", "100", "glitch-lens"},
	{"百度百科企业词条极速创建通道", "GLM", "100", "glitch-lens"},
	{"intFragmentation", "Gemini", "100", "glitch-lens"},
	{"SRPInterfaceItem", "Gemini", "100", "glitch-lens"},
	{"opencamerastudio", "Gemini", "100", "glitch-lens"},
	{"widgetToFullName", "Gemini", "100", "glitch-lens"},
	{"<think_never_used_51bce0c785ca2f68081bfa7d91973934>", "Seed/豆包", "100", "glitch-lens"},
}

// GlitchTokensFromLiterature liste les jetons pièges publiés (recherches sur le
// tokenizer GPT-2/GPT-3 « SolidGoldMagikarp », et listes reprises par les scanners
// de vulnérabilités type garak / agent-threat-rules).
//
// Attention : cette liste n'inclut PAS les « 7 ghost tokens kimi-k3-ghost-special-
// tokens » que tu as mentionnés — ils ne figurent pas dans le CSV du paquet et je
// n'ai trouvé aucune source publique les nommant. Les inventer produirait un
// détecteur qui ne matche rien tout en donnant l'illusion de couvrir le cas. Charge
// un CSV (voir LoadGlitchCSV) dès que tu as la liste réelle.
var GlitchTokensFromLiterature = []GlitchToken{
	{"davidjl", "GPT", "", "littérature"},
	{"SolidGoldMagikarp", "GPT", "", "littérature"},
	{"ForgeModLoader", "GPT", "", "littérature"},
	{"PsyNetMessage", "GPT", "", "littérature"},
	{"wcsstore", "GPT", "", "littérature"},
	{"guiActive", "GPT", "", "littérature"},
	{"guiActiveUn", "GPT", "", "littérature"},
	{"Dragonbound", "GPT", "", "littérature"},
	{"unfocusedRange", "GPT", "", "littérature"},
	{"裏覚醒", "GPT", "", "littérature"},
	{"petertodd", "GPT", "", "littérature"},
	{"ScaledRich", "GPT", "", "littérature"},
	{"externalToEVA", "GPT", "", "littérature"},
	{"TheNitromeFan", "GPT", "", "littérature"},
	{"StreamerBot", "GPT", "", "littérature"},
	{"cloneembedreportprint", "GPT", "", "littérature"},
	{"theoretisch", "GPT", "", "littérature"},
	{"植物百科通", "CJK", "", "littérature (id 137606)"},
	{"jsii", "Qwen", "", "littérature"},
	{"jiaqi", "Qwen", "", "littérature"},
	{"chuyu-gengduo-xinxi", "Qwen", "", "littérature"},
}

// DefaultGlitchTokens concatène les deux sources ci-dessus.
func DefaultGlitchTokens() []GlitchToken {
	out := make([]GlitchToken, 0, len(GlitchTokensFromPackage)+len(GlitchTokensFromLiterature))
	out = append(out, GlitchTokensFromPackage...)
	out = append(out, GlitchTokensFromLiterature...)
	return out
}

// LoadGlitchCSV lit un fichier au format du paquet glitch-lens : la première ligne
// est un en-tête, la colonne 0 contient le jeton, la colonne 1 la famille de modèles.
//
// Le format est toléré : les colonnes 2 et 3 sont facultatives, et un en-tête absent
// est accepté. L'intérêt est de pouvoir brancher une liste plus complète (ou mise à
// jour) sans recompiler.
func LoadGlitchCSV(path string) ([]GlitchToken, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("ouvrir %s: %w", path, err)
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1 // tolère les colonnes manquantes
	var out []GlitchToken
	first := true
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("lire %s: %w", path, err)
		}
		if len(rec) == 0 {
			continue
		}
		tok := strings.TrimSpace(rec[0])
		if tok == "" {
			continue
		}
		if first {
			first = false
			// En-tête reconnu : on l'ignore (comparaison insensible à la casse).
			if strings.Contains(strings.ToLower(tok), "glitch") {
				continue
			}
		}
		g := GlitchToken{Token: tok, Source: path}
		if len(rec) > 1 {
			g.ModelSeries = strings.TrimSpace(rec[1])
		}
		if len(rec) > 2 {
			g.ErrorRate = strings.TrimSpace(rec[2])
		}
		out = append(out, g)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: aucun jeton lisible", path)
	}
	return out, nil
}
