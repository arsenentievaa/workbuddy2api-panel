package pool

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// ---------------------------------------------------------------------------
// 6004 模型级 limit 独立冷却（issue：多模型独立计时）
// ---------------------------------------------------------------------------

// TestModelCooldownsIndependent 核心：模型 A 触发 6004（重置 2h 后），模型 B 再触发
// 6004（重置 1h 后）→
//  1. A 的冷却独立保留：1h 后 A 仍在限额中、B 已恢复；
//  2. until（全账号级）不被任何模型的 6004 覆盖。
func TestModelCooldownsIndependent(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	base := time.Now()
	resetA := base.Add(2 * time.Hour)
	resetB := base.Add(1 * time.Hour)

	p.mu.Lock()
	e := p.byUID["u1"]
	e.modelCooldowns = map[string]modelCooldown{
		"glm-5.3": {Until: resetA, ResetAt: resetA},
		"hy3-x":   {Until: resetB, ResetAt: resetB},
	}
	p.mu.Unlock()

	now := base.Add(90 * time.Minute)
	if e.healthyForModel(now, "glm-5.3") {
		t.Fatalf("90m 后 A(glm-5.3, reset 2h) 仍应限额中，但 healthyForModel 放行了")
	}
	if !e.healthyForModel(now, "hy3-x") {
		t.Fatalf("90m 后 B(hy3-x, reset 1h) 应已恢复，但 healthyForModel 仍拦截")
	}
	if !e.until.IsZero() {
		t.Errorf("until=%v 应为零值（6004 模型级冷却不写 until）", e.until)
	}
}

// TestModelCooldownsBDoesNotOverwriteA 6004（带重置时间）现在是账户级软冷却：
// 写 until（冻结所有模型），不再写 modelCooldowns。
func TestModelCooldownsBDoesNotOverwriteA(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	resetA := time.Now().Add(2 * time.Hour)
	p.CooldownSoftForModel("u1", 600*time.Second, resetA, "glm-5.3", "6004 model rate limit")

	p.mu.RLock()
	e := p.byUID["u1"]
	until := e.until
	n := len(e.modelCooldowns)
	p.mu.RUnlock()
	if until.IsZero() {
		t.Fatalf("6004 应写账户级 until（冻结所有模型），但 until 为零值")
	}
	if n != 0 {
		t.Errorf("6004 不应再写 modelCooldowns，got %d 条", n)
	}
}

// TestCooldownSoftForModelSetsUntil 带解析时间的 6004 现在写账户级 until
// （冻结所有模型），不再写 modelCooldowns（无模型豁免）。
func TestCooldownSoftForModelSetsUntil(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	reset := time.Now().Add(30 * time.Minute)
	p.CooldownSoftForModel("u1", 600*time.Second, reset, "glm-5.3", "6004 model rate limit")
	p.mu.RLock()
	e := p.byUID["u1"]
	until := e.until
	_, ok := e.modelCooldowns["glm-5.3"]
	p.mu.RUnlock()
	if until.IsZero() {
		t.Errorf("6004 应写 until（账户级冻结），got 零值")
	}
	if ok {
		t.Errorf("6004 不应写 modelCooldowns（无模型豁免）")
	}
}

// TestCooldownSoftForModelCapsUntil 6004 写账户级 until，截断到 soft_rate_max。
func TestCooldownSoftForModelCapsUntil(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetSoftRateMax(10 * time.Minute)
	reset := time.Now().Add(2 * time.Hour) // 远超封顶 → until 截断到 10m
	before := time.Now()
	p.CooldownSoftForModel("u1", 600*time.Second, reset, "glm-5.3", "6004 model rate limit")
	p.mu.RLock()
	until := p.byUID["u1"].until
	p.mu.RUnlock()
	if rem := until.Sub(before); rem <= 0 || rem > 10*time.Minute+time.Second {
		t.Errorf("until 应在 (0,10m] 区间，实际剩余 %v", rem)
	}
}

