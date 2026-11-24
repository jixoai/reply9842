package main

// routes_pane_test.go — 线路面板与配置收集的护栏。
//
// 钉住两件容易回归的事：
//  1. GUI 启动时 MigrateRoutes 恒产出一条线路 → start() 必须把 netMode 的
//     实时探测结果（物理网卡 / 代理 URL）补进线路，否则「直连绕 TUN」失效
//  2. 删除线路保护：不允许删到 0 条（没有线路的客户端起不来）

import (
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"

	"github.com/jixoai/reply9842/internal/relayclient"
	"github.com/jixoai/reply9842/internal/route"
)

// 直连模式下，从旧配置迁移出来的线路应被补上物理网卡地址
func TestDirectModeFillsLocalAddr(t *testing.T) {
	a := &app{
		netMode:     relayclient.NetworkDirect,
		routesDraft: route.Set{{ID: "默认线路", Kind: route.KindDirect, Server: "h:28443"}},
	}
	// 模拟 start() 里那段补值逻辑（与 main.go 的实现保持同一语义）
	cfgLocalAddr := "192.168.1.106"
	rs := a.routesDraft.Normalize()
	for i := range rs {
		if rs[i].Kind == route.KindDirect && rs[i].LocalAddr == "" && a.netMode == relayclient.NetworkDirect {
			rs[i].LocalAddr = cfgLocalAddr
		}
	}
	if rs[0].LocalAddr != cfgLocalAddr {
		t.Fatalf("直连线路应被补上源地址，实际 %q", rs[0].LocalAddr)
	}
	// 用户显式填的不能被覆盖
	rs2 := route.Set{{ID: "x", Kind: route.KindDirect, Server: "h:28443", LocalAddr: "10.0.0.5"}}
	for i := range rs2 {
		if rs2[i].Kind == route.KindDirect && rs2[i].LocalAddr == "" && a.netMode == relayclient.NetworkDirect {
			rs2[i].LocalAddr = cfgLocalAddr
		}
	}
	if rs2[0].LocalAddr != "10.0.0.5" {
		t.Fatalf("用户填的源地址被覆盖了：%q", rs2[0].LocalAddr)
	}
}

// 代理模式下，缺 URL 的代理线路应被补上全局 proxyURL
func TestProxyModeFillsProxyURL(t *testing.T) {
	a := &app{
		netMode:     relayclient.NetworkProxy,
		proxyURL:    "socks5://127.0.0.1:1081",
		routesDraft: route.Set{{ID: "代理1", Kind: route.KindProxy, Server: "h:28443"}},
	}
	rs := a.routesDraft.Normalize()
	for i := range rs {
		if rs[i].Kind == route.KindProxy && rs[i].Proxy == "" && a.netMode == relayclient.NetworkProxy {
			rs[i].Proxy = a.proxyURL
		}
	}
	if rs[0].Proxy != a.proxyURL {
		t.Fatalf("代理线路应被补上代理 URL，实际 %q", rs[0].Proxy)
	}
	if err := rs.Validate(); err != nil {
		t.Fatalf("补值后应合法：%v", err)
	}
}

// 补值之后仍缺代理 URL（netMode 不是 proxy）应校验失败，而不是带着坏配置启动
func TestProxyRouteWithoutURLStillFailsValidation(t *testing.T) {
	a := &app{
		netMode:     relayclient.NetworkDirect, // 不是代理模式，补不上
		routesDraft: route.Set{{ID: "代理1", Kind: route.KindProxy, Server: "h:28443"}},
	}
	rs := a.routesDraft.Normalize()
	for i := range rs {
		if rs[i].Kind == route.KindProxy && rs[i].Proxy == "" && a.netMode == relayclient.NetworkProxy {
			rs[i].Proxy = a.proxyURL
		}
	}
	if err := rs.Validate(); err == nil {
		t.Fatal("缺代理 URL 的代理线路必须校验失败（否则启动后才报错）")
	}
}

