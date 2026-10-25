package upstream

import (
	"encoding/json"
)

// TrimForProbe reduit un corps de requete a ce qui suffit pour une SONDE
// d'authentification : le dernier message du client, un prompt systeme court, et les
// reglages de generation. Les outils et tout l'historique sont retires.
//
// POURQUOI. Une sonde (« quel modele es-tu ? ») etait envoyee au fournisseur externe avec
// TOUTE la conversation : 346 Ko en moyenne, parfois plusieurs centaines de milliers de
// jetons, factures au prix fort. Or la reponse ne depend que de la question posee. Le
// 2026-10-25, l'exploitant a constate une sur-sollicitation du fournisseur : le volume
// venait de la taille des corps, pas du nombre de sondes.
//
// Le prompt systeme est conserve s'il reste court (il porte souvent l'identite demandee),
// supprime au-dela de systemMax octets pour ne pas recreer le probleme.
func TrimForProbe(body []byte, systemMax int) []byte {
	if len(body) == 0 {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return body
	}
	var dernier any
	for i := len(msgs) - 1; i >= 0; i-- {
		m, ok := msgs[i].(map[string]any)
		if !ok {
			continue
		}
		if role, _ := m["role"].(string); role == "user" {
			dernier = msgs[i]
			break
		}
	}
	if dernier == nil {
		return body
	}
	obj["messages"] = []any{dernier}
	delete(obj, "tools")
	delete(obj, "tool_choice")
	delete(obj, "functions")
	delete(obj, "parallel_tool_calls")
	if s, present := obj["system"]; present {
		texte, estChaine := s.(string)
		if !estChaine || systemMax <= 0 || len(texte) > systemMax {
			delete(obj, "system")
		}
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}
