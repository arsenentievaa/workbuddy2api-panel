// 选号：Pick 簇（healthy 三因子加权 Top5 短名单 + 加权随机 + 全冷却兜底 + 在途占满过滤）。
package pool

import (
	"log"
	"math/rand/v2"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
)

// Pick 单一选号入口（无请求级轮换、无 realm 过滤，模型感知缺省账号级）。
// 需要请求级轮换（tried）或分池（realm）时用 PickExcludingForRealm。
func (p *Pool) Pick() *auth.Auth {
	return p.pick(nil, "", "")
}

// PickExcluding 同上，但跳过 tried 中的 uid（请求级轮换）。
// 挑选策略：healthy 账号中按权重取前 5 名，再在 Top5 内按同一权重加权随机抽签，
// 意图是打散热点，避免永远打同一个账号。
func (p *Pool) PickExcluding(tried map[string]bool) *auth.Auth {
	return p.pick(tried, "", "")
}

// PickExcludingForModel 模型感知选号：等同 PickExcluding，但对「6004 模型级冷却中的
// 账号」进行模型豁免——请求模型与其 trigger 模型不同时视为可用（issue #31）。
// reqModel 为空时即普通 PickExcluding（不影响既有调用语义）。
func (p *Pool) PickExcludingForModel(tried map[string]bool, reqModel string) *auth.Auth {
	return p.pick(tried, reqModel, "")
}

// PickExcludingForRealm 模型感知 + 分池选号：候选集先按 Realm()==realm 过滤
// （realm 空 = 不过滤，退化为 PickExcludingForModel），再按模型健康口径判定。
// 供 handler 在 global/cn 双域下分流（global 模型请求只路由 global 账号）。
func (p *Pool) PickExcludingForRealm(tried map[string]bool, reqModel, realm string) *auth.Auth {
	return p.pick(tried, reqModel, realm)
}

// PickExcludingForRealmOrigin 同 PickExcludingForRealm，但额外报告结果是否来自
// 「全冷却兜底」（pickEarliestExpiryLocked）。handler 用它区分：
//
//	(a, false) → 正常健康号；a==nil 表示真的无可用号
//	(a, true)  → 兜底号（健康号全占满在途或全冷却时的产物）
//
// 饱和排队（queue.go）需要这个区别：拿到兜底号时若「容量是唯一瓶颈」，应等名额
// 而不是把请求推给刚被限流/熔断的号——推过去既救不了请求，又加重该号的风控风险。
func (p *Pool) PickExcludingForRealmOrigin(tried map[string]bool, reqModel, realm string) (*auth.Auth, bool) {
	return p.pickOrigin(tried, reqModel, realm)
}

// pick 在 healthy 候选集中按权重加权随机选出账号，并记录 lastUsed（防并发撞号）。
// reqModel 非空时把健康口径换成 healthyForModel（6004 模型豁免生效）。
// realm 非空时候选过滤叠加 Realm()==realm 谓词（分池选号域）。
// pick 是 pickOrigin 的兼容壳：多数调用方不关心兜底来源（零回归）。
func (p *Pool) pick(tried map[string]bool, reqModel, realm string) *auth.Auth {
	a, _ := p.pickOrigin(tried, reqModel, realm)
	return a
}

// pickShortlistSize 短名单长度：加权抽签只在这 K 个里进行（历史行为，勿改）。
const pickShortlistSize = 5

// weighted 选号候选项：权重与成本分层一次算好后缓存，比较器只读字段（不现算），
// 避免 O(n log n) 次冗余浮点计算。
type weighted struct {
	e      *entry
	w      float64
	tier   int
	cost1k float64
}

// pickBefore 报告 a 是否应排在 b 之前：单价升序 → 权重降序 → uid 升序。
//
// 这个比较器是**严格全序**：uid 唯一保证没有两个不同候选「相等」；各浮点项经
// weightOf / modelCostOf 的既有守卫（maxCredits>0、credits>0、tokens>0）不可能为
// NaN，故 `!=` 与 `<` 在浮点项上是一致的。
//
// 全序这个性质是本次优化的根据，有两条推论：
//  1. 排序结果与输入顺序无关 → 可以用有界插入（pickTopK）替代全排序，前 K 个逐一相等；
//  2. 原实现在 sort 之前做的「等权重 Fisher-Yates 洗牌」对结果**没有影响**（洗牌只能
//     改变「相等元素」的相对次序，而严格全序下不存在相等元素），属于纯开销：一次
//     与候选数同阶的洗牌 + 一个 PCG 分配。故本次一并移除；其注释所述「等权重账号被
//     字典序饿死」的问题实际由 uid 兜底比较 + LRU 兜底（按 usedSeq 取全局最旧）承担。
//     该等价性由 TestPickTopKMatchesFullSort 用全排序作 oracle 差分验证。
func pickBefore(a, b weighted) bool {
	if a.cost1k != b.cost1k {
		return a.cost1k < b.cost1k
	}
	if a.w != b.w {
		return a.w > b.w
	}
	return a.e.a.UID < b.e.a.UID
}

