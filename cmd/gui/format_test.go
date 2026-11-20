package main

import (
	"errors"
	"strings"
	"testing"
)

// humanError 的三个新增映射都来自真实事故：
//   - 代理 + QUIC：必然失败，应直接告诉用户改自动/TCP；
//   - tcp tls: EOF：常见成因是代理 TUN 劫持，而不是服务端崩溃；
//   - quic 超时：UDP 被拦，改 TCP/自动即可。
func TestHumanErrorProxyAndTransportHints(t *testing.T) {
	cases := []struct {
		err  string
		want string
	}{
		{"dial: transport: 代理模式不支持 QUIC（HTTP/SOCKS 代理只承载 TCP），请改用 tcp 或 auto", "代理模式不支持 QUIC"},
		{"dial: tcp tls: EOF", "直连"},
		{"dial: quic dial: context deadline exceeded", "QUIC 无响应"},
	}
	for _, c := range cases {
		got := humanError(errors.New(c.err))
		if !strings.Contains(got, c.want) {
			t.Errorf("humanError(%q) = %q，期望包含 %q", c.err, got, c.want)
		}
	}
}
