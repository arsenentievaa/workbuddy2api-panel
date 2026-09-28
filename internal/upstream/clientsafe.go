// clientsafe.go 客户端面脱敏与错误标准化（安全审计修复的单一事实来源）。
//
// # 威胁模型
//
// 本网关的上游是腾讯 CodeBuddy（国内版），下游客户端是 NewAPI（渠道 12/13）。
// NewAPI 会把网关返回的文案与响应字段**继续透给最终客户**。因此任何到达客户端的
// "后端身份"都是泄漏，而不是内部细节。审计实测确认的泄漏面：
//
//	响应 model 字段        真实后端模型名（deepseek-v4.1-flash / glm-* / kimi-*）
//	                       → 直接暴露 claude→deepseek 替换，使 Claude 身份注入
//	                       （prompt.Identity）完全失效（流式每帧都带）
//	响应 id / system_      上游消息 id、部署指纹、服务层级 → 后端关联标识
//	fingerprint / service_tier
//	SSE error 帧 error.code  上游协议码（6004 / 11140 / 11145 / 11102 / 12153 …）
//	SSE error 帧 requestId   上游关联 id
//	gateway_hint 文案        "on this backend" / "another account" → 暴露多账号池架构
//	/healthz 的 X-Service    "workbuddy2api" 明文服务名
//
// # 纪律
//
//  1. 面向客户端的文本只能来自本文件的表；上游原文一律不进客户端面。
//  2. LeaksInternal 是**最后一道闸**：任何待发文本在写出前过一遍，命中内部标记就
//     整条替换为通用文案（宁可少信息，不可泄漏）。
//  3. 客户的模型名是客户自己的输入，必须原样回显（OpenAI 兼容语义），因此扫描时
//     用 allow 参数把客户模型名从待检文本里剔除——否则 "glm-5.2" 这类**公开**
//     模型名会被误判成泄漏，把一个合法的 400 变成 503。
package upstream

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// NewResponseID 生成**网关自造**的客户端面响应 id（chatcmpl- + 24 hex）。
// 用途：替换上游消息 id。客户端需要的是"同一条响应内稳定"的 id（便于按 id 归并），
// 而不是上游的真实标识——后者会把一次客户端请求与上游具体消息关联起来。
func NewResponseID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()) // 退化路径：仍不暴露上游 id
	}
	return "chatcmpl-" + hex.EncodeToString(b[:])
}

// ClientError 客户端面的标准化错误（状态码 + 公开 code + 通用文案）。
type ClientError struct {
	Status  int
	Code    string
	Message string
}

// genericClientError 兜底：任何"说不清"或"疑似泄漏"的情形都收敛到这里。
func genericClientError() ClientError {
	return ClientError{
		Status:  http.StatusServiceUnavailable,
		Code:    "service_unavailable",
		Message: "the service is temporarily unavailable; please try again later",
	}
}

// StandardizeClientError 把上游错误形态映射为标准 HTTP 语义的客户端错误。
//
// 映射表（审计要求）：
//
//	余额/积分耗尽        → 503 Service Unavailable（**有意不用 402**，见 ErrHardCredit 分支）
//	配额/限流            → 429 Too Many Requests
//	模型不存在           → 404 Not Found
//	请求本身有问题       → 400 Bad Request
//	其余（账号/服务故障）→ 503 Service Unavailable
//
// 有意偏离审计清单的一处：**上游账号 session 失效不映射为 401**。401 会让客户端
// 以为"自己的密钥错了"而停止重试——但这是网关侧账号的问题，与客户端凭据无关；
// 且 NewAPI 对渠道 401 常直接判死渠道，会把一次上游抖动升级成渠道下线。故它归
// "服务故障" → 503（我们这边的问题，请重试）。客户端凭据错误仍在 withAuth 里
// 返回 401（那条才是真正的客户端鉴权失败）。
//
// allow 传入客户的模型名（可多个）以便扫描时剔除，见文件头纪律 3。
func StandardizeClientError(kind ErrKind, upstreamStatus int, allow ...string) ClientError {
	ce := standardForKind(kind)
	// 最后一道闸：候选文案/码里若出现内部标记，整条换成通用 503。
	if LeaksInternal(ce.Message, allow...) || LeaksInternal(ce.Code, allow...) {
		return genericClientError()
	}
	// 防御：上游状态码本身不该被回显（可能是 4xx/5xx 之外的私有码）。
	if upstreamStatus != 0 && (upstreamStatus < 100 || upstreamStatus > 599) {
		return genericClientError()
	}
	return ce
}

