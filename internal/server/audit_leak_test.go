package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 安全审计的端到端守卫：这些用例把"客户端面不得出现后端身份 / 中转架构"变成
// 可回归的契约。上游 fixture 全部**故意**携带内部信息（真实后端模型名、上游协议码、
// 中文原文、WAF 页面、部署指纹），任何一条泄漏都会让用例失败。

const auditClientModel = "claude-opus-5"

// sseBackend 模拟上游真实流：model=deepseek-v4.1-flash（后端真名）、上游 id、
// system_fingerprint / service_tier 指纹字段。
const sseBackend = "data: {\"id\":\"up-upstream-1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"deepseek-v4.1-flash\",\"system_fingerprint\":\"fp_zeabur_deadbeef\",\"service_tier\":\"scale\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n" +
	"data: {\"id\":\"up-upstream-1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"deepseek-v4.1-flash\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n" +
	"data: [DONE]\n\n"

// architectureWords 中转/池化架构措辞：客户端面出现即视为架构披露。
var architectureWords = []string{"upstream", "backend", "account pool", "in-flight", "workbuddy", "codebuddy", "tencent"}

func assertNoLeak(t *testing.T, label, body string, allow ...string) {
	t.Helper()
	if upstream.LeaksInternal(body, allow...) {
		t.Errorf("%s: internal reference leaked: %s", label, body)
	}
	low := strings.ToLower(body)
	for _, m := range allow {
		low = strings.ReplaceAll(low, strings.ToLower(m), "")
	}
	for _, w := range architectureWords {
		if strings.Contains(low, w) {
			t.Errorf("%s: architecture disclosed (%q): %s", label, w, body)
		}
	}
}

// TestResponseModelDoesNotLeakBackend 非流式：响应 model 必须是**客户端请求的**模型名，
// 绝不是上游真实模型名（审计泄漏 P0）。
func TestResponseModelDoesNotLeakBackend(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseBackend, true })
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}), Upstream: up})

	cases := []struct {
		name        string
		body        string
		originModel string
		want        string
	}{
		{"回显客户端请求名", `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`, "", "glm-5.2"},
		{"NewAPI 重定向：X-Origin-Model 优先", `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`, auditClientModel, auditClientModel},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(tc.body))
			if tc.originModel != "" {
				req.Header.Set("X-Origin-Model", tc.originModel)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != 200 {
				t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
			}
			var resp map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("bad json: %v", err)
			}
			if resp["model"] != tc.want {
				t.Errorf("model=%v want %q (must echo the CLIENT model, never the backend)", resp["model"], tc.want)
			}
			assertNoLeak(t, "response", rec.Body.String(), tc.want)
			if strings.Contains(rec.Body.String(), "deepseek-v4.1-flash") {
				t.Errorf("backend model name leaked: %s", rec.Body)
			}
			// 上游指纹字段与上游 id 都不得出现
			for _, banned := range []string{"fp_zeabur_deadbeef", "system_fingerprint", "service_tier", "up-upstream-1"} {
				if strings.Contains(rec.Body.String(), banned) {
					t.Errorf("leaked %q: %s", banned, rec.Body)
				}
			}
			id, _ := resp["id"].(string)
			if !strings.HasPrefix(id, "chatcmpl-") {
				t.Errorf("id=%q must be a gateway-generated id", id)
			}
		})
	}
}

// TestStreamChunksDoNotLeakBackend 流式：**每一帧**都不得带后端真名/上游 id/指纹字段。
func TestStreamChunksDoNotLeakBackend(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseBackend, true })
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}), Upstream: up})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("X-Origin-Model", auditClientModel)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	assertNoLeak(t, "stream", body, auditClientModel)

	if strings.Contains(body, "deepseek-v4.1-flash") {
		t.Errorf("stream frame leaks the backend model name:\n%s", body)
	}
	for _, banned := range []string{"up-upstream-1", "fp_zeabur_deadbeef", "system_fingerprint", "service_tier"} {
		if strings.Contains(body, banned) {
			t.Errorf("stream leaks %q:\n%s", banned, body)
		}
	}
	// 每帧 model 必须是客户端模型名。
	frames := 0
	for _, ln := range strings.Split(body, "\n") {
		ln = strings.TrimSpace(ln)
		if !strings.HasPrefix(ln, "data: ") {
			continue
		}
		p := strings.TrimPrefix(ln, "data: ")
		if p == "[DONE]" {
			continue
		}
		var o map[string]any
		if json.Unmarshal([]byte(p), &o) != nil {
			continue
		}
		frames++
		if o["model"] != auditClientModel {
			t.Errorf("frame model=%v want %q", o["model"], auditClientModel)
		}
	}
	if frames == 0 {
		t.Fatal("no frames parsed — test is vacuous")
	}
}

// TestStreamErrorFrameHidesUpstreamProtocol 流式错误帧：上游协议码 6004 / requestId /
// 中文原文都不得到达客户端（审计前只有 message 被掩码，code 与 requestId 原样透出）。
func TestStreamErrorFrameHidesUpstreamProtocol(t *testing.T) {
	frame := `data: {"error":{"code":6004,"message":"当前模型使用人数过多，请稍后重试","requestId":"req-upstream-secret"}}` + "\n\n" +
		"data: [DONE]\n\n"
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, frame, true })
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}), Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	body := rec.Body.String()
	assertNoLeak(t, "stream error frame", body, "glm-5.2")
	for _, banned := range []string{"6004", "req-upstream-secret", "当前模型", "稍后重试"} {
		if strings.Contains(body, banned) {
			t.Errorf("stream error frame leaks %q:\n%s", banned, body)
		}
	}
}

