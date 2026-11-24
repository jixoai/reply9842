// Package basefind — 客户端基线选择：在最近的历史请求里找一个当 dcz 字典
//
// 正交意图（2 个）：
//  1. 历史表：保留最近若干「唯一」请求体，按字节预算淘汰最旧的
//  2. 选基线：从新到旧扫，第一个前缀占比达标的就用
//
// 刻意不做的事（越简单越有效）：
//   - 不做评分函数：前缀占比是唯一决定因素。LZ77 的压缩率主要由「匹配到多少
//     历史字节」决定，体积接近度、时间远近都是噪音。
//   - 不做 LRU：会话单调推进，新请求天然更可能当基线。FIFO 就够。
//   - 不做多基线投票：实测（调研）块级方案比整文档 delta 差 13 倍，多候选同理无收益。
//   - 不区分 Agent：历史里同时存各 Agent 请求，靠前缀匹配自然分流。
package basefind

import (
	"sync"

	"github.com/jixoai/reply9842/internal/codec"
)

// blockSize 对齐块大小。
//
// 实测（14 条真实 agent 请求，56 组配对）：4K/8K/16K 的判别力几乎相同
// （区分度 87.5% / 87.2% / 86.9%），说明变化点稀疏、块大小不敏感。取 4K
// 是因为它对齐粒度最细，且在 1MB 级请求上索引开销可忽略。
const blockSize = 4096

// minReuseRatio 块命中复用率下限：复用字节 / 基线总字节。
//
// 实测数据（26 组值得 vs 30 组不值得）：
//
//	值得组复用率均值 89.4%，最低 84%
//	不值得组均值 1.8%，最高 2.2%
//
// 中间是巨大空档，阈值取 0.3~0.6 等价。取 0.5 居中。
const minReuseRatio = 0.5

// Finder 客户端历史窗口
type Finder struct {
	mu     sync.Mutex
	budget int64 // 总字节预算
	used   int64
	items  []*candidate // 新→旧
}

type candidate struct {
	hash codec.Hash
	body []byte
}

// New budgetMB 为历史总字节预算（MB）。建议 64：能容纳约 35 条 1.8MB 请求，
// 足以覆盖多 Agent 并发的交错窗口（实测相邻间隔最大 21 条）。
func New(budgetMB int64) *Finder {
	if budgetMB <= 0 {
		budgetMB = 64
	}
	return &Finder{budget: budgetMB << 20}
}

// Remember 记录一次已发出的 body。同一内容只存一份。
func (f *Finder) Remember(body []byte) codec.Hash {
	h := codec.Sum(body)
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, c := range f.items {
		if c.hash == h {
			return h
		}
	}
	cp := make([]byte, len(body))
	copy(cp, body)
	f.items = append([]*candidate{{hash: h, body: cp}}, f.items...)
	f.used += int64(len(cp))

	// 字节预算淘汰：从尾部（最旧）丢
	for f.used > f.budget && len(f.items) > 1 {
		last := f.items[len(f.items)-1]
		f.items = f.items[:len(f.items)-1]
		f.used -= int64(len(last.body))
	}
	return h
}