// TestHealthyForModelAfter6004 6004（带重置时间）现在是账户级冷却：所有模型都不可选。
func TestHealthyForModelAfter6004(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	reset := time.Now().Add(30 * time.Minute)
	p.CooldownSoftForModel("u1", 600*time.Second, reset, "glm-5.3", "6004 model rate limit")
	p.mu.RLock()
	e := p.byUID["u1"]
	p.mu.RUnlock()

	now := time.Now()
	if e.healthyForModel(now, "glm-5.3") {
		t.Fatal("触发模型 glm-5.3 应不可选")
	}
	if e.healthyForModel(now, "hy3-x") {
		t.Fatal("其他模型 hy3-x 也不可选（6004 冻结账户，无模型豁免）")
	}
	if e.healthy(now) {
		t.Fatal("账号级 healthy 应 false（6004 冻结账户）")
	}
}

// TestModelCooldownsTwoLimitsBothBlock 同一账号两个模型同时 6004：这两个模型都不可选
// （无账号级冷却），其他模型仍可选。
func TestModelCooldownsTwoLimitsBothBlock(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	resetA := time.Now().Add(2 * time.Hour)
	resetB := time.Now().Add(2 * time.Hour)
	p.mu.Lock()
	e := p.byUID["u1"]
	e.modelCooldowns = map[string]modelCooldown{
		"glm-5.3": {Until: resetA, ResetAt: resetA},
		"hy3-x":   {Until: resetB, ResetAt: resetB},
	}
	p.mu.Unlock()

	now := time.Now()
	if e.healthyForModel(now, "glm-5.3") {
		t.Fatal("glm-5.3 应被自身冷却拦截")
	}
	if e.healthyForModel(now, "hy3-x") {
		t.Fatal("hy3-x 应被自身冷却拦截")
	}
	if !e.healthyForModel(now, "other") {
		t.Fatal("other 模型应可选（多模型限流不应让账号级不可选）")
	}
}

// TestCooldownPreservedByNoteSuccess 6004（账户级冷却）在成功后不清 until
// （NoteSuccess 只清 softStreak/熔断/连败，冷却截止由墙钟或签到决定）。
func TestCooldownPreservedByNoteSuccess(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	resetA := time.Now().Add(2 * time.Hour)
	p.CooldownSoftForModel("u1", 600*time.Second, resetA, "glm-5.3", "6004 model rate limit")
	p.NoteSuccess("u1")
	p.mu.RLock()
	e := p.byUID["u1"]
	until := e.until
	n := len(e.modelCooldowns)
	p.mu.RUnlock()
	if until.IsZero() {
		t.Errorf("NoteSuccess 不应清 until（冷却截止仍有效）")
	}
	if n != 0 {
		t.Errorf("6004 不应写 modelCooldowns，got %d", n)
	}
}

// TestModelCooldownsClearedByRevive 签到解冻（reviveCoolingLocked）→ 模型级 6004 冷却清零。
func TestModelCooldownsClearedByRevive(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftForModel("u1", 600*time.Second, time.Now().Add(time.Hour), "glm-5.3", "6004")
	p.ReenableIfCredits("u1", 500, 0)
	p.mu.RLock()
	n := len(p.byUID["u1"].modelCooldowns)
	p.mu.RUnlock()
	if n != 0 {
		t.Errorf("revive 后 modelCooldowns=%d want 0", n)
	}
}

// TestModelCooldownsLazyCleanup 已过期的模型冷却在 pick（写锁路径）时被惰性清理。
func TestModelCooldownsLazyCleanup(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.mu.Lock()
	e := p.byUID["u1"]
	e.modelCooldowns = map[string]modelCooldown{
		"old": {Until: time.Now().Add(-time.Minute), ResetAt: time.Now().Add(-time.Minute)},
	}
	p.mu.Unlock()
	got := p.PickExcludingForModel(nil, "fresh")
	if got == nil || got.UID != "u1" {
		t.Fatalf("过期模型冷却不应拦截 u1, got %+v", got)
	}
	p.mu.RLock()
	_, still := e.modelCooldowns["old"]
	p.mu.RUnlock()
	if still {
		t.Error("过期模型冷却条目应在 pick 时被清理")
	}
}