// TestStreamNonJSONFrameNotForwarded 非 JSON 的 data 帧（WAF HTML 页/纯文本）不得原样外发。
func TestStreamNonJSONFrameNotForwarded(t *testing.T) {
	page := "data: <html><body>403 Forbidden — APISIX at workbuddy-origin.internal</body></html>\n\n" +
		"data: [DONE]\n\n"
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, page, true })
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}), Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	body := rec.Body.String()
	assertNoLeak(t, "non-JSON frame", body, "glm-5.2")
	for _, banned := range []string{"<html>", "APISIX", "403 Forbidden"} {
		if strings.Contains(body, banned) {
			t.Errorf("non-JSON upstream page leaked %q:\n%s", banned, body)
		}
	}
}

// TestEveryErrorKindLeaksNothing 逐个错误类型：状态码符合审计映射表，且响应体不含
// 任何内部信息（上游协议码 / 中文原文 / 后端名 / 架构措辞）。这是审计第 5 项的
// "每种错误都发一次请求"的可执行版本（用假上游确定性触发，不打扰生产账号）。
func TestEveryErrorKindLeaksNothing(t *testing.T) {
	cases := []struct {
		name       string
		upStatus   int
		upBody     string
		wantStatus int
	}{
		{"solde épuisé", 402, `{"code":1,"msg":"余额不足"}`, http.StatusServiceUnavailable},
		{"quota / 6004", 429, `{"code":6004,"msg":"当前模型使用人数过多"}`, http.StatusTooManyRequests},
		{"session morte (12153)", 401, `{"code":12153,"msg":"Offline user session not found"}`, http.StatusServiceUnavailable},
		{"compte en faute (11140)", 429, `{"code":11140,"msg":"request illegal"}`, http.StatusServiceUnavailable},
		{"modèle absent (11102)", 400, `{"code":11102,"msg":"service info not found"}`, http.StatusNotFound},
		{"prompt trop long (11115)", 400, `{"code":11115,"msg":"prompt is too long, max 200000 tokens"}`, http.StatusBadRequest},
		{"image invalide (11135)", 400, `{"code":11135,"msg":"invalid_image_data"}`, http.StatusBadRequest},
		{"params illisibles (11101)", 400, `{"code":11101,"msg":"Unmarshal chat params failed"}`, http.StatusBadRequest},
		{"WAF 403 (page HTML)", 403, `<html><head><title>403 Forbidden</title></head><body>APISIX workbuddy-origin</body></html>`, http.StatusServiceUnavailable},
		{"erreur serveur", 500, `{"code":500,"msg":"internal error at codebuddy backend"}`, http.StatusServiceUnavailable},
		{"4xx générique", 400, `{"code":400,"msg":"bad request"}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := newFakeUpstream(t, func(string) (int, string, bool) { return tc.upStatus, tc.upBody, false })
			h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}), Upstream: up})

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
				strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))

			if rec.Code != tc.wantStatus {
				t.Errorf("status=%d want %d (body=%s)", rec.Code, tc.wantStatus, rec.Body)
			}
			body := rec.Body.String()
			assertNoLeak(t, "error response", body, "glm-5.2")
			// 上游原文的关键片段一个都不许出现。
			for _, banned := range []string{
				"余额不足", "当前模型使用人数过多", "Offline user session not found",
				"request illegal", "service info not found", "prompt is too long",
				"invalid_image_data", "Unmarshal chat params failed",
				"<html>", "APISIX", "internal error at",
				`"code":1`, `"code":6004`, `"code":12153`, `"code":11140`, `"code":11102`,
				`"code":11115`, `"code":11135`, `"code":11101`,
			} {
				if strings.Contains(body, banned) {
					t.Errorf("leaked upstream fragment %q: %s", banned, body)
				}
			}
			// 错误信封形状必须保持 OpenAI 兼容（错误路径不能被"脱敏"成非 JSON）。
			var env struct {
				Error *struct {
					Message string `json:"message"`
					Type    string `json:"type"`
					Code    string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || env.Error == nil || env.Error.Message == "" {
				t.Fatalf("error envelope broken: %v body=%s", err, rec.Body)
			}
		})
	}
}

// TestErrorResponsesCarryNoInternalHeaders 错误响应不得携带任何身份/内部头。
func TestErrorResponsesCarryNoInternalHeaders(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 500, `{"code":500,"msg":"codebuddy internal"}`, false
	})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}), Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	for k, v := range rec.Header() {
		joined := strings.ToLower(k + ":" + strings.Join(v, ","))
		for _, banned := range []string{"workbuddy", "codebuddy", "tencent", "device", "upstream", "zeabur", "x-service", "x-request-id"} {
			if strings.Contains(joined, banned) {
				t.Errorf("error response header discloses %q: %s", banned, joined)
			}
		}
	}
}
