// queue.go 饱和排队：当「存在健康账号、但都被在途上限占满」时，请求进入有界 FIFO
// 队列等待名额释放，而不是立刻 503，也不落到 pickEarliestExpiryLocked 的冷却兜底
// ——兜底会把刚被限流/熔断的号再打一遍，正是排队要避免的事（WorkBuddy 的限流
// 一旦触发持续时间很长，把请求推给冷却号既救不了请求又加重封禁风险）。
//
// 语义边界（与既有调度互不覆盖，零回归）：
//   - 只有「容量是唯一瓶颈」才排队：存在健康（含 6004 模型豁免口径）、非禁用、
//     且不在请求级 tried 集合内的账号。真正无健康号（全冷却/全禁用/全 CoolHard）
//     时 WaitForSlot 返回 (false, nil)，调用方维持既有「冷却兜底 → 503」语义。
//   - 等待有硬上限：max_wait（单请求总预算，跨多次排队累计）与 max_waiters（队深）。
//     两者任一 <= 0 = 队列关停，WaitForSlot 立即返回 (false, nil)，调用方回落既有
//     行为——这是运维一键回滚开关（面板热改，见 SetQueue）。
//   - 唤醒是事件驱动，且**不轮询**：Release（名额释放）唤醒该域队首；容量批量增加
//     （账号新增/解冻/上限调高）广播唤醒。纯「占满」时时间不会改变任何事，等待者
//     只阻塞在 channel 上（零 CPU）。存在冷却/熔断/模型级限流的账号时，定时器被精确
//     设在**下一个冷却到期时刻**（nextCapacityAtLocked 取最早者），到期复核一次。
//     这样每个等待者每秒都不需要扫描整个池——「数百账号」下这才是可扩展的做法。
//
// 规模假设（数百账号）：本文件的判定不在每次请求上做全池扫描，只在「已在等待」时做
// 一次 O(账号数) 复核（与 pick 自身的 O(账号数) 同阶），且复核只在被唤醒或冷却到期
// 时发生（不是每秒）。名额上限天然随账号数线性增长（账号数 × max_in_flight），
// 新增账号会被 Add/SyncToDir 的事件立刻纳入，无需重启或改配置。
//
// 已知取舍（有意为之，不是缺陷）：
//   - 不抢占：等待者被唤醒后仍需自己 Pick，此刻新到的请求也可能先抢到刚释放的名额
//     （窗口 = Release 与唤醒 goroutine 实际运行之间的几微秒）。极端饱和下个别
//     等待者可能多等几轮。换取的收益是不必改造 pick 的选号路径（零回归风险）。
//   - 唤醒即空转：servableForModelLocked 用与 Pick 同一套谓词（tried + realm +
//     模型口径 + inFlightFull），保证「被唤醒 ⇒ Pick 必能选出号」，不会活锁。
package pool

import (
	"context"
	"errors"
	"time"
)

// 队列错误。三者均属「本地调度类」错误（非上游错误），调用方据此回 503 并带自己的
// hint；不得把上游原文混进来（本地没有原文，见 handler 末端透传注释）。
var (
	// ErrQueueFull 队深已达上限：拒绝入队（背压）。等待者已满时再堆积只会把延迟
	// 推向无穷，不如立刻失败让上游（NewAPI）去重试/降级。
	ErrQueueFull = errors.New("pool: wait queue full")
	// ErrQueueTimeout 等待超出预算仍未取得容量。
	ErrQueueTimeout = errors.New("pool: wait queue timeout")
)

// waiter 队列表项。ch 缓冲 1：唤醒方非阻塞投递，投递失败说明该 waiter 已有未消费的
// 唤醒（无需重复投递）。gone 标记退出，表项惰性摘除。
type waiter struct {
	ch    chan struct{}
	realm string
	start time.Time
	gone  bool
}

// QueueConfig 队列参数（config 注入；见 SetQueue）。
type QueueConfig struct {
	MaxWaiters int           // 队深上限；<= 0 = 关停
	MaxWait    time.Duration // 单请求等待预算；<= 0 = 关停
}

