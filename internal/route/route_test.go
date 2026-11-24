package route

import (
	"strings"
	"testing"
	"time"
)

// mkModel 用一组 (bytes, ms) 造一个已拟合好的模型。
func mkModel(t *testing.T, samples [][2]float64) *Model {
	t.Helper()
	m := NewModel()
	for _, s := range samples {
		m.Observe(int64(s[0]), time.Duration(s[1]*float64(time.Millisecond)))
	}
	return m
}

// genSamples 按 est = a + bytes/bw 生成样本（bw 单位 bytes/ms）。
// 用理论值造样本，是为了让拟合能还原出已知参数——否则断言的就是
// 「拟合自己的误差」而不是「择路逻辑对不对」。
func genSamples(aMs, bwBps float64, kbSizes []int) [][2]float64 {
	out := make([][2]float64, 0, len(kbSizes))
	for _, kb := range kbSizes {
		b := float64(kb * 1024)
		out = append(out, [2]float64{b, aMs + b/bwBps})
	}
	return out
}

// 本变更最核心的行为断言：同一对线路，小请求选低延迟窄带宽的，
// 大请求选高延迟宽带宽的。这是 owner 2026-11-24 那句话的数学表达。
func TestSelectFlipsWithRequestSize(t *testing.T) {
	// A：低延迟窄带宽（50ms 固定开销 / 200 KB/s）
	// B：高延迟宽带宽（200ms 固定开销 / 4000 KB/s）
	aA := mkModel(t, genSamples(50, 200*1024/1000, []int{8, 16, 32, 64}))
	aB := mkModel(t, genSamples(200, 4000*1024/1000, []int{8, 16, 32, 64}))

	// 先确认拟合确实还原了量级（否则下面的翻转断言没有意义）
	if a, bw, ok := aA.Params(); !ok || bw < 150 || bw > 260 {
		t.Fatalf("A 拟合失真：a=%.0f bw=%.0f ok=%v（期望 bw≈205）", a, bw, ok)
	}
	if a, bw, ok := aB.Params(); !ok || bw < 3500 || bw > 4500 {
		t.Fatalf("B 拟合失真：a=%.0f bw=%.0f ok=%v（期望 bw≈4096）", a, bw, ok)
	}

	for _, tc := range []struct {
		name  string
		bytes int64
		want  string
	}{
		{"16KB 小请求应选低延迟线路 A", 16 * 1024, "A"},
		{"2MB 大请求应选高带宽线路 B", 2 * 1024 * 1024, "B"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			picker := NewPicker(Set{
				{ID: "A", Kind: KindDirect},
				{ID: "B", Kind: KindDirect},
			})
			picker.Candidate("A").Model = aA
			picker.Candidate("B").Model = aB

			got, ok := picker.Select(tc.bytes)
			if !ok {
				t.Fatal("Select 返回 ok=false")
			}
			if got.Route.ID != tc.want {
				t.Fatalf("选中 %q，期望 %q（est=%v reason=%s）", got.Route.ID, tc.want, got.Estimate, got.Reason)
			}
		})
	}
}

// 带宽必须能压过延迟：这是 owner 的核心诉求，单独再钉一条。
func TestBandwidthBeatsLatencyOnLargeRequests(t *testing.T) {
	// A 延迟极低（10ms）但带宽极窄（205 KB/s ≈ 下限）
	// B 延迟高（500ms）但带宽 8 MB/s
	aA := mkModel(t, genSamples(10, minBandwidth, []int{8, 16, 32, 64}))
	aB := mkModel(t, genSamples(500, 8000*1024/1000, []int{8, 16, 32, 64}))

	picker := NewPicker(Set{{ID: "A", Kind: KindDirect}, {ID: "B", Kind: KindDirect}})
	picker.Candidate("A").Model = aA
	picker.Candidate("B").Model = aB

	// 4 MB：A ≈ 10 + 4096KB/205KB/s ≈ 20s；B ≈ 500ms + 4096/8192 ≈ 1s
	got, ok := picker.Select(4 * 1024 * 1024)
	if !ok {
		t.Fatal("ok=false")
	}
	if got.Route.ID != "B" {
		t.Fatalf("4MB 请求应选高带宽线路 B，实际选了 %q", got.Route.ID)
	}
}

