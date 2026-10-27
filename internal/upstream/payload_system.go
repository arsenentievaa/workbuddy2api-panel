package upstream

import "strings"

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
	// Tout le systeme part dans le DERNIER message utilisateur.
	//
	// Historique : pour que l'amont en tienne compte, on le transportait dans le PREMIER
	// message utilisateur. Or Claude Code fait varier une partie de son prompt systeme a
	// chaque tour (heure, budget de jetons, etat du depot). Place en tete, ce contenu
	// volatil invalide la totalite du cache de prefixe : constat du 2026-10-27 chez deux
	// clients, seulement ~2 200 jetons relus (les definitions d'outils) et ~52 000
	// refactures a chaque tour.
	//
	// En le portant par le dernier message, l'historique des tours precedents reste
	// identique et le cache est relu ; les consignes arrivent juste avant la demande.
	var systemes []string
	dernierUtilisateur := -1
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		switch strings.ToLower(strings.TrimSpace(role)) {
		case "system", "developer":
			if txt := texteDuContenu(msg["content"]); txt != "" {
				systemes = append(systemes, txt)
			}
		case "user":
			dernierUtilisateur = len(msgs) - 1 // recalcule plus bas
		}
	}
	sansSysteme := make([]any, 0, len(msgs))
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			sansSysteme = append(sansSysteme, m)
			continue
		}
		role, _ := msg["role"].(string)
		if strings.EqualFold(strings.TrimSpace(role), "system") || strings.EqualFold(strings.TrimSpace(role), "developer") {
			continue
		}
		sansSysteme = append(sansSysteme, msg)
	}
	if len(systemes) == 0 {
		return
	}
	prefixe := strings.Join(systemes, "\n\n")
	// dernier message utilisateur de la liste nettoyee
	for i := len(sansSysteme) - 1; i >= 0; i-- {
		msg, ok := sansSysteme[i].(map[string]any)
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); strings.EqualFold(strings.TrimSpace(role), "user") {
			msg["content"] = prefixe + "\n\n" + texteDuContenu(msg["content"])
			sansSysteme[i] = msg
			obj["messages"] = sansSysteme
			return
		}
	}
	obj["messages"] = append([]any{map[string]any{"role": "user", "content": prefixe}}, sansSysteme...)
	_ = dernierUtilisateur
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