// QueueStats 队列运行态快照（/status 观测用；只读，不参与调度决策）。
type QueueStats struct {
	Enabled        bool           `json:"enabled"`
	Waiting        int            `json:"waiting"`
	WaitingByRealm map[string]int `json:"waiting_by_realm"`
	MaxWaiters     int            `json:"max_waiters"`
	MaxWaitMS      int64          `json:"max_wait_ms"`
	OldestWaitMS   int64          `json:"oldest_wait_ms"`
	Episodes       int64          `json:"episodes"`  // 累计入队次数
	Admitted       int64          `json:"admitted"`  // 入队后确实等到容量的次数
	TimedOut       int64          `json:"timed_out"` // 等待超预算
	Rejected       int64          `json:"rejected"`  // 队满未入队
	AvgWaitMS      int64          `json:"avg_wait_ms"`
}

// SetQueue 注入队列参数（main 从 config 解析后调用，支持面板热改）。
// 与 SetCostExploreInterval 同风格：0 是**合法值**（关停），不做「非正值保留默认」
// ——不设 0 语义就无法一键回滚。关停时唤醒全部在等待者，使它们立刻复核并以
// (false, nil) 退出，不等满预算。
func (p *Pool) SetQueue(cfg QueueConfig) {
	if cfg.MaxWaiters < 0 {
		cfg.MaxWaiters = 0
	}
	if cfg.MaxWait < 0 {
		cfg.MaxWait = 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.queueMaxWaiters == cfg.MaxWaiters && p.queueMaxWait == cfg.MaxWait {
		return
	}
	p.queueMaxWaiters = cfg.MaxWaiters
	p.queueMaxWait = cfg.MaxWait
	// 参数变化（含关停）：唤醒全部等待者重新复核。关停后它们会在复核点看到
	// enabled=false 并以 (false, nil) 退出（见 WaitForSlot 循环）。
	p.wakeAllLocked()
}

// QueueMaxWait 报告当前等待预算（handler 用它算单请求的 deadline）。队列关停时返回 0。
func (p *Pool) QueueMaxWait() time.Duration {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.queueMaxWaiters <= 0 {
		return 0
	}
	return p.queueMaxWait
}

// QueueStats 返回队列运行态快照。
func (p *Pool) QueueStats() QueueStats {
	p.mu.RLock()
	defer p.mu.RUnlock()
	st := QueueStats{
		Enabled:        p.queueMaxWaiters > 0 && p.queueMaxWait > 0,
		WaitingByRealm: map[string]int{},
		MaxWaiters:     p.queueMaxWaiters,
		MaxWaitMS:      p.queueMaxWait.Milliseconds(),
		Episodes:       p.queueEpisodes,
		Admitted:       p.queueAdmitted,
		TimedOut:       p.queueTimedOut,
		Rejected:       p.queueRejected,
	}
	if p.queueAdmitted > 0 {
		st.AvgWaitMS = (p.queueWaitTotal / time.Duration(p.queueAdmitted)).Milliseconds()
	}
	now := time.Now()
	for realm, ws := range p.waiters {
		for _, w := range ws {
			if w.gone {
				continue
			}
			st.Waiting++
			st.WaitingByRealm[realm]++
			if age := now.Sub(w.start); age.Milliseconds() > st.OldestWaitMS {
				st.OldestWaitMS = age.Milliseconds()
			}
		}
	}
	return st
}

// WaitableForModel 报告排队是否**适用**：容量是唯一瓶颈（存在健康号，只是没名额）。
// 供 handler 在 Pick 返回 nil 时区分「该排队」与「该走既有兜底/503」。只看不看
// inFlightFull——占满正是排队的理由。
//
// tried 必须传入（与 Pick 同一集合）：否则「唯一空闲号已被本请求 tried」会造成
// 唤醒-空转活锁（等待判定为可等待，Pick 却因 tried 排除而恒 nil）。
func (p *Pool) WaitableForModel(tried map[string]bool, reqModel, realm string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.waitableForModelLocked(tried, reqModel, realm, time.Now())
}

// WaitForSlot 在有界 FIFO 队列中等待容量。返回值语义：
//
//	(true, nil)  → 等待期间观测到可服务容量，调用方应立即重试选号
//	(false, nil) → 不适用等待（队列关停 / 无健康号 / tried 已排除全部）→ 调用方
//	               维持既有语义（冷却兜底或 503），不得当作错误
//	(false, err) → ErrQueueFull / ErrQueueTimeout / ctx.Err()
//
// deadline 是**本请求的绝对等待截止**（跨多次排队累计），由调用方算一次后传入；
// 这样即使被反复唤醒-重试，总等待也不会超过预算。
func (p *Pool) WaitForSlot(ctx context.Context, tried map[string]bool, reqModel, realm string, deadline time.Time) (bool, error) {
	now := time.Now()

	p.mu.Lock()
	if p.queueMaxWaiters <= 0 || p.queueMaxWait <= 0 {
		p.mu.Unlock()
		return false, nil // 关停：走既有语义
	}
	if !p.waitableForModelLocked(tried, reqModel, realm, now) {
		p.mu.Unlock()
		return false, nil // 无健康号 → 不是容量问题
	}
	if p.liveWaitersLocked(realm) >= p.queueMaxWaiters {
		p.queueRejected++
		p.mu.Unlock()
		return false, ErrQueueFull
	}
	w := &waiter{ch: make(chan struct{}, 1), realm: realm, start: now}
	p.waiters[realm] = append(p.waiters[realm], w)
	p.queueEpisodes++
	p.mu.Unlock()

	// 先扣预算再入等待：budget<=0 说明预算已耗尽（上一次排队就用光了），直接超时。
	budget := time.Until(deadline)
	if budget <= 0 {
		p.finishWaiter(w, &p.queueTimedOut, 0)
		return false, ErrQueueTimeout
	}
	budgetTimer := time.NewTimer(budget)
	defer budgetTimer.Stop()

	for {
		// 复核（入口 + 每次唤醒/到期后）：关停或已退出 → 走既有语义；有名额 →
		// 取得「可以再去 Pick」的信号。这是信号不是名额本身，真名额仍由调用方
		// 随后的 Acquire 原子占用。
		p.mu.Lock()
		if w.gone || p.queueMaxWaiters <= 0 || p.queueMaxWait <= 0 {
			p.mu.Unlock()
			p.finishWaiter(w, nil, 0)
			return false, nil
		}
		if p.servableForModelLocked(tried, reqModel, realm, time.Now()) {
			p.mu.Unlock()
			p.finishWaiter(w, &p.queueAdmitted, time.Since(w.start))
			return true, nil
		}
		// 时间能否改变现状？能则把定时器设在最早的那个冷却到期点；不能（全是占满）
		// 则零值 → 只等事件唤醒，不设周期轮询（数百账号下的可扩展性关键）。
		nextAt := p.nextCapacityAtLocked(tried, reqModel, realm, time.Now())
		p.mu.Unlock()

		var nextC <-chan time.Time
		var nextTimer *time.Timer
		if !nextAt.IsZero() {
			d := time.Until(nextAt)
			if d < 0 {
				d = 0
			}
			nextTimer = time.NewTimer(d)
			nextC = nextTimer.C
		}

		select {
		case <-w.ch:
			// Release 或容量变化唤醒。
		case <-nextC:
			// 某个冷却/熔断/模型级限流到期：复核一次。
		case <-budgetTimer.C:
			p.finishWaiter(w, &p.queueTimedOut, 0)
			return false, ErrQueueTimeout
		case <-ctx.Done():
			p.finishWaiter(w, nil, 0)
			return false, ctx.Err()
		}
		if nextTimer != nil {
			nextTimer.Stop()
		}
	}
}

// nextCapacityAtLocked 返回「下一个可能因时间推移而出现可用名额」的时刻；零值 =
// 时间不会改变现状（只能靠 Release / 容量事件，如账号新增、解冻、上限调高）。
//
// 某账号「对本模型可用」的时刻 = max(until, breakerUntil, degradeUntil,
// modelCooldowns[reqModel].Until)——因为 healthyForModel 要求这些截止**全部**已过。
//
//	已禁用        → 跳过（只能靠人工复活，那是事件不是时间）
//	被 tried 排除 → 跳过（本请求不会再选它）
//	realm 不符    → 跳过（域隔离）
//	时刻已过      → 跳过（说明当前不是时间在拦它，可能只是占满）
//
// 调用方需持锁。每个账号只做常数次比较，不分配内存。
func (p *Pool) nextCapacityAtLocked(tried map[string]bool, reqModel, realm string, now time.Time) time.Time {
	var next time.Time
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if e.disabled {
			continue
		}
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		ready := e.until
		if e.breakerUntil.After(ready) {
			ready = e.breakerUntil
		}
		if e.degradeUntil.After(ready) {
			ready = e.degradeUntil
		}
		if reqModel != "" {
			if mc, ok := e.modelCooldowns[reqModel]; ok && mc.Until.After(ready) {
				ready = mc.Until
			}
		}
		if !ready.After(now) {
			continue
		}
		if next.IsZero() || ready.Before(next) {
			next = ready
		}
	}
	return next
}