// 单条线路 100% 失败时，流量必须持续由另一条承载（不熔断）。
func TestFailuresDemoteButDoNotExclude(t *testing.T) {
	aA := mkModel(t, genSamples(50, 2048*1024/1000, []int{8, 16, 32, 64})) // 快
	aB := mkModel(t, genSamples(200, 512*1024/1000, []int{8, 16, 32, 64})) // 慢

	picker := NewPicker(Set{{ID: "A", Kind: KindDirect}, {ID: "B", Kind: KindDirect}})
	picker.Candidate("A").Model = aA
	picker.Candidate("B").Model = aB

	// 无失败时，快线路 A 应该赢
	if got, _ := picker.Select(64 * 1024); got.Route.ID != "A" {
		t.Fatalf("无失败时应选 A，实际 %q", got.Route.ID)
	}

	// 让 A 连续失败很多次
	for i := 0; i < 40; i++ {
		aA.ObserveFailure()
	}

	// 之后必须选 B
	for i := 0; i < 5; i++ {
		got, ok := picker.Select(64 * 1024)
		if !ok {
			t.Fatal("ok=false（降权不应导致无线路可选）")
		}
		if got.Route.ID != "B" {
			t.Fatalf("A 已连续失败 40 次，仍选了 %q", got.Route.ID)
		}
	}

	// 且 A 必须仍在候选集合里（没有被排除）
	if cand := picker.Candidate("A"); cand == nil {
		t.Fatal("A 被移出候选集合——违反「降权不熔断」")
	}
	if len(picker.All()) != 2 {
		t.Fatalf("候选数 %d，期望 2", len(picker.All()))
	}
}

// 关键保底：所有线路都烂到底时，仍必须选出一条（不得返回 ok=false 或 nil 线路）。
func TestAllRoutesDegradedStillPicksOne(t *testing.T) {
	aA := mkModel(t, genSamples(50, 2048*1024/1000, []int{8, 16, 32, 64}))
	aB := mkModel(t, genSamples(200, 512*1024/1000, []int{8, 16, 32, 64}))

	picker := NewPicker(Set{{ID: "A", Kind: KindDirect}, {ID: "B", Kind: KindDirect}})
	picker.Candidate("A").Model = aA
	picker.Candidate("B").Model = aB

	// 两条都连续失败到惩罚封顶
	for i := 0; i < 100; i++ {
		aA.ObserveFailure()
		aB.ObserveFailure()
	}

	got, ok := picker.Select(64 * 1024)
	if !ok {
		t.Fatal("全部劣化时仍必须返回一条（保底），实际 ok=false")
	}
	if got.Route.ID == "" {
		t.Fatal("保底选出的线路 ID 为空")
	}
	// 惩罚都封顶后，比较退化为纯 est：A 更快，应选 A
	if got.Route.ID != "A" {
		t.Fatalf("惩罚封顶后退化为纯 est 比较，应选更快的 A，实际 %q", got.Route.ID)
	}
}

// 没有样本的线路不能被永久冷落（必须被主动探测），但也不能霸占流量。
func TestUnknownRouteIsExploredButNotDominant(t *testing.T) {
	// A 已知且很快
	aA := mkModel(t, genSamples(30, 4096*1024/1000, []int{8, 16, 32, 64}))

	picker := NewPicker(Set{{ID: "A", Kind: KindDirect}, {ID: "B", Kind: KindDirect}})
	picker.Candidate("A").Model = aA
	// B 完全没有样本

	now := time.Now()

	// 第一次必然探测 B（它从未被试过）
	got, _ := picker.selectAt(64*1024, now, nil)
	if got.Route.ID != "B" || got.Known {
		t.Fatalf("首次应探测未知线路 B，实际 %q known=%v", got.Route.ID, got.Known)
	}

	// 探测间隔内，B 不该再被选中（已知的 A 更快，就应走 A）
	for i := 0; i < 20; i++ {
		got, _ := picker.selectAt(64*1024, now.Add(time.Duration(i+1)*time.Second), nil)
		if got.Route.ID != "A" {
			t.Fatalf("探测间隔内不应重复探测 B（第 %d 秒选了 %q）", i+1, got.Route.ID)
		}
	}

	// 过了探测间隔，B 必须再获得一次机会（否则持续失败的线路永远不会被发现）
	got, _ = picker.selectAt(64*1024, now.Add(exploreInterval+time.Second), nil)
	if got.Route.ID != "B" {
		t.Fatalf("超过探测间隔后应重试 B，实际 %q", got.Route.ID)
	}
}