// pickTopK 有界选择：返回 in 中按 pickBefore 排序的前 k 个（保序），buf 为复用缓冲
// （len/cap ≥ k）。等价于 sort.SliceStable(in, pickBefore)[:k]，但为 O(len(in)·k)
// 且**零分配**——在数百/数千账号下取代 O(n log n) 的反射式排序。
func pickTopK(in []weighted, k int, buf []weighted) []weighted {
	out := buf[:0]
	for _, c := range in {
		// 二分不必：k=5 时线性回退最多 5 步，比二分更省。
		pos := len(out)
		for pos > 0 && pickBefore(c, out[pos-1]) {
			pos--
		}
		if pos >= k {
			continue // 比现有前 k 名都差 → 直接丢弃
		}
		if len(out) < k {
			out = append(out, weighted{})
		}
		copy(out[pos+1:], out[pos:])
		out[pos] = c
	}
	return out
}

// pickOrigin 是选号的唯一实现（pick 与 PickExcludingForRealmOrigin 共用），
// 额外返回「本次是否走了全冷却兜底」。
func (p *Pool) pickOrigin(tried map[string]bool, reqModel, realm string) (*auth.Auth, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	realmOK := func(e *entry) bool { return realm == "" || e.a.Realm() == realm }
	healthyOf := func(e *entry) bool { return realmOK(e) && e.healthy(now) }
	if reqModel != "" {
		healthyOf = func(e *entry) bool { return realmOK(e) && e.healthyForModel(now, reqModel) }
	}
	// 成本分层（reqModel 非空时）：按该模型的实测扣费把候选分层，只保留最优层。
	//   0 = 已实测免费（限免期/夜间免费的号，最强偏好）
	//   1 = 无观测（含观测过期）
	//   2 = 已实测收费
	// 为什么"无观测"排在"已实测收费"之前：新号的限免状态只能靠实测发现，
	// 若已知收费的号恒压过未知号，那台免费的号永远轮不到，也就永远学不到。
	// 为什么用硬过滤而非仅排序：pickWeighted 会在候选内加权随机，只排序的话
	// 收费号仍有机会抽中，达不到"优先免费"的语义。
	costTier := func(e *entry) (int, float64) {
		mc, ok := e.modelCostOf(reqModel, now)
		if !ok {
			return 1, 0
		}
		if mc.CostPer1k <= 0 {
			return 0, 0
		}
		return 2, mc.CostPer1k
	}
	// ── 第一趟：过滤 + maxCredits + 成本分层，一趟算完 ─────────────────────────
	// 写入**池级复用缓冲** pickWS：pick 在写锁内串行执行，缓冲被本函数独占，故复用
	// 安全且稳态零分配。原实现把过滤 / maxCredits / bestTier / hasTier1 拆成 3~4 趟
	// 独立 O(账号数) 循环、每趟各自 append 重建切片——数百账号下这是每请求的主要
	// 浪费之一（实测 2000 账号约 114 KB/请求，其中 cands+ws 各占大头）。
	ws := p.pickWS[:0]
	var maxCredits int64
	bestTier := 2
	hasTier1 := false
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		// 惰性清理过期的模型级冷却与成本台账（两者的 map 都不无限膨胀；
		// status 只读遍历天然跳过过期项，但内存条目必须在此真正删除）。
		e.pruneExpiredModelCooldowns(now)
		e.pruneExpiredModelCosts(now)
		if !healthyOf(e) {
			continue
		}
		if p.inFlightFull(e) {
			continue // 在途占满：跳过（max=0 不限时不触发）
		}
		// maxCredits 统一用**全集口径**（tier 过滤前的全部 healthy 候选）：截断排序与
		// 抽签权重共享同一基准，两个阶段权重可比。
		if e.credits > maxCredits {
			maxCredits = e.credits
		}
		ti, ci := costTier(e)
		if ti < bestTier {
			bestTier = ti
		}
		// hasTier1 原实现只在「tier0 垄断 + 有模型名」时另扫一趟；此处恒记录（一个
		// bool 赋值），省掉那一趟 O(N)。
		if ti == 1 {
			hasTier1 = true
		}
		ws = append(ws, weighted{e: e, tier: ti, cost1k: ci}) // w 第二趟填（需 maxCredits）
	}
	if len(ws) == 0 {
		p.pickWS = ws // 回写保留已扩容的底层数组（下个请求复用）
		// 全冷却兜底：无 healthy 候选时，从冷却账号里选 until 最早到期的一个
		// （熔断/冷却共用 expiry 口径，取较早截止者）。禁用的账号永不参与兜底。
		return p.pickEarliestExpiryLocked(tried, now, realm), true
	}
	explored := false // 本次 pick 是否切了探索层（事件日志在选中号确定后打）
	// 条件探索（issue #136 方案 a′）：tier 0 垄断层存在（bestTier==0 且 reqModel
	// 非空）且候选含 tier 1（冻结存在）且距上次探索 ≥ 窗口（零值 timer=从未探索
	// →首次满足即探）时，本次 pick 生效层切 tier 1-only——探索=搭车改道，把一个
	// 既有真实用户请求改道给未知号（零新增上游请求；IP 维度零增量，WAF 友好）。
	// 成功 → NoteModelCost 首观测 → 毕业（tier 0/2，下一轮 pick 立即生效）；
	// 失败 → 既有错误策略照常，无探测风暴。
	// hasTier1 复用本循环上方 costTier 的预计算口径（每候选一次的契约不变）。
	// timer 同锁写入：并发 pick 串行进入写锁，只有一个进入者能通过窗口判定
	//（天然防重复探索）。key = realm + "\x1f" + reqModel：同模型名可跨域，
	// 探索节奏按 (域, 模型) 独立；realm==""（Pick 老语义）单独成键。
	if p.costExploreInterval > 0 && bestTier == 0 && reqModel != "" {
		// hasTier1 已在第一趟记录（不再为它单扫一趟 O(N)）。
		key := realm + "\x1f" + reqModel
		if hasTier1 && now.Sub(p.exploreLast[key]) >= p.costExploreInterval {
			p.exploreLast[key] = now
			p.costExploreEvents++
			bestTier = 1
			explored = true
		}
	}
	// ── 第二趟：补权重（需要 maxCredits）+ 成本分层硬过滤 ──────────────────────
	// 权重仍每候选只算一次（不在比较器里现算）；分层字段已在第一趟缓存，此处不再
	// 查 modelCostOf。原地压缩到 ws 前段（n <= i，向左搬移安全），零分配。
	n := 0
	for i := range ws {
		if ws[i].tier != bestTier {
			continue
		}
		ws[i].w = p.weightOf(ws[i].e, maxCredits, now)
		ws[n] = ws[i]
		n++
	}
	ws = ws[:n]
	p.pickWS = ws // 回写保留已扩容的底层数组（下个请求复用，稳态零分配）
	// 短名单：按 (单价升序, 权重降序, uid 升序) 取前 5。原实现先对**全体**候选做
	// sort.SliceStable 再截断（O(n log n) + reflect 交换 + 每请求重建两个切片），
	// 在数百/数千账号下成为每请求的主要开销；pickTopK 用有界插入做到 O(n·5)、零分配，
	// 且结果与全排序前缀**逐一相等**（严格全序，证明见 pickBefore）。
	// 原实现在此之前的「等权重洗牌」已移除：对严格全序而言它是无效开销（同见 pickBefore）。
	shortlist := pickTopK(ws, pickShortlistSize, p.pickTop[:])

	// 防并发撞号：在持锁内基于「上次选中时刻」过滤，但同一批并发 goroutine 会串行进入
	// 本函数（写锁），每个进入者都把 lastUsed 置为 now —— 于是同一瞬间的第 2..N 个
	// 进入者看到前一个账号 lastUsed==now（距今 0 < minPickGap），被自然挤向其他账号。
	// 关键：lastUsed 在锁内赋值，使时间窗口判定在并发下可重入。
	// 栈上定长数组：短名单至多 5 个，无需堆分配。
	var eligBuf [pickShortlistSize]*entry
	eligible := eligBuf[:0]
	for _, c := range shortlist {
		if now.Sub(c.e.lastUsed) >= minPickGap {
			eligible = append(eligible, c.e)
		}
	}
	var e *entry
	if len(eligible) == 0 {
		// 短名单全部刚被用过：LRU 兜底，在**全候选 ws**（非仅短名单）里取最旧者。
		// 用 usedSeq 单调序号而非 lastUsed 墙钟比较：Windows 等平台 time.Now() 精度
		// ~0.5ms，快速连续选号时所有 lastUsed 完全相等，Before 全 false 会恒选第一个
		// 导致集中。usedSeq 严格全序，与时间精度无关。
		//
		// 决胜口径必须与原实现（candsAll 已排序，从 [0] 起步、仅严格更小时替换）逐一
		// 等价：**usedSeq 最小者优先，同 usedSeq 时取比较器序更优先者**。这一点在
		// usedSeq 全为 0（从未被选中过的号）时尤其重要——那时原实现回落为「排序首位」
		// （= 权重最高/单价最低者），而不是随便挑一个。故此处做一次
		// (usedSeq, pickBefore) 字典序最小，而不必真的排序：
		// 该兜底只依赖候选的*集合*与比较器，与遍历顺序无关。
		var best weighted
		found := false
		for i := range ws {
			c := ws[i]
			if !found || c.e.usedSeq < e.usedSeq || (c.e.usedSeq == e.usedSeq && pickBefore(c, best)) {
				e, best, found = c.e, c, true
			}
		}
	} else {
		e = p.pickWeighted(eligible) // eligible 保序 = 短名单比较器序子集
	}
	if explored {
		// 探索事件日志（可观测性）：选中号此时才确定，故在选中点打出。
		// 毕业结果由相邻的既有日志闭环（免费号无日志、收费号走 NoteModelCost
		// 常规路径）。
		log.Printf("[pool] cost explore model=%s realm=%q acct=%s window=%s",
			reqModel, realm, logfmt.Label(e.a.UID, e.a.Nickname), p.costExploreInterval)
	}
	e.lastUsed = now // 锁内即时标记：下一个进入 pick 的 goroutine 立即看到本号已用
	p.pickSeq++
	e.usedSeq = p.pickSeq // 单调序号：保证 usedSeq 严格全序（防惊群/LRU 的权威依据）
	return e.a, false
}

