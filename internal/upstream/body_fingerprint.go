package upstream

import (
	"encoding/json"
	"fmt"
	"strings"
)

// bodyFingerprint decrit la FORME d'un corps de requete, sans jamais en recopier le
// contenu : nombre de messages, suite des roles, types de blocs, taille, outils, images.
// Sert au diagnostic des 400 « parametres invalides » remontes par l'amont.
func bodyFingerprint(body string) string {
	var obj map[string]any
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		return fmt.Sprintf("corps non-JSON (%d octets)", len(body))
	}
	var roles []string
	var types []string
	images, toolUses, toolResults := 0, 0, 0
	msgs, _ := obj["messages"].([]any)
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		roles = append(roles, role)
		switch c := msg["content"].(type) {
		case string:
			types = append(types, "str")
		case []any:
			var sous []string
			for _, b := range c {
				bloc, ok := b.(map[string]any)
				if !ok {
					continue
				}
				t, _ := bloc["type"].(string)
				sous = append(sous, t)
				switch t {
				case "image_url", "image":
					images++
				case "tool_use":
					toolUses++
				case "tool_result":
					toolResults++
				}
			}
			if len(sous) == 0 {
				sous = append(sous, "vide")
			}
			types = append(types, strings.Join(sous, "+"))
		default:
			types = append(types, "absent")
		}
	}
	outils, _ := obj["tools"].([]any)
	model, _ := obj["model"].(string)
	return fmt.Sprintf("model=%s msgs=%d roles=[%s] blocs=[%s] tools=%d images=%d tool_use=%d tool_result=%d octets=%d",
		model, len(msgs), strings.Join(roles, ","), strings.Join(types, ","), len(outils), images, toolUses, toolResults, len(body))
}