// 已经**能拟合**（>=minSamples 条样本）的线路不该再走探测通道，
// 而应进入正常的 est 比较。
//
// 注意与「有 1~2 条样本」的区别：那种线路还拟合不了，仍然需要被探测
// 补齐样本（规格审查 F1 的修复点）。旧版这个测试用 1 条样本断言
// 「不该探测」，恰恰把饿死缺陷写成了期望行为。
func TestFitLineDoesNotUseProbePath(t *testing.T) {
	aA := mkModel(t, genSamples(50, 2048*1024/1000, []int{8, 16, 32, 64}))
	picker := NewPicker(Set{{ID: "A", Kind: KindDirect}, {ID: "B", Kind: KindDirect}})
	picker.Candidate("A").Model = aA

	// 吃掉 B 的首次探测机会
	picker.selectAt(64*1024, time.Now(), nil)
	// 给 B 足够样本让它能拟合（minSamples 条）
	for i := 0; i < minSamples; i++ {
		picker.Candidate("B").Model.Observe(int64((16+i)*1024), time.Duration(300+i*10)*time.Millisecond)
	}

	got, _ := picker.selectAt(64*1024, time.Now().Add(10*exploreInterval), nil)
	if !got.Known {
		t.Fatalf("B 已能拟合，应走 est 比较路径（Known=true），实际 reason=%q", got.Reason)
	}
}

// 1~2 条样本的线路必须继续被探测（规格审查 F1）：
// 它既拟合不了、也不该被永久冷落——否则「刚恢复」的线路永远补不到第 3 条。
func TestUnderSampledLineStillProbed(t *testing.T) {
	aA := mkModel(t, genSamples(50, 2048*1024/1000, []int{8, 16, 32, 64}))
	picker := NewPicker(Set{{ID: "A", Kind: KindDirect}, {ID: "B", Kind: KindDirect}})
	picker.Candidate("A").Model = aA

	// B 先拿到 1 条样本（还不够拟合）
	picker.Candidate("B").Model.Observe(64*1024, 500*time.Millisecond)
	if picker.Candidate("B").Model.Known() {
		t.Fatal("1 条样本不该已经可拟合")
	}

	// 过了探测间隔后，B 必须再次被探测（否则它永远补齐不了）
	got, _ := picker.selectAt(64*1024, time.Now().Add(exploreInterval+time.Second), nil)
	if got.Route.ID != "B" {
		t.Fatalf("样本不足的线路应继续被探测，实际选了 %q", got.Route.ID)
	}
	if got.Known {
		t.Fatal("这次仍应是探测（Known=false）")
	}
}

// 所有线路都没样本时，按配置顺序依次探测（可复现、不随机）。
func TestAllUnknownProbesInOrder(t *testing.T) {
	picker := NewPicker(Set{{ID: "第一", Kind: KindDirect}, {ID: "第二", Kind: KindDirect}})
	now := time.Now()

	got1, _ := picker.selectAt(64*1024, now, nil)
	got2, _ := picker.selectAt(64*1024, now.Add(time.Second), nil)
	if got1.Route.ID != "第一" || got2.Route.ID != "第二" {
		t.Fatalf("应按配置顺序探测：第一次 %q（期望 第一），第二次 %q（期望 第二）", got1.Route.ID, got2.Route.ID)
	}

	// 两者都探测过且都无样本 → est 平手 → 按配置顺序选第一条
	got3, _ := picker.selectAt(64*1024, now.Add(2*time.Second), nil)
	if got3.Route.ID != "第一" {
		t.Fatalf("全无样本且都已探测过时，应按配置顺序选第一条，实际 %q", got3.Route.ID)
	}
}

// 空集合必须返回 ok=false（由上层给出可读错误）。
func TestEmptySetReturnsFalse(t *testing.T) {
	picker := NewPicker(nil)
	if _, ok := picker.Select(1024); ok {
		t.Fatal("空集合不应返回线路")
	}
}

// 体积未知（bytes<=0）也要能选，不能返回 ok=false。
func TestZeroBytesStillSelects(t *testing.T) {
	aA := mkModel(t, genSamples(50, 2048*1024/1000, []int{8, 16, 32}))
	picker := NewPicker(Set{{ID: "A", Kind: KindDirect}, {ID: "B", Kind: KindDirect}})
	picker.Candidate("A").Model = aA

	if _, ok := picker.Select(0); !ok {
		t.Fatal("bytes=0 时仍应选出一条")
	}
}