// TestModelCooldownsExpiredAllowsSameModel 过期后同模型请求也放行（read 路径无清理也可选）。
func TestModelCooldownsExpiredAllowsSameModel(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.mu.Lock()
	e := p.byUID["u1"]
	e.modelCooldowns = map[string]modelCooldown{
		"glm-5.3": {Until: time.Now().Add(-time.Nanosecond), ResetAt: time.Now().Add(-time.Nanosecond)},
	}
	p.mu.Unlock()
	if !e.healthyForModel(time.Now(), "glm-5.3") {
		t.Fatal("过期后同模型请求应放行")
	}
}

// TestRateLimitedModelsMultiModel 6004 多模型同时限流 → /status 台账全部展示。
func TestRateLimitedModelsMultiModel(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	resetA := time.Now().Add(2 * time.Hour)
	resetB := time.Now().Add(1 * time.Hour)
	p.mu.Lock()
	e := p.byUID["u1"]
	e.modelCooldowns = map[string]modelCooldown{
		"glm-5.3": {Until: resetA, ResetAt: resetA, Reason: "6004 model rate limit"},
		"hy3-x":   {Until: resetB, ResetAt: resetB, Reason: "6004 model rate limit"},
	}
	p.mu.Unlock()

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("status missing")
	}
	if len(st.RateLimitedModels) != 2 {
		t.Fatalf("rate_limited_models=%+v want 2 行", st.RateLimitedModels)
	}
	wantModels := map[string]bool{"glm-5.3": true, "hy3-x": true}
	for _, row := range st.RateLimitedModels {
		if !wantModels[row.Model] {
			t.Errorf("unexpected row model=%q", row.Model)
		}
		delete(wantModels, row.Model)
	}
	if len(wantModels) != 0 {
		t.Errorf("缺行: %v", wantModels)
	}
}

// TestRateLimitedModelsMultiModelStableOutput 多模型台账行按模型名排序（稳定输出）。
func TestRateLimitedModelsMultiModelStableOutput(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	resetA := time.Now().Add(2 * time.Hour)
	resetB := time.Now().Add(1 * time.Hour)
	p.mu.Lock()
	e := p.byUID["u1"]
	e.modelCooldowns = map[string]modelCooldown{
		"hy3-x":   {Until: resetB, ResetAt: resetB, Reason: "r"},
		"glm-5.3": {Until: resetA, ResetAt: resetA, Reason: "r"},
	}
	p.mu.Unlock()
	st, _ := p.Status("u1")
	got := []string{st.RateLimitedModels[0].Model, st.RateLimitedModels[1].Model}
	want := []string{"glm-5.3", "hy3-x"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rows=%v want sorted %v", got, want)
	}
}

// TestRateLimitedModelsEachModelHasOwnUntil 台账行 Until = 该模型独立冷却截止，
// Status.Until（账号级）不受 6004 影响（无账号级冷却时为零值）。
func TestRateLimitedModelsEachModelHasOwnUntil(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	resetA := time.Now().Add(2 * time.Hour)
	resetB := time.Now().Add(1 * time.Hour)
	p.mu.Lock()
	e := p.byUID["u1"]
	e.modelCooldowns = map[string]modelCooldown{
		"glm-5.3": {Until: resetA, ResetAt: resetA, Reason: "r"},
		"hy3-x":   {Until: resetB, ResetAt: resetB, Reason: "r"},
	}
	p.mu.Unlock()
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("status missing")
	}
	if !st.Until.IsZero() {
		t.Fatalf("Status.Until 应零值（无账号级冷却），got %v", st.Until)
	}
	for _, row := range st.RateLimitedModels {
		want := resetA
		if row.Model == "hy3-x" {
			want = resetB
		}
		if d := row.Until.Sub(want); d < -time.Second || d > time.Second {
			t.Errorf("%s row.Until=%v want ~%v", row.Model, row.Until, want)
		}
	}
}