// pickEarliestExpiryLocked 全冷却兜底：在非禁用的软冷却/熔断账号中选截止最早的一个。
// 分级：disabled 永不参与；CoolHard（余额耗尽，等签到的号）同样排除——调了必 402，浪费轮换并产生噪音日志；
// CoolSoft 与熔断号允许参与（可能已恢复，失败成本仅一轮换）。
// 被 tried 排除、在途占满的账号同样跳过（维持请求级轮换 + 租约语义）。无任何可用返回 nil。
func (p *Pool) pickEarliestExpiryLocked(tried map[string]bool, now time.Time, realm string) *auth.Auth {
	var best *entry
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if realm != "" && e.a.Realm() != realm {
			continue // 域过滤：池内跨 realm 的冷却账号不参与本 realm 兜底
		}
		if e.disabled {
			continue // 禁用的账号永不参与兜底
		}
		if e.coolKind == CoolHard && !e.until.IsZero() && now.Before(e.until) {
			continue // 余额耗尽号（处于有效 hard 冷却期）不参与兜底：等签到恢复，调了必 402
		}
		if p.inFlightFull(e) {
			continue
		}
		exp := e.expiry(now)
		if exp.IsZero() {
			continue
		}
		if best == nil || exp.Before(best.expiry(now)) {
			best = e
		}
	}
	if best == nil {
		return nil
	}
	log.Printf("WARN: [pool] fallback_earliest_expiry acct=%s until=%s kind=%s", logfmt.Label(best.a.UID, best.a.Nickname), best.expiry(now).Format(time.RFC3339), best.fallbackKind(now))
	best.lastUsed = time.Now()
	// 兜底同样是「选中」，必须与 pick() 正常路径、粘性命中路径（PickByUIDForModel）
	// 一样推进 usedSeq/pickSeq：否则被兜底反复选中的账号 usedSeq 恒为 0，在 pick 的
	// LRU 兜底（按 usedSeq 取最旧）眼里永远是「最旧」，刚被用过就被立刻再选——
	// 防集中/防惊群失效（entry.usedSeq 契约：每次被选中时取 p.pickSeq 自增值）。
	p.pickSeq++
	best.usedSeq = p.pickSeq
	return best.a
}

