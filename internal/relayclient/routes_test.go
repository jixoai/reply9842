package relayclient

// routes_test.go — 多线路配置的回归与迁移。
//
// 三条要钉住的东西（对应 openspec/specs/transport 的三个 Requirement）：
//  1. 老配置（只有单 server / 单 proxy）必须无损迁移成一条等价线路
//  2. 单线路时的建连参数与旧路径逐字段等价（本变更不得改变既有行为）
//  3. 多线路时每次线路都得到独立 Dialer（粘性状态不互相污染）

import (
	"testing"

	"github.com/jixoai/reply9842/internal/route"
	"github.com/jixoai/reply9842/internal/transport"
)

// 老配置（direct）: 只有 server + localAddr，无 Routes → 合成一条 direct 线路
func TestRoutesMigrateLegacyDirect(t *testing.T) {
	c := Config{Server: "magpie.aiweb.xin", LocalAddr: "192.168.1.106", Transport: "auto"}
	c.Normalize()

	if len(c.Routes) != 1 {
		t.Fatalf("应合成 1 条线路，实际 %d", len(c.Routes))
	}
	r := c.Routes[0]
	if r.Kind != route.KindDirect {
		t.Fatalf("kind 应为 direct，实际 %q", r.Kind)
	}
	if r.LocalAddr != "192.168.1.106" {
		t.Fatalf("源地址丢失：%q", r.LocalAddr)
	}
	if r.Server != "magpie.aiweb.xin:28443" {
		t.Fatalf("server 应为 magpie.aiweb.xin:28443，实际 %q", r.Server)
	}
	if r.Transport != "auto" {
		t.Fatalf("transport 应为 auto，实际 %q", r.Transport)
	}
	if !r.IsEnabled() {
		t.Fatal("合成线路应默认启用")
	}
}

// 老配置（proxy）: 有 proxy URL → 合成一条 proxy 线路，且 URL 原样保留
func TestRoutesMigrateLegacyProxy(t *testing.T) {
	c := Config{Server: "magpie.aiweb.xin", Proxy: "socks5://user:pass@10.0.0.1:1080"}
	c.Normalize()

	if len(c.Routes) != 1 {
		t.Fatalf("应合成 1 条线路，实际 %d", len(c.Routes))
	}
	r := c.Routes[0]
	if r.Kind != route.KindProxy {
		t.Fatalf("kind 应为 proxy，实际 %q", r.Kind)
	}
	if r.Proxy != "socks5://user:pass@10.0.0.1:1080" {
		t.Fatalf("代理 URL 丢失或改写：%q", r.Proxy)
	}
	// 代理 + 显式 QUIC：保留原值但必须非法（Validate 报错），
	// 而不是被静默改掉（规格审查 F2）
	c2 := Config{Server: "h", Proxy: "socks5://p:1080", Transport: "quic"}
	c2.Normalize()
	if got := c2.Routes[0].Transport; got != "quic" {
		t.Fatalf("不该静默改掉用户填的 quic，实际变成 %q", got)
	}
	if err := c2.Routes.Validate(); err == nil {
		t.Fatal("代理 + QUIC 应当校验失败")
	}
}

// 迁移必须幂等：反复 Normalize 不漂移
func TestRoutesNormalizeIdempotent(t *testing.T) {
	c := Config{Server: "magpie.aiweb.xin", Proxy: ""}
	c.Normalize()
	first := []route.Route(c.Routes)

	for i := 0; i < 5; i++ {
		c.Normalize()
	}
	if len(c.Routes) != len(first) {
		t.Fatalf("反复 Normalize 改变了线路数：%d → %d", len(first), len(c.Routes))
	}
	for i := range first {
		if c.Routes[i].ID != first[i].ID || c.Routes[i].Kind != first[i].Kind ||
			c.Routes[i].Server != first[i].Server || c.Routes[i].Transport != first[i].Transport {
			t.Fatalf("第 %d 条线路漂移：\n  首次=%+v\n  后来=%+v", i, first[i], c.Routes[i])
		}
	}
}

// 显式给了 Routes 时，不得被合成逻辑覆盖
func TestRoutesExplicitNotOverridden(t *testing.T) {
	c := Config{
		Server: "fallback.example:28443",
		Routes: route.Set{
			{ID: "主", Kind: route.KindDirect},
			{ID: "备", Kind: route.KindRelay, Server: "other.example:28443"},
		},
	}
	c.Normalize()

	if len(c.Routes) != 2 {
		t.Fatalf("显式 2 条线路被改成 %d 条", len(c.Routes))
	}
	// 缺 server 的那条应继承全局，而不是被填成默认域名
	if c.Routes[0].Server != "fallback.example:28443" {
		t.Fatalf("缺 server 的线路应继承全局 server，实际 %q", c.Routes[0].Server)
	}
	if c.Routes[1].Server != "other.example:28443" {
		t.Fatalf("中继线路的自有 server 不应被覆盖，实际 %q", c.Routes[1].Server)
	}
}

