package transport

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 起一个最小的 Server（自签证书），返回监听地址
func startTestServer(t *testing.T) (addr, certFile, keyFile string, stop func()) {
	t.Helper()
	dir := t.TempDir()
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("createcert: %v", err)
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(
		&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(
		&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	// 端口 0 让内核选，但要先拿到具体端口：先用一个临时 listener 探一个
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	addr = l.Addr().String()
	l.Close()

	ctx, cancel := context.WithCancel(context.Background())
	srv := &Server{Addr: addr, CertFile: certFile, KeyFile: keyFile}
	accepts, err := srv.Listen(ctx)
	if err != nil {
		cancel()
		t.Fatalf("listen: %v", err)
	}
	go func() {
		for a := range accepts {
			go func(a Accept) {
				// 读一帧就回一帧，够验证连通性
				b, err := a.Conn.Recv()
				if err != nil {
					a.Conn.Close()
					return
				}
				// Recv 返回的是已剥掉长度前缀的 payload，回写时必须重新封装
				_ = a.Conn.Send(mustFrame(string(b)))
				a.Conn.Close()
			}(a)
		}
	}()

	// 等监听就绪
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			c.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	return addr, certFile, keyFile, func() { cancel() }
}

// TestDialLocalAddr 验证 LocalAddr 能把出站连接绑到指定源地址。
//
// 存在的理由：代理软件的 TUN 全局模式（0.0.0.0/1 + 128.0.0.0/1 → utun）
// 会把发往隧道服务器的流量也导进代理，TLS 握手被中途切断，客户端只见
// "tcp tls: EOF"。绑到物理网卡 IP 能让内核直接走该网卡，绕过 utun。
// 这个测试锁住「LocalAddr 真的被用上了」这一行为。
func TestDialLocalAddr(t *testing.T) {
	addr, _, _, stop := startTestServer(t)
	defer stop()

	d := &Dialer{
		Addr:       addr,
		ServerName: "localhost",
		Insecure:   true,
		Mode:       KindTCP,
		// 127.0.0.1 是有效的源地址，绑定后仍能连本机的 server
		LocalAddr: "127.0.0.1",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, err := d.Dial(ctx)
	if err != nil {
		t.Fatalf("Dial with LocalAddr: %v", err)
	}
	defer c.Close()

	// Conn.Send 的语义是「原样写入字节」，长度前缀由上层（meta.WriteFrame）
	// 负责。这里两侧都用 transport 自己的帧封装，保证与真实链路一致。
	if err := writeAll(c, mustFrame("ping")); err != nil {
		t.Fatalf("send: %v", err)
	}
	got, err := c.Recv()
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if string(got) != "ping" {
		t.Fatalf("echo = %q, want ping", got)
	}
}

// mustFrame 按 readFrame 的格式（4 字节大端长度 + payload）封装一帧
func mustFrame(s string) []byte {
	b := []byte(s)
	out := make([]byte, 4+len(b))
	out[0] = byte(len(b) >> 24)
	out[1] = byte(len(b) >> 16)
	out[2] = byte(len(b) >> 8)
	out[3] = byte(len(b))
	copy(out[4:], b)
	return out
}

// TestDialLocalAddrBareIP 验证只给 IP（不带端口）也能被规整成 IP:0
func TestDialLocalAddrBareIP(t *testing.T) {
	addr, _, _, stop := startTestServer(t)
	defer stop()

	for _, la := range []string{"127.0.0.1", "127.0.0.1:0"} {
		d := &Dialer{
			Addr: addr, ServerName: "localhost", Insecure: true,
			Mode: KindTCP, LocalAddr: la,
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		c, err := d.Dial(ctx)
		cancel()
		if err != nil {
			t.Fatalf("LocalAddr=%q: %v", la, err)
		}
		c.Close()
	}
}

// TestDialLocalAddrInvalid 无效源地址必须报错，而不是静默回退
// （静默回退会让用户以为绑定生效了，实际仍在走 TUN）
func TestDialLocalAddrInvalid(t *testing.T) {
	d := &Dialer{
		Addr: "127.0.0.1:1", ServerName: "localhost", Insecure: true,
		Mode: KindTCP, LocalAddr: "no-such-interface-ip",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := d.Dial(ctx); err == nil {
		t.Fatal("无效 LocalAddr 应该报错")
	}
}

// TestALPNNegotiated 锁住 ALPN 常量真的被放进 tls.Config。
// 服务端与客户端不匹配时表现为 "no application protocol"，排查成本很高。
func TestALPNNegotiated(t *testing.T) {
	addr, _, _, stop := startTestServer(t)
	defer stop()

	cfg := &tls.Config{
		NextProtos:         []string{ALPN},
		ServerName:         "localhost",
		InsecureSkipVerify: true,
	}
	raw, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer raw.Close()
	tc := tls.Client(raw, cfg)
	if err := tc.Handshake(); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if got := tc.ConnectionState().NegotiatedProtocol; got != ALPN {
		t.Fatalf("ALPN = %q, want %q", got, ALPN)
	}
}
