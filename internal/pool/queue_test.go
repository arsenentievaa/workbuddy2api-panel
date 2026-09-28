package pool

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// qPool 构造 n 个健康 CN 账号的池（realm 缺省判 cn）。队列默认关停，由用例显式
// SetQueue 开启——保持“Pool 零值 = 既有行为”的可断言性。
func qPool(t *testing.T, n int) *Pool {
	t.Helper()
	p := New("")
	for i := 0; i < n; i++ {
		p.Add(&auth.Auth{UID: string(rune('a' + i))})
	}
	return p
}

// qWaitFor 轮询等待条件成立（避免用固定 sleep 造成 flaky）。超时即 Fail。
func qWaitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

// qWaitDeadline 返回一个从现在起 d 之后的等待截止（用例统一口径）。
func qWaitDeadline(d time.Duration) time.Time { return time.Now().Add(d) }

// TestQueueDisabledByDefault 队列零值关停：Pool 不替调用方决定策略。此时即使
// 「容量是唯一瓶颈」，WaitForSlot 也必须立刻返回 (false,nil) 让调用方回落既有语义。
func TestQueueDisabledByDefault(t *testing.T) {
	p := qPool(t, 1)
	p.SetMaxInFlight(1)
	if !p.Acquire("a") {
		t.Fatal("acquire")
	}
	// 有健康号、只是占满 → 容量是唯一瓶颈。
	if !p.WaitableForModel(nil, "", "cn") {
		t.Fatal("expected waitable: healthy account exists, only in-flight cap blocks it")
	}
	if got := p.QueueMaxWait(); got != 0 {
		t.Fatalf("QueueMaxWait=%v want 0 when disabled", got)
	}
	waited, err := p.WaitForSlot(context.Background(), nil, "", "cn", qWaitDeadline(time.Second))
	if waited || err != nil {
		t.Fatalf("disabled queue should return (false,nil), got (%v,%v)", waited, err)
	}
}