// 单线路时，DialerSpec 与旧 Dialer 构造逐字段等价
func TestSingleRouteSpecMatchesLegacyDialer(t *testing.T) {
	c := Config{
		Server: "magpie.aiweb.xin", ServerName: "magpie.aiweb.xin",
		LocalAddr: "10.1.1.1", Transport: "auto", Insecure: true,
	}
	c.Normalize()

	// 先用旧方式构造（模拟变更前的代码路径）
	legacy := &transport.Dialer{
		Addr: c.Server, ServerName: c.ServerName, LocalAddr: c.LocalAddr,
		Proxy: c.Proxy, Insecure: c.Insecure, Mode: transport.KindAuto,
	}

	// 再用新方式（routes → spec）
	sf, err := specsFor(c)
	if err != nil {
		t.Fatalf("specsFor: %v", err)
	}
	if len(sf) != 1 {
		t.Fatalf("单线路应产 1 个 spec，实际 %d", len(sf))
	}
	s := sf[0]
	if s.Addr != legacy.Addr || s.ServerName != legacy.ServerName ||
		s.LocalAddr != legacy.LocalAddr || s.Proxy != legacy.Proxy ||
		s.Insecure != legacy.Insecure || s.Mode != legacy.Mode {
		t.Fatalf("新旧构造不等价：\n  legacy=%+v\n  spec  =%+v", legacy, s)
	}
}

// 多线路：每条线路一个独立 Dialer 实例（粘性 prefer 状态不能共享）
func TestMultiRouteIndependentDialers(t *testing.T) {
	specs := []transport.DialerSpec{
		{ID: "A", Addr: "a:28443", Mode: transport.KindAuto},
		{ID: "B", Addr: "b:28443", Mode: transport.KindAuto},
	}
	dA := specs[0].NewDialer()
	dB := specs[1].NewDialer()
	if dA == dB {
		t.Fatal("不同线路必须是独立的 Dialer 实例")
	}
	// 指针不同还不够，要确认它们是不同的对象（不是同一个字段的别名）
	if dA.Addr == dB.Addr {
		t.Fatal("Addr 应各自独立")
	}
}

// 代理线路的 QUIC 请求必须在 spec 阶段就被纠正（不能等建连才报错）
// 代理 + QUIC 必须在配置层面被判为非法（规格审查 F2）。
//
// 早期实现让 config 层静默改成 auto，于是「建连前即报出可读错误」的承诺
// 在生产路径上不可达。现在由 route.Validate 报错，specsFor 之前就会被拦。
func TestProxyRouteQuicIsRejected(t *testing.T) {
	c := Config{
		Routes: route.Set{
			{ID: "代理", Kind: route.KindProxy, Proxy: "socks5://p:1080", Transport: "quic"},
		},
	}
	c.Normalize()

	if err := c.Routes.Validate(); err == nil {
		t.Fatal("代理 + QUIC 应当校验失败（而不是被静默纠正）")
	}
	if got := c.Routes[0].Transport; got != "quic" {
		t.Fatalf("不该静默改掉用户填的 quic，实际变成 %q", got)
	}
}

// 禁用的线路不产生 spec（也不参与择路）
func TestDisabledRouteExcluded(t *testing.T) {
	off := false
	c := Config{
		Routes: route.Set{
			{ID: "开", Kind: route.KindDirect, Server: "a:28443"},
			{ID: "关", Kind: route.KindDirect, Server: "b:28443", Enabled: &off},
		},
	}
	c.Normalize()

	sf, err := specsFor(c)
	if err != nil {
		t.Fatalf("specsFor: %v", err)
	}
	if len(sf) != 1 || sf[0].ID != "开" {
		t.Fatalf("只应有「开」一个 spec，实际 %+v", sf)
	}
}

