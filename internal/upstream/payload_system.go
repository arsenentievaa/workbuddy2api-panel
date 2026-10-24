package upstream

import (
	"log"
	"strings"
)

// L'amont ne tient aucun compte des messages de role « system » : verifie le
// 2026-10-24, une consigne systeme (« reponds uniquement par PINEAPPLE ») n'est ni
// suivie ni comptee dans prompt_tokens (574 jetons avec ou sans systeme, alors que le
// meme texte en message utilisateur fait 17 380). Consequence : tout le prompt systeme
// des clients (regles de Claude Code, CLAUDE.md, consignes d'agent) partait a la
// poubelle, et ne pouvait pas non plus etre mis en cache.
//
// On transporte donc le systeme en tete du premier message utilisateur. L'instruction
// redevient effective, elle est comptee, et elle devient cacheable.
func mergeSystemIntoUser(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return
	}
	var systemes []string
	restants := make([]any, 0, len(msgs))
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			restants = append(restants, m)
			continue
		}
		if role, _ := msg["role"].(string); strings.EqualFold(strings.TrimSpace(role), "system") {
			if txt := texteDuContenu(msg["content"]); txt != "" {
				systemes = append(systemes, txt)
			}
			continue
		}
		restants = append(restants, m)
	}
	if len(systemes) == 0 {
		return
	}
	prefixe := strings.Join(systemes, "\n\n")
	log.Printf("[payload] systeme fusionne dans le premier message utilisateur (%d caracteres)", len(prefixe))
	for i, m := range restants {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); strings.EqualFold(strings.TrimSpace(role), "user") {
			msg["content"] = prefixe + "\n\n" + texteDuContenu(msg["content"])
			restants[i] = msg
			obj["messages"] = restants
			return
		}
	}
	obj["messages"] = append([]any{map[string]any{"role": "user", "content": prefixe}}, restants...)
}

// texteDuContenu aplatit un contenu de message (chaine, ou blocs type text).
func texteDuContenu(contenu any) string {
	switch v := contenu.(type) {
	case string:
		return v
	case []any:
		var parties []string
		for _, bloc := range v {
			m, ok := bloc.(map[string]any)
			if !ok {
				continue
			}
			if typ, _ := m["type"].(string); typ != "" && typ != "text" {
				continue
			}
			if txt, ok := m["text"].(string); ok && txt != "" {
				parties = append(parties, txt)
			}
		}
		return strings.Join(parties, "\n\n")
	}
	return ""
}
