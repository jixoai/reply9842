package route

// pick.go — 按「预计完成时间」择路 + 自动降权 + 保底
//
// ── 这一层的唯一职责：给定本次请求的字节数，回答「走哪条」 ─────────────────
// 它不建连、不发包、不管重试——那些是 relayclient / transport 的事。
// 把决策与执行分开的理由：决策需要能脱离网络单独测试（注入任意样本组合，
// 断言选择结果），而执行需要真网络。混在一起就只能靠集成测试，慢且脆。
//
// ── 降权模型（owner 2026-11-24 裁决：「自动降权 + 仍保留兜底」）───────────
//
//	权重 = 1 / (est × penalty)
//	penalty = 1 + failStep × 连续失败次数      （上限 failCap）
//
// 即：**预计耗时越小、且越少失败，越容易胜出**；失败不淘汰，只是让它排在后面。
// 连续失败次数在成功一次后清零（Model.Observe 里做），所以「恢复」是自动的。
//
// 为什么不用「失败超过 N 次就踢出候选」：owner 的原话是三条线可能同时瞬断，
// 全踢光就没有可用线路了。降权让「最不差的还能上」，比硬熔断安全。
//
// ── 探索与保底 ────────────────────────────────────────────────────────────
// 样本不足的线路 known=false，est 未知。此时不能：
//   (a) 直接排除它——那它永远拿不到样本，会永久冷落（饿死）
//   (b) 给 est=∞——同理
//   (c) 给 est=0——那它永远赢，会把流量全吸走却不产生有效数据
//
// 第一版的做法是把未知线路的 est 放大 exploreFactor（1.35）倍后参与比较。
// 实测下来这是**错的**：放大意味着它必须比最优线路快 35% 才能胜出，而它
// 根本没有样本，于是它永远赢不了——「探索」名不副实，新加的线路会被永久
// 冷落。（这个缺陷是被 route_test.go 的 TestUnknownRouteIsExploredButNotDominant
// 抓出来的，不是想出来的。）
//
// 现在改为**时间门控的主动探测**：
//
//	存在「从未成功过」的启用线路，且它距上次尝试已超过 exploreInterval
//	→ 本次直接选它。
//
// 语义更诚实：未知线路每 exploreInterval 至少被真实试一次，拿到样本后立刻
// 汇入正常的 est 比较。代价被显式限定为「每 30 秒最多一次探测」，而不是
// 「按比例污染一部分流量」。失败线路样本仍为空，于是每 30 秒被重试一次——
// 这正好等价于一个内建健康探测，不需要另做探活。

import (
	"fmt"
	"math"
	"sync"
	"time"
)

// 降权与探索参数。
const (
	// failStep 每连续失败一次，惩罚系数增加的比例。
	//
	// 取 0.5：连续失败 2 次 → 惩罚 2×（相当于「就算它快一倍也不会赢」），
	// 4 次 → 3×。这个斜率在「一次偶发失败」与「线路真坏了」之间够分辨：
	// 偶发失败一次只让它的 est 变成 1.5 倍，仍可能靠带宽优势赢回来。
	failStep = 0.5

	// failCap 惩罚上限。
	//
	// 取 32：连续失败 60 次以上都按 32× 计。有上限的理由是**保底**：
	// 全部线路都烂到底时，它们之间的惩罚都封顶，比较退化为「纯 est 比较」，
	// 于是仍能选出「最不差」的一条。若无上限，全是天文数字时排序失去意义。
	failCap = 32.0

	// exploreInterval 未知线路的**首次**探测间隔（也是退避基数）。
	//
	// 取 30 秒：既保证「新加线路在 30 秒内就能拿到第一个样本」，又把探测
	// 成本限制在「至多每 30 秒一次额外请求」。
	exploreInterval = 30 * time.Second

	// probeBackoffCap 探测退避的上限。
	//
	// 对抗审查指出的问题：无退避时，一条黑洞型线路（SYN 无响应，dial 要
	// 等满 10 秒）会每 30 秒吃掉一个**真实用户请求**并让它多等 10 秒，
	// 永不收敛。按累计探测失败次数指数退避（30s→60s→120s→…）把长期
	// 代价压到可接受，同时保留「线路恢复后能被发现」的能力。
	//
	// 封顶 10 分钟：再长的退避意味着一条恢复的线路要等太久才回到候选，
	// 而它本来可能正是最优的那条。
	probeBackoffCap = 10 * time.Minute
)