// TestModelCooldownsPickSkipsFrozenAccount 6004 冻结账户：所有模型都跳过该号。
func TestModelCooldownsPickSkipsFrozenAccount(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 1000, 0)
	p.SetCredits("u2", 1, 0)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "6004")
	if got := p.PickExcludingForModel(nil, "glm-5.3"); got == nil || got.UID != "u2" {
		t.Fatalf("glm-5.3 请求应跳过 u1, got %+v", got)
	}
	if got := p.PickExcludingForModel(nil, "hy3-x"); got == nil || got.UID != "u2" {
		t.Fatalf("hy3-x 请求也应跳过 u1（账户冻结，无豁免）, got %+v", got)
	}
}

// TestServableNow6004NotServable 6004 现在是账户级冷却 → 池不可服务。
func TestServableNow6004NotServable(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "6004")
	if p.ServableNow() {
		t.Fatal("6004 冻结账户，ServableNow 应 false")
	}
}

// ---------------------------------------------------------------------------
// healthyForModel 优先级（全账号级先判，模型级 6004 后判）
// ---------------------------------------------------------------------------

// TestHealthyForModelAccountCooledBeatsModelNotCooled 全账号冷却（until）优先于模型
// 独立冷却：账号级 until 未到期的账号，即使该模型没有 6004 独立冷却也不可选
// （旧实现先查 modelCooled 再查 healthy，逻辑上等价的短路位置不同；锁死新语义：
// 全账号冷却优先）。
func TestHealthyForModelAccountCooledBeatsModelNotCooled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolSoft, time.Hour, "429 rate limit") // 全账号级 until 冷却，无 modelCooldowns
	p.mu.RLock()
	e := p.byUID["u1"]
	p.mu.RUnlock()

	if e.healthyForModel(time.Now(), "glm-5.3") {
		t.Fatal("全账号 until 冷却中且模型无独立冷却：该模型也不可选（全账号冷却优先）")
	}
	if e.healthyForModel(time.Now(), "") {
		t.Fatal("空模型名同样不可选（等价 healthy 短路到全账号冷却）")
	}
}

// TestHealthyForModelAccountCooledWithModelCooldownStillBlocked 全账号冷却 + 该模型
// 也有 6004 独立冷却 → 不可选（无论哪条拦截都一致，优先级短路不误放行）。
func TestHealthyForModelAccountCooledWithModelCooldownStillBlocked(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolSoft, time.Hour, "429 rate limit")
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "6004")
	p.mu.RLock()
	e := p.byUID["u1"]
	p.mu.RUnlock()
	if e.healthyForModel(time.Now(), "glm-5.3") {
		t.Fatal("全账号冷却 + 模型独立冷却双拦截，仍应不可选")
	}
}

// TestHealthyForModelDisabledBeatsModelNotCooled disabled 是全账号级的最强冷却：
// 即使模型没有独立冷却也永不可选（disabled > 模型级）。
func TestHealthyForModelDisabledBeatsModelNotCooled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "session dead")
	p.mu.RLock()
	e := p.byUID["u1"]
	p.mu.RUnlock()
	if e.healthyForModel(time.Now(), "glm-5.3") {
		t.Fatal("disabled 账号即使模型无独立冷却也不可选")
	}
}

// TestHealthyForModelBreakerBeatsModelNotCooled 熔断（breakerUntil）是全账号级的
// 冷却：熔断期内即使模型没有独立冷却也不可选。
func TestHealthyForModelBreakerBeatsModelNotCooled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetBreaker(1, time.Hour, time.Hour)
	p.NoteError("u1") // 触发熔断
	p.mu.RLock()
	e := p.byUID["u1"]
	bt := e.breakerUntil
	p.mu.RUnlock()
	if bt.IsZero() {
		t.Fatal("precondition: breaker should be open")
	}
	if e.healthyForModel(time.Now(), "glm-5.3") {
		t.Fatal("熔断期内即使模型无独立冷却也不可选")
	}
}