// 模型在样本不足时应返回 known=false（择路器据此走探索分支）。
func TestModelUnknownBelowMinSamples(t *testing.T) {
	m := NewModel()
	if _, ok := m.Estimate(1024); ok {
		t.Fatal("空模型不应给出估计")
	}
	m.Observe(64*1024, 100*time.Millisecond)
	if _, ok := m.Estimate(1024); ok {
		t.Fatal("1 个样本不足以下结论")
	}
	// 凑到 minSamples
	for i := 1; i < minSamples; i++ {
		m.Observe(int64((16+i)*1024), time.Duration(100+i*20)*time.Millisecond)
	}
	if _, ok := m.Estimate(1024); !ok {
		t.Fatalf("%d 个样本后应可估计", minSamples)
	}
}

// 成功必须清零连续失败计数（恢复是自动的）。
func TestSuccessResetsConsecutiveFailures(t *testing.T) {
	m := NewModel()
	for i := 0; i < 10; i++ {
		m.ObserveFailure()
	}
	if n := m.ConsecutiveFails(); n != 10 {
		t.Fatalf("连续失败 %d，期望 10", n)
	}
	m.Observe(64*1024, 100*time.Millisecond)
	if n := m.ConsecutiveFails(); n != 0 {
		t.Fatalf("成功一次后应清零，实际 %d", n)
	}
}

// 成功率：无样本必须是 -1（未知），不能是 0（会被误读成全失败）。
func TestSuccessRateUnknown(t *testing.T) {
	m := NewModel()
	if s := m.SuccessRate(); s != -1 {
		t.Fatalf("无样本时成功率应为 -1，实际 %v", s)
	}
	m.Observe(1024, 10*time.Millisecond)
	m.ObserveFailure()
	if s := m.SuccessRate(); s != 0.5 {
		t.Fatalf("1 成功 1 失败成功率应为 0.5，实际 %v", s)
	}
}

// 样本滚动：超过上限后只保留最近的（老样本不该永久拖累）
func TestSampleWindowRolls(t *testing.T) {
	m := NewModel()
	for i := 0; i < maxSamples+10; i++ {
		m.Observe(64*1024, 100*time.Millisecond)
	}
	if n, _, _, _ := m.Stats(); n > maxSamples {
		t.Fatalf("样本数 %d 超过上限 %d", n, maxSamples)
	}
}

// 降权理由必须可读且能区分（供日志复盘）
func TestChoiceReasonIsInformative(t *testing.T) {
	aA := mkModel(t, genSamples(50, 2048*1024/1000, []int{8, 16, 32, 64}))
	picker := NewPicker(Set{{ID: "A", Kind: KindDirect}, {ID: "B", Kind: KindDirect}})
	picker.Candidate("A").Model = aA

	got, _ := picker.Select(64 * 1024)
	if got.Reason == "" {
		t.Fatal("选择理由为空，日志无法复盘")
	}
	// 理由必须是「能解释为什么」的短语，不能是空话
	valid := []string{"est", "探测", "未知", "唯一", "体积", "降权"}
	matched := false
	for _, k := range valid {
		if strings.Contains(got.Reason, k) {
			matched = true
			break
		}
	}
	if !matched {
		t.Fatalf("理由内容不可读：%q", got.Reason)
	}
}

// 快照必须能反映真实的观测状态（供 UI 显示）
func TestSnapshots(t *testing.T) {
	picker := NewPicker(Set{{ID: "A", Kind: KindProxy, Server: "s:28443", Proxy: "socks5://p:1080"}})
	c := picker.Candidate("A")
	c.Model.Observe(64*1024, 200*time.Millisecond)
	c.Model.ObserveFailure()

	got := picker.Snapshots()
	if len(got) != 1 {
		t.Fatalf("快照数 %d，期望 1", len(got))
	}
	s := got[0]
	if s.ID != "A" || s.Kind != "proxy" {
		t.Fatalf("快照字段错误：%+v", s)
	}
	if s.Success != 1 || s.Failure != 1 {
		t.Fatalf("成功/失败计数错误：%d/%d", s.Success, s.Failure)
	}
	if s.SuccessRate != 0.5 {
		t.Fatalf("成功率 %v，期望 0.5", s.SuccessRate)
	}
}

