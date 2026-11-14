package transport

// proxydial.go —— 「代理」模式下把 TCP 打到代理，再在隧道里跑 TLS。
//
// 支持的代理：
//
//	http://[user:pass@]host:port     HTTP CONNECT（明文隧道）
//	https://[user:pass@]host:port    CONNECT over TLS
//	socks5://[user:pass@]host:port   SOCKS5（RFC 1928 + 用户名/密码认证）
//
// 为什么只有 TCP：QUIC 跑在 UDP 上，HTTP CONNECT 与 SOCKS5 都是 TCP 隧道，
// 没有标准方式承载 UDP（SOCKS5 的 UDP ASSOCIATE 多数代理不启用）。
// 所以代理模式下传输强制降级到 TCP —— 这比「静默连不上」诚实。

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"
)

// dialViaProxy 连到 proxy 指定的代理，并让它把连接接到 target（host:port）。
func dialViaProxy(ctx context.Context, proxyURL, target string) (net.Conn, error) {
	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("proxy: 解析 %q: %w", proxyURL, err)
	}
	host := u.Host
	if u.Port() == "" {
		switch u.Scheme {
		case "http":
			host = net.JoinHostPort(u.Hostname(), "80")
		case "https":
			host = net.JoinHostPort(u.Hostname(), "443")
		case "socks5", "socks5h":
			host = net.JoinHostPort(u.Hostname(), "1080")
		default:
			return nil, fmt.Errorf("proxy: 不支持的协议 %q（可选 http / https / socks5）", u.Scheme)
		}
	}

	d := &net.Dialer{Timeout: 10 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, fmt.Errorf("proxy: 连不上 %s: %w", host, err)
	}

	switch u.Scheme {
	case "http", "https":
		if u.Scheme == "https" {
			tc := tls.Client(conn, &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12})
			if err := tc.HandshakeContext(ctx); err != nil {
				conn.Close()
				return nil, fmt.Errorf("proxy: TLS 握手失败: %w", err)
			}
			conn = tc
		}
		if err := httpConnect(conn, target, u); err != nil {
			conn.Close()
			return nil, err
		}
		return conn, nil
	case "socks5", "socks5h":
		if err := socks5Handshake(conn, target, u); err != nil {
			conn.Close()
			return nil, err
		}
		return conn, nil
	}
	conn.Close()
	return nil, fmt.Errorf("proxy: 不支持的协议 %q", u.Scheme)
}

// httpConnect 发 CONNECT 并校验 200。
func httpConnect(conn net.Conn, target string, u *url.URL) error {
	if dl, ok := connDeadlineFrom(conn); ok {
		_ = dl.SetDeadline(time.Now().Add(15 * time.Second))
		defer dl.SetDeadline(time.Time{})
	}
	var auth string
	if u.User != nil {
		pass, _ := u.User.Password()
		token := base64.StdEncoding.EncodeToString([]byte(u.User.Username() + ":" + pass))
		auth = "Proxy-Authorization: Basic " + token + "\r\n"
	}
	req := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n" + auth + "\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		return fmt.Errorf("proxy: CONNECT 发送失败: %w", err)
	}
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil {
		return fmt.Errorf("proxy: CONNECT 无响应: %w", err)
	}
	// 状态行形如 "HTTP/1.1 200 Connection established"
	var code int
	if _, err := fmt.Sscanf(line, "HTTP/%s %d", new(string), &code); err != nil {
		return fmt.Errorf("proxy: 无法解析 CONNECT 响应 %q", line)
	}
	if code != 200 {
		return fmt.Errorf("proxy: CONNECT 被拒绝（HTTP %d）", code)
	}
	// 吃掉剩余响应头
	for {
		h, err := br.ReadString('\n')
		if err != nil {
			return nil
		}
		if h == "\r\n" || h == "\n" {
			break
		}
	}
	return nil
}

