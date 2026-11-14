package relayclient

import (
	"net"
	"testing"
)

// 默认配置必须能直接连上内置服务端。
//
// 2026-11-14 起默认目标改成**域名**（magpie.aiweb.xin），理由：
//   - SNI 就是它自己，客户端不需要第二栏
//   - DNS 已切香港，IP 变化只改 DNS，客户端不用动
// 曾经的坑仍要守住：默认目标若是 IP 而没有 SNI，TLS 必然报
// `x509: cannot validate certificate for <ip> ... no IP SANs`。
func TestDefaultsAreConnectable(t *testing.T) {
	c := Config{}
	c.Normalize()

	if want := NormalizeTarget(DefaultServer); c.Server != want {
		t.Fatalf("Server = %q，期望 %q（默认端口要自动补上）", c.Server, want)
	}
	host, _, err := net.SplitHostPort(c.Server)
	if err != nil {
		t.Fatalf("默认目标不是 host:port：%q", c.Server)
	}
	if net.ParseIP(host) != nil {
		t.Fatal("默认目标应当是域名：IP 目标会逼用户再填证书域名")
	}
	if SNINeeded(c.Server) {
		t.Fatal("域名目标不应需要额外的证书域名")
	}
	t.Logf("Server=%s Transport=%s", c.Server, c.Transport)
}

// 用户自定义上游时不强塞内置 SNI（否则连别家服务端会带错 SNI）。
func TestCustomServerKeepsEmptyServerName(t *testing.T) {
	c := Config{Server: "example.com:28443"}
	c.Normalize()
	if c.ServerName != "" {
		t.Fatalf("自定义上游时不应自动填内置 SNI，实际 %q", c.ServerName)
	}
}