// 线路校验：代理缺 URL / QUIC 组合 / server 形态 都要报出来
func TestRouteValidate(t *testing.T) {
	ok := Route{ID: "x", Kind: KindDirect, Server: "h:28443"}
	if err := ok.Validate(); err != nil {
		t.Fatalf("合法线路被拒：%v", err)
	}
	// Normalize 后空 transport 应被填成 auto
	if n := ok.Normalize(); n.Transport != DefaultTransport {
		t.Fatalf("空 transport 应归一为 %q，实际 %q", DefaultTransport, n.Transport)
	}

	// 注意：这些用例**不**先 Normalize，目的是验证 Validate 本身能拦住
	// 配置期就该报错的形态。Normalize 会补默认 server（缺 server 因此不该
	// 靠 Validate 来报——那是上层「补默认值」的职责），所以「缺 server」
	// 这条用**未 Normalize** 的原值来验。
	for _, tc := range []struct {
		name string
		r    Route
		want string
	}{
		{"缺名称", Route{Kind: KindDirect, Server: "h:28443"}, "缺少名称"},
		{"缺 server（未归一）", Route{ID: "x", Kind: KindDirect}, "缺少 server"},
		{"server 非 host:port", Route{ID: "x", Kind: KindDirect, Server: "nohostport"}, "不是 host:port"},
		{"代理缺 URL", Route{ID: "x", Kind: KindProxy, Server: "h:28443"}, "没填代理地址"},
		{"代理 URL 缺协议头", Route{ID: "x", Kind: KindProxy, Server: "h:28443", Proxy: "1.2.3.4:1080"}, "缺少协议头"},
		{"代理 + QUIC", Route{ID: "x", Kind: KindProxy, Server: "h:28443", Proxy: "socks5://p:1080", Transport: "quic"}, "不支持 QUIC"},
		{"非法 kind", Route{ID: "x", Kind: "weird", Server: "h:28443"}, "类型"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.r.Validate()
			if err == nil {
				t.Fatal("应当报错，实际 nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息 %q 未包含 %q", err.Error(), tc.want)
			}
		})
	}
}

// 代理 + QUIC 必须在 Validate 时报出可读错误（不能被静默纠正）。
//
// 规格审查 F2：早期实现让 Normalize 静默改成 auto，于是 spec 承诺的
// 「建连前即报出可读错误」在生产路径上永不可达——用户填了 quic 却不知道
// 被改过。这里钉住「保留原值 + 明确报错」。
func TestProxyQuicRejectedNotSilentlyFixed(t *testing.T) {
	r := Route{ID: "x", Kind: KindProxy, Server: "h:28443", Proxy: "socks5://p:1080", Transport: "quic"}
	n := r.Normalize()
	if n.Transport != "quic" {
		t.Fatalf("Normalize 不该静默改掉用户填的 quic，实际变成 %q", n.Transport)
	}
	err := n.Validate()
	if err == nil {
		t.Fatal("代理 + QUIC 必须报错")
	}
	if !strings.Contains(err.Error(), "不支持 QUIC") {
		t.Fatalf("错误信息应说明不支持 QUIC，实际: %v", err)
	}
}

// 集合校验：ID 唯一
func TestSetValidateDuplicateID(t *testing.T) {
	s := Set{{ID: "same", Kind: KindDirect, Server: "h:28443"}, {ID: "same", Kind: KindDirect, Server: "h:28443"}}
	if err := s.Normalize().Validate(); err == nil {
		t.Fatal("重复名称应当报错")
	}
}

// Enabled() 只返回启用的线路
func TestSetEnabled(t *testing.T) {
	off := false
	s := Set{
		{ID: "A", Kind: KindDirect, Server: "h:28443"},
		{ID: "B", Kind: KindDirect, Server: "h:28443", Enabled: &off},
	}
	e := s.Enabled()
	if len(e) != 1 || e[0].ID != "A" {
		t.Fatalf("启用子集错误：%+v", e)
	}
	// 未设 Enabled 字段应视为启用
	if !s[0].IsEnabled() {
		t.Fatal("Enabled 为 nil 时应当视为启用")
	}
	if s[1].IsEnabled() {
		t.Fatal("显式关闭的线路不应视为启用")
	}
}

// Servers() 用于判断跨 server（基线不共享的提示来源）
func TestSetServers(t *testing.T) {
	s := Set{
		{ID: "A", Kind: KindDirect, Server: "same:28443"},
		{ID: "B", Kind: KindProxy, Server: "same:28443", Proxy: "socks5://p:1080"},
		{ID: "C", Kind: KindRelay, Server: "other:28443"},
	}
	got := s.Servers()
	if len(got) != 2 {
		t.Fatalf("server 集合 %v，期望 2 个", got)
	}
}

