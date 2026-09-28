package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestLeaksInternalDetectsMarkers 泄漏检测器本身必须有效（否则"最后一道闸"是摆设）。
func TestLeaksInternalDetectsMarkers(t *testing.T) {
	leaky := []string{
		"insufficient balance on workbuddy",
		"CodeBuddy account pool exhausted",
		"upstream codebuddy returned an error",
		"tencent copilot.tencent.com rejected the request",
		`{"code":6004,"msg":"limit"}`,
		`{"code": 11140,"msg":"request illegal"}`,
		`{"code":12153}`,
		`{"code":14018}`,
		"read /app/auths/workbuddy-123.json",
		"volume vol-abc mounted at /app/data",
		"hosted on zeabur",
		"upstash redis at internal",
		"backend deepseek-v4.1-flash",
		"glm-5.2 is the real model",
		"kimi-k3-1 fallback",
	}
	for _, s := range leaky {
		if !LeaksInternal(s) {
			t.Errorf("LeaksInternal(%q)=false, want true", s)
		}
	}
	clean := []string{
		"",
		"the service is temporarily unavailable; please try again later",
		"rate limited; retry after the reset window",
		"request context exceeds the model's limit; reduce history/message size",
		"insufficient balance for this request; contact your provider to top up",
	}
	for _, s := range clean {
		if LeaksInternal(s) {
			t.Errorf("LeaksInternal(%q)=true, want false", s)
		}
	}
}

// TestLeaksInternalAllowsPublicModelNames 是标记与公开模型名的冲突化解：allow 传入
// 客户自己的模型名后，回显该名字不算泄漏；不放行则会被拦（证明 allow 真在起作用）。
func TestLeaksInternalAllowsPublicModelNames(t *testing.T) {
	for _, m := range []string{"glm-5.2", "deepseek-v4.1-flash", "kimi-k3-1"} {
		hint := "model " + m + " does not support images; pick one with supports_images=true from /v1/models"
		if LeaksInternal(hint, m) {
			t.Errorf("hint echoing the client's own model %q must NOT be treated as a leak: %s", m, hint)
		}
		if !LeaksInternal(hint) {
			t.Errorf("without allow, %q must be flagged (allow is doing nothing)", m)
		}
	}
}

// TestStandardizeClientErrorTable 审计要求的映射表逐项锁定。
func TestStandardizeClientErrorTable(t *testing.T) {
	cases := []struct {
		kind   ErrKind
		status int
		code   string
	}{
		{ErrHardCredit, 503, "insufficient_balance"},  // 余额 → 503（有意不用 402）
		{ErrSoftRate, 429, "rate_limit_exceeded"},     // 配额/限流 → 429
		{ErrModelBlocked, 404, "model_not_found"},     // 模型不存在 → 404
		{ErrNotFound, 404, "model_not_found"},         //
		{ErrBadParams, 400, "invalid_request_error"},  // 请求本身的问题 → 400
		{ErrClient, 400, "invalid_request_error"},     //
		{ErrContentBlocked, 400, "content_blocked"},   //
		{ErrPromptTooLong, 400, "prompt_too_long"},    //
		{ErrImageInvalid, 400, "image_invalid"},       //
		{ErrSessionDead, 503, "service_unavailable"},  // 上游账号问题 → 503（非 401，见实现注释）
		{ErrAccountFault, 503, "service_unavailable"}, //
		{ErrWafBlock, 503, "service_unavailable"},     //
		{ErrServer, 503, "service_unavailable"},       //
		{ErrNone, 503, "service_unavailable"},         //
	}
	for _, tc := range cases {
		ce := StandardizeClientError(tc.kind, 500)
		if ce.Status != tc.status || ce.Code != tc.code {
			t.Errorf("kind=%v → (%d,%s) want (%d,%s)", tc.kind, ce.Status, ce.Code, tc.status, tc.code)
		}
		if LeaksInternal(ce.Message) || LeaksInternal(ce.Code) {
			t.Errorf("kind=%v text leaks: %q / %q", tc.kind, ce.Message, ce.Code)
		}
	}
}

