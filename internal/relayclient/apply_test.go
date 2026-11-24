package relayclient

import (
	"context"
	"io"
	"log"
	"net"
	"testing"
	"time"

	"github.com/jixoai/reply9842/internal/route"
)

// discard 一个不打印的 logger：这些测试故意去连不存在的服务器，
// 默认 logger 会把每次重试都刷到测试输出里。
func newSilentRunner(t *testing.T, c Config) *Runner {
	t.Helper()
	dir := t.TempDir()
	c.LogPath = dir + "/client.jsonl"
	c.StatsPath = dir + "/stats.json"
	// :0 让内核分配端口：固定端口可能被本机真实运行的客户端占着。
	c.Listen = "127.0.0.1:0"
	r := New(c)
	r.SetLogger(log.New(io.Discard, "", 0))
	return r
}

// 运行中 ApplyConfig：优雅重启，新线路立刻生效。
//
// owner 2026-11-24：「如果是运行中的就直接接纳新的配置」——这条守的就是
// 这句话。用真实监听与真实 Start/Stop，不 mock 生命周期。
func TestApplyConfigRestartsWhenRunning(t *testing.T) {
	c := Config{
		Server: "magpie.aiweb.xin:28443", Transport: "tcp",
		Routes: route.Set{{ID: "A", Kind: route.KindDirect, Server: "magpie.aiweb.xin:28443"}},
	}
	r := newSilentRunner(t, c)
	if err := r.Start(); err != nil {
		t.Fatalf("首次启动: %v", err)
	}
	if !r.Running() {
		t.Fatal("应处于运行中")
	}

	off := false
	next := r.Config()
	next.Routes = route.Set{
		{ID: "A", Kind: route.KindDirect, Server: "magpie.aiweb.xin:28443"},
		{ID: "B", Kind: route.KindRelay, Server: "127.0.0.1:1", Enabled: &off},
	}
	restarted, err := r.ApplyConfig(next)
	if err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	if !restarted {
		t.Fatal("运行中应用应报告 restarted=true")
	}
	if !r.Running() {
		t.Fatal("应用后必须仍在运行（否则「一键生效」等于把服务停了）")
	}
	if got := len(r.Config().Routes); got != 2 {
		t.Fatalf("新配置未生效：线路数 %d", got)
	}
	// Routes() 只返回**启用**线路的快照（停用线路不参与择路，没有质量数据）。
	// 新配置里 B 是停用的，所以这里应当是 1 条；关键是它不能还是旧配置的 A 独苗
	// ——下面用 Config() 的条数兜住「新配置真的进去了」。
	if got := len(r.Routes()); got != 1 {
		t.Fatalf("启用线路快照应为 1 条（B 停用），实际 %d 条", got)
	}
	if err := r.Stop(context.Background()); err != nil {
		t.Fatalf("停止: %v", err)
	}
}

// 未运行时 ApplyConfig 只换配置，不启动监听。
//
// 语义：用户没在跑的时候改线路，本来就会在下次启动生效；这时把它「顺带启动」
// 会违反「手动停止是明确表态」的既有约定。
func TestApplyConfigWhenStoppedDoesNotStart(t *testing.T) {
	r := newSilentRunner(t, Config{
		Server: "magpie.aiweb.xin:28443", Transport: "tcp",
	})
	next := Config{
		Server: "other.example:28443",
		Routes: route.Set{{ID: "X", Kind: route.KindDirect, Server: "other.example:28443"}},
	}
	restarted, err := r.ApplyConfig(next)
	if err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	if restarted {
		t.Fatal("未运行时不该重启")
	}
	if r.Running() {
		t.Fatal("未运行时 ApplyConfig 不该启动 Runner")
	}
	if got := r.Config().Routes[0].ID; got != "X" {
		t.Fatalf("配置应已更新，实际线路 %q", got)
	}
}

// ApplyConfig 之后监听必须真的在服务——不是「状态显示运行中」而已。
//
// 为什么不发应用层请求（自查后收窄）：那会依赖本机网络环境——这台机器开着
// 代理的 TUN 全局模式，不绑源地址时到上游的包会被劫持，请求只能拿到 502；
// 换句话说「请求通不通」测的是环境，不是 ApplyConfig。CI 上同样会红。
// 应用层的连通性由 `scripts/e2e/multipath.sh`（可控流式上游）负责。
//
// 这里只验证「重启后确实有一个能接受连接的监听」——这才是 ApplyConfig
// 的职责边界，且与网络环境无关。
func TestApplyConfigListenerServesAfterRestart(t *testing.T) {
	r := newSilentRunner(t, Config{
		Server: "magpie.aiweb.xin:28443", Transport: "tcp",
		Routes: route.Set{{ID: "good", Kind: route.KindDirect, Server: "magpie.aiweb.xin:28443"}},
	})
	if err := r.Start(); err != nil {
		t.Fatalf("启动: %v", err)
	}
	defer func() { _ = r.Stop(context.Background()) }()

	on := true
	next := r.Config()
	next.Routes = route.Set{
		{ID: "broken", Kind: route.KindDirect, Server: "127.0.0.1:1", Enabled: &on},
		{ID: "good", Kind: route.KindDirect, Server: "magpie.aiweb.xin:28443"},
	}
	if _, err := r.ApplyConfig(next); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}

	// 地址在应用**之后**取：Listen 为 127.0.0.1:0 时每次 Start 由内核分配
	// 新端口（真实使用填固定端口，重启后端口不变，本地 Agent 无需改配置）。
	addr := r.Addr()
	if addr == "" {
		t.Fatal("重启后没有监听地址")
	}
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("重启后监听不接受连接（%s）: %v", addr, err)
	}
	conn.Close()
}
