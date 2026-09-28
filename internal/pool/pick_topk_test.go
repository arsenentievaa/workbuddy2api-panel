package pool

import (
	"fmt"
	"math/rand/v2"
	"sort"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestPickTopKMatchesFullSort 是本次「用有界选择替代全排序」的等价性证明
// （差分测试，oracle = 全排序 + 截断）。
//
// 对大量随机候选集（刻意制造大量并列：cost1k/w 只取少数取值），断言：
//   - pickTopK 的输出与 sort.SliceStable(pickBefore) 的前 k 个**逐一相等**；
//   - 且对**洗牌后的输入**同样成立 → 结果与输入顺序无关。
//
// 顺序无关性是「移除原实现在 sort 之前的等权重 Fisher-Yates 洗牌」的正当性依据：
// 洗牌只能改变相等元素的相对次序，而 pickBefore 是严格全序（末项 uid 唯一），
// 不存在相等元素，故洗牌对结果零影响。若哪天比较器被改成非全序（例如去掉 uid
// 兜底、或引入可能为 NaN 的字段），本测试会立刻失败——这正是它存在的意义。
func TestPickTopKMatchesFullSort(t *testing.T) {
	rng := rand.New(rand.NewPCG(0x5eed, 0xbeef))
	for trial := 0; trial < 3000; trial++ {
		n := 1 + rng.IntN(12) // 覆盖 n<k、n==k、n>k
		in := make([]weighted, 0, n)
		used := map[string]bool{}
		for i := 0; i < n; i++ {
			uid := fmt.Sprintf("u%02d", rng.IntN(1_000_000))
			for used[uid] { // 比较器末项依赖 uid 唯一 → 用例必须保证唯一
				uid = fmt.Sprintf("u%02d", rng.IntN(1_000_000))
			}
			used[uid] = true
			in = append(in, weighted{
				e:      &entry{a: &auth.Auth{UID: uid}},
				cost1k: float64(rng.IntN(3)) / 4, // 0 / 0.25 / 0.5（制造并列）
				w:      float64(rng.IntN(3)) + 1, // 1 / 2 / 3
				tier:   rng.IntN(3),
			})
		}

		want := make([]weighted, len(in))
		copy(want, in)
		sort.SliceStable(want, func(i, j int) bool { return pickBefore(want[i], want[j]) })
		k := pickShortlistSize
		if len(want) > k {
			want = want[:k]
		}

		shuffled := make([]weighted, len(in))
		copy(shuffled, in)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })

		var buf [pickShortlistSize]weighted
		got := pickTopK(shuffled, k, buf[:])
		if len(got) != len(want) {
			t.Fatalf("trial %d: len=%d want %d", trial, len(got), len(want))
		}
		for i := range got {
			if got[i].e.a.UID != want[i].e.a.UID {
				t.Fatalf("trial %d: [%d]=%s want %s\n  in=%v", trial, i, got[i].e.a.UID, want[i].e.a.UID, uidsOf(in))
			}
		}
	}
}

// TestPickTopKEmpty 边界：空输入 → 空输出（不 panic、不返回哨兵）。
func TestPickTopKEmpty(t *testing.T) {
	var buf [pickShortlistSize]weighted
	if got := pickTopK(nil, pickShortlistSize, buf[:]); len(got) != 0 {
		t.Fatalf("len=%d want 0", len(got))
	}
}

// TestPickLRUTieBreakPrefersComparatorOrder 锁死一个容易被优化打破的既有语义：
// 从未被选中过的号 usedSeq 全为 0 → LRU 兜底必须回落到**比较器序首位**
// （原实现是 candsAll[0] 起步、仅严格更小时替换，全 0 时不会替换），
// 而不是「随便挑一个」。原实现靠「先全排序」隐含满足；现在靠显式决胜，本用例守住它。
func TestPickLRUTieBreakPrefersComparatorOrder(t *testing.T) {
	old := minPickGap
	minPickGap = time.Hour // 任何 lastUsed 都在窗口内 → 必走 LRU 兜底
	defer func() { minPickGap = old }()

	p := New("")
	for _, uid := range []string{"b", "a", "d", "c", "e", "f", "g"} {
		p.Add(&auth.Auth{UID: uid})
	}
	// 全部「刚被用过」（同一时刻 → 权重完全相同，比较器只能靠 uid 决胜），但 usedSeq
	// 仍为 0（模拟「刚启动/刚加入池、从未真正被选中」的号）。
	// 注意：lastUsed 不能留在零值——零值 lastUsed 表示「从未使用」，反而满足
	// minPickGap，会走正常抽签而不是 LRU 兜底（易踩的坑）。
	now := time.Now()
	p.mu.Lock()
	for _, e := range p.byUID {
		e.lastUsed = now
	}
	p.mu.Unlock()

	got := p.Pick()
	if got == nil {
		t.Fatal("pick returned nil")
	}
	if got.UID != "a" {
		t.Fatalf("LRU tie-break picked %q want %q (comparator-first by uid)", got.UID, "a")
	}
}

func uidsOf(ws []weighted) []string {
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		out = append(out, w.e.a.UID)
	}
	return out
}