// TestStandardizeRejectsLeakyOverride 即便表内文案被改脏，闸门也必须收敛到通用 503。
func TestStandardizeRejectsLeakyOverride(t *testing.T) {
	// 直接验证闸门函数（表本身已由上一测试保证干净）。
	if _, replaced := InternalLeakGuard("workbuddy internal error", ""); !replaced {
		t.Error("guard must replace leaky text")
	}
	if got, replaced := InternalLeakGuard("clean message", ""); replaced || got != "clean message" {
		t.Errorf("guard must pass clean text through: %q replaced=%v", got, replaced)
	}
	if got := SafeHint("backend workbuddy", ""); got != "" {
		t.Errorf("leaky hint must be dropped, got %q", got)
	}
	if got := SafeHint("rate limited; retry later", ""); got != "rate limited; retry later" {
		t.Errorf("clean hint must survive, got %q", got)
	}
}

// TestHintsDoNotLeakInternalRefs 遍历所有 Kind 与两种形态判定，锁定 hint 文案不含
// 内部引用（含"account"/"backend"/"pool"这类架构措辞）。
func TestHintsDoNotLeakInternalRefs(t *testing.T) {
	kinds := []ErrKind{
		ErrHardCredit, ErrSoftRate, ErrSessionDead, ErrNotFound, ErrServer,
		ErrContentBlocked, ErrBadParams, ErrAccountFault, ErrModelBlocked,
		ErrWafBlock, ErrPromptTooLong, ErrImageInvalid, ErrClient, ErrNone,
	}
	ctxs := []HintContext{
		{},
		{Model: "glm-5.2", HasImage: true, ModelInCatalog: true, ModelSupportsImages: false},
		{Model: "deepseek-v4.1-flash", HasImage: true, ModelInCatalog: true, ModelSupportsImages: true},
	}
	// 上游原文形态（hint 判定会读它，但绝不能把它带出去）
	bodies := []string{
		"",
		`{"code":11133,"msg":"model_param_invalid"}`,
		`{"code":11135,"msg":"invalid_image_data, please replace the image"}`,
		`{"code":6004,"msg":"当前模型使用人数过多，请稍后重试"}`,
		`{"code":12153,"msg":"Offline user session not found"}`,
		`{"code":11140,"msg":"request illegal"}`,
		`{"code":11102,"msg":"service info not found"}`,
	}
	architecture := []string{"account", "backend", "pool", "gateway", "upstream", "workbuddy", "codebuddy", "deepseek", "glm", "kimi", "tencent"}
	for _, k := range kinds {
		for _, body := range bodies {
			for _, ctx := range ctxs {
				h := SafeHint(GatewayHint(k, body, ctx), ctx.Model)
				if h == "" {
					continue
				}
				if LeaksInternal(h, ctx.Model) {
					t.Errorf("hint leaks internal ref: kind=%v body=%q hint=%q", k, body, h)
				}
				// 架构措辞单独扫（它们不在 internalWords 里：不是"名字"而是"说法"）。
				// 同样剔除客户自己的模型名——"model glm-5.2 does not support images"
				// 是回显客户输入，不是泄漏。
				probe := strings.ToLower(h)
				if m := strings.TrimSpace(ctx.Model); m != "" {
					probe = strings.ReplaceAll(probe, strings.ToLower(m), "")
				}
				for _, a := range architecture {
					if strings.Contains(probe, a) {
						t.Errorf("hint discloses architecture (%q): kind=%v hint=%q", a, k, h)
					}
				}
			}
		}
	}
}

// TestSanitizeErrorFrame sanity：message 掩码 + code 标准化 + requestId 删除。
func TestSanitizeErrorFrame(t *testing.T) {
	in := `{"error":{"code":6004,"message":"当前模型使用人数过多，请稍后重试","requestId":"req-upstream-secret","type":"upstream_error"}}`
	out := sanitizeErrorFrame(in)
	for _, banned := range []string{"6004", "req-upstream-secret", "当前模型", "稍后重试"} {
		if strings.Contains(out, banned) {
			t.Errorf("sanitized frame still contains %q: %s", banned, out)
		}
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(out), &obj); err != nil {
		t.Fatalf("output must stay valid JSON: %v (%s)", err, out)
	}
	e := obj["error"].(map[string]any)
	if e["code"] != "rate_limit_exceeded" {
		t.Errorf("code=%v want the public code rate_limit_exceeded", e["code"])
	}
	if _, ok := e["requestId"]; ok {
		t.Error("requestId must be removed")
	}
	if _, ok := e["message"].(string); !ok {
		t.Error("message must remain a string")
	}
}