// standardForKind 是映射表的唯一实现（不含泄漏判定，便于测试逐项断言）。
func standardForKind(kind ErrKind) ClientError {
	switch kind {
	case ErrHardCredit:
		// 审计决策（2026-09-28）：**保持 503，不映射 402**。理由：NewAPI 常把上游
		// 402 视为渠道永久性失败并自动禁用该渠道——渠道 12 是主力，一次上游账号
		// 欠费就会升级成渠道下线（临时故障 → 长时间不可用）。503 让 NewAPI 走
		// 重试/换渠道，故障面收敛在本次请求。
		// body 里的 code 仍保留 insufficient_balance：NewAPI 的判据是 HTTP 状态码，
		// 不会因 body 的 code 触发状态级逻辑；网关侧监控可用它区分欠费与其他故障。
		return ClientError{http.StatusServiceUnavailable, "insufficient_balance",
			"insufficient balance for this request; contact your provider to top up"}
	case ErrSoftRate:
		return ClientError{http.StatusTooManyRequests, "rate_limit_exceeded",
			"rate limited; please wait a moment and try again"}
	case ErrModelBlocked:
		return ClientError{http.StatusNotFound, "model_not_found",
			"the requested model is not available; pick a model from /v1/models"}
	case ErrNotFound:
		return ClientError{http.StatusNotFound, "model_not_found",
			"the requested model is not available; pick a model from /v1/models"}
	case ErrContentBlocked:
		return ClientError{http.StatusBadRequest, "content_blocked",
			"request content was rejected by the content policy"}
	case ErrPromptTooLong:
		return ClientError{http.StatusBadRequest, "prompt_too_long",
			"request context is too long"}
	case ErrImageInvalid:
		return ClientError{http.StatusBadRequest, "image_invalid",
			"image request was rejected"}
	case ErrBadParams:
		return ClientError{http.StatusBadRequest, "invalid_request_error",
			"invalid request parameters"}
	case ErrClient:
		return ClientError{http.StatusBadRequest, "invalid_request_error",
			"invalid request parameters"}
	default:
		// ErrNone / ErrServer / ErrSessionDead / ErrAccountFault / ErrWafBlock /
		// 传输层抖动 → 服务故障（见函数注释对 401 的偏离说明）。
		return genericClientError()
	}
}

// internalWords 内部产品/基础设施名——出现在客户端面即为泄漏。
// 注意这里只放"后端身份"词；公开模型名（glm-5.2 / deepseek-v4.1-flash）不在列，
// 它们是网关在 /v1/models 主动售卖的名字（由 allow 参数在扫描时剔除）。
var internalWords = []string{
	// 上游产品/公司
	"workbuddy", "codebuddy", "tencent", "copilot.tencent", "腾讯",
	// 上游模型供应商。这三个既是审计点名的标记，也是网关 /v1/models **公开售卖**
	// 的模型名前缀——两者冲突由 allow 参数化解：调用方把"客户端请求的模型名"传进
	// allow，扫描前先剔除，于是"回显客户自己的模型名"不算泄漏，而"在客户没要
	// deepseek 的请求里冒出 deepseek"仍会被拦下（正是 claude→deepseek 替换那一类）。
	"deepseek", "glm", "kimi",
	"qwen", "doubao", "hunyuan", "minimax", "mimo",
	// 平台/基础设施
	"zeabur", "upstash", "minio", "one-api", "new-api", "newapi", "lumia",
	"brightdata", "bright data", "brd-customer", "superproxy",
	// 内部路径 / 文件 / 卷
	"/app/auths", "/app/data", "/app/config", "one-api.db", "state.json", "auths/", "/data/",
}

// internalCodePatterns 上游私有协议码的 JSON 形态。用**带键**形态而非裸数字：
// 裸数字会与 token 数（"11140 tokens"）误撞，把合法文案误判成泄漏。
var internalCodePatterns = []string{
	`"code":6004`, `"code": 6004`,
	`"code":11101`, `"code": 11101`,
	`"code":11102`, `"code": 11102`,
	`"code":11115`, `"code": 11115`,
	`"code":11128`, `"code": 11128`,
	`"code":11133`, `"code": 11133`,
	`"code":11135`, `"code": 11135`,
	`"code":11140`, `"code": 11140`,
	`"code":11145`, `"code": 11145`,
	`"code":12153`, `"code": 12153`,
	`"code":14017`, `"code": 14017`,
	`"code":14018`, `"code": 14018`,
}

// LeaksInternal 报告文本是否含内部标记。allow 里的子串在扫描前被剔除（大小写
// 不敏感）——用于放行客户自己的模型名等**合法回显**。
func LeaksInternal(text string, allow ...string) bool {
	if text == "" {
		return false
	}
	low := strings.ToLower(text)
	for _, a := range allow {
		if a = strings.TrimSpace(a); a != "" {
			low = strings.ReplaceAll(low, strings.ToLower(a), "")
		}
	}
	for _, w := range internalWords {
		if strings.Contains(low, w) {
			return true
		}
	}
	for _, p := range internalCodePatterns {
		if strings.Contains(low, p) {
			return true
		}
	}
	return false
}

// InternalLeakGuard 是发给客户端文本的最后一道闸：命中内部标记就整条替换为
// generic。返回 (净化后的文本, 是否发生了替换) 便于调用方观测/打点。
func InternalLeakGuard(text string, allow ...string) (string, bool) {
	if !LeaksInternal(text, allow...) {
		return text, false
	}
	return genericClientError().Message, true
}

// GenericErrorFrame 返回一条客户端安全的通用 SSE error 帧（JSON 字符串）。
// 用于替换**无法解析**的上游 data 帧：非 JSON 的 data 行常是 WAF 拦截页（HTML）
// 或厂商纯文本错误，原样外发会带出源站主机名/IP/厂商字样。
func GenericErrorFrame(kind ErrKind) string {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message": MaskErrorMessage(kind),
		"type":    "api_error",
		"code":    PublicErrorCode(kind),
	}})
	return string(b)
}

// SafeHint 给可选的 gateway_hint 过最后一道闸：命中内部标记 → 返回空串
// （宁可不给提示，也不泄漏——hint 是补充说明，缺席不影响客户端解析）。
func SafeHint(hint string, allow ...string) string {
	if LeaksInternal(hint, allow...) {
		return ""
	}
	return hint
}

// PublicErrorCode 返回某个 ErrKind 的公开 code（客户端面），供只需要 code 的场景
// （如 SSE 错误帧重写）复用同一张表。
func PublicErrorCode(kind ErrKind) string { return standardForKind(kind).Code }

// PublicStatus 返回某个 ErrKind 的标准状态码。
func PublicStatus(kind ErrKind) int { return standardForKind(kind).Status }