// 删除保护：最后一条不许删
func TestRemoveRouteKeepsAtLeastOne(t *testing.T) {
	a := &app{routesDraft: route.Set{{ID: "唯一的", Kind: route.KindDirect, Server: "h:28443"}}}
	a.settingsPath = t.TempDir() + "/s.json"
	a.removeRoute(0)
	if len(a.routesDraft) != 1 {
		t.Fatalf("删到 %d 条：最后一条必须保留", len(a.routesDraft))
	}
	if a.notice == "" {
		t.Fatal("应当给出「至少保留一条线路」的提示")
	}
}

// 删除中间一条：剩下的顺序正确
func TestRemoveRouteMiddle(t *testing.T) {
	a := &app{
		routesDraft: route.Set{
			{ID: "A", Kind: route.KindDirect, Server: "h:28443"},
			{ID: "B", Kind: route.KindDirect, Server: "h:28443"},
			{ID: "C", Kind: route.KindDirect, Server: "h:28443"},
		},
	}
	a.settingsPath = t.TempDir() + "/s.json"
	a.removeRoute(1)
	if len(a.routesDraft) != 2 || a.routesDraft[0].ID != "A" || a.routesDraft[1].ID != "C" {
		t.Fatalf("删中间后应为 A,C，实际 %+v", a.routesDraft)
	}
}

// 新建中继：草稿的 server 必须留空（避免悄悄复制一个同地址的线路）。
func TestAddRelayLeavesServerEmpty(t *testing.T) {
	a := &app{server: "magpie.aiweb.xin", settingsPath: t.TempDir() + "/s.json"}
	a.beginAddRoute(route.KindRelay)
	if a.pendingRoute.Kind != route.KindRelay {
		t.Fatalf("草稿类型应为 relay，实际 %q", a.pendingRoute.Kind)
	}
	if a.pendingRoute.Server == "magpie.aiweb.xin" ||
		strings.Contains(a.pendingRoute.Server, "magpie.aiweb.xin") {
		t.Fatal("中继草稿不该自动填入同一台 server（它的语义就是指向另一台）")
	}
}

// 新建代理：**不预设代理地址**。
//
// 这条测试守的是 owner 2026-11-24 的批评：「添加代理的时候你就直接用 7890
// 端口了」——硬编码一个端口会让用户以为「已经填好了」，而绝大多数人的
// 代理并不在 7890。占位符给示例，字段留空，由用户填真实值。
func TestAddProxyLeavesURLBlank(t *testing.T) {
	a := &app{server: "magpie.aiweb.xin", settingsPath: t.TempDir() + "/s.json"}
	a.beginAddRoute(route.KindProxy)
	if a.pendingRoute.Proxy != "" {
		t.Fatalf("代理草稿不该预设地址，实际 %q", a.pendingRoute.Proxy)
	}
	// 留空时校验必须失败（诚实提示用户填），填上后必须通过
	if err := a.pendingRoute.Normalize().Validate(); err == nil {
		t.Fatal("代理草稿缺地址时应校验失败")
	}
	a.pendingRoute.Proxy = "socks5://127.0.0.1:1081"
	if err := a.pendingRoute.Normalize().Validate(); err != nil {
		t.Fatalf("填好地址后应合法：%v", err)
	}
}

// 质量文案：无样本时显示「未采集」，不是 0（0 会被误读成极快）
func TestRouteQualityTextUnknown(t *testing.T) {
	a := &app{routeSnaps: []route.Snapshot{{ID: "A", Enabled: true, Known: false}}}
	if got := a.routeQualityText("A"); got != "未采集" {
		t.Fatalf("无样本应显示「未采集」，实际 %q", got)
	}
}