// 三条线路的完整配置（直连 + 两个代理）必须被接受
func TestThreeRoutesMixedAccepted(t *testing.T) {
	c := Config{
		Routes: route.Set{
			{ID: "直连", Kind: route.KindDirect, Server: "magpie.aiweb.xin"},
			{ID: "代理1", Kind: route.KindProxy, Server: "magpie.aiweb.xin", Proxy: "http://127.0.0.1:7890"},
			{ID: "代理2", Kind: route.KindProxy, Server: "magpie.aiweb.xin", Proxy: "socks5://127.0.0.1:1081"},
		},
	}
	c.Normalize()

	if err := c.Routes.Validate(); err != nil {
		t.Fatalf("三条混搭线路应合法：%v", err)
	}
	sf, err := specsFor(c)
	if err != nil {
		t.Fatalf("specsFor: %v", err)
	}
	if len(sf) != 3 {
		t.Fatalf("应 3 个 spec，实际 %d", len(sf))
	}
	// 全部指向同一 server 时，不该触发「跨 server」提示
	if got := c.Routes.Servers(); len(got) != 1 {
		t.Fatalf("同 server 三条线路的 Servers() 应只有 1 个，实际 %v", got)
	}
}

// 跨 server（含 relay）必须被识别出来（GUI/日志据此提示基线不共享）
func TestCrossServerDetected(t *testing.T) {
	c := Config{
		Routes: route.Set{
			{ID: "直连", Kind: route.KindDirect, Server: "magpie.aiweb.xin"},
			{ID: "中继", Kind: route.KindRelay, Server: "118.26.39.57:28443"},
		},
	}
	c.Normalize()
	if got := c.Routes.Servers(); len(got) != 2 {
		t.Fatalf("跨 server 应识别出 2 个，实际 %v", got)
	}
}

// 所有线路都禁用 → specsFor 必须报错（不能静默启动一个没有出口的客户端）
func TestAllRoutesDisabledRejected(t *testing.T) {
	off := false
	c := Config{
		Routes: route.Set{
			{ID: "A", Kind: route.KindDirect, Server: "a:28443", Enabled: &off},
		},
	}
	c.Normalize()
	if _, err := specsFor(c); err == nil {
		t.Fatal("全部线路禁用时应当报错")
	}
}

// 全局 -local-addr 必须继承到每条 direct 线路（用 -routes-file 时的真实缺陷）。
//
// 实测：`-routes-file + -local-addr` 组合曾连不上（502/EOF），根因是
// normalizeRoutes 在 Routes 非空的分支里只继承 Server/Transport，漏了 LocalAddr。
func TestGlobalLocalAddrInheritedByRoutes(t *testing.T) {
	c := Config{
		LocalAddr: "192.168.1.106",
		Routes: route.Set{
			{ID: "A", Kind: route.KindDirect, Server: "h:28443"},
			{ID: "B", Kind: route.KindRelay, Server: "h2:28443"},
		},
	}
	c.Normalize()

	if c.Routes[0].LocalAddr != "192.168.1.106" {
		t.Fatalf("direct 线路应继承全局源地址，实际 %q", c.Routes[0].LocalAddr)
	}
	// relay 不该被强塞源地址（Validate 会拒绝 relay+localAddr）
	if c.Routes[1].LocalAddr != "" {
		t.Fatalf("relay 线路不该继承源地址，实际 %q", c.Routes[1].LocalAddr)
	}
	if err := c.Routes.Validate(); err != nil {
		t.Fatalf("继承后应合法：%v", err)
	}
}

// 用户逐条指定的源地址优先于全局
func TestPerRouteLocalAddrWins(t *testing.T) {
	c := Config{
		LocalAddr: "10.0.0.1",
		Routes: route.Set{
			{ID: "A", Kind: route.KindDirect, Server: "h:28443", LocalAddr: "10.0.0.2"},
		},
	}
	c.Normalize()
	if c.Routes[0].LocalAddr != "10.0.0.2" {
		t.Fatalf("逐条指定的地址被全局覆盖了：%q", c.Routes[0].LocalAddr)
	}
}

// 全局 -proxy 只继承到 proxy 线路，不会被 direct 吃掉（否则静默走代理）
func TestGlobalProxyOnlyInheritedByProxyRoutes(t *testing.T) {
	c := Config{
		Proxy: "socks5://p:1080",
		Routes: route.Set{
			{ID: "P", Kind: route.KindProxy, Server: "h:28443"},
			{ID: "D", Kind: route.KindDirect, Server: "h:28443"},
		},
	}
	c.Normalize()
	if c.Routes[0].Proxy != "socks5://p:1080" {
		t.Fatalf("proxy 线路应继承全局代理，实际 %q", c.Routes[0].Proxy)
	}
	if c.Routes[1].Proxy != "" {
		t.Fatalf("direct 线路不该继承代理，实际 %q", c.Routes[1].Proxy)
	}
	if err := c.Routes.Validate(); err != nil {
		t.Fatalf("应合法：%v", err)
	}
}
