package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// Points 2 et 3 : la réponse présentée au client doit porter les marqueurs d'Anthropic,
// jamais ceux du backend (call_... côté DeepSeek, chatcmpl- côté OpenAI).
func TestMaskResponseAsAnthropic_IDsAndToolCalls(t *testing.T) {
	resp := map[string]any{
		"id":    "chatcmpl-abc123",
		"model": "claude-sonnet-5-5",
		"choices": []any{
			map[string]any{
				"index": float64(0),
				"message": map[string]any{
					"role":    "assistant",
					"content": "hello",
					"tool_calls": []any{
						map[string]any{"id": "call_00_ET_YD8OFdpJHMgBFPAr9Q8Z8959", "type": "function"},
					},
				},
			},
		},
		"usage": map[string]any{
			"prompt_tokens":             float64(10),
			"completion_tokens":         float64(2),
			"reasoning_tokens":          float64(7),
			"completion_tokens_details": map[string]any{"reasoning_tokens": float64(7)},
		},
	}

	out := MaskResponseAsAnthropic(resp)

	// Point 3 : l'id de la réponse ne doit plus être au format OpenAI.
	if err := json.NewEncoder(&strings.Builder{}).Encode(out); err != nil {
		t.Fatalf("non sérialisable : %v", err)
	}
	choices, _ := out["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices attendu 1, obtenu %d", len(choices))
	}
	msg, _ := choices[0].(map[string]any)["message"].(map[string]any)
	tcs, _ := msg["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("tool_calls attendu 1, obtenu %d", len(tcs))
	}
	id := tcs[0].(map[string]any)["id"].(string)
	if strings.HasPrefix(id, "call_") {
		t.Fatalf("Point 2 non corrigé : identifiant d'outil encore au format backend : %s", id)
	}
	if !strings.HasPrefix(id, "toolu_") {
		t.Fatalf("Point 2 : identifiant d'outil attendu en toolu_, obtenu %s", id)
	}
	if strings.Contains(id, "_") && strings.Contains(strings.TrimPrefix(id, "toolu_"), "_") {
		t.Fatalf("Point 2 : l'identifiant garde des soulignés internes : %s", id)
	}

	// Point 5 : les compteurs propres au raisonnement ne doivent plus sortir.
	usage, _ := out["usage"].(map[string]any)
	if _, present := usage["reasoning_tokens"]; present {
		t.Fatal("Point 5 non corrigé : reasoning_tokens encore présent")
	}
	if _, present := usage["completion_tokens_details"]; present {
		t.Fatal("Point 5 non corrigé : completion_tokens_details encore présent")
	}
	if usage["prompt_tokens"] != float64(10) || usage["completion_tokens"] != float64(2) {
		t.Fatal("les compteurs standards doivent rester intacts")
	}
}

// Point 4 : une réponse dont le contenu est vide mais qui a un raisonnement ne doit jamais
// partir vide chez le client.
func TestMaskResponseAsAnthropic_EmptyWithReasoning(t *testing.T) {
	resp := map[string]any{
		"choices": []any{
			map[string]any{
				"index": float64(0),
				"message": map[string]any{
					"role":              "assistant",
					"content":           "",
					"reasoning_content": "je réfléchis longuement",
				},
			},
		},
	}
	out := MaskResponseAsAnthropic(resp)
	msg, _ := out["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if _, present := msg["reasoning_content"]; present {
		t.Fatal("le raisonnement du backend ne doit jamais être exposé")
	}
	content, _ := msg["content"].(string)
	if strings.TrimSpace(content) == "" {
		t.Fatal("Point 4 non corrigé : contenu vide après raisonnement")
	}
}