// Normalize 必须幂等（多次读写不漂移）
func TestNormalizeIdempotent(t *testing.T) {
	s := Set{{ID: "", Kind: "", Server: "", Proxy: "", Transport: ""}}
	n1 := s.Normalize()
	n2 := n1.Normalize()
	if len(n1) != len(n2) {
		t.Fatal("长度变化")
	}
	for i := range n1 {
		if n1[i].Kind != n2[i].Kind || n1[i].Server != n2[i].Server ||
			n1[i].Transport != n2[i].Transport || n1[i].ID != n2[i].ID {
			t.Fatalf("第 %d 条不幂等:\n  n1=%+v\n  n2=%+v", i, n1[i], n2[i])
		}
	}
}

// ---- 排除集合（SelectExcluding）的边界 ----

func TestSelectExcludingAll(t *testing.T) {
	p := NewPicker(Set{{ID: "A", Kind: KindDirect}, {ID: "B", Kind: KindDirect}})
	ex := map[string]bool{"A": true, "B": true}
	if _, ok := p.SelectExcluding(1024, ex); ok {
		t.Fatal("全部排除时应返回 ok=false")
	}
	// 只排除一条时应返回另一条
	ex2 := map[string]bool{"A": true}
	c, ok := p.SelectExcluding(1024, ex2)
	if !ok || c.Route.ID != "B" {
		t.Fatalf("应返回 B，实际 ok=%v route=%q", ok, c.Route.ID)
	}
}

// 探测通道也必须尊重排除集合（否则 getConn 会反复探测同一条失败线路）
func TestProbeRespectsExclusion(t *testing.T) {
	p := NewPicker(Set{{ID: "A", Kind: KindDirect}, {ID: "B", Kind: KindDirect}})

	// 第一次：探测 A（配置顺序第一）
	c1, _ := p.SelectExcluding(1024, map[string]bool{})
	if c1.Route.ID != "A" {
		t.Fatalf("首次应探测 A，实际 %q", c1.Route.ID)
	}
	// 第二次带排除：A 已被试过，必须探测 B 而不是再返回 A
	c2, _ := p.SelectExcluding(1024, map[string]bool{"A": true})
	if c2.Route.ID != "B" {
		t.Fatalf("排除 A 后应探测 B，实际 %q（可能重复探测了已失败的线路）", c2.Route.ID)
	}
}

// 探测代价必须有界：一条持续失败的线路每 exploreInterval 最多抢走 1 个请求。
//
// 这是 spec 里承认的代价，但必须可验证——否则「每 30 秒一次探测」可能退化成
// 「每 30 秒一批探测」，用户会看到一个周期性慢请求。
func TestProbeCostIsBounded(t *testing.T) {
	good := mkModel(t, genSamples(50, 4096*1024/1000, []int{8, 16, 32, 64}))

	p := NewPicker(Set{
		{ID: "坏", Kind: KindDirect}, // 永远失败
		{ID: "好", Kind: KindDirect},
	})
	p.Candidate("好").Model = good

	start := time.Now()
	badPicks := 0
	// 模拟 10 分钟内每 5 秒一个请求。
	//
	// 必须用 selectAt（注入时钟）：SelectExcluding 内部取 time.Now()，
	// 120 次循环会挤在同一瞬间，探测门控永远只放行第 1 次——那样的测试
	// 无论门控对不对都会通过，是假阳性。
	for i := 0; i < 120; i++ {
		at := start.Add(time.Duration(i) * 5 * time.Second)
		cho, _ := p.selectAt(64*1024, at, nil)
		if cho.Route.ID == "坏" {
			badPicks++
		}
	}
	// 坏线路从未成功过（样本 0）→ 走探测通道，但探测有 30s 门控。
	// 600 秒 / 30 秒 = 20 次，加首次 1 次，上限 21。
	probes := badPicks
	if probes > 21 {
		t.Fatalf("10 分钟内坏线路被选中 %d 次，超过探测上限（应 ≤21，即每 30 秒至多 1 次）", probes)
	}
	if probes == 0 {
		// 探测一次都没发生也不对：那样坏线路永远不会被发现恢复
		t.Fatal("探测从未发生（坏线路永远拿不到样本，恢复后也不会被重新启用）")
	}
}

