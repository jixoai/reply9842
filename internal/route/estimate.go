package route

// estimate.go — 吞吐模型：est(bytes) = a + bytes/bw
//
// ── 为什么不是「平均速度」这个单一数字 ──────────────────────────────────
// 单看平均速度会把「固定开销」摊进带宽里，得到一条斜率漂亮但截距失真的线：
// 实测 16 KB 请求的「有效吞吐」只有 5 MB/s，而 256 KB 请求能到 13 MB/s——
// 差的那 8 MB/s 不是带宽差，是小请求被固定开销（握手、TTFB、队头）拖累。
// 用两参数模型把这两件事分开，才能正确回答「这次这个大小的请求，哪条更快」。
//
// ── 为什么用最小二乘而不是更复杂的拟合 ──────────────────────────────────
// 样本来自真实生产请求，体积分布高度不均（绝大多数是 16–64 KB）。任何更花哨
// 的拟合（鲁棒回归、分位数回归）在这种长尾样本上都需要调参，而调出来的参数
// 没人能从日志里复核。两参数最小二乘有闭式解、能心算复核，是这个场景的上限。

import (
	"math"
	"sync"
	"time"
)

// 模型参数的先验与边界。
const (
	// minSamples 拟合所需的最少样本数。
	//
	// 取 3 而非 10：线路刚恢复时不该被冷落太久（见 pick.go 的探索语义），
	// 3 个样本已经能给出「有量级意义」的两参数估计。再少就退化为一个点。
	minSamples = 3

	// maxSamples 参与拟合的样本上限（最近 N 条）。
	//
	// 取 24：线路质量的退化是分钟级的（丢包时段通常持续数分钟到数十分钟），
	// 更长的历史会把「刚才很糟」稀释成「平均还行」。24 条在实测负载下
	// （相邻请求中位间隔 6 秒）约覆盖 2–3 分钟，正好对得上「最近手感」。
	maxSamples = 24

	// minBandwidth 带宽下限（字节/毫秒 = KB/s）。
	//
	// 没有下限时，极慢样本会拟出接近 0 的 bw，于是 est(bytes) 爆炸成天文数字，
	// 排序被单一离群点主导。200 KB/s 是 owner 明确提过的「能用的最慢线路」
	// 量级，作为下限合理：比它更慢的线路本来就该被判为不可用。
	minBandwidth = 200.0 * 1024 / 1000 // ≈205 KB/s

	// maxBandwidth 带宽上限，仅用于防止除零与数值溢出（100 MB/s）。
	maxBandwidth = 100.0 * 1024 * 1024 / 1000

	// maxOverhead 固定开销上限（毫秒）。超长封顶避免 est 被一条病态样本拽飞。
	maxOverhead = 120000.0
)

// sample 一条实测样本。
type sample struct {
	bytes float64 // 本请求在隧道上的总字节（上行 + 下行 wire）
	ms    float64 // 该请求的传输耗时（ms_wire 口径）
}

// Model 一条线路的吞吐模型（并发安全）。
//
// 语义要点：Model 是**纯观测**的——它只回答「这条线最近怎么样」，
// 不持有任何「选不选它」的决策（那是 Picker 的事）。这个分离让两侧都能
// 独立测试，也避免「评分里偷偷混进了降权因子」这种难以解释的行为。
type Model struct {
	mu      sync.Mutex
	samples []sample

	// 拟合结果缓存（样本变化时才重算）
	a  float64 // 固定开销，ms
	bw float64 // 带宽，字节/毫秒
	ok bool    // 参数是否可用（样本足够且拟合成功）

	// 失败计数（供 Picker 降权）。只记「连续失败」，成功即清零。
	fails int
	// lastFailAt 最近一次失败的时刻，用于**时间衰减**。
	//
	// 为什么需要它（规格审查 F4 抓出的缺陷，属 P1）：
	// 没有衰减时，一条线路把失败堆到惩罚封顶后就再也不会被选中——于是
	// 永远拿不到「成功一次」的机会，永远不清零。这在效果上就是 owner
	// 明令禁止的「硬熔断」，只是换了个名字。
	//
	// 衰减语义：EffectiveFails 按 halfLife 指数衰减，隔了足够久之后
	// 惩罚自动回到 1×，线路重新进入正常竞争。
	lastFailAt time.Time
	// success/fail 累计（供 UI 显示成功率）
	success int64
	failure int64
}