// TestHealthyForModelHealthyAccountNoModelCooldown 全账号健康 + 无任何模型独立冷却 →
// 所有模型都可选（基线）。
func TestHealthyForModelHealthyAccountNoModelCooldown(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.mu.RLock()
	e := p.byUID["u1"]
	p.mu.RUnlock()
	for _, m := range []string{"glm-5.3", "hy3-x", ""} {
		if !e.healthyForModel(time.Now(), m) {
			t.Errorf("健康账号对模型 %q 应可选", m)
		}
	}
}

// TestHealthyForModelAccountCooledAllowsNothingAfterUntil 优先级锁死的另一端：
// 全账号 until 冷却到期后，模型无独立冷却的请求恢复正常（模型级判定接管）。
func TestHealthyForModelAccountCooledAllowsNothingAfterUntil(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolSoft, time.Millisecond, "429")
	p.mu.RLock()
	e := p.byUID["u1"]
	p.mu.RUnlock()
	// until 尚未到期：不可选（优先级：全账号级先判）。
	if e.healthyForModel(time.Now(), "glm-5.3") {
		t.Fatal("until 冷却内不可选")
	}
	// 等 until 过期：模型无独立冷却 → 恢复可选。
	time.Sleep(10 * time.Millisecond)
	if !e.healthyForModel(time.Now(), "glm-5.3") {
		t.Fatal("until 过期后应恢复可选")
	}
}

// TestHealthyForModelPriorityViaPick 6004 与账号级冷却同属账户级：两者都冻结账户，
// 对任何模型都不可选。
func TestHealthyForModelPriorityViaPick(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "cooled"})
	p.Add(&auth.Auth{UID: "exempt"})
	p.SetCredits("cooled", 100, 0)
	p.SetCredits("exempt", 50, 0)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Cooldown("cooled", CoolSoft, time.Hour, "429")
	p.CooldownSoftForModel("exempt", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "6004")

	// 两个账号都账户级冷却 → 无健康候选 → 兜底选最早到期（exempt，5m vs 1h）。
	if got := p.PickExcludingForModel(nil, "glm-5.3"); got == nil || got.UID != "exempt" {
		t.Fatalf("glm-5.3 请求兜底应选最早到期的 exempt, got %+v", got)
	}
}

// TestModelCooldownsPersist 6004（账户级 until）持久化到 state.json 并跨重启恢复。
func TestModelCooldownsPersist(t *testing.T) {
	dir := t.TempDir()
	fp := dir + "/state.json"
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "6004")
	p.Flush()
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "until") {
		t.Errorf("state.json 应持久化账户级 until: %s", raw)
	}
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	p2.mu.RLock()
	until := p2.byUID["u1"].until
	p2.mu.RUnlock()
	if until.IsZero() {
		t.Errorf("重载后 until 应恢复（账户级冷却持久化）")
	}
}

// TestModelCooldownsPersistCompatOldState 旧 state.json 无 modelCooldowns 字段正常加载
// （缺字段零值，模型冷却清零退化为账号级，向后兼容）。
func TestModelCooldownsPersistCompatOldState(t *testing.T) {
	dir := t.TempDir()
	fp := dir + "/state.json"
	old := `{"accounts":{"u1":{"credits":100,"until":"2099-01-01T00:00:00Z","cool_kind":1}}}`
	if err := os.WriteFile(fp, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.mu.RLock()
	n := len(p.byUID["u1"].modelCooldowns)
	until := p.byUID["u1"].until
	p.mu.RUnlock()
	if n != 0 {
		t.Errorf("旧文件加载后 modelCooldowns=%d want 0", n)
	}
	if until.IsZero() {
		t.Error("旧文件 until 应照常加载（账号级冷却兼容）")
	}
}
