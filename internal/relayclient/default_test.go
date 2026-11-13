package relayclient

import "testing"

// 默认配置必须能直接连上内置服务端。
//
// 回归护栏：曾经 Server 是 IP 而 ServerName 留空，transport 按 host 推导出
// "47.82.76.102" 作为 SNI，而服务端证书的 SAN 只有 DNS:magpie.aiweb.xin，
// 于是 TLS 报 `x509: cannot validate certificate for <ip> because it doesn't
// contain any IP SANs`。TCP 与 QUIC 都会失败，用户完全无法连接。
func TestDefaultsAreConnectable(t *testing.T) {
	c := Config{}
	c.Normalize()

	if c.Server != DefaultServer {
		t.Fatalf("Server = %q, 期望 %q", c.Server, DefaultServer)
	}
	// 关键断言：上游是 IP 时 SNI 不能为空，否则推导成 IP 必然校验失败
	if c.ServerName == "" {
		t.Fatal("ServerName 为空：上游是 IP 时必须显式指定证书域名，否则 TLS 校验必失败")
	}
	if c.ServerName != DefaultServerName {
		t.Fatalf("ServerName = %q, 期望 %q", c.ServerName, DefaultServerName)
	}
	t.Logf("Server=%s ServerName=%s Transport=%s", c.Server, c.ServerName, c.Transport)
}

// 用户自定义上游时不强塞内置 SNI（否则连别家服务端会带错 SNI）。
func TestCustomServerKeepsEmptyServerName(t *testing.T) {
	c := Config{Server: "example.com:28443"}
	c.Normalize()
	if c.ServerName != "" {
		t.Fatalf("自定义上游时不应自动填内置 SNI，实际 %q", c.ServerName)
	}
}
