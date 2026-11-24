package main

// routes_pane_test.go — 线路面板与配置收集的护栏。
//
// 钉住两件容易回归的事：
//  1. GUI 启动时 MigrateRoutes 恒产出一条线路 → start() 必须把 netMode 的
//     实时探测结果（物理网卡 / 代理 URL）补进线路，否则「直连绕 TUN」失效
//  2. 删除线路保护：不允许删到 0 条（没有线路的客户端起不来）

import (
	"testing"

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

// 添加中继线路：server 必须留空（避免悄悄复制一个同地址的线路）
func TestAddRelayLeavesServerEmpty(t *testing.T) {
	a := &app{server: "magpie.aiweb.xin", settingsPath: t.TempDir() + "/s.json"}
	a.addRoute(route.KindRelay)
	if len(a.routesDraft) != 1 {
		t.Fatal("应新增一条")
	}
	if a.routesDraft[0].Server == "magpie.aiweb.xin" {
		t.Fatal("中继线路不该自动填入同一台 server（它的语义就是指向另一台）")
	}
}

// 添加代理线路：给一个可用的默认 URL（避免一加就是坏配置）
func TestAddProxyHasDefaultURL(t *testing.T) {
	a := &app{server: "magpie.aiweb.xin", settingsPath: t.TempDir() + "/s.json"}
	a.addRoute(route.KindProxy)
	if a.routesDraft[0].Proxy == "" {
		t.Fatal("代理线路应有默认代理地址")
	}
	if err := a.routesDraft.Normalize().Validate(); err != nil {
		t.Fatalf("新加的代理线路应合法：%v", err)
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

// 演示数据必须包含一条停用线路（否则截图无法验证停用态的视觉表达）
func TestDemoRoutesHasDisabledSample(t *testing.T) {
	rs := demoRoutes()
	disabled := 0
	for _, r := range rs {
		if !r.IsEnabled() {
			disabled++
		}
	}
	if disabled != 1 {
		t.Fatalf("演示数据应有 1 条停用线路（供截图验证状态表达），实际 %d 条", disabled)
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
	a.addRoute(route.KindRelay)

	// 刚加出来是禁用的「请填另一台服务器」状态：必须校验失败（诚实）
	if err := a.routesDraft.Normalize().Validate(); err == nil {
		t.Fatal("新加的中继缺 server，校验应当失败以提示用户填写")
	}

	// 模拟用户在编辑区里逐格填入（即编辑区提供的四个字段）
	a.routesDraft[0].Server = "47.117.152.3:28443"
	a.routesDraft[0].ServerName = "magpie.aiweb.xin"
	a.routesDraft[0].LocalAddr = "192.168.1.106"
	a.routesDraft[0].Transport = "tcp"

	rs := a.routesDraft.Normalize()
	if err := rs.Validate(); err != nil {
		t.Fatalf("编辑区能填出的配置必须合法，实际: %v", err)
	}
	// 填进去的值不能在任何一步被清掉（Normalize 不得抹掉显式 SNI/源地址）
	if rs[0].ServerName != "magpie.aiweb.xin" || rs[0].LocalAddr != "192.168.1.106" {
		t.Fatalf("显式字段被改写: %+v", rs[0])
	}
}

// 编辑区改完 → 保存 → 重新加载，四个字段必须一字不差地回来。
//
// 这条守的是「改了等于没改」：Settings.Equal 曾漏比 ServerName，
// 那种缺口在 GUI 里的表现就是用户改完、界面接受、重启后值没了。
func TestRouteEditorFieldsSurviveSaveReload(t *testing.T) {
	dir := t.TempDir()
	a := &app{server: "magpie.aiweb.xin", settingsPath: dir + "/s.json"}
	a.addRoute(route.KindRelay)
	a.routesDraft[0].ID = "上海"
	a.routesDraft[0].Server = "47.117.152.3:28443"
	a.routesDraft[0].ServerName = "magpie.aiweb.xin"
	a.routesDraft[0].LocalAddr = "192.168.1.106"
	a.routesDraft[0].Transport = "tcp"
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
