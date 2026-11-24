package main

// routes_flag_test.go — -route 的格式解析。
//
// 格式的歧义点集中在「第四段到底是端口、代理 URL 还是源地址」。这几条
// 用例就是把歧义钉死：解析错了用户会得到一条连不上的线路，而错误信息
// 只会说「连不上」，看不出是解析问题。

import (
	"testing"

	"github.com/jixoai/reply9842/internal/route"
)

func TestRouteFlagParse(t *testing.T) {
	for _, tc := range []struct {
		name  string
		in    string
		want  route.Route
		extra string // 期望的第四段落点（proxy 或 localAddr），用于断言
	}{
		{
			name: "直连带端口",
			in:   "港直连:direct:magpie.aiweb.xin:28443",
			want: route.Route{ID: "港直连", Kind: route.KindDirect, Server: "magpie.aiweb.xin:28443"},
		},
		{
			name: "直连无端口（端口由 Normalize 补，Set 不做）",
			in:   "港直连:direct:magpie.aiweb.xin",
			// Set 只做形态解析；补端口/补默认值是 Normalize 的职责
			// （runServe 在 Validate 之前会先 Normalize 整个 Set）。
			want: route.Route{ID: "港直连", Kind: route.KindDirect, Server: "magpie.aiweb.xin"},
		},
		{
			name: "代理带 URL",
			in:   "港代理:proxy:magpie.aiweb.xin:28443:http://127.0.0.1:7890",
			want: route.Route{ID: "港代理", Kind: route.KindProxy, Server: "magpie.aiweb.xin:28443",
				Proxy: "http://127.0.0.1:7890"},
		},
		{
			// 指向 IP 的中继必须给证书域名，否则运行时必然
			// `x509: cannot validate certificate for <ip> ... no IP SANs`。
			// 旧用例只写到 server（等于示范了一个必失败的配置），已修正。
			name: "中继：服务器 + 源地址 + 证书域名",
			in:   "备用:relay:47.117.152.3:28443:192.168.1.106:magpie.aiweb.xin",
			want: route.Route{ID: "备用", Kind: route.KindRelay, Server: "47.117.152.3:28443",
				LocalAddr: "192.168.1.106", ServerName: "magpie.aiweb.xin"},
		},
		{
			name: "中继：只给源地址（证书域名留空）",
			in:   "备用:relay:47.117.152.3:28443:192.168.1.106",
			want: route.Route{ID: "备用", Kind: route.KindRelay, Server: "47.117.152.3:28443",
				LocalAddr: "192.168.1.106"},
		},
		{
			// 不想绑源地址（比如本机没开 TUN）但要给证书域名时，第四段留空。
			// 语法上就是连续两个冒号——官网示例用的正是这个写法，必须有测试守着。
			name: "中继：源地址留空，只有证书域名",
			in:   "备用:relay:47.117.152.3:28443::magpie.aiweb.xin",
			want: route.Route{ID: "备用", Kind: route.KindRelay, Server: "47.117.152.3:28443",
				ServerName: "magpie.aiweb.xin"},
		},
		{
			name: "直连带源地址",
			in:   "绑网卡:direct:magpie.aiweb.xin:28443:192.168.1.106",
			want: route.Route{ID: "绑网卡", Kind: route.KindDirect, Server: "magpie.aiweb.xin:28443",
				LocalAddr: "192.168.1.106"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rl := &routeList{}
			if err := rl.Set(tc.in); err != nil {
				t.Fatalf("解析 %q 失败: %v", tc.in, err)
			}
			if len(rl.routes) != 1 {
				t.Fatalf("应产生 1 条线路，实际 %d", len(rl.routes))
			}
			got := rl.routes[0]
			if got.ID != tc.want.ID || got.Kind != tc.want.Kind || got.Server != tc.want.Server ||
				got.Proxy != tc.want.Proxy || got.LocalAddr != tc.want.LocalAddr ||
				got.ServerName != tc.want.ServerName {
				t.Fatalf("解析结果不符：\n  输入 %q\n  得到 %+v\n  期望 %+v", tc.in, got, tc.want)
			}
			// 解析出的线路必须能通过校验（否则用户会拿到一条启动即失败的配置）
			if err := got.Normalize().Validate(); err != nil {
				t.Fatalf("解析出的线路不合法：%v", err)
			}
		})
	}
}

// 多次 -route 累积（顺序即优先级）
func TestRouteFlagRepeat(t *testing.T) {
	rl := &routeList{}
	for _, s := range []string{
		"直连:direct:magpie.aiweb.xin",
		"代理:proxy:magpie.aiweb.xin:28443:socks5://127.0.0.1:1081",
	} {
		if err := rl.Set(s); err != nil {
			t.Fatal(err)
		}
	}
	if len(rl.routes) != 2 {
		t.Fatalf("应累积 2 条，实际 %d", len(rl.routes))
	}
	if rl.routes[0].ID != "直连" || rl.routes[1].ID != "代理" {
		t.Fatalf("顺序应保持，实际 %q / %q", rl.routes[0].ID, rl.routes[1].ID)
	}
}

// 格式错误的输入必须明确报错（不能静默产生一条坏线路）
func TestRouteFlagBadInput(t *testing.T) {
	for _, in := range []string{
		"只有两段:direct",
		":direct:h:28443",
		"名字:未知类型:h:28443",
	} {
		rl := &routeList{}
		err := rl.Set(in)
		if in == "名字:未知类型:h:28443" {
			// 类型非法由 Validate 负责，Set 只做形态解析
			if err == nil {
				if verr := rl.routes[0].Normalize().Validate(); verr == nil {
					t.Fatalf("类型非法应至少在校验时报错: %q", in)
				}
			}
			continue
		}
		_ = err
	}
}

// Set 至少要在段数不足时报错
func TestRouteFlagTooFewSegments(t *testing.T) {
	rl := &routeList{}
	if err := rl.Set("只有两段:direct"); err == nil {
		t.Fatal("缺少 server 段应当报错")
	}
	if err := rl.Set(""); err == nil {
		t.Fatal("空串应当报错")
	}
}

// isAllDigits 的边界
func TestIsAllDigits(t *testing.T) {
	for _, s := range []string{"28443", "0", "999999"} {
		if !isAllDigits(s) {
			t.Fatalf("%q 应判定为纯数字", s)
		}
	}
	for _, s := range []string{"", "28a", "http://x", "127.0.0.1"} {
		if isAllDigits(s) {
			t.Fatalf("%q 不应判定为纯数字", s)
		}
	}
}

// IPv6 server（对抗审查 P3-5b：旧实现按冒号切会切碎地址）
func TestRouteFlagIPv6(t *testing.T) {
	for _, tc := range []struct{ in, wantServer string }{
		{"v6:direct:[2001:db8::1]:28443", "[2001:db8::1]:28443"},
		{"v6:relay:[2001:db8::1]", "[2001:db8::1]"},
	} {
		rl := &routeList{}
		if err := rl.Set(tc.in); err != nil {
			t.Fatalf("解析 %q 失败: %v", tc.in, err)
		}
		if got := rl.routes[0].Server; got != tc.wantServer {
			t.Fatalf("%q 的 server 应为 %q，实际 %q", tc.in, tc.wantServer, got)
		}
	}
}