// Point 2, forme réelle du chemin non streamé : Aggregate construit tool_calls en
// []map[string]any et non en []any. La conversion doit s'appliquer à cette forme aussi,
// sinon les appels synchrones sortent avec des identifiants du backend (call_...).
func TestMaskResponseAsAnthropic_AggregateShape(t *testing.T) {
	resp := map[string]any{
		"id":    "msg_abc",
		"model": "claude-sonnet-5-5",
		"choices": []any{
			map[string]any{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": "Ok.",
					"tool_calls": []map[string]any{
						{"id": "call_00_wFIQ8IB79qJ807Ufcuwf2830", "type": "function",
							"function": map[string]any{"name": "ping", "arguments": `{"v":1}`}},
					},
				},
			},
		},
	}

	out := MaskResponseAsAnthropic(resp)
	msg, _ := out["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	calls, ok := msg["tool_calls"].([]map[string]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("tool_calls attendu 1 dans la forme Aggregate, obtenu %#v", msg["tool_calls"])
	}
	if id := calls[0]["id"]; id != "toolu_00wFIQ8IB79qJ807Ufcuwf2830" {
		t.Fatalf("Point 2 non corrigé sur la forme Aggregate : id=%v", id)
	}
}

// Point 2, bout en bout : un flux upstream réel agrégé puis masqué doit sortir en toolu_.
func TestMaskResponseAsAnthropic_AggregatedStream(t *testing.T) {
	flux := strings.Join([]string{
		`data: {"id":"chatcmpl-a","object":"chat.completion.chunk","created":1,"model":"deepseek-v4.1-flash","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`,
		`data: {"id":"chatcmpl-a","object":"chat.completion.chunk","created":1,"model":"deepseek-v4.1-flash","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_00_7XnQ2","type":"function","function":{"name":"lookup","arguments":"{\"q\":1}"}}]}}]}`,
		`data: {"id":"chatcmpl-a","object":"chat.completion.chunk","created":1,"model":"deepseek-v4.1-flash","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}, "\n")

	agg, err := Aggregate(strings.NewReader(flux))
	if err != nil {
		t.Fatalf("agrégation : %v", err)
	}
	out := MaskResponseAsAnthropic(agg)
	msg, _ := out["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	switch calls := msg["tool_calls"].(type) {
	case []map[string]any:
		if len(calls) != 1 || !strings.HasPrefix(calls[0]["id"].(string), "toolu_") {
			t.Fatalf("Point 2 non corrigé après agrégation : %#v", calls)
		}
	case []any:
		if len(calls) != 1 || !strings.HasPrefix(calls[0].(map[string]any)["id"].(string), "toolu_") {
			t.Fatalf("Point 2 non corrigé après agrégation : %#v", calls)
		}
	default:
		t.Fatalf("tool_calls absent après agrégation : %#v", msg)
	}
}

// Point 3 (streaming) : chaque trame doit porter msg_ et les identifiants d'outils doivent être
// convertis au passage.
func TestMaskAsAnthropic_StreamFrames(t *testing.T) {
	flux := strings.Join([]string{
		`data: {"id":"chatcmpl-x1","object":"chat.completion.chunk","model":"claude-sonnet-5-5","choices":[{"index":0,"delta":{"role":"assistant","content":"hel"}}]}`,
		`data: {"id":"chatcmpl-x1","object":"chat.completion.chunk","model":"claude-sonnet-5-5","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_00_ABC_def","type":"function","function":{"name":"lookup"}}]}}]}`,
		`data: [DONE]`,
	}, "\n")

	out := string(MaskAsAnthropic([]byte(flux), true))
	if strings.Contains(out, "chatcmpl-") {
		t.Fatalf("Point 3 (streaming) non corrigé, trame encore en chatcmpl- :\n%s", out)
	}
	if !strings.Contains(out, `"msg_`) {
		t.Fatalf("Point 3 (streaming) : aucune trame en msg_ :\n%s", out)
	}
	if strings.Contains(out, "call_00_ABC_def") {
		t.Fatalf("Point 2 (streaming) non corrigé :\n%s", out)
	}
	if !strings.Contains(out, "toolu_00ABCdef") {
		t.Fatalf("Point 2 (streaming) : identifiant d'outil non converti :\n%s", out)
	}
	// Le suffixe ne doit plus contenir de souligné : c'est la marque des identifiants du backend.
	if strings.Contains(out, "toolu_00_") || strings.Contains(out, "toolu_ABC_def") {
		t.Fatalf("Point 2 (streaming) : soulignés internes conservés :\n%s", out)
	}
	if !strings.Contains(out, "[DONE]") {
		t.Fatal("la trame de fin doit être conservée")
	}
}