// TestSanitizeErrorFrameNonJSONBecomesGeneric 非 JSON 错误体（WAF HTML 页/纯文本）
// 绝不原样外发——这是审计新发现的一条真实泄漏路径。
func TestSanitizeErrorFrameNonJSONBecomesGeneric(t *testing.T) {
	html := `<html><head><title>403 Forbidden</title></head><body>APISIX at workbuddy-origin.internal rejected</body></html>`
	out := sanitizeErrorFrame(html)
	for _, banned := range []string{"<html>", "APISIX", "workbuddy", "403 Forbidden"} {
		if strings.Contains(out, banned) {
			t.Errorf("non-JSON error body leaked %q: %s", banned, out)
		}
	}
	if !json.Valid([]byte(out)) {
		t.Errorf("must emit a valid JSON error frame, got %s", out)
	}
}

// TestMaskErrorMessageNeverLeaks 掩码文案本身必须干净（防回归）。
func TestMaskErrorMessageNeverLeaks(t *testing.T) {
	for k := ErrNone; k <= ErrClient; k++ {
		if m := MaskErrorMessage(k); LeaksInternal(m) {
			t.Errorf("MaskErrorMessage(%v) leaks: %q", k, m)
		}
	}
}

// TestNewResponseIDShape 网关自造 id 形态：chatcmpl- 前缀 + 足够随机、长度固定。
func TestNewResponseIDShape(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		id := NewResponseID()
		if !strings.HasPrefix(id, "chatcmpl-") {
			t.Fatalf("id=%q want chatcmpl- prefix", id)
		}
		if strings.Contains(id, "up-") || len(id) != len("chatcmpl-")+24 {
			t.Fatalf("id=%q unexpected shape", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}

// TestSanitizeUsageDropsNonStandardFields 白名单：把生产实测出现的非规范字段全部剥掉，
// 只留 OpenAI 规范的三个顶层计数。
func TestSanitizeUsageDropsNonStandardFields(t *testing.T) {
	usage := map[string]any{
		// 规范字段（保留）
		"prompt_tokens":     527,
		"completion_tokens": 16,
		"total_tokens":      543,
		// 审计实测的非规范字段（必须剥掉）
		"prompt_cache_hit_tokens":     384,
		"prompt_cache_miss_tokens":    143,
		"prompt_cache_write_tokens":   0,
		"cached_tokens":               0,
		"cache_creation_input_tokens": 0,
		"cache_read_input_tokens":     0,
		"completion_thinking_tokens":  16,
		"credit":                      0.01,
		"prompt_tokens_details":       map[string]any{"cached_tokens": 384},
		"completion_tokens_details":   map[string]any{"reasoning_tokens": 16},
	}
	got := SanitizeUsage(usage)
	if len(got) != 3 {
		t.Fatalf("usage=%v want only the 3 standard top-level keys", got)
	}
	for _, k := range []string{"prompt_tokens", "completion_tokens", "total_tokens"} {
		if _, ok := got[k]; !ok {
			t.Errorf("standard key %q must be kept", k)
		}
	}
	for _, k := range []string{
		"prompt_cache_hit_tokens", "prompt_cache_miss_tokens", "prompt_cache_write_tokens",
		"cached_tokens", "cache_creation_input_tokens", "cache_read_input_tokens",
		"completion_thinking_tokens", "credit", "prompt_tokens_details", "completion_tokens_details",
	} {
		if _, ok := got[k]; ok {
			t.Errorf("non-standard key %q must be stripped", k)
		}
	}
}

// TestSanitizeUsageNil usage 缺失（nil）→ nil，不 panic、不造字段。
func TestSanitizeUsageNil(t *testing.T) {
	if got := SanitizeUsage(nil); got != nil {
		t.Errorf("SanitizeUsage(nil)=%v want nil", got)
	}
}
