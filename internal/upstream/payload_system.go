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
	// Chaque bloc systeme rejoint le PROCHAIN message utilisateur, a sa place.
	//
	// Pourquoi pas tout en tete : Claude Code insere des blocs systeme DYNAMIQUES au fil
	// de la conversation (avis de fichier modifie, rappels horodates). Les remonter dans le
	// premier message changeait les tout premiers jetons a chaque tour, ce qui invalidait
	// la TOTALITE du cache de prefixe : le tour 2 relisait 640 jetons et refacturait 87 000
	// (mesure du 2026-10-26). En les laissant a leur position, le prefixe des tours
	// precedents reste identique et le cache est relu.
	enAttente := make([]string, 0, 2)
	sortie := make([]any, 0, len(msgs))
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			sortie = append(sortie, m)
			continue
		}
		role, _ := msg["role"].(string)
		role = strings.TrimSpace(role)
		if strings.EqualFold(role, "system") {
			if txt := sansEnteteAttribution(texteDuContenu(msg["content"])); txt != "" {
				enAttente = append(enAttente, txt)
			}
			continue
		}
		if strings.EqualFold(role, "user") && len(enAttente) > 0 {
			msg["content"] = strings.Join(enAttente, "\n\n") + "\n\n" + texteDuContenu(msg["content"])
			enAttente = enAttente[:0]
		}
		sortie = append(sortie, msg)
	}
	// Systeme restant apres le dernier message utilisateur : un message utilisateur final,
	// comme avant (aucun contenu perdu).
	if len(enAttente) > 0 {
		sortie = append(sortie, map[string]any{"role": "user", "content": strings.Join(enAttente, "\n\n")})
	}
	obj["messages"] = sortie
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

// sansEnteteAttribution retire la ligne d'attribution que Claude Code met en tete de son
// prompt systeme : « x-anthropic-billing-header: cc_version=…; cc_entrypoint=cli;
// cc_prompt_index=…; cc_turn_index=…; … ». Ses valeurs changent a CHAQUE requete.
//
// POURQUOI : nos amonts font du cache de prefixe implicite (seuls les premiers jetons
// strictement identiques sont reutilises). Cette ligne, placee en tete du prompt, invalidait
// donc tout le cache a chaque tour. Mesure du 2026-10-28 : deux requetes identiques a cette
// ligne pres -> 0 jeton relu ; sans la ligne, le second tour relit 99,8 % du contexte.
// Anthropic ne s'en sert pas pour l'inference (attribution de facturation uniquement) et
// propose meme une option pour la desactiver : la retirer ne change rien pour le modele.
func sansEnteteAttribution(texte string) string {
	if !strings.Contains(texte, "x-anthropic-billing-header") {
		return texte
	}
	lignes := strings.Split(texte, "\n")
	filtrees := make([]string, 0, len(lignes))
	for _, ligne := range lignes {
		if strings.HasPrefix(strings.TrimSpace(ligne), "x-anthropic-billing-header") {
			continue
		}
		filtrees = append(filtrees, ligne)
	}
	return strings.TrimLeft(strings.Join(filtrees, "\n"), "\n")
}