// 质量文案：有样本时包含 1MB 预计耗时（用户关心的是「传完要多久」）
func TestRouteQualityTextShowsEstimate(t *testing.T) {
	a := &app{routeSnaps: []route.Snapshot{{
		ID: "A", Enabled: true, Known: true, Est1MB: 2500,
		BandwidthKBps: 4096, SuccessRate: 0.9,
	}}}
	got := a.routeQualityText("A")
	if got == "" || got == "未采集" {
		t.Fatalf("应有质量文案，实际 %q", got)
	}
	// 必须体现「时间」而不是只有带宽——那是 owner 的核心诉求
	if !containsStr(got, "1MB") {
		t.Fatalf("应包含 1MB 预计耗时，实际 %q", got)
	}
}

// 停用线路的文案
func TestRouteQualityTextDisabled(t *testing.T) {
	a := &app{routeSnaps: []route.Snapshot{{ID: "A", Enabled: false}}}
	if got := a.routeQualityText("A"); got != "已停用" {
		t.Fatalf("应显示「已停用」，实际 %q", got)
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// 折叠摘要的启用计数必须与实际状态一致（视觉复核抓到过「演示数据里第三条
// 其实没停用，但摘要写 3 启用」——那是夹具错误；这里钉住计数逻辑本身）。
func TestEnabledCountMatchesState(t *testing.T) {
	off := false
	rs := route.Set{
		{ID: "A", Kind: route.KindDirect, Server: "h:28443"},
		{ID: "B", Kind: route.KindDirect, Server: "h:28443"},
		{ID: "C", Kind: route.KindRelay, Server: "h2:28443", Enabled: &off},
	}
	n, enabled := len(rs), 0
	for _, r := range rs {
		if r.IsEnabled() {
			enabled++
		}
	}
	if n != 3 || enabled != 2 {
		t.Fatalf("应为 3 条 / 2 启用，实际 %d / %d", n, enabled)
	}
}

// 中继线路同样需要源地址绑定：中继只是服务端再转发一跳，本机到中继这
// 一跳仍是本机外连，TUN 全局模式照样劫持。曾只补 direct，实测 relay
// 线路全程 tls: EOF、永不参与择路。
func TestFillRuntimeFieldsCoversRelay(t *testing.T) {
	rs := route.Set{
		{ID: "香港", Kind: route.KindDirect, Server: "h:28443"},
		{ID: "上海", Kind: route.KindRelay, Server: "47.117.152.3:28443"},
		{ID: "代理", Kind: route.KindProxy, Server: "h:28443", Proxy: "http://p:7890"},
	}
	fillRuntimeRouteFields(rs, relayclient.NetworkDirect, "192.168.1.106", "")
	if rs[0].LocalAddr != "192.168.1.106" {
		t.Fatalf("direct 未补源地址: %+v", rs[0])
	}
	if rs[1].LocalAddr != "192.168.1.106" {
		t.Fatalf("relay 未补源地址（这正是 tls: EOF 的根因）: %+v", rs[1])
	}
	if rs[2].LocalAddr != "" {
		t.Fatalf("proxy 不该补源地址: %+v", rs[2])
	}

	// 用户显式填的不能被覆盖
	rs2 := route.Set{{ID: "x", Kind: route.KindDirect, Server: "h:28443", LocalAddr: "10.0.0.9"}}
	fillRuntimeRouteFields(rs2, relayclient.NetworkDirect, "192.168.1.106", "")
	if rs2[0].LocalAddr != "10.0.0.9" {
		t.Fatalf("显式源地址被覆盖: %+v", rs2[0])
	}

	// 代理模式：只补代理线路
	rs3 := route.Set{
		{ID: "a", Kind: route.KindDirect, Server: "h:28443"},
		{ID: "b", Kind: route.KindProxy, Server: "h:28443"},
	}
	fillRuntimeRouteFields(rs3, relayclient.NetworkProxy, "", "socks5://p:1080")
	if rs3[0].LocalAddr != "" || rs3[0].Proxy != "" {
		t.Fatalf("direct 在代理模式下不该变: %+v", rs3[0])
	}
	if rs3[1].Proxy != "socks5://p:1080" {
		t.Fatalf("proxy 未补代理地址: %+v", rs3[1])
	}
}

// 线路编辑态的零值安全：&app{} 构造出来必须「没有线路在编辑」。
// （这条守的是 +1 编码的意义——用下标语义时零值会误展开第一条。）
func TestEditingRouteZeroValueMeansNone(t *testing.T) {
	var a app
	if got := a.editingRoute(); got != -1 {
		t.Fatalf("零值应表示「无」，实际 %d", got)
	}
	a.setEditingRoute(2)
	if got := a.editingRoute(); got != 2 {
		t.Fatalf("设置后应读回 2，实际 %d", got)
	}
	a.setEditingRoute(-1)
	if got := a.editingRoute(); got != -1 {
		t.Fatalf("清除后应为 -1，实际 %d", got)
	}
}

// 删除线路时编辑态必须跟着修正：否则用户删掉上一条后，
// 编辑区会突然变成另一条线路的字段（改 A 实际改到 B）。
func TestRemoveRouteKeepsEditTarget(t *testing.T) {
	mk := func() *app {
		x := &app{routesDraft: route.Set{
			{ID: "A", Kind: route.KindDirect, Server: "h:28443"},
			{ID: "B", Kind: route.KindDirect, Server: "h:28443"},
			{ID: "C", Kind: route.KindDirect, Server: "h:28443"},
		}}
		x.settingsPath = t.TempDir() + "/s.json"
		return x
	}

	// 删被编辑的那条 → 收起
	a := mk()
	a.setEditingRoute(1)
	a.removeRoute(1)
	if a.editingRoute() != -1 {
		t.Fatalf("删掉正在编辑的线路后应收起，实际 %d", a.editingRoute())
	}

	// 删前面的 → 下标左移，仍指向同一条（原本 index 2 = C）
	a = mk()
	a.setEditingRoute(2)
	a.removeRoute(0)
	if got := a.editingRoute(); got != 1 {
		t.Fatalf("删前面后应指向 1，实际 %d", got)
	}
	if a.routesDraft[1].ID != "C" {
		t.Fatalf("编辑目标应仍是 C，实际 %s", a.routesDraft[1].ID)
	}

	// 删后面的 → 不动
	a = mk()
	a.setEditingRoute(0)
	a.removeRoute(2)
	if got := a.editingRoute(); got != 0 {
		t.Fatalf("删后面不该影响编辑态，实际 %d", got)
	}
}

// 关键回归：新增中继 → 只靠 GUI 能填的字段 → 必须能通过校验。
//
// 这是「加了就废」的防线：中继指向 IP 时 server_name 与 local_addr 都是
// 必填（缺了 x509 失败 / TUN 下 EOF），而此前 GUI 一个输入框都没有，
// 用户没有任何途径填它们。
func TestRelayRouteConfigurableThroughGUI(t *testing.T) {
	a := &app{server: "magpie.aiweb.xin", settingsPath: t.TempDir() + "/s.json"}
	a.beginAddRoute(route.KindRelay)

	// 草稿态：还没有进入 routesDraft，更没有落盘
	if len(a.routesDraft) != 0 {
		t.Fatalf("草稿不该进入线路列表，实际 %d 条", len(a.routesDraft))
	}
	// 中继留空 server：此时必须校验失败（诚实地说「请填另一台服务器」）
	if err := a.pendingRoute.Normalize().Validate(); err == nil {
		t.Fatal("中继草稿缺 server，校验应当失败以提示用户填写")
	}

	// 模拟用户在草稿表单里逐格填入
	a.pendingRoute.Server = "47.117.152.3:28443"
	a.pendingRoute.ServerName = "magpie.aiweb.xin"
	a.pendingRoute.LocalAddr = "192.168.1.106"
	a.pendingRoute.Transport = "tcp"
	a.commitPendingRoute()

	if len(a.routesDraft) != 1 {
		t.Fatalf("添加后应有 1 条线路，实际 %d", len(a.routesDraft))
	}
	rs := a.routesDraft.Normalize()
	if err := rs.Validate(); err != nil {
		t.Fatalf("编辑区能填出的配置必须合法，实际: %v", err)
	}
	// 填进去的值不能在任何一步被清掉（Normalize 不得抹掉显式 SNI/源地址）
	if rs[0].ServerName != "magpie.aiweb.xin" || rs[0].LocalAddr != "192.168.1.106" {
		t.Fatalf("显式字段被改写: %+v", rs[0])
	}
	// 草稿要清空，否则下次点「＋」会看到上一条的残留
	if a.pendingRoute.ID != "" {
		t.Fatalf("提交后草稿应清空，实际 %+v", a.pendingRoute)
	}
}

// 编辑区改完 → 保存 → 重新加载，四个字段必须一字不差地回来。
//
// 这条守的是「改了等于没改」：Settings.Equal 曾漏比 ServerName，
// 那种缺口在 GUI 里的表现就是用户改完、界面接受、重启后值没了。
func TestRouteEditorFieldsSurviveSaveReload(t *testing.T) {
	dir := t.TempDir()
	a := &app{server: "magpie.aiweb.xin", settingsPath: dir + "/s.json"}
	a.beginAddRoute(route.KindRelay)
	a.pendingRoute.ID = "上海"
	a.pendingRoute.Server = "47.117.152.3:28443"
	a.pendingRoute.ServerName = "magpie.aiweb.xin"
	a.pendingRoute.LocalAddr = "192.168.1.106"
	a.pendingRoute.Transport = "tcp"
	a.commitPendingRoute()
	if err := a.saveSettings(); err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	got := relayclient.LoadSettings(a.settingsPath)
	if len(got.Routes) != 1 {
		t.Fatalf("读回线路数 %d", len(got.Routes))
	}
	r := got.Routes[0]
	if r.ID != "上海" || r.Server != "47.117.152.3:28443" ||
		r.ServerName != "magpie.aiweb.xin" ||
		r.LocalAddr != "192.168.1.106" || r.Transport != "tcp" {
		t.Fatalf("字段在往返中丢失: %+v", r)
	}
}

// 「有改动未生效」的判断口径：必须逐字段比，不能只比条数。
//
// owner 2026-11-24：「编辑完是落入一个未应用的配置中，最后实现一键生效」。
// 这条守的是「一键生效」的前置条件——判断错了就会要么一直提示有改动
// （用户以为点了没用），要么改完不提示（用户以为已经生效）。
func TestRoutesDirtyDetectsFieldChange(t *testing.T) {
	// appliedRoutes 在真实路径上恒为 Normalize 后的值（start/applyRoutes 都
	// 先 Normalize 再赋值），所以比较的基准也必须 Normalize——否则测试会
	// 因为「一侧补了默认 Transport、另一侧没有」而误报。
	applied := route.Set{
		{ID: "香港", Kind: route.KindDirect, Server: "magpie.aiweb.xin:28443",
			ServerName: "magpie.aiweb.xin", LocalAddr: "192.168.1.106"},
	}.Normalize()

	// 完全相同 → 不 dirty
	if !applied.Normalize().Equal(applied) {
		t.Fatal("同一份配置应判为相等")
	}

	// 只改 SNI 一个字段 → 必须 dirty（漏比字段是「改了等于没改」的老坑）
	mut := route.Set{{ID: "香港", Kind: route.KindDirect, Server: "magpie.aiweb.xin:28443",
		ServerName: "other.example", LocalAddr: "192.168.1.106"}}
	if mut.Normalize().Equal(applied) {
		t.Fatal("只改 server_name 应判为有改动")
	}

	// 停用一条 → dirty（启停也是生效状态的一部分）
	off := false
	mut2 := route.Set{{ID: "香港", Kind: route.KindDirect, Server: "magpie.aiweb.xin:28443",
		ServerName: "magpie.aiweb.xin", LocalAddr: "192.168.1.106", Enabled: &off}}
	if mut2.Normalize().Equal(applied) {
		t.Fatal("停用线路应判为有改动")
	}
}

// 未运行时不该提示「有改动未生效」：没有正在跑的东西需要生效。
func TestRoutesDirtyFalseWhenNotRunning(t *testing.T) {
	a := &app{routesDraft: route.Set{
		{ID: "A", Kind: route.KindDirect, Server: "changed:28443"},
	}}
	a.appliedRoutes = route.Set{
		{ID: "A", Kind: route.KindDirect, Server: "original:28443"},
	}
	// runner 为 nil → running() 为 false
	if a.routesDirty() {
		t.Fatal("未运行时不该提示有改动未生效（改动会在下次启动生效）")
	}
}

// 草稿取消后必须彻底清空：否则下一次点「＋」会看到上一条的残留字段。
func TestCancelPendingRouteClearsDraft(t *testing.T) {
	a := &app{server: "magpie.aiweb.xin", settingsPath: t.TempDir() + "/s.json"}
	a.beginAddRoute(route.KindProxy)
	a.pendingRoute.Proxy = "http://127.0.0.1:9999"
	a.pendingRoute.ID = "改过的名字"
	a.cancelPendingRoute()

	if a.pendingRoute.ID != "" || a.pendingRoute.Proxy != "" {
		t.Fatalf("取消后草稿应清空，实际 %+v", a.pendingRoute)
	}
	if len(a.routesDraft) != 0 {
		t.Fatal("取消不该往线路列表里加东西")
	}
}

// 草稿的重名会挡住提交（线路名是身份，日志与面板都靠它区分）。
func TestCommitRejectsDuplicateID(t *testing.T) {
	a := &app{server: "magpie.aiweb.xin", settingsPath: t.TempDir() + "/s.json"}
	a.routesDraft = route.Set{{ID: "香港", Kind: route.KindDirect, Server: "h:28443"}}
	a.beginAddRoute(route.KindDirect)
	a.pendingRoute.ID = "香港" // 撞名
	a.pendingRoute.Server = "h2:28443"
	a.commitPendingRoute()

	if len(a.routesDraft) != 1 {
		t.Fatalf("重名不该被加入，实际 %d 条", len(a.routesDraft))
	}
	// 草稿要保留（用户只是改个名就能继续，不该丢弃他填的内容）
	if a.pendingRoute.ID != "香港" {
		t.Fatal("提交失败时草稿应保留，用户可以改名重试")
	}
}

// 草稿态的列表开关文案必须与真实状态一致。
//
// 视觉终验 P1：早期草稿期**强制**收起列表，于是摘要写「列表已收起」、
// 右边按钮还显示「折叠」——点它既不像折叠也不像展开。
// 现在 beginAddRoute 只收起一次，之后允许展开，文案必须跟着状态走。
func TestDraftModeListToggleConsistent(t *testing.T) {
	mk := func() *app {
		x := newTestApp(t)
		x.tab = tabSettings
		x.routesExpanded = true
		x.routesDraft = route.Set{
			{ID: "A", Kind: route.KindDirect, Server: "h:28443"},
		}
		return x
	}

	// 进入草稿：列表被收起一次 → 摘要说「列表已收起」，按钮应是「展开」
	a := mk()
	a.beginAddRoute(route.KindProxy)
	if a.routesExpanded {
		t.Fatal("进入草稿时应收起列表一次")
	}
	tt := ui.NewTester(a.view, defaultWinWidth, defaultWinHeight)
	if !tt.HasText("列表已收起") {
		t.Errorf("收起时应说明「列表已收起」")
	}
	if tt.HasText("折叠") {
		t.Errorf("列表已收起时按钮不该显示「折叠」（应显示「展开」）")
	}
	// **草稿表单必须可见**——这是最容易漏的一条（我自己漏过）：折叠只该跳过
	// 列表，不能连草稿一起跳过。视觉第四轮用像素差抓到过「点＋代理后表单根本
	// 不在屏上，草稿态与折叠态只差摘要行」。
	for _, want := range []string{"新建代理线路", "服务器", "代理地址", "测试连接", "添加", "取消"} {
		if !tt.HasText(want) {
			t.Errorf("草稿态必须能看到 %q，否则用户点完＋代理什么也填不了：%q", want, tt.Texts())
		}
	}

	// 注意：按钮 2026-11-25 已图标化（雪佛龙），HasText 命中的是它的
	// **可访问名**（「展开线路列表」/「折叠线路列表」），不是可见文字。
	// 这是有意的——纯图标对读屏是黑箱，必须带 label；测试顺着这个语义
	// 断言「状态与按钮语义一致」仍然成立。

	// 用户主动展开：摘要不再说「已收起」，按钮变「折叠」
	b := mk()
	b.beginAddRoute(route.KindProxy)
	b.routesExpanded = true
	tt2 := ui.NewTester(b.view, defaultWinWidth, defaultWinHeight)
	if tt2.HasText("列表已收起") {
		t.Errorf("展开后不该再提示「列表已收起」")
	}
	if !tt2.HasText("折叠") {
		t.Errorf("展开后按钮应显示「折叠」")
	}
}

// 草稿期间「还需填写：X」必须点名缺的那一项，而不是只说「不能添加」。
//
// 视觉复验 N2 的延伸要求：禁用按钮只能表达「不能点」，说不出「为什么」。
func TestPendingRouteMissingNamesField(t *testing.T) {
	base := route.Route{ID: "代理1", Kind: route.KindProxy, Server: "magpie.aiweb.xin:28443"}
	if got := pendingRouteMissing(base); !strings.Contains(got, "代理地址") {
		t.Fatalf("缺代理地址时应点名它，实际 %q", got)
	}
	base.Proxy = "127.0.0.1:7890" // 缺协议头
	if got := pendingRouteMissing(base); !strings.Contains(got, "协议头") {
		t.Fatalf("缺协议头时应指出，实际 %q", got)
	}
	base.Proxy = "socks5://127.0.0.1:1080"
	if got := pendingRouteMissing(base); got != "线路名称（不能与现有线路重名）" {
		t.Fatalf("必填都齐了应只剩重名检查，实际 %q", got)
	}

	// 中继指向 IP 时必须点名证书域名
	relay := route.Route{ID: "中继1", Kind: route.KindRelay, Server: "47.117.152.3:28443"}
	if got := pendingRouteMissing(relay); !strings.Contains(got, "证书域名") {
		t.Fatalf("IP 中继缺 SNI 时应点名证书域名，实际 %q", got)
	}
}

// 放弃改动必须真的回到「已应用」那份，且不能与它共享底层数组。
//
// owner 2026-11-25：「立即应用旁边是不是应该给一个取消呢」——有接受就要有
// 放弃，否则想反悔的用户只能手改回去或去翻 JSON。
func TestRevertRoutesRestoresApplied(t *testing.T) {
	applied := route.Set{
		{ID: "香港", Kind: route.KindDirect, Server: "magpie.aiweb.xin:28443",
			ServerName: "magpie.aiweb.xin", LocalAddr: "192.168.1.106"},
	}.Normalize()

	a := &app{settingsPath: t.TempDir() + "/s.json"}
	a.appliedRoutes = applied
	// 用户改了一些东西（还加了一条）
	a.routesDraft = route.Set{
		{ID: "香港", Kind: route.KindDirect, Server: "changed.example:28443"},
		{ID: "新增", Kind: route.KindDirect, Server: "other.example:28443"},
	}
	a.revertRoutes()

	if !a.routesDraft.Normalize().Equal(applied) {
		t.Fatalf("放弃后应回到已应用的配置，实际 %+v", a.routesDraft)
	}

	// 关键：不能共享底层数组——否则之后编辑 routesDraft 会改到 appliedRoutes，
	// 「有没有未应用改动」从此永远判定为否。
	a.routesDraft[0].Server = "mutated:28443"
	if a.appliedRoutes[0].Server == "mutated:28443" {
		t.Fatal("revert 之后两份切片共享底层数组，改一份会污染另一份")
	}
}
