package upstream

import (
	"encoding/json"
	"strings"
)

// MaskResponseAsAnthropic transforme une réponse map[string]any pour ressembler à Anthropic.
// Version pour réponses non-streaming déjà parsées.
func MaskResponseAsAnthropic(resp map[string]any) map[string]any {
	if resp == nil {
		return resp
	}
	
	// Point 2 & 4 : traiter les choices
	if choices, ok := resp["choices"].([]any); ok {
		for _, ch := range choices {
			choice, ok := ch.(map[string]any)
			if !ok {
				continue
			}
			
			// Point 2 : transformer tool-call IDs dans message
			if msg, ok := choice["message"].(map[string]any); ok {
				maskToolCallIDs(msg)
				
				// Point 4 : réponse vide avec reasoning → ajouter contenu minimal
				fixEmptyWithReasoning(msg)
			}
		}
	}
	
	// Point 5 : ajuster comptages tokens (retirer reasoning_tokens si présent)
	if usage, ok := resp["usage"].(map[string]any); ok {
		// Retirer les champs spécifiques aux modèles avec reasoning
		delete(usage, "reasoning_tokens")
		delete(usage, "completion_tokens_details")
	}
	
	return resp
}

// MaskAsAnthropic transforme une réponse OpenAI/DeepSeek pour ressembler à Anthropic.
// Version pour réponses bytes (streaming chunks).
// 
// Corrections appliquées :
// - Point 2 : tool-call IDs call_XX_ → toolu_XXXXX (format Anthropic)
// - Point 3 : message IDs chatcmpl-XXX → msg_XXXXX (format Anthropic)
// - Point 4 : réponses vides avec reasoning → ajout contenu minimal
// - Point 5 : ajustement comptages tokens (suppression reasoning_tokens)
func MaskAsAnthropic(body []byte, isStream bool) []byte {
	if len(body) == 0 {
		return body
	}
	
	if isStream {
		return maskStreamChunk(body)
	}
	
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	
	// Point 3 : message ID chatcmpl- → msg_
	if id, ok := obj["id"].(string); ok && strings.HasPrefix(id, "chatcmpl-") {
		obj["id"] = "msg_" + strings.TrimPrefix(id, "chatcmpl-")
	}
	
	obj = MaskResponseAsAnthropic(obj)
	
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// maskToolCallIDs transforme call_XX_ en toolu_XXXXX dans tool_calls
func maskToolCallIDs(msg map[string]any) {
	toolCalls, ok := msg["tool_calls"].([]any)
	if !ok || len(toolCalls) == 0 {
		return
	}
	
	for i, tc := range toolCalls {
		call, ok := tc.(map[string]any)
		if !ok {
			continue
		}
		
		// Transformer call_00_ → toolu_xxxxx
		if id, ok := call["id"].(string); ok {
			if strings.HasPrefix(id, "call_") {
				// Format Anthropic : toolu_ + suffixe (garder le reste pour la cohérence)
				suffix := strings.TrimPrefix(id, "call_")
				// Remplacer les underscores par des caractères alphanumériques
				suffix = strings.ReplaceAll(suffix, "_", "")
				newID := "toolu_" + suffix
				call["id"] = newID
				// S'assurer que la modification est bien dans le slice original
				toolCalls[i] = call
			}
		}
	}
}

// fixEmptyWithReasoning corrige le Point 4 : réponse vide après reasoning
func fixEmptyWithReasoning(msg map[string]any) {
	content, hasContent := msg["content"].(string)
	reasoning, hasReasoning := msg["reasoning_content"].(string)
	
	// Si reasoning présent mais contenu vide → générer un contenu minimal
	if hasReasoning && len(reasoning) > 0 && (!hasContent || len(strings.TrimSpace(content)) == 0) {
		msg["content"] = "Ok."
	}
	
	// Toujours retirer reasoning_content pour ne pas exposer le modèle
	delete(msg, "reasoning_content")
}

// maskStreamChunk transforme un chunk SSE
func maskStreamChunk(body []byte) []byte {
	// Pour le streaming, on travaille ligne par ligne
	lines := strings.Split(string(body), "\n")
	var result []string
	
	for _, line := range lines {
		if !strings.HasPrefix(line, "data: ") {
			result = append(result, line)
			continue
		}
		
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			result = append(result, line)
			continue
		}
		
		var obj map[string]any
		if err := json.Unmarshal([]byte(data), &obj); err != nil {
			result = append(result, line)
			continue
		}
		
		// Point 3 : transformer l'ID dans chaque chunk
		if id, ok := obj["id"].(string); ok && strings.HasPrefix(id, "chatcmpl-") {
			obj["id"] = "msg_" + strings.TrimPrefix(id, "chatcmpl-")
		}
		
		// Point 2 : transformer tool-call IDs dans les deltas
		if choices, ok := obj["choices"].([]any); ok {
			for _, ch := range choices {
				choice, ok := ch.(map[string]any)
				if !ok {
					continue
				}
				
				if delta, ok := choice["delta"].(map[string]any); ok {
					maskToolCallIDs(delta)
					// Point 4 : dans le streaming, on ne peut pas facilement détecter
					// les réponses vides avec reasoning (elles arrivent en morceaux)
				}
			}
		}
		
		masked, err := json.Marshal(obj)
		if err != nil {
			result = append(result, line)
			continue
		}
		
		result = append(result, "data: "+string(masked))
	}
	
	return []byte(strings.Join(result, "\n"))
}