// waitableForModelLocked 报告「容量是唯一瓶颈」：存在健康（模型口径，含 6004 豁免）、
// 非禁用、realm 匹配、未在 tried 内的账号。不看 inFlightFull。调用方需持锁。
func (p *Pool) waitableForModelLocked(tried map[string]bool, reqModel, realm string, now time.Time) bool {
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if e.healthyForModel(now, reqModel) {
			return true
		}
	}
	return false
}

// servableForModelLocked 报告「此刻真有名额」：健康口径同 waitableForModelLocked，
// 且未占满在途。与 Pick 的候选谓词一致（tried + realm + 模型 + inFlightFull），
// 故「本函数为真 ⇒ Pick 必返回非 nil」，等待者不会空转。调用方需持锁。
func (p *Pool) servableForModelLocked(tried map[string]bool, reqModel, realm string, now time.Time) bool {
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		if e.healthyForModel(now, reqModel) {
			return true
		}
	}
	return false
}

// liveWaitersLocked 返回该域未退出的等待者数量。调用方需持锁。
func (p *Pool) liveWaitersLocked(realm string) int {
	n := 0
	for _, w := range p.waiters[realm] {
		if !w.gone {
			n++
		}
	}
	return n
}

// finishWaiter 标记 waiter 退出、惰性摘除队列表项，并按需累加计数与等待时长。
// counter 为 nil 时只做退出（ctx 取消不计入任何队列计数——客户端自己走了，不是
// 网关的容量问题，混进 timed_out 会污染告警口径）。幂等：重复调用只生效一次。
func (p *Pool) finishWaiter(w *waiter, counter *int64, waited time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if w.gone {
		return
	}
	w.gone = true
	if counter != nil {
		*counter++
		if waited > 0 {
			p.queueWaitTotal += waited
		}
	}
	p.trimWaitersLocked(w.realm)
}