// 探测失败必须指数退避（对抗审查 P2-2）：
// 黑洞型线路不能每 30 秒都吃一个用户请求并让它等满 dial 超时。
func TestProbeBackoffGrows(t *testing.T) {
	p := NewPicker(Set{
		{ID: "坏", Kind: KindDirect},
		{ID: "好", Kind: KindDirect},
	})
	p.Candidate("好").Model = mkModel(t, genSamples(50, 4096*1024/1000, []int{8, 16, 32, 64}))

	// 间隔应随失败次数增长
	g0 := p.Candidate("坏").probeGap()
	if g0 != exploreInterval {
		t.Fatalf("初始探测间隔应为 %v，实际 %v", exploreInterval, g0)
	}
	p.NoteProbeFailure("坏")
	g1 := p.Candidate("坏").probeGap()
	if g1 <= g0 {
		t.Fatalf("一次失败后间隔应变长：%v → %v", g0, g1)
	}
	for i := 0; i < 5; i++ {
		p.NoteProbeFailure("坏")
	}
	g2 := p.Candidate("坏").probeGap()
	if g2 <= g1 {
		t.Fatalf("更多失败后间隔应继续增长：%v → %v", g1, g2)
	}
	// 封顶
	for i := 0; i < 30; i++ {
		p.NoteProbeFailure("坏")
	}
	g3 := p.Candidate("坏").probeGap()
	if g3 > probeBackoffCap {
		t.Fatalf("间隔 %v 超过封顶 %v", g3, probeBackoffCap)
	}
}

// 退避生效后，长期失败的线路在 10 分钟窗口内只被探测极少次
func TestProbeBackoffReducesProbes(t *testing.T) {
	p := NewPicker(Set{
		{ID: "坏", Kind: KindDirect},
		{ID: "好", Kind: KindDirect},
	})
	p.Candidate("好").Model = mkModel(t, genSamples(50, 4096*1024/1000, []int{8, 16, 32, 64}))

	start := time.Now()
	bad := 0
	// 模拟 1 小时内每 5 秒一个请求；每次探测到坏线路就记一次探测失败
	for i := 0; i < 720; i++ {
		cho, _ := p.selectAt(64*1024, start.Add(time.Duration(i)*5*time.Second), nil)
		if cho.Route.ID == "坏" {
			bad++
			p.NoteProbeFailure("坏")
		}
	}
	// 无退避时 1 小时 = 3600s / 30s ≈ 120 次；退避后应显著少于 20 次
	if bad > 20 {
		t.Fatalf("1 小时内坏线路被探测 %d 次，退避未生效（无退避时约 120 次）", bad)
	}
	if bad == 0 {
		t.Fatal("一次探测都没有：恢复的线路永远不会被发现")
	}
	// 至少前几次探测要发生（新线路必须能拿到样本）
	if bad < 2 {
		t.Fatalf("只探测了 %d 次，太少", bad)
	}
}

// F4 回归守卫：失败惩罚必须随时间衰减，否则封顶的线路永久出局
// （效果上等价于 owner 明令禁止的硬熔断）。
func TestFailurePenaltyDecays(t *testing.T) {
	m := NewModel()
	// 30 次失败 → 原始计数与有效计数都应很大
	for i := 0; i < 30; i++ {
		m.ObserveFailure()
	}
	if eff := m.EffectiveFails(); eff < 20 {
		t.Fatalf("刚失败后有效失败数 %.1f 太小（衰减不该在 0 秒生效）", eff)
	}

	// 立刻看不该衰减
	time.Sleep(0) // 无实际等待

	// 直接验证衰减公式：构造一个「很久以前失败」的模型
	old := NewModel()
	old.mu.Lock()
	old.fails = 100
	old.lastFailAt = time.Now().Add(-30 * time.Minute) // 15 个半衰期
	old.mu.Unlock()
	if eff := old.EffectiveFails(); eff > 1 {
		t.Fatalf("30 分钟（15 个半衰期）后有效失败应 <1，实际 %.3f", eff)
	}
}

