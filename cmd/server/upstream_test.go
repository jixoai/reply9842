package main

import (
	"context"
	"net"
	"testing"
)

func TestParseUpstreams(t *testing.T) {
	u, err := parseUpstreams("http://127.0.0.1:3425, http://127.0.0.1:3426")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(u.targets) != 2 {
		t.Fatalf("候选数 = %d，想要 2", len(u.targets))
	}
	if got := u.URL("/v1/messages"); got != "http://127.0.0.1:3425/v1/messages" {
		t.Fatalf("URL = %q", got)
	}

	if _, err := parseUpstreams("127.0.0.1:3425"); err == nil {
		t.Fatal("缺 scheme 应当报错，而不是运行时才失败")
	}
	if _, err := parseUpstreams(" , "); err == nil {
		t.Fatal("空列表应当报错")
	}
}

// 蓝绿轮转：首选槽 down 时必须自动落到备用槽，而不是把 connection refused
// 直接当成上游失败（2026-11-16 生产事故的直接原因）。
func TestUpstreamDialFailsOver(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	live := ln.Addr().String()

	// 拿一个「确定没人监听」的端口：listen 后立刻关掉。
	deadLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := deadLn.Addr().String()
	deadLn.Close()

	u, err := parseUpstreams("http://" + dead + ",http://" + live)
	if err != nil {
		t.Fatal(err)
	}
	// Transport 传进来的 addr 应当被忽略，候选列表才是权威。
	conn, err := u.dial(context.Background(), "tcp", "10.255.255.1:1")
	if err != nil {
		t.Fatalf("应当落到备用槽: %v", err)
	}
	defer conn.Close()
	if got := conn.RemoteAddr().String(); got != live {
		t.Fatalf("连到了 %s，想要 %s", got, live)
	}
}