// Candidate 一条参与择路的线路（Route 本体 + 它的质量模型）。
//
// Route 是配置（用户可编辑、可持久化），Model 是运行时观测（不落盘）。
// 两者绑在一个 Candidate 里只是为了让 Picker 的签名简单；它们的生命周期
// 与可变性完全不同，所以刻意不合并成一个 struct 的方法。
type Candidate struct {
	Route Route
	Model *Model

	// lastProbe 该线路最近一次被「探测性选中」的时刻（见文件头的探索语义）。
	// 零值表示从未探测过——因此新加入的线路会立即获得一次机会。
	lastProbe time.Time

	// probeFailures 该线路累计的「探测失败」次数，用于指数退避。
	// 成功拿到样本后不再走探测通道，这个计数自然失去意义（不必清零）。
	probeFailures int
}

// Picker 跨线路择路（并发安全）。
type Picker struct {
	mu     sync.Mutex
	cands  []*Candidate
	byID   map[string]*Candidate
	picked map[string]int64 // 每条线路被选中的次数（观测用）
}

// NewPicker 用一组线路构建。空列表返回一个「永远选不出」的 Picker
// （Select 返回 ok=false），由上层给出可读错误。
func NewPicker(routes Set) *Picker {
	p := &Picker{
		byID:   make(map[string]*Candidate, len(routes)),
		picked: make(map[string]int64, len(routes)),
	}
	for _, r := range routes {
		r = r.Normalize()
		c := &Candidate{Route: r, Model: NewModel()}
		p.cands = append(p.cands, c)
		p.byID[r.ID] = c
	}
	return p
}

// Candidate 按 ID 取一条候选（供上层在请求结束后回写样本）。
func (p *Picker) Candidate(id string) *Candidate {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.byID[id]
}

// All 返回全部候选的快照（供 UI 展示质量）。
func (p *Picker) All() []*Candidate {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*Candidate, len(p.cands))
	copy(out, p.cands)
	return out
}

// Choice 一次择路的结果。
type Choice struct {
	Route     Route
	Estimate  time.Duration // 用于比较的估计值（含惩罚与探索因子）
	Known     bool          // 该线路是否有足够样本
	Reason    string        // 人可读的选择理由（写进日志，供事后解释）
	RankIndex int           // 在本次比较中的名次（0 = 最优）
}

// Select 为一次「预计要搬 bytes 字节」的请求择路。
//
// bytes 传 0 或负数时按「未知体积」处理：此时只用已知线路里 est 最小的，
// 未知线路不参与（没有体积就无从计算 est，强行参与等于瞎猜）。
//
// 返回值 ok=false 只在**没有任何启用线路**时出现——不是「全部线路都差」。
// 全部线路都差时必须仍然返回一条（保底），这是 owner 的明确裁决。
func (p *Picker) Select(bytes int64) (Choice, bool) {
	return p.selectAt(bytes, time.Now(), nil)
}

// SelectExcluding 与 Select 相同，但跳过 excluded 里的线路。
//
// 用途：一次请求内的重试循环必须做到「每条线路最多试一次」。没有这个参数时
// 一条从未成功过的线路（样本数 0）会永远走探测通道被优先选中——失败不进
// 拟合、样本仍是 0——于是同一次请求里被反复打同一条死线路。
// （这是真实 e2e 抓出的缺陷，不是推演出来的。）
//
// 边界：excluded 覆盖全部启用线路时返回 ok=false，调用方据此报错，
// 而不是静默拿一条已试过的线路重来。
func (p *Picker) SelectExcluding(bytes int64, excluded map[string]bool) (Choice, bool) {
	return p.selectAt(bytes, time.Now(), excluded)
}

