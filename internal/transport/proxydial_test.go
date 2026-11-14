package transport

import (
	"bufio"
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// startEcho 起一个回显 TCP 服务，充当「隧道服务器」。
func startEcho(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(c, c)
			}(c)
		}
	}()
	return ln.Addr().String()
}

// startHTTPConnectProxy 起一个最小的 HTTP CONNECT 代理。
func startHTTPConnectProxy(t *testing.T, wantAuth string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				line, err := br.ReadString('\n')
				if err != nil {
					return
				}
				fields := strings.Fields(line)
				if len(fields) < 2 || fields[0] != "CONNECT" {
					return
				}
				authOK := wantAuth == ""
				var target string
				for {
					h, err := br.ReadString('\n')
					if err != nil {
						return
					}
					if strings.HasPrefix(strings.ToLower(h), "proxy-authorization:") && wantAuth != "" {
						authOK = strings.Contains(h, wantAuth)
					}
					if h == "\r\n" || h == "\n" {
						break
					}
				}
				if !authOK {
					c.Write([]byte("HTTP/1.1 407 Proxy Authentication Required\r\n\r\n"))
					return
				}
				target = fields[1]
				up, err := net.Dial("tcp", target)
				if err != nil {
					c.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
					return
				}
				defer up.Close()
				c.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
				go io.Copy(up, br)
				io.Copy(c, up)
			}(c)
		}
	}()
	return ln.Addr().String()
}

// TestDialViaHTTPProxy 代理模式下 CONNECT 必须真的把数据打通。
func TestDialViaHTTPProxy(t *testing.T) {
	target := startEcho(t)
	proxy := startHTTPConnectProxy(t, "")

	conn, err := dialViaProxy(context.Background(), "http://"+proxy, target)
	if err != nil {
		t.Fatalf("dialViaProxy: %v", err)
	}
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write([]byte("ping-through-proxy")); err != nil {
		t.Fatalf("写: %v", err)
	}
	buf := make([]byte, 32)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("读: %v", err)
	}
	if got := string(buf[:n]); got != "ping-through-proxy" {
		t.Fatalf("回显不一致：%q", got)
	}
}

// TestDialViaProxyRejects 代理拒绝时要把 HTTP 状态码说清楚，而不是干等。
func TestDialViaProxyRejects(t *testing.T) {
	target := startEcho(t)
	proxy := startHTTPConnectProxy(t, "Basic SECRET")

	_, err := dialViaProxy(context.Background(), "http://"+proxy, target)
	if err == nil {
		t.Fatal("代理要求认证但 URL 没带凭据，应当失败")
	}
	if !strings.Contains(err.Error(), "407") && !strings.Contains(err.Error(), "被拒绝") {
		t.Fatalf("错误信息应包含拒绝原因：%v", err)
	}
}

// TestDialViaProxyUnsupportedScheme 不认识的协议要直接报错，不能静默直连。
func TestDialViaProxyUnsupportedScheme(t *testing.T) {
	if _, err := dialViaProxy(context.Background(), "ftp://127.0.0.1:1", "example.com:1"); err == nil {
		t.Fatal("ftp:// 应当被拒绝")
	}
}