// NewModel 创建一个空模型。
func NewModel() *Model { return &Model{} }

// Observe 记录一次成功请求的样本并刷新拟合。
//
// bytes 用**隧道实际字节**而不是明文请求体：择路决策发生在编码之后
// （编码后才知道 payload 多大），用 wire 字节才对得上「这条线要搬多少」。
func (m *Model) Observe(bytes int64, d time.Duration) {
	if bytes <= 0 || d <= 0 {
		return // 零字节或零耗时样本对拟合无信息量，纳入只会引入噪声
	}
	b := float64(bytes)
	msv := float64(d.Microseconds()) / 1000.0
	// 单条样本的「隐含带宽」离谱时（比如 ms 记录为 0.001ms），说明这份样本
	// 的计时口径有问题（多见于复用了连接但耗时被截断），丢弃而不是污染模型。
	if msv <= 0 || b/msv > maxBandwidth*10 {
		return
	}

	m.mu.Lock()
	m.samples = append(m.samples, sample{bytes: b, ms: msv})
	if len(m.samples) > maxSamples {
		m.samples = append(m.samples[:0], m.samples[len(m.samples)-maxSamples:]...)
	}
	m.fails = 0
	m.lastFailAt = time.Time{}
	m.success++
	m.refitLocked()
	m.mu.Unlock()
}

// ObserveFailure 记录一次失败。失败不进入拟合（没有耗时/字节语义），
// 只驱动降权；连续失败次数会被 Picker 用来加速衰减。
func (m *Model) ObserveFailure() {
	m.mu.Lock()
	m.fails++
	m.failure++
	m.lastFailAt = time.Now()
	m.mu.Unlock()
}

// refitLocked 用最小二乘拟合 ms = a + b*bytes 里的 b（= 1/bw），截距即 a。
//
// 退化情形（所有样本体积几乎相同）无法分离 a 与 bw：此时把 a 置 0，
// 用平均吞吐兜底。这不是「拟合失败」，而是「数据只支持回答一个数」。
func (m *Model) refitLocked() {
	n := len(m.samples)
	if n < minSamples {
		m.ok = false
		return
	}

	var sx, sy, sxx, sxy float64
	for _, s := range m.samples {
		x, y := s.bytes/1024.0, s.ms // x 用 KB，避免 b 出现极大极小值
		sx += x
		sy += y
		sxx += x * x
		sxy += x * y
	}
	nf := float64(n)
	den := nf*sxx - sx*sx

	if math.Abs(den) < 1e-9 {
		// 体积方差过小：只能给平均吞吐，a 设 0
		if avgMs := sy / nf; avgMs > 0 {
			m.a = 0
			m.bw = clampBW((nf * 1024.0) / avgMs) // (KB → bytes) / ms
			m.ok = true
			return
		}
		m.ok = false
		return
	}

	// y = a + k*x，其中 x 是 KB，k = ms/KB
	k := (nf*sxy - sx*sy) / den
	a := (sy - k*sx) / nf

	// k <= 0 意味着「字节越多越快」，物理上只可能是样本太杂（比如中途换了
	// 上游或线路质量剧变）。此时退回平均吞吐，不给出负斜率。
	if k <= 0 {
		if avgMs := sy / nf; avgMs > 0 {
			m.a = 0
			m.bw = clampBW((nf * 1024.0) / avgMs)
			m.ok = true
			return
		}
		m.ok = false
		return
	}

	m.a = clampFloat(a, 0, maxOverhead)
	// k = ms/KB  →  bw = 1/k KB/ms →  KB/ms × 1024 = bytes/ms
	m.bw = clampBW(1024.0 / k)
	m.ok = true
}