// inFlightFull 报告账号是否已占满在途名额（上限按 realm 分档，见 inFlightLimit；
// limit=0 不限 → 恒 false）。调用方需已持 p.mu（读锁或写锁均可，本方法只读上限）。
func (p *Pool) inFlightFull(e *entry) bool {
	limit := p.inFlightLimit(e)
	if limit <= 0 {
		return false
	}
	return e.inFlight.Load() >= int64(limit)
}

// minPickGap 防并发撞号窗口：同一账号在该窗口内不重复被选中（除非 top5 全部刚被用过）。
// 生产默认 100ms；纯加权分布测试可临时置 0 关闭防撞号。
var minPickGap = 100 * time.Millisecond

// pickWeighted 三因子加权随机（claude-api selectWeightedRandom 参考口径）：
//
//		weight = credits 比例 × 10 + idleWeight + successRate × 3
//
//	  - credits 比例 = 该号 credits / 候选集内最大 credits（避免量纲爆炸）
//	  - idleWeight = min(距 lastUsed 小时数 × idleWeightPerHour, idleWeightMax)；从未使用给满分
//	  - successRate = successCount/(successCount+errTotal)；无请求记录给 1.5（中性偏信任）
//
// credits 全 0 时仍按 idle+successRate 加权（不退化均匀随机）。
// 权重为浮点，用 int64 定点（×1e6）抽签可保持确定性随机源注入（randInt64N 语义不变）。
// 随机源优先用 p.randInt64N（仅供测试注入确定性），nil 时回退 math/rand/v2 全局源。
func (p *Pool) pickWeighted(cands []*entry) *entry {
	now := time.Now()
	var maxCredits int64
	for _, e := range cands {
		if e.credits > maxCredits {
			maxCredits = e.credits
		}
	}
	const scale = 1_000_000 // 定点放大：int64 累加权重大整数抽签
	// 复用池级缓冲（调用方持写锁 → 独占；稳态零分配）。cands 至多是短名单（≤5），
	// 所以这个缓冲恒小。
	if cap(p.pickWeights) < len(cands) {
		p.pickWeights = make([]int64, len(cands))
	}
	weights := p.pickWeights[:len(cands)]
	var total int64
	for i, e := range cands {
		w := p.weightOf(e, maxCredits, now)
		weights[i] = int64(w * scale)
		total += weights[i]
	}
	rnd := rand.Int64N
	if p.randInt64N != nil {
		rnd = p.randInt64N
	}
	if total <= 0 {
		return cands[int(rnd(int64(len(cands))))]
	}
	r := rnd(total)
	var acc int64
	for i, e := range cands {
		acc += weights[i]
		if r < acc {
			return e
		}
	}
	return cands[len(cands)-1]
}