// trimWaitersLocked 就地压缩该域队列，摘除已退出的表项。整表过滤（而非只削头部）
// 保证内存不随历史等待累积；上界是 max_waiters（默认 200），成本可忽略。
func (p *Pool) trimWaitersLocked(realm string) {
	ws := p.waiters[realm]
	if len(ws) == 0 {
		return
	}
	live := ws[:0]
	for _, w := range ws {
		if !w.gone {
			live = append(live, w)
		}
	}
	if len(live) == 0 {
		delete(p.waiters, realm)
		return
	}
	p.waiters[realm] = live
}

// wakeLocked 唤醒该域队首的一个等待者（非阻塞投递，无等待者时空操作）。
// 调用方需持写锁。一个释放的名额唤醒一个等待者——不用广播，避免惊群。
func (p *Pool) wakeLocked(realm string) {
	for _, w := range p.waiters[realm] {
		if w.gone {
			continue
		}
		select {
		case w.ch <- struct{}{}:
		default: // 已有未消费的唤醒：无需重复投递
		}
		return
	}
}

// wakeAllLocked 唤醒全部等待者（容量**批量**增加或队列参数变化时用：新增账号、
// 解冻、上限调高、关停）。唤醒后每个等待者自行复核谓词，拿不到名额的会继续等。
// 调用方需持写锁。
func (p *Pool) wakeAllLocked() {
	for _, ws := range p.waiters {
		for _, w := range ws {
			if w.gone {
				continue
			}
			select {
			case w.ch <- struct{}{}:
			default:
			}
		}
	}
}