func clampBW(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return minBandwidth
	}
	return clampFloat(v, minBandwidth, maxBandwidth)
}

func clampFloat(v, lo, hi float64) float64 {
	if math.IsNaN(v) || v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// Estimate 返回「传输 bytes 字节预计要多久」。known=false 表示样本不足。
//
// bytes 是本次请求预计要搬的隧道字节总量（上行 wire + 预期下行 wire）。
// 只给上行体积也够用：择路发生在上行发送前，下行体积未知；经验上下行与
// 上行同阶（实测 up/down ≈ 14.8:1 是**明文**比，wire 上 drill 后两者同量级）。
func (m *Model) Estimate(bytes int64) (est time.Duration, known bool) {
	if bytes <= 0 {
		return 0, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.ok {
		return 0, false
	}
	ms := m.a + float64(bytes)/m.bw
	if ms <= 0 {
		ms = 1 // 下限 1ms：保证 est 恒为正，避免下游出现 0 导致的除零/排序歧义
	}
	return time.Duration(ms * float64(time.Millisecond)), true
}

// Known 是否已有可用的拟合结果（=该线路能否参与 est 比较）。
//
// Picker 用它区分「需要探测的线路」（还拟合不了）与「已知线路」（可比较）。
// 判据与 Estimate 的 ok 保持一致——两处若不一致就会出现「能估但不探测」
// 或「探测但已可估」的裂缝（规格审查 F1 正是前者）。
func (m *Model) Known() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ok
}

// Params 返回拟合参数的快照（a 毫秒、bw 字节/毫秒）。供 UI 与日志展示。
func (m *Model) Params() (a float64, bwKBps float64, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// bw 是 字节/ms = KB/s，直接返回
	return m.a, m.bw, m.ok
}

// Stats 返回观测统计快照。
func (m *Model) Stats() (samples int, success, failure int64, consecutiveFails int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.samples), m.success, m.failure, m.fails
}

// ConsecutiveFails 连续失败次数（原始值，供 UI 展示）。
func (m *Model) ConsecutiveFails() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.fails
}

// failHalfLife 失败计数的衰减半衰期。
//
// 取 2 分钟：线路「偶尔丢包」与「真的坏了」的时间尺度不同——实测香港链路
// 的劣化时段持续数分钟到数十分钟。2 分钟意味着一条线路失败后若安静两分钟，
// 其惩罚减半；封顶（~16 次失败）的线路约 8 分钟后回到 1×，重新参与竞争。
//
// 更短（如 30s）会让「持续劣化」的线路频繁被重试、把用户请求反复拖慢；
// 更长（如 30min）则接近永不复原，等于变相熔断。
const failHalfLife = 2 * time.Minute

// EffectiveFails 返回**经过时间衰减后**的连续失败次数（Picker 用它算惩罚）。
//
// 这是 F4 的修复核心：只看原始 fails 会让线路永久出局。衰减让「安静了足够久
// 的线路」自动回到候选，而真正的持续失败仍会被惩罚压住（因为它每次失败都
// 刷新 lastFailAt）。
func (m *Model) EffectiveFails() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fails <= 0 {
		return 0
	}
	if m.lastFailAt.IsZero() {
		return float64(m.fails)
	}
	elapsed := time.Since(m.lastFailAt)
	if elapsed <= 0 {
		return float64(m.fails)
	}
	// fails × 0.5^(elapsed/halfLife)
	//
	// 直接用 math.Pow：纯数学衰减没有符号/精度歧义（先前手写位移+泰勒展开
	// 是过度设计，反而更容易算错）。
	return float64(m.fails) * math.Pow(0.5, float64(elapsed)/float64(failHalfLife))
}

// SuccessRate 成功率；无样本时返回 -1（表示「未知」，与「0%」区分开）。
func (m *Model) SuccessRate() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	total := m.success + m.failure
	if total == 0 {
		return -1
	}
	return float64(m.success) / float64(total)
}
