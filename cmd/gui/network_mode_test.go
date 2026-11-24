package main

// 这个文件原本测「网络三选一下的传输方式选项」与「启动时纠正 proxy+QUIC」。
//
// 2026-11-24 owner：「线路和网络是不是应该合并到一起，不然很奇怪？」
// 网络三选一与传输方式两行已删除——它们是「第一条线路的字段」的另一种写法。
// 但**代理不能承载 QUIC** 这条约束仍然成立，只是落点从界面搬到了配置迁移：
// 旧 GUI 允许存下这个非法组合，升级后若直接交给 route.Validate 会被拒绝，
// 客户端起不来。所以纠正逻辑留在 MigrateRoutes，这里测它。

import (
	"testing"

	"github.com/jixoai/reply9842/internal/relayclient"
	"github.com/jixoai/reply9842/internal/route"
)

func TestMigrationCoercesProxyQUIC(t *testing.T) {
	cases := []struct {
		name      string
		mode      string
		proxyURL  string
		transport string
		wantKind  route.Kind
		wantTrans string
	}{
		{"代理 + QUIC → 纠正为 auto", relayclient.NetworkProxy,
			"http://127.0.0.1:7890", "quic", route.KindProxy, "auto"},
		{"代理 + TCP 不动", relayclient.NetworkProxy,
			"http://127.0.0.1:7890", "tcp", route.KindProxy, "tcp"},
		{"直连 + QUIC 保留", relayclient.NetworkDirect, "", "quic", route.KindDirect, "quic"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := relayclient.Settings{
				NetworkMode: c.mode, ProxyURL: c.proxyURL,
				Transport: c.transport, Server: "magpie.aiweb.xin",
			}
			rs := s.MigrateRoutes()
			if len(rs) != 1 {
				t.Fatalf("应迁移出 1 条线路，实际 %d", len(rs))
			}
			if rs[0].Kind != c.wantKind {
				t.Fatalf("kind = %q，期望 %q", rs[0].Kind, c.wantKind)
			}
			if rs[0].Transport != c.wantTrans {
				t.Fatalf("transport = %q，期望 %q", rs[0].Transport, c.wantTrans)
			}
			// 纠正后必须能通过校验（否则用户升级即不可用）
			if err := rs.Validate(); err != nil {
				t.Fatalf("迁移结果应合法：%v", err)
			}
		})
	}
}