// TestQueueWaitsThenAdmitted 核心路径：占满 → 排队阻塞 → Release 唤醒 → 取得容量信号。
func TestQueueWaitsThenAdmitted(t *testing.T) {
	p := qPool(t, 1)
	p.SetMaxInFlight(1)
	p.SetQueue(QueueConfig{MaxWaiters: 10, MaxWait: 2 * time.Second})
	if !p.Acquire("a") {
		t.Fatal("acquire")
	}

	type res struct {
		waited bool
		err    error
	}
	done := make(chan res, 1)
	go func() {
		w, err := p.WaitForSlot(context.Background(), nil, "", "cn", qWaitDeadline(2*time.Second))
		done <- res{w, err}
	}()

	// 名额未释放前不得返回。
	select {
	case r := <-done:
		t.Fatalf("returned before release: (%v,%v)", r.waited, r.err)
	case <-time.After(150 * time.Millisecond):
	}
	qWaitFor(t, "waiter enqueued", func() bool { return p.QueueStats().Waiting == 1 })

	p.Release("a")
	select {
	case r := <-done:
		if !r.waited || r.err != nil {
			t.Fatalf("want (true,nil), got (%v,%v)", r.waited, r.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("not woken after Release")
	}

	st := p.QueueStats()
	if !st.Enabled {
		t.Fatal("Enabled=false want true")
	}
	if st.Episodes != 1 || st.Admitted != 1 || st.TimedOut != 0 || st.Rejected != 0 {
		t.Fatalf("stats=%+v want episodes=1 admitted=1 others=0", st)
	}
	if st.Waiting != 0 {
		t.Fatalf("waiting=%d want 0 after admit", st.Waiting)
	}
}

// TestQueueTimeout 等待预算耗尽 → ErrQueueTimeout，并计入 timed_out。
func TestQueueTimeout(t *testing.T) {
	p := qPool(t, 1)
	p.SetMaxInFlight(1)
	p.SetQueue(QueueConfig{MaxWaiters: 10, MaxWait: 200 * time.Millisecond})
	if !p.Acquire("a") {
		t.Fatal("acquire")
	}
	waited, err := p.WaitForSlot(context.Background(), nil, "", "cn", qWaitDeadline(200*time.Millisecond))
	if waited || !errors.Is(err, ErrQueueTimeout) {
		t.Fatalf("want (false,ErrQueueTimeout), got (%v,%v)", waited, err)
	}
	st := p.QueueStats()
	if st.TimedOut != 1 || st.Admitted != 0 || st.Waiting != 0 {
		t.Fatalf("stats=%+v want timed_out=1", st)
	}
}

// TestQueueBudgetSharedAcrossEpisodes deadline 是**绝对**截止：预算已过再调用必须
// 立刻超时，不得因为「每次排队各自计时」而无限延长总等待。
func TestQueueBudgetSharedAcrossEpisodes(t *testing.T) {
	p := qPool(t, 1)
	p.SetMaxInFlight(1)
	p.SetQueue(QueueConfig{MaxWaiters: 10, MaxWait: time.Minute})
	if !p.Acquire("a") {
		t.Fatal("acquire")
	}
	start := time.Now()
	waited, err := p.WaitForSlot(context.Background(), nil, "", "cn", time.Now().Add(-time.Millisecond))
	if waited || !errors.Is(err, ErrQueueTimeout) {
		t.Fatalf("expired deadline should time out immediately, got (%v,%v)", waited, err)
	}
	if el := time.Since(start); el > 200*time.Millisecond {
		t.Fatalf("expired deadline waited %v want immediate", el)
	}
}

// TestQueueFull 队深上限 = 背压：装满后新请求不入队，直接 ErrQueueFull。
func TestQueueFull(t *testing.T) {
	p := qPool(t, 1)
	p.SetMaxInFlight(1)
	p.SetQueue(QueueConfig{MaxWaiters: 1, MaxWait: 2 * time.Second})
	if !p.Acquire("a") {
		t.Fatal("acquire")
	}
	hold := make(chan struct{})
	go func() {
		_, _ = p.WaitForSlot(context.Background(), nil, "", "cn", qWaitDeadline(2*time.Second))
		close(hold)
	}()
	qWaitFor(t, "first waiter enqueued", func() bool { return p.QueueStats().Waiting == 1 })

	waited, err := p.WaitForSlot(context.Background(), nil, "", "cn", qWaitDeadline(time.Second))
	if waited || !errors.Is(err, ErrQueueFull) {
		t.Fatalf("want (false,ErrQueueFull), got (%v,%v)", waited, err)
	}
	if got := p.QueueStats().Rejected; got != 1 {
		t.Fatalf("rejected=%d want 1", got)
	}
	p.Release("a")
	<-hold
}

// TestQueueCtxCancel 客户端断连 → 退出等待，且**不计入** timed_out（客户端自己走了，
// 不是网关容量问题，混进超时计数会污染告警口径）。
func TestQueueCtxCancel(t *testing.T) {
	p := qPool(t, 1)
	p.SetMaxInFlight(1)
	p.SetQueue(QueueConfig{MaxWaiters: 10, MaxWait: 2 * time.Second})
	if !p.Acquire("a") {
		t.Fatal("acquire")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := p.WaitForSlot(ctx, nil, "", "cn", qWaitDeadline(2*time.Second))
		done <- err
	}()
	qWaitFor(t, "waiter enqueued", func() bool { return p.QueueStats().Waiting == 1 })
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not unwind the wait")
	}
	st := p.QueueStats()
	if st.TimedOut != 0 {
		t.Fatalf("ctx cancel must not count as timeout: %+v", st)
	}
	if st.Waiting != 0 {
		t.Fatalf("waiting=%d want 0 after cancel", st.Waiting)
	}
}

// TestQueueNotWaitableWithoutHealthy 真无健康号（CoolHard 余额耗尽）→ 不排队，
// 维持既有「冷却兜底 → 503」语义。这是「保护账号不被反复打 402」的关键边界。
func TestQueueNotWaitableWithoutHealthy(t *testing.T) {
	p := qPool(t, 1)
	p.SetMaxInFlight(1)
	p.SetQueue(QueueConfig{MaxWaiters: 10, MaxWait: time.Second})
	p.Cooldown("a", CoolHard, time.Hour, "insufficient balance")

	if p.WaitableForModel(nil, "", "cn") {
		t.Fatal("CoolHard account must not count as waitable capacity")
	}
	waited, err := p.WaitForSlot(context.Background(), nil, "", "cn", qWaitDeadline(time.Second))
	if waited || err != nil {
		t.Fatalf("want (false,nil) fallthrough, got (%v,%v)", waited, err)
	}
	if st := p.QueueStats(); st.Episodes != 0 {
		t.Fatalf("must not enqueue when nothing is healthy: %+v", st)
	}
}

// TestQueueTriedExcludedPreventsSpin tried 是请求级轮换集合，必须与 Pick 同口径传入：
// 若唯一健康号已被本请求 tried 排除，等待就是「唤醒-空转」活锁，必须判为不可等待。
func TestQueueTriedExcludedPreventsSpin(t *testing.T) {
	p := qPool(t, 1)
	p.SetMaxInFlight(1)
	p.SetQueue(QueueConfig{MaxWaiters: 10, MaxWait: time.Second})
	tried := map[string]bool{"a": true}

	if p.WaitableForModel(tried, "", "cn") {
		t.Fatal("tried-excluded-only pool must not be waitable (would livelock)")
	}
	waited, err := p.WaitForSlot(context.Background(), tried, "", "cn", qWaitDeadline(time.Second))
	if waited || err != nil {
		t.Fatalf("want (false,nil), got (%v,%v)", waited, err)
	}
}

// TestQueueRealmIsolation 域隔离：cn 请求不得被 global 账号的空闲名额唤醒。
func TestQueueRealmIsolation(t *testing.T) {
	p := New("")
	cn := &auth.Auth{UID: "cn1"}
	gl := &auth.Auth{UID: "gl1"}
	if _, err := auth.BackfillRealmFor(gl, "global"); err != nil {
		t.Fatalf("backfill global realm: %v", err)
	}
	p.Add(cn)
	p.Add(gl)
	p.SetMaxInFlight(1)
	p.SetQueue(QueueConfig{MaxWaiters: 10, MaxWait: 300 * time.Millisecond})
	if !p.Acquire("cn1") {
		t.Fatal("acquire cn1")
	}
	// cn 域被占满：global 的空闲不影响 cn 判定。
	if !p.WaitableForModel(nil, "", "cn") {
		t.Fatal("cn should be waitable")
	}
	if p.WaitableForModel(nil, "", "global") == false {
		t.Fatal("global has a free account, should be waitable")
	}
	// 排队 cn 必须在预算内超时（global 的名额与 cn 无关）。
	waited, err := p.WaitForSlot(context.Background(), nil, "", "cn", qWaitDeadline(300*time.Millisecond))
	if waited || !errors.Is(err, ErrQueueTimeout) {
		t.Fatalf("cn wait must not be satisfied by global capacity, got (%v,%v)", waited, err)
	}
}

// TestQueueWakesOnCapacityIncrease 上限调高 = 容量增加 → 主动唤醒（不必等活性 ticker）。
func TestQueueWakesOnCapacityIncrease(t *testing.T) {
	p := qPool(t, 1)
	p.SetMaxInFlight(1)
	p.SetQueue(QueueConfig{MaxWaiters: 10, MaxWait: 2 * time.Second})
	if !p.Acquire("a") {
		t.Fatal("acquire")
	}
	done := make(chan bool, 1)
	go func() {
		w, _ := p.WaitForSlot(context.Background(), nil, "", "cn", qWaitDeadline(2*time.Second))
		done <- w
	}()
	qWaitFor(t, "waiter enqueued", func() bool { return p.QueueStats().Waiting == 1 })

	p.SetMaxInFlight(2) // 容量 +1 → wakeAll
	select {
	case w := <-done:
		if !w {
			t.Fatal("want admitted after capacity increase")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("capacity increase did not wake the waiter")
	}
}

// TestQueueWakesOnAccountRevive 解冻/复活 = 容量增加 → 唤醒（余额恢复后等待者不必干等）。
func TestQueueWakesOnAccountRevive(t *testing.T) {
	p := qPool(t, 2)
	p.SetMaxInFlight(1)
	p.SetQueue(QueueConfig{MaxWaiters: 10, MaxWait: 2 * time.Second})
	p.Disable("b", "test")
	if !p.Acquire("a") {
		t.Fatal("acquire a")
	}
	done := make(chan bool, 1)
	go func() {
		w, _ := p.WaitForSlot(context.Background(), nil, "", "cn", qWaitDeadline(2*time.Second))
		done <- w
	}()
	qWaitFor(t, "waiter enqueued", func() bool { return p.QueueStats().Waiting == 1 })

	p.Revive("b") // b 恢复健康且空闲 → 可服务容量出现
	select {
	case w := <-done:
		if !w {
			t.Fatal("want admitted after revive")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("revive did not wake the waiter")
	}
}

// TestQueueWakeOnDisable Mid-wait 关停队列（=0）→ 等待者立刻以 (false,nil) 退出，
// 不等满预算。这是运维一键回滚的热路径（面板保存即生效）。
func TestQueueWakeOnDisable(t *testing.T) {
	p := qPool(t, 1)
	p.SetMaxInFlight(1)
	p.SetQueue(QueueConfig{MaxWaiters: 10, MaxWait: 3 * time.Second})
	if !p.Acquire("a") {
		t.Fatal("acquire")
	}
	type res struct {
		waited bool
		err    error
	}
	done := make(chan res, 1)
	go func() {
		w, err := p.WaitForSlot(context.Background(), nil, "", "cn", qWaitDeadline(3*time.Second))
		done <- res{w, err}
	}()
	qWaitFor(t, "waiter enqueued", func() bool { return p.QueueStats().Waiting == 1 })

	start := time.Now()
	p.SetQueue(QueueConfig{MaxWaiters: 0, MaxWait: 0}) // 关停
	select {
	case r := <-done:
		if r.waited || r.err != nil {
			t.Fatalf("disabled mid-wait should fall through with (false,nil), got (%v,%v)", r.waited, r.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("disable did not release the waiter")
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("disable took %v to release waiter; want prompt", el)
	}
	if p.QueueStats().Enabled {
		t.Fatal("Enabled should be false after disable")
	}
}

// TestQueueNoWaiterLeak 反复超时/取消后等待表必须清空（惰性摘除真正生效，不随历史增长）。
func TestQueueNoWaiterLeak(t *testing.T) {
	p := qPool(t, 1)
	p.SetMaxInFlight(1)
	p.SetQueue(QueueConfig{MaxWaiters: 5, MaxWait: 20 * time.Millisecond})
	if !p.Acquire("a") {
		t.Fatal("acquire")
	}
	for i := 0; i < 5; i++ {
		if _, err := p.WaitForSlot(context.Background(), nil, "", "cn", qWaitDeadline(20*time.Millisecond)); !errors.Is(err, ErrQueueTimeout) {
			t.Fatalf("iter %d: err=%v want ErrQueueTimeout", i, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.WaitForSlot(ctx, nil, "", "cn", qWaitDeadline(time.Second)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel err=%v", err)
	}

	p.mu.RLock()
	left := len(p.waiters)
	p.mu.RUnlock()
	if left != 0 {
		t.Fatalf("waiters map leaked %d realm entries: %+v", left, p.waiters)
	}
	if st := p.QueueStats(); st.Waiting != 0 {
		t.Fatalf("stats.Waiting=%d want 0", st.Waiting)
	}
}

// TestQueueWakesOnCooldownExpiry 时间驱动的唤醒：没有 Release，靠冷却自然到期。
// 定时器必须精确设在到期点（醒来晚于到期，而不是靠周期轮询碰运气）——这是
// 「不轮询」的实现契约，也是数百账号下 CPU 不被烧掉的原因。
func TestQueueWakesOnCooldownExpiry(t *testing.T) {
	p := qPool(t, 2) // a 健康、b 冷却
	p.SetMaxInFlight(1)
	p.SetQueue(QueueConfig{MaxWaiters: 10, MaxWait: 3 * time.Second})
	p.Cooldown("b", CoolSoft, 150*time.Millisecond, "short soft cooldown")
	if !p.Acquire("a") {
		t.Fatal("acquire a")
	}
	done := make(chan bool, 1)
	go func() {
		w, _ := p.WaitForSlot(context.Background(), nil, "", "cn", qWaitDeadline(3*time.Second))
		done <- w
	}()
	qWaitFor(t, "waiter enqueued", func() bool { return p.QueueStats().Waiting == 1 })

	start := time.Now()
	select {
	case w := <-done:
		if !w {
			t.Fatal("want admitted once the cooldown expired")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cooldown expiry did not release the waiter (timer path broken)")
	}
	if el := time.Since(start); el < 100*time.Millisecond {
		t.Fatalf("woke after %v, before the 150ms cooldown expired — timer is not set to the expiry", el)
	}
}

// TestQueueWakesOnAccountAdded 新增账号 = 容量增加 → 广播唤醒（「账号会不断加进来」的
// 核心契约：不必重启、不必等预算耗尽，新号上线即在等待者视野内）。
func TestQueueWakesOnAccountAdded(t *testing.T) {
	p := qPool(t, 1)
	p.SetMaxInFlight(1)
	p.SetQueue(QueueConfig{MaxWaiters: 10, MaxWait: 2 * time.Second})
	if !p.Acquire("a") {
		t.Fatal("acquire")
	}
	done := make(chan bool, 1)
	go func() {
		w, _ := p.WaitForSlot(context.Background(), nil, "", "cn", qWaitDeadline(2*time.Second))
		done <- w
	}()
	qWaitFor(t, "waiter enqueued", func() bool { return p.QueueStats().Waiting == 1 })

	p.Add(&auth.Auth{UID: "b"})
	select {
	case w := <-done:
		if !w {
			t.Fatal("want admitted after a new account was added")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("account addition did not wake the waiter")
	}
}

// BenchmarkQueueDecision 队列裁决的单次成本随账号数变化。用来回答「上百/上千账号时
// 排队逻辑还撑得住吗」。
//
// 两个子基准都刻意构造成**全表扫描**（不提前返回）——这才是等待者的真实成本：
//
//	servable_all_full  所有账号占满 → servableForModelLocked 扫完全表返回 false
//	                   （每次唤醒/冷却到期复核走这条）
//	waitable_none_alive 所有账号都在冷却 → waitableForModelLocked 扫完全表返回 false
//
// 注意 WaitableForModel 在「有账号只是占满」时会命中第一个健康号就 true 返回，
// 那是最好情况、不是队列的瓶颈；不要用那个数字代表规模成本。
func BenchmarkQueueDecision(b *testing.B) {
	for _, n := range []int{100, 500, 2000} {
		b.Run(fmt.Sprintf("all_full/accounts=%d", n), func(b *testing.B) {
			p := New("")
			for i := 0; i < n; i++ {
				p.Add(&auth.Auth{UID: strconv.Itoa(i)})
			}
			p.SetMaxInFlight(1)
			for i := 0; i < n; i++ {
				p.Acquire(strconv.Itoa(i)) // 全部占满
			}
			now := time.Now()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				p.mu.RLock()
				if p.servableForModelLocked(nil, "deepseek-v4.1-flash", "cn", now) {
					b.Fatal("no account should be servable")
				}
				p.mu.RUnlock()
			}
		})
		b.Run(fmt.Sprintf("none_healthy/accounts=%d", n), func(b *testing.B) {
			p := New("")
			for i := 0; i < n; i++ {
				uid := strconv.Itoa(i)
				p.Add(&auth.Auth{UID: uid})
				p.Cooldown(uid, CoolSoft, time.Hour, "bench")
			}
			now := time.Now()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				p.mu.RLock()
				if p.waitableForModelLocked(nil, "deepseek-v4.1-flash", "cn", now) {
					b.Fatal("no account should be waitable")
				}
				p.mu.RUnlock()
			}
		})
		// 对照：正常路径的选号成本（每个请求都要付，与排队无关）。队列只在饱和时
		// 额外加一次同阶扫描，量级不变——这是「排队不会拖慢正常请求」的依据。
		b.Run(fmt.Sprintf("pick_all_free/accounts=%d", n), func(b *testing.B) {
			p := New("")
			for i := 0; i < n; i++ {
				uid := strconv.Itoa(i)
				p.Add(&auth.Auth{UID: uid})
				p.SetCredits(uid, 1000, 0)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if p.Pick() == nil {
					b.Fatal("want an account")
				}
			}
		})
	}
}

// TestQueueStatsMaxWaiters 观测字段与配置一致（面板/告警据此判断是否开了排队）。
func TestQueueStatsMaxWaiters(t *testing.T) {
	p := qPool(t, 1)
	p.SetQueue(QueueConfig{MaxWaiters: 42, MaxWait: 1500 * time.Millisecond})
	st := p.QueueStats()
	if !st.Enabled || st.MaxWaiters != 42 || st.MaxWaitMS != 1500 {
		t.Fatalf("stats=%+v want enabled max_waiters=42 max_wait_ms=1500", st)
	}
}
