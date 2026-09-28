// Package livecfg 运行期可变配置的并发安全持有者。
//
// 背景：进程启动时读入的配置是普通字段（读多写零），但管理面板允许在线改配置，
// 于是少量"可热生效"的字段需要有并发安全的读写点。此处用不可变快照 + atomic 指针：
// 读方 Load 拿到一致视图，写方 Store 整体替换，无锁无数据竞争。
//
// 只承载**读路径深、热改需求强**的少数字段；池参数/排程参数等各有既有 setter
// （pool.SetBreaker、scheduler.Reconfigure 等），不重复收编到这里。
package livecfg

import (
	"sync/atomic"
	"time"
)

// Snapshot 一次读取的不可变配置视图。
type Snapshot struct {
	APIKey               string        // 网关/面板共同鉴权密钥；空 = 不鉴权
	SoftCooldown         time.Duration // 429 软冷却基数（<=0 时调用方回退内置默认）
	SanitizeFingerprints bool          // 出站请求体指纹脱敏
	// AdminAPIKey 管理面（/status、/panel/**）专用密钥，与数据面 APIKey 分离。
	// 目的：客户/下游即使拿到数据面密钥也无法访问管理面（账号池、积分、成本、面板）。
	// 空 = 回落 APIKey（向后兼容：既有部署不改配置即维持原行为，不会把自己锁在外面）。
	// 恢复路径：忘记该密钥时置空配置里的 admin_api_key，或用 WB2A_ADMIN_API_KEY 环境变量。
	AdminAPIKey string
	// HealthzServiceHeader /healthz 是否回写 X-Service 响应头（默认 false）。
	// 安全审计：该头在**无鉴权的公网探活端点**上明文暴露服务名 workbuddy2api。
	// 响应体 service 字段保留（宿主可读）；需要旧行为的宿主可用
	// features.healthz_service_header=true 一键恢复，无需改代码。
	HealthzServiceHeader bool
}

// Holder 原子持有当前快照。
type Holder struct {
	p atomic.Pointer[Snapshot]
}

// New 以初始快照构建。
func New(s Snapshot) *Holder {
	h := &Holder{}
	h.Store(s)
	return h
}

// Load 返回当前快照（Holder 为 nil 或从未 Store 时返回零值快照，调用方无需判空）。
func (h *Holder) Load() Snapshot {
	if h == nil {
		return Snapshot{}
	}
	if s := h.p.Load(); s != nil {
		return *s
	}
	return Snapshot{}
}

// Store 整体替换快照。
func (h *Holder) Store(s Snapshot) { h.p.Store(&s) }

// AdminKey 返回管理面生效密钥：AdminAPIKey 非空时用它，否则回落 APIKey。
// 回落语义是刻意的——未配置专用管理密钥的部署行为与审计前完全一致。
func (s Snapshot) AdminKey() string {
	if s.AdminAPIKey != "" {
		return s.AdminAPIKey
	}
	return s.APIKey
}