// socks5Handshake 走 RFC 1928（CONNECT）+ RFC 1929（用户名/密码）。
func socks5Handshake(conn net.Conn, target string, u *url.URL) error {
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		return fmt.Errorf("socks5: 目标地址 %q: %w", target, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return fmt.Errorf("socks5: 端口 %q: %w", portStr, err)
	}

	if dl, ok := connDeadlineFrom(conn); ok {
		_ = dl.SetDeadline(time.Now().Add(15 * time.Second))
		defer dl.SetDeadline(time.Time{})
	}

	// 1. 版本 + 认证方式
	methods := []byte{0x00} // no-auth
	if u.User != nil {
		methods = append(methods, 0x02) // username/password
	}
	if _, err := conn.Write(append([]byte{0x05, byte(len(methods))}, methods...)); err != nil {
		return fmt.Errorf("socks5: 握手发送失败: %w", err)
	}
	buf := make([]byte, 2)
	if _, err := readFull(conn, buf); err != nil {
		return fmt.Errorf("socks5: 握手无响应: %w", err)
	}
	if buf[0] != 0x05 {
		return fmt.Errorf("socks5: 版本不符（%d）", buf[0])
	}
	switch buf[1] {
	case 0x00:
		// 直通
	case 0x02:
		if u.User == nil {
			return fmt.Errorf("socks5: 代理要求认证，但 URL 里没有用户名")
		}
		pass, _ := u.User.Password()
		ub := []byte(u.User.Username())
		pb := []byte(pass)
		msg := append([]byte{0x01, byte(len(ub))}, ub...)
		msg = append(msg, byte(len(pb)))
		msg = append(msg, pb...)
		if _, err := conn.Write(msg); err != nil {
			return fmt.Errorf("socks5: 认证发送失败: %w", err)
		}
		if _, err := readFull(conn, buf); err != nil {
			return fmt.Errorf("socks5: 认证无响应: %w", err)
		}
		if buf[1] != 0x00 {
			return fmt.Errorf("socks5: 认证失败")
		}
	case 0xFF:
		return fmt.Errorf("socks5: 代理不接受任何认证方式")
	default:
		return fmt.Errorf("socks5: 代理选择了不支持的认证方式 %d", buf[1])
	}

	// 2. CONNECT 请求
	var req []byte
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			req = append([]byte{0x05, 0x01, 0x00, 0x01}, v4...)
		} else {
			req = append([]byte{0x05, 0x01, 0x00, 0x04}, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return fmt.Errorf("socks5: 域名过长")
		}
		req = append([]byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}, host...)
	}
	req = append(req, byte(port>>8), byte(port))
	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("socks5: CONNECT 发送失败: %w", err)
	}

	// 3. 响应：VER REP RSV ATYP ADDR PORT
	head := make([]byte, 4)
	if _, err := readFull(conn, head); err != nil {
		return fmt.Errorf("socks5: CONNECT 无响应: %w", err)
	}
	if head[1] != 0x00 {
		return fmt.Errorf("socks5: CONNECT 失败（REP=%d）", head[1])
	}
	var skip int
	switch head[3] {
	case 0x01:
		skip = 4
	case 0x03:
		l := make([]byte, 1)
		if _, err := readFull(conn, l); err != nil {
			return err
		}
		skip = int(l[0])
	case 0x04:
		skip = 16
	default:
		return fmt.Errorf("socks5: 未知地址类型 %d", head[3])
	}
	if _, err := readFull(conn, make([]byte, skip+2)); err != nil {
		return err
	}
	return nil
}

func readFull(c net.Conn, b []byte) (int, error) {
	got := 0
	for got < len(b) {
		n, err := c.Read(b[got:])
		got += n
		if err != nil {
			return got, err
		}
		if n == 0 {
			return got, fmt.Errorf("proxy: 连接被对端关闭")
		}
	}
	return got, nil
}

// connDeadlineFrom 取底层可设 deadline 的连接（http CONNECT 之后 conn 可能
// 被 TLS 包了一层，两者都实现 net.Conn）。
func connDeadlineFrom(c net.Conn) (interface{ SetDeadline(time.Time) error }, bool) {
	if c == nil {
		return nil, false
	}
	return c, true
}