// selectAt 是 Select 的实现，时钟与排除集合都可注入（测试用）。
//
// 为什么把时间作为参数而不是内部取 now()：探测门控的时间语义必须可测。
// 用真实时钟时，测「30 秒后再探测」要么 sleep 30 秒（测试慢到没人跑），
// 要么放过这个行为不验。注入时钟让它是纯粹的纯函数级测试。
func (p *Picker) selectAt(bytes int64, now time.Time, excluded map[string]bool) (Choice, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.cands) == 0 {
		return Choice{}, false
	}
	if p.picked == nil {
		p.picked = make(map[string]int64, len(p.cands))
	}

	// ── 优先：时间门控的主动探测（见文件头「探索与保底」）──
	//
	// 只探测「从未成功过」的线路（样本为空）。已经有过成功样本的线路即使
	// 当前被降权，也走正常 est 比较——它不算「未知」，只是「暂时不行」。
	for _, c := range p.cands {
		if excluded[c.Route.ID] {
			continue // 本次请求已经试过它了（见 SelectExcluding）
		}
		// 判据是「能否拟合」而不是「有没有样本」（规格审查 F1，P1）：
		//
		// 原来用 n > 0，于是 1~2 条样本的线路两头不占——不满足探测条件
		// （样本非空），也不满足 est 条件（Estimate 需要 >=minSamples=3），
		// 只能落到「已知最优 ×1.35」的兜底估值，永远排在别人后面。
		// 而「线路刚恢复、成功过一次又停摆」正好是这个形态，于是它永久
		// 饿死、再也补不到第 3 条样本。
		//
		// 改成用 Model.Known()（= 拟合是否可用）：只要还不够拟合，就走
		// 探测通道（仍受退避门控，代价有界）。
		if c.Model.Known() {
			continue
		}
		// 退避门控：连续探测失败的线路拉长重试间隔（见 probeBackoffCap）
		if gap := c.probeGap(); gap > 0 && !c.lastProbe.IsZero() && now.Sub(c.lastProbe) < gap {
			continue
		}
		c.lastProbe = now
		p.picked[c.Route.ID]++
		return Choice{
			Route:    c.Route,
			Known:    false,
			Reason:   "主动探测（该线路尚无样本，按间隔重试以采集数据）",
			Estimate: 0,
		}, true
	}

	// 保留集：排除掉的线路不参与下面的 est 比较。若排除后一条不剩，
	// 返回 ok=false（调用方据此报「本次请求所有线路都试过了」）。
	live := make([]*Candidate, 0, len(p.cands))
	for _, c := range p.cands {
		if !excluded[c.Route.ID] {
			live = append(live, c)
		}
	}
	if len(live) == 0 {
		return Choice{}, false
	}

	// 先求已知线路里最优的原始 est，作为未知线路的兜底基准。
	bestKnown := math.Inf(1)
	if bytes > 0 {
		for _, c := range live {
			if est, ok := c.Model.Estimate(bytes); ok {
				if raw := msOf(est); raw < bestKnown {
					bestKnown = raw
				}
			}
		}
	}

	evals := make([]eval, 0, len(live))
	for _, c := range live {
		e := eval{c: c}

		if bytes <= 0 {
			// 未知体积：只能用已有样本的「典型耗时」排序。
			if a, bw, ok := c.Model.Params(); ok {
				e.est = a + 1024.0/bw // 1 KB 参照点，仅用于排序
				e.known = true
			} else if math.IsInf(bestKnown, 0) {
				e.est = 0 // 全都没样本：平手，按配置顺序裁决
			} else {
				e.est = bestKnown
			}
		} else if est, ok := c.Model.Estimate(bytes); ok {
			e.est = msOf(est)
			e.known = true
		} else {
			// 仍无样本（且本轮未轮到探测）：给它一个「不比已知最优更好」的
			// 估值，排在已知线路之后，但**不排除**（保底语义）。
			if math.IsInf(bestKnown, 0) || bestKnown <= 0 {
				e.est = 0 // 全都没样本：平手，按配置顺序
			} else {
				e.est = bestKnown * 1.35
			}
		}

		// NaN 一律视为最差（但**不排除**：保底语义要求它仍然可被选中）
		if math.IsNaN(e.est) {
			e.est = math.Inf(1)
		}

		// 降权：连续失败放大 est；有上限（保底语义，见 failCap 注释）。
		//
		// 用 EffectiveFails（带时间衰减）而不是原始计数：只看原始值会让
		// 惩罚封顶的线路永久出局——它再也拿不到「成功一次」的机会来清零，
		// 效果上等于硬熔断（owner 明确禁止；规格审查 F4）。
		if fails := c.Model.EffectiveFails(); fails > 0 {
			penalty := 1.0 + floatStep*fails
			if penalty > failCap {
				penalty = failCap
			}
			e.est *= penalty
		}
		evals = append(evals, e)
	}

	// 选最小；相等时先出现的胜出（配置顺序 = 用户设定的优先级）。
	// 严格小于比较天然实现「稳定」：同分不替换，所以顺序可复现。
	best := 0
	for i := 1; i < len(evals); i++ {
		if evals[i].est < evals[best].est {
			best = i
		}
	}

	winner := evals[best]
	p.picked[winner.c.Route.ID]++

	return Choice{
		Route:     winner.c.Route,
		Estimate:  time.Duration(winner.est * float64(time.Millisecond)),
		Known:     winner.known,
		Reason:    p.reasonLocked(winner, len(evals), bytes),
		RankIndex: best,
	}, true
}

