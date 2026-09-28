package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 管理面（/status）与数据面（/v1/*）的密钥分离契约。
//
// 审计目标：客户/下游只持有数据面密钥。即使它泄漏，也不能读到账号池
// （昵称、UID、积分、成本账本）。因此 /status 必须用管理密钥。

const (
	dataKey  = "sk-data-plane"
	adminKey = "sk-admin-plane"
)

func auditHandler(t *testing.T, data, admin string, panel http.Handler) *Handler {
	t.Helper()
	return NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: upstream.New(),
		Panel:    panel,
		Live:     livecfg.New(livecfg.Snapshot{APIKey: data, AdminAPIKey: admin}),
	})
}

func get(h *Handler, path, bearer string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestAdminKeySeparatesStatusFromDataPlane 核心契约：
//   - 数据面密钥 → /v1/models 放行、/status **拒绝**
//   - 管理密钥   → /status 放行
func TestAdminKeySeparatesStatusFromDataPlane(t *testing.T) {
	h := auditHandler(t, dataKey, adminKey, nil)

	if rec := get(h, "/v1/models", dataKey); rec.Code != http.StatusOK {
		t.Errorf("/v1/models with data key: code=%d want 200", rec.Code)
	}
	if rec := get(h, "/status", dataKey); rec.Code != http.StatusUnauthorized {
		t.Errorf("/status with DATA key: code=%d want 401 — a client key must not read the account pool", rec.Code)
	}
	if rec := get(h, "/status", adminKey); rec.Code != http.StatusOK {
		t.Errorf("/status with admin key: code=%d want 200", rec.Code)
	}
	if rec := get(h, "/status", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("/status without key: code=%d want 401", rec.Code)
	}
	// 管理密钥不该能当数据面密钥用（分离是双向的）。
	if rec := get(h, "/v1/models", adminKey); rec.Code != http.StatusUnauthorized {
		t.Errorf("/v1/models with ADMIN key: code=%d want 401 (planes must not be interchangeable)", rec.Code)
	}
}

// TestAdminKeyEmptyFallsBackToDataKey 未配置管理密钥 → 回落数据面密钥，直接升级
// 部署不会把自己锁在面板外（迁移安全性）。
func TestAdminKeyEmptyFallsBackToDataKey(t *testing.T) {
	h := auditHandler(t, dataKey, "", nil)
	if rec := get(h, "/status", dataKey); rec.Code != http.StatusOK {
		t.Errorf("fallback: /status with data key: code=%d want 200", rec.Code)
	}
	if rec := get(h, "/status", adminKey); rec.Code != http.StatusUnauthorized {
		t.Errorf("fallback: an unset admin key must not authenticate: code=%d", rec.Code)
	}
}

// TestAdminKeyLiveReload 管理密钥热生效（面板改配置立即生效，无需重启）。
func TestAdminKeyLiveReload(t *testing.T) {
	holder := livecfg.New(livecfg.Snapshot{APIKey: dataKey, AdminAPIKey: adminKey})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: upstream.New(),
		Live:     holder,
	})
	if rec := get(h, "/status", dataKey); rec.Code != http.StatusUnauthorized {
		t.Fatalf("precondition: data key must be rejected, got %d", rec.Code)
	}
	// 轮换管理密钥后，旧管理密钥立即失效、新密钥立即生效。
	holder.Store(livecfg.Snapshot{APIKey: dataKey, AdminAPIKey: "sk-rotated"})
	if rec := get(h, "/status", adminKey); rec.Code != http.StatusUnauthorized {
		t.Errorf("old admin key must stop working after rotation: %d", rec.Code)
	}
	if rec := get(h, "/status", "sk-rotated"); rec.Code != http.StatusOK {
		t.Errorf("rotated admin key must work: %d", rec.Code)
	}
}

// TestAdminAuthFailureDoesNotDiscloseEndpoint 401 文案不得暗示"这是管理端点"，
// 否则等于给探测者指路。
func TestAdminAuthFailureDoesNotDiscloseEndpoint(t *testing.T) {
	h := auditHandler(t, dataKey, adminKey, nil)
	rec := get(h, "/status", dataKey)
	body := strings.ToLower(rec.Body.String())
	for _, banned := range []string{"admin", "panel", "management", "workbuddy", "pool"} {
		if strings.Contains(body, banned) {
			t.Errorf("401 body discloses endpoint nature (%q): %s", banned, rec.Body)
		}
	}
	// 与数据面 401 文案**完全一致**（不可区分）。
	dataRec := get(h, "/v1/models", "wrong")
	if rec.Body.String() != dataRec.Body.String() {
		t.Errorf("admin 401 must be indistinguishable from data 401:\n admin=%s\n data =%s",
			rec.Body, dataRec.Body)
	}
}

// TestSnapshotAdminKeyResolution 解析规则单测（Live 层）。
func TestSnapshotAdminKeyResolution(t *testing.T) {
	cases := []struct{ data, admin, want string }{
		{"d", "a", "a"}, // 配置了管理密钥 → 用它
		{"d", "", "d"},  // 未配置 → 回落数据面
		{"", "", ""},    // 都不配 → 不鉴权（既有语义）
	}
	for _, c := range cases {
		got := livecfg.Snapshot{APIKey: c.data, AdminAPIKey: c.admin}.AdminKey()
		if got != c.want {
			t.Errorf("AdminKey(data=%q admin=%q)=%q want %q", c.data, c.admin, got, c.want)
		}
	}
}