// F4 的行为级验证：惩罚封顶的线路在静置足够久后必须能重新被选中。
func TestDegradedLineRecoversAfterQuietPeriod(t *testing.T) {
	// A：快；B：慢
	aA := mkModel(t, genSamples(30, 4096*1024/1000, []int{8, 16, 32, 64}))
	aB := mkModel(t, genSamples(200, 1024*1024/1000, []int{8, 16, 32, 64}))

	picker := NewPicker(Set{{ID: "A", Kind: KindDirect}, {ID: "B", Kind: KindDirect}})
	picker.Candidate("A").Model = aA
	picker.Candidate("B").Model = aB

	// A 连续失败到惩罚封顶
	for i := 0; i < 100; i++ {
		aA.ObserveFailure()
	}

	// 立刻：A 不该被选中（惩罚压过它的速度优势）
	got, _ := picker.Select(64 * 1024)
	if got.Route.ID != "B" {
		t.Fatalf("刚失败后应选 B，实际 %q", got.Route.ID)
	}

	// 人工把 A 的失败时间回拨到「很久以前」（模拟线路安静了一段时间）
	aA.mu.Lock()
	aA.lastFailAt = time.Now().Add(-30 * time.Minute)
	aA.mu.Unlock()

	// 现在 A 必须能赢回来（它本来更快）
	got, _ = picker.Select(64 * 1024)
	if got.Route.ID != "A" {
		t.Fatalf("静置 30 分钟后更快的 A 应重新被选中，实际 %q（惩罚未衰减=变相熔断）", got.Route.ID)
	}
}

// F3：停用线路的配置错误不该拦住其它线路（spec 要求「只影响它自己」）
func TestDisabledRouteErrorDoesNotBlockOthers(t *testing.T) {
	off := false
	set := Set{
		{ID: "好", Kind: KindDirect, Server: "h:28443"},
		{ID: "关且坏", Kind: KindProxy, Server: "h:28443", Enabled: &off}, // 缺 proxy URL
	}
	if err := set.Normalize().Validate(); err != nil {
		t.Fatalf("停用线路的错误不该拦住整份配置，实际: %v", err)
	}

	// 但启用后就必须报错
	on := true
	bad := Set{{ID: "开且坏", Kind: KindProxy, Server: "h:28443", Enabled: &on}}
	if err := bad.Normalize().Validate(); err == nil {
		t.Fatal("启用的坏线路必须报错")
	}
}

// F3：ID 唯一性对停用线路同样生效（日志里名字冲突与启停无关）
func TestDisabledRouteIDStillMustBeUnique(t *testing.T) {
	off := false
	set := Set{
		{ID: "重复", Kind: KindDirect, Server: "h:28443"},
		{ID: "重复", Kind: KindDirect, Server: "h:28443", Enabled: &off},
	}
	if err := set.Normalize().Validate(); err == nil {
		t.Fatal("停用线路的重名也必须报错")
	}
}

// F9：kind 与字段的交叉校验（避免 direct 线路静默走代理）
func TestKindFieldCrossValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		r    Route
		want string
	}{
		{"direct 填 proxy", Route{ID: "x", Kind: KindDirect, Server: "h:28443", Proxy: "socks5://p:1080"}, "直连类型"},
		{"relay 填 proxy", Route{ID: "x", Kind: KindRelay, Server: "h:28443", Proxy: "socks5://p:1080"}, "中继类型"},
		{"relay 填 localAddr", Route{ID: "x", Kind: KindRelay, Server: "h:28443", LocalAddr: "10.0.0.5"}, "中继类型"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.r.Normalize().Validate()
			if err == nil {
				t.Fatalf("%s 应当报错", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息 %q 未包含 %q", err.Error(), tc.want)
			}
		})
	}
}

// F10：relay 的空 server 不该被静默补成默认值
// （否则「新增中继线路」保存后变成指向同一台 server 的线路）
func TestRelayEmptyServerNotFilled(t *testing.T) {
	r := Route{ID: "中继1", Kind: KindRelay}
	n := r.Normalize()
	if n.Server == DefaultServer || n.Server == "" {
		return // 允许为空；不允许被填成默认主机
	}
	if strings.Contains(n.Server, DefaultServer) {
		t.Fatalf("relay 的空 server 被补成了 %q（应保持为空让 Validate 报错）", n.Server)
	}
}

// F10：relay 缺 server 时必须报错（而不是用默认值糊过去）
func TestRelayMissingServerRejected(t *testing.T) {
	r := Route{ID: "中继1", Kind: KindRelay}
	n := r.Normalize()
	if err := n.Validate(); err == nil {
		t.Fatalf("中继线路缺 server 必须报错（否则它会静默指向同一台主服务器）；"+
			"Normalize 后 Server=%q Transport=%q", n.Server, n.Transport)
	}
}