// probeGap 该线路当前的探测间隔（按累计失败次数指数退避，封顶 probeBackoffCap）。
//
// 指数而非线性：黑洞型线路的失败代价是「用户请求等满 dial 超时」，
// 线性退避（30s→60s→90s）压不住；指数能在一小时内把代价降到分钟级。
func (c *Candidate) probeGap() time.Duration {
	if c.probeFailures <= 0 {
		return exploreInterval
	}
	// 30s × 2^k，k = 失败次数；用位移避免 math.Pow 的浮点
	shift := c.probeFailures
	if shift > 10 {
		shift = 10 // 30s × 2^10 = 8.5h，已远超 cap，先夹住防溢出
	}
	g := exploreInterval * time.Duration(1<<uint(shift))
	if g > probeBackoffCap {
		g = probeBackoffCap
	}
	return g
}

// NoteProbeFailure 记一次「探测失败」——由调用方在「这条线路是被探测选中的
// 且请求最终失败了」时调用。
//
// 为什么要显式区分「探测失败」与普通失败：只有探测失败需要驱动退避。
// 已知线路的普通失败已经由 ObservationFailure 的连续计数降权，不需要
// 再拉长什么间隔（它本来就在正常候选里比较）。
func (p *Picker) NoteProbeFailure(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.byID[id]; ok && c.probeFailures < 32 {
		c.probeFailures++
	}
}

// msOf 把 Duration 转成毫秒浮点（内部比较统一用 ms）。
func msOf(d time.Duration) float64 { return float64(d.Microseconds()) / 1000.0 }

// floatStep 是 failStep 的 float 形式（避免每次比较时转换）。
const floatStep = float64(failStep)

// reasonLocked 生成人可读的选择理由。
//
// 为什么值得花代码写这个：这条理由会进 per-request 日志。事后回答
// 「当时为什么走这条」时，没有它就只能靠猜模型状态——而模型状态是易变的，
// 日志里的理由字符串才是可复盘的证据。
func (p *Picker) reasonLocked(w eval, n int, bytes int64) string {
	if n == 1 {
		return "唯一线路"
	}
	fails := w.c.Model.EffectiveFails()
	switch {
	case !w.known && bytes > 0:
		return "无样本，按已知最优×1.35 估算"
	case fails > 0:
		pen := 1.0 + floatStep*fails
		if pen > failCap {
			pen = failCap
		}
		return fmt.Sprintf("est 最低且已降权（有效失败 %.1f 次 ×%.1f）", fails, pen)
	case bytes <= 0:
		return "体积未知，按典型耗时最低"
	default:
		return "est 最低"
	}
}

// eval 是 pick 内部用的评估结果（提到包级是为了给 reasonLocked 的签名用）。
type eval struct {
	c     *Candidate
	est   float64
	known bool
}

// Snapshot 一条线路的质量快照（供 UI / status / 日志）。
type Snapshot struct {
	ID            string  `json:"id"`
	Kind          string  `json:"kind"`
	Server        string  `json:"server,omitempty"`
	Enabled       bool    `json:"enabled"`
	Known         bool    `json:"known"`
	OverheadMS    float64 `json:"overhead_ms"`
	BandwidthKBps float64 `json:"bandwidth_kbps"`
	Samples       int     `json:"samples"`
	Success       int64   `json:"success"`
	Failure       int64   `json:"failure"`
	ConsecFails   int     `json:"consecutive_fails"`
	SuccessRate   float64 `json:"success_rate"` // -1 表示未知
	Picked        int64   `json:"picked"`
	// Est1MB 以 1 MB 为参照点的预计耗时（ms），让用户直观比较线路
	Est1MB int64 `json:"est_1mb_ms"`
}

// Snapshots 全部线路的质量快照。
func (p *Picker) Snapshots() []Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Snapshot, 0, len(p.cands))
	for _, c := range p.cands {
		a, bw, ok := c.Model.Params()
		samples, succ, fail, consec := c.Model.Stats()
		s := Snapshot{
			ID:            c.Route.ID,
			Kind:          string(c.Route.Kind),
			Server:        c.Route.Server,
			Enabled:       c.Route.IsEnabled(),
			Known:         ok,
			OverheadMS:    a,
			BandwidthKBps: bw,
			Samples:       samples,
			Success:       succ,
			Failure:       fail,
			ConsecFails:   consec,
			SuccessRate:   c.Model.SuccessRate(),
			Picked:        p.picked[c.Route.ID],
		}
		if ok {
			if est, known := c.Model.Estimate(1024 * 1024); known {
				s.Est1MB = est.Milliseconds()
			}
		}
		out = append(out, s)
	}
	return out
}
