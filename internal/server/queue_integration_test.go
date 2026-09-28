package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 饱和排队的端到端测试（queue.go + handler 接线）。全部走真实 HTTP 入口，断言
// 「占满在途」时不再立刻 503，而是排队等名额并在释放后被正常服务。

const qStreamBody = `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`

// qHandler 起一个「上游恒成功」的处理器 + 单账号满员池。
// 返回的 release 用于手动腾出名额（模拟并发请求结束）。
func qHandler(t *testing.T, p *pool.Pool) *Handler {
	t.Helper()
	fake := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	return NewHandler(Config{Pool: p, Upstream: fake})
}

// qFire 发起一次流式 chat 请求，结果通过 channel 回传（便于断言「此刻还没返回」）。
type qResult struct {
	code int
	body string
}

func qFire(h *Handler) <-chan qResult {
	ch := make(chan qResult, 1)
	go func() {
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(qStreamBody))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		ch <- qResult{rec.Code, rec.Body.String()}
	}()
	return ch
}

// qWaitPool 轮询等待池条件（避免固定 sleep 造成 flake）。
func qWaitPool(t *testing.T, what string, cond func() bool) {
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

// TestChatQueuesWhenSaturated 主路径：健康号占满 → 请求排队（不 503）→ 名额释放 →
// 正常 200 服务。断言 admitted 计数证明走的是排队路径而非兜底/直通。
func TestChatQueuesWhenSaturated(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetMaxInFlight(1)
	p.SetQueue(pool.QueueConfig{MaxWaiters: 10, MaxWait: 3 * time.Second})
	if !p.Acquire("u1") { // 手动占满（相当于另一个并发请求在途）
		t.Fatal("acquire")
	}
	h := qHandler(t, p)

	ch := qFire(h)
	select {
	case r := <-ch:
		t.Fatalf("saturated pool returned immediately (%d, %s) — queue not used", r.code, r.body)
	case <-time.After(300 * time.Millisecond):
	}
	qWaitPool(t, "request enqueued", func() bool { return p.QueueStats().Waiting == 1 })

	p.Release("u1")
	select {
	case r := <-ch:
		if r.code != 200 {
			t.Fatalf("after release: code=%d body=%s want 200", r.code, r.body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queued request never resumed after slot release")
	}
	if st := p.QueueStats(); st.Admitted != 1 || st.TimedOut != 0 {
		t.Fatalf("stats=%+v want admitted=1 timed_out=0", st)
	}
}

// TestChatQueueTimeoutReturns503 等待预算耗尽 → 503 + 专属 code，且不与既有
// no_healthy_account 混淆（运维据此区分「没号」与「排队太久」）。
func TestChatQueueTimeoutReturns503(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetMaxInFlight(1)
	p.SetQueue(pool.QueueConfig{MaxWaiters: 10, MaxWait: 150 * time.Millisecond})
	if !p.Acquire("u1") {
		t.Fatal("acquire")
	}
	h := qHandler(t, p)

	select {
	case r := <-qFire(h):
		if r.code != http.StatusServiceUnavailable {
			t.Fatalf("code=%d want 503 body=%s", r.code, r.body)
		}
		if !strings.Contains(r.body, "queue_timeout") {
			t.Fatalf("body=%s want queue_timeout code", r.body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queue timeout did not return")
	}
	if st := p.QueueStats(); st.TimedOut != 1 {
		t.Fatalf("stats=%+v want timed_out=1", st)
	}
}

// TestChatQueueFullReturns503 队深打满 = 背压：立刻 503 + queue_full（不无限堆积）。
func TestChatQueueFullReturns503(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetMaxInFlight(1)
	p.SetQueue(pool.QueueConfig{MaxWaiters: 1, MaxWait: 2 * time.Second})
	if !p.Acquire("u1") {
		t.Fatal("acquire")
	}
	// 占住唯一的等待位。
	go func() {
		_, _ = p.WaitForSlot(context.Background(), nil, "", "cn", time.Now().Add(2*time.Second))
	}()
	qWaitPool(t, "seat holder enqueued", func() bool { return p.QueueStats().Waiting == 1 })

	h := qHandler(t, p)
	select {
	case r := <-qFire(h):
		if r.code != http.StatusServiceUnavailable {
			t.Fatalf("code=%d want 503 body=%s", r.code, r.body)
		}
		if !strings.Contains(r.body, "queue_full") {
			t.Fatalf("body=%s want queue_full code", r.body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queue_full did not return")
	}
	if st := p.QueueStats(); st.Rejected != 1 {
		t.Fatalf("stats=%+v want rejected=1", st)
	}
}

// TestChatQueueDisabledFailsFast 队列关停（0）= 既有行为：立刻 503，不入队。
// 这是运维一键回滚路径的可断言保证。
func TestChatQueueDisabledFailsFast(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetMaxInFlight(1)
	// 不调 SetQueue：Pool 零值 = 关停。
	if !p.Acquire("u1") {
		t.Fatal("acquire")
	}
	h := qHandler(t, p)

	start := time.Now()
	select {
	case r := <-qFire(h):
		if r.code != http.StatusServiceUnavailable {
			t.Fatalf("code=%d want 503", r.code)
		}
		if strings.Contains(r.body, "queue_") {
			t.Fatalf("disabled queue must not emit queue codes: %s", r.body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("disabled queue did not fail fast")
	}
	if el := time.Since(start); el > 500*time.Millisecond {
		t.Fatalf("disabled queue took %v; want fast fail", el)
	}
	if st := p.QueueStats(); st.Episodes != 0 || st.Waiting != 0 {
		t.Fatalf("disabled queue must not enqueue: %+v", st)
	}
}

// TestChatNoHealthyDoesNotQueue CoolHard（余额耗尽）号不参与排队：真无健康号时
// 立即 503，不占等待位、不空转（保护账号不被反复打 402）。
func TestChatNoHealthyDoesNotQueue(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetMaxInFlight(1)
	p.SetQueue(pool.QueueConfig{MaxWaiters: 10, MaxWait: 2 * time.Second})
	p.Cooldown("u1", pool.CoolHard, time.Hour, "insufficient balance")
	h := qHandler(t, p)

	select {
	case r := <-qFire(h):
		if r.code != http.StatusServiceUnavailable {
			t.Fatalf("code=%d want 503", r.code)
		}
		if strings.Contains(r.body, "queue_") {
			t.Fatalf("no healthy account must not produce queue codes: %s", r.body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no-healthy path did not return")
	}
	if st := p.QueueStats(); st.Episodes != 0 {
		t.Fatalf("must not enqueue when nothing is healthy: %+v", st)
	}
}

// TestChatSaturationPrefersQueueOverCooldownFallback 语义核心：健康号只是被占满、
// 同时另有软冷却号可被兜底选中时，**必须排队等健康号**，而不是把请求推给正在
// 冷却（= 刚被限流）的号。对照组：队列关停时回落旧行为（立刻用兜底号）。
func TestChatSaturationPrefersQueueOverCooldownFallback(t *testing.T) {
	setup := func(t *testing.T, queueEnabled bool) (*pool.Pool, *Handler) {
		t.Helper()
		p := testPoolWith(
			&auth.Auth{UID: "hot", AccessToken: "at", ExpiresAt: 9999999999},
			&auth.Auth{UID: "cool", AccessToken: "at", ExpiresAt: 9999999999},
		)
		p.SetMaxInFlight(1)
		p.Cooldown("cool", pool.CoolSoft, time.Hour, "soft rate limited")
		if queueEnabled {
			p.SetQueue(pool.QueueConfig{MaxWaiters: 10, MaxWait: 3 * time.Second})
		}
		if !p.Acquire("hot") { // 唯一的健康号被占满
			t.Fatal("acquire hot")
		}
		return p, qHandler(t, p)
	}

	t.Run("queue_enabled_waits", func(t *testing.T) {
		p, h := setup(t, true)
		ch := qFire(h)
		select {
		case r := <-ch:
			t.Fatalf("should have queued instead of dispatching the cooling account: (%d, %s)", r.code, r.body)
		case <-time.After(300 * time.Millisecond):
		}
		qWaitPool(t, "request enqueued", func() bool { return p.QueueStats().Waiting == 1 })
		p.Release("hot")
		select {
		case r := <-ch:
			if r.code != 200 {
				t.Fatalf("code=%d body=%s want 200", r.code, r.body)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("queued request never resumed")
		}
		if st := p.QueueStats(); st.Admitted != 1 {
			t.Fatalf("stats=%+v want admitted=1", st)
		}
	})

	t.Run("queue_disabled_falls_back", func(t *testing.T) {
		p, h := setup(t, false)
		select {
		case r := <-qFire(h):
			// 旧行为：立刻用冷却兜底号（假上游恒成功 → 200）。关键断言是「没排队」。
			if r.code != 200 {
				t.Fatalf("control: code=%d body=%s want 200 (fallback dispatched)", r.code, r.body)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("control case did not return")
		}
		if st := p.QueueStats(); st.Episodes != 0 {
			t.Fatalf("control: queue disabled must not enqueue: %+v", st)
		}
	})
}

// TestStatusExposesQueue /status 透出队列运行态（waiting / 计数 / 上限），
// 让运维能分辨「没号」与「只是占满」。
func TestStatusExposesQueue(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetQueue(pool.QueueConfig{MaxWaiters: 7, MaxWait: 2500 * time.Millisecond})
	h := NewHandler(Config{Pool: p, Upstream: upstreamFakeOK(t)})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`"queue"`, `"enabled":true`, `"max_waiters":7`, `"max_wait_ms":2500`} {
		if !strings.Contains(body, want) {
			t.Fatalf("status body missing %s: %s", want, body)
		}
	}
}

// TestQueueScalesWithAccountCount 「账号会加到数百个」的可扩展性契约：
//   - 名额上限随账号数线性增长：只要还有**任意一个**账号空闲，请求就直连它、绝不排队；
//   - 只有全部占满（= 没有任何账号能接收）才进队列，且新号上线/名额释放能被唤醒。
func TestQueueScalesWithAccountCount(t *testing.T) {
	const n = 400
	auths := make([]*auth.Auth, 0, n)
	for i := 0; i < n; i++ {
		auths = append(auths, &auth.Auth{UID: fmt.Sprintf("u%03d", i), AccessToken: "at", ExpiresAt: 9999999999})
	}
	p := testPoolWith(auths...)
	p.SetMaxInFlight(1) // 400 账号 × 1 = 400 个名额
	p.SetQueue(pool.QueueConfig{MaxWaiters: 50, MaxWait: 2 * time.Second})
	h := qHandler(t, p)

	// 占满 399 个，留 1 个空闲 → 必须直连，不排队。
	for i := 0; i < n-1; i++ {
		if !p.Acquire(fmt.Sprintf("u%03d", i)) {
			t.Fatalf("acquire u%03d", i)
		}
	}
	start := time.Now()
	select {
	case r := <-qFire(h):
		if r.code != 200 {
			t.Fatalf("with a free account: code=%d body=%s want 200", r.code, r.body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("an account was free but the request was not served")
	}
	if el := time.Since(start); el > 500*time.Millisecond {
		t.Fatalf("took %v with a free account; must be direct (no queue)", el)
	}
	if st := p.QueueStats(); st.Episodes != 0 {
		t.Fatalf("queued although an account was free: %+v", st)
	}

	// 占满最后一个 → 此刻真无账号可用 → 必须排队（而不是 503）。
	if !p.Acquire(fmt.Sprintf("u%03d", n-1)) {
		t.Fatal("acquire last account")
	}
	ch := qFire(h)
	select {
	case r := <-ch:
		t.Fatalf("all %d accounts busy: returned (%d,%s) instead of queueing", n, r.code, r.body)
	case <-time.After(300 * time.Millisecond):
	}
	qWaitPool(t, "request enqueued at scale", func() bool { return p.QueueStats().Waiting == 1 })

	p.Release("u000")
	select {
	case r := <-ch:
		if r.code != 200 {
			t.Fatalf("after release: code=%d body=%s want 200", r.code, r.body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queued request never resumed at scale")
	}
	if st := p.QueueStats(); st.Admitted != 1 {
		t.Fatalf("stats=%+v want admitted=1", st)
	}
}

// upstreamFakeOK 供只读端点测试用的恒成功假上游。
func upstreamFakeOK(t *testing.T) *upstream.Client {
	t.Helper()
	return newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
}