// weightOf 计算单个账号的三因子权重。
func (p *Pool) weightOf(e *entry, maxCredits int64, now time.Time) float64 {
	w := 1.0
	// 1. credits 比例 ×10（会计入 mid-credit 锚点，避免全员 0 时 credits 项为 0）。
	if maxCredits > 0 {
		w += float64(e.credits) / float64(maxCredits) * 10
	}
	// 1b. 快过期积分加成：官方活动赠送的奖励积分按批过期，不用就作废。
	// creditsExpiring 占总量比例越高，越应优先被消耗——把"快过期占比"作为独立的
	// 强权重项（×expiringWeight），让快过期积分多的号优先选。与 credits 总量项
	// 正交：那是按总量，这是按过期紧迫度。
	if e.credits > 0 && e.creditsExpiring > 0 {
		w += float64(e.creditsExpiring) / float64(e.credits) * expiringWeight
	}
	// 2. 闲置补偿。
	if e.lastUsed.IsZero() {
		w += p.idleWeightMax // 从未使用 → 满分
	} else {
		hours := now.Sub(e.lastUsed).Hours()
		idleW := hours * p.idleWeightPerHour
		if idleW > p.idleWeightMax {
			idleW = p.idleWeightMax
		}
		if idleW < 0 {
			idleW = 0 // lastUsed 在未来（时钟回拨）时钳 0
		}
		w += idleW
	}
	// 3.（原「成功率 ×3」因子已删，对齐上游 success-ema-review：errTotal 是终身
	// 累计、只增不减，成功率 = successCount/(successCount+errTotal) 会让早期出过错
	// 的号被永久压权且永不恢复；瞬时健康信号已由冷却/熔断/连败降权承接。）
	return w
}

// SetCredits 更新账号余额。

// expiringWeight 快过期积分占比的权重系数（三因子之外的第四因子）。
// 取 8：略低于 credits 总量项（×10），足以在"快过期多"与"总量相近"的号之间拉开差距，
// 又不至于压过总量项让"总量大但快过期少"的号被完全饿死。
const expiringWeight = 8.0