// Forget 丢掉某条基线。
//
// 用途（跨 server 场景，真实 e2e 抓出的缺陷）：服务端回 FrameDeltaMiss
// 说明**那台 server 的 store 里没有这份基线**。此时客户端若仍把它记在
// finder 里，下一次请求还会选中它、再吃一次 miss、再全量重发——形成
// 「每次都 miss」的稳定劣化。
//
// 基线是**按 server 隔离的服务端状态**（store 是进程内的纯 KV），而客户端
// 的 finder 是全局的。跨 server 线路共存时，这条不一致必然出现；丢掉它
// 让客户端下一轮重新找基线（大概率改选一个对端也有的）。
func (f *Finder) Forget(h codec.Hash) bool {
	if h.IsZero() {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, c := range f.items {
		if c.hash == h {
			f.used -= int64(len(c.body))
			f.items = append(f.items[:i], f.items[i+1:]...)
			return true
		}
	}
	return false
}

// Pick 为 newBody 选基线：从新到旧，第一个块命中复用率 >= minReuseRatio 的。
// 返回零哈希表示「无合适基线，应发全量」。
//
// 为什么不用「从头的最长公共前缀」：实测 56 组配对里，prefix 对「是否值得
// 做 delta」的区分度是 **0%** —— 真实 agent 请求的第 43 字节起就是变化的
// （如 JSON 中段的数值字段），但第 64 字节之后有 90% 内容完全相同。
// 块命中率能跨过变化点，实测区分度 87.5%。
func (f *Finder) Pick(newBody []byte) (codec.Hash, []byte) {
	h, b, _ := f.PickWithReuse(newBody)
	return h, b
}

// PickWithReuse 与 Pick 相同，但额外返回**基线实际被复用的字节数**。
//
// 为什么需要这个数（对抗审查指出的缺陷）：
// blockReuseRatio 的分母是**基线长度**，扫到 reused >= len(base) 就提前退出。
// 于是「64 KB 旧基线 + 1.4 MB 新增内容」的 1.5 MB 请求复用率算出 100%（命中），
// 但真正被字典覆盖的只有 64 KB，剩下 1.4 MB 仍要按普通 zstd 压——实际 wire
// 约 200–400 KB，而不是「几百字节」。
//
// 择路需要一个正确的量级估计：`est ≈ 0.2 × (len(body) − reused)`。
// 没有 reused 这个数就只能猜，猜小了会把大请求塞进窄带线路。
func (f *Finder) PickWithReuse(newBody []byte) (codec.Hash, []byte, int) {
	f.mu.Lock()
	items := make([]*candidate, len(f.items))
	copy(items, f.items)
	f.mu.Unlock()

	if len(newBody) == 0 {
		return codec.Hash{}, nil, 0
	}
	for _, c := range items {
		if len(c.body) < blockSize {
			continue
		}
		if r, reused := blockReuse(newBody, c.body); r >= minReuseRatio {
			return c.hash, c.body, reused
		}
	}
	return codec.Hash{}, nil, 0
}

// blockReuseRatio 估算「基线有多少字节可被新请求复用」。
//
// 算法：把基线按 blockSize 切块建哈希索引，然后在新请求上扫描——
// 命中则跳过整块并计入复用，未命中则小步滑动以重新对齐（近似 rolling）。
//
// 复杂度 O(len(new)/step)，step 在命中和未命中时不同，最坏 O(n*4)。
// 1MB 请求约 1-3ms，可接受（相比 zstd 压缩本身 10-70ms）。
func blockReuseRatio(base, newBody []byte) float64 {
	r, _ := blockReuse(newBody, base)
	return r
}

// blockReuse 返回（复用率, 复用字节数）。
//
// 复用率的分母是**基线长度**（「基线有多少被用上」），复用字节数的上限
// 因此是 len(base)——这正是 PickWithReuse 需要它的原因。
func blockReuse(newBody, base []byte) (float64, int) {
	if len(base) < blockSize || len(newBody) < blockSize {
		return 0, 0
	}
	idx := make(map[string]struct{}, len(base)/blockSize+1)
	for i := 0; i+blockSize <= len(base); i += blockSize {
		idx[string(base[i:i+blockSize])] = struct{}{}
	}

	const missStep = blockSize / 4 // 未命中时小步滑动，重新对齐到真实块边界
	reused := 0
	for j := 0; j+blockSize <= len(newBody); {
		if _, ok := idx[string(newBody[j:j+blockSize])]; ok {
			reused += blockSize
			j += blockSize // 命中后再尝试整块前进（连续命中很快）
		} else {
			j += missStep
		}
		if reused >= len(base) {
			break
		}
	}
	r := float64(reused) / float64(len(base))
	if r > 1 {
		r = 1
	}
	return r, reused
}

// Len 当前候选数
func (f *Finder) Len() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.items)
}

// Bytes 当前历史占用字节
func (f *Finder) Bytes() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.used
}

// Clear 清空历史
func (f *Finder) Clear() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items = nil
	f.used = 0
}
