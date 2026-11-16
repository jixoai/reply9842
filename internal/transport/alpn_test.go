package transport

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
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

	"github.com/quic-go/quic-go"
)

// ── ALPN 编码协商矩阵 ─────────────────────────────────────────────────────
//
// 这张表是 dcz2 互操作的全部依据，四格都要有测试：
//
//	               老服务端（只 accept /1）        新服务端（accept /2,/1）
//	老客户端（只 offer /1）   与今天一致（v1↔v1）   必须仍被接受（v1↔v1）
//	新客户端（offer /2,/1）   自动降级到 v1          选到 v2（dcz2 生效）
//
// 另锁一条反面证据：新客户端**只** offer /2 时对老服务端是**握手失败**，
// 不是降级——这就是客户端必须带 v1 的原因（crypto/tls 的 negotiateALPN
// 在无交集时直接报 "client requested unsupported application protocols"）。

// startProtoServer 起一个 Server（QUIC+TCP 同端口），ALPN 接受列表按 dcz2 决定。
func startProtoServer(t *testing.T, dcz2 bool) (addr string, stop func()) {
	t.Helper()
	dir := t.TempDir()
	certFile := filepath.Join(dir, "c.pem")
	keyFile := filepath.Join(dir, "k.pem")

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
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
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("createcert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	// 先探一个空闲端口（与 startTestServer 同法）
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	addr = l.Addr().String()
	l.Close()

	ctx, cancel := context.WithCancel(context.Background())
	srv := &Server{Addr: addr, CertFile: certFile, KeyFile: keyFile, Dcz2: dcz2}
	accepts, err := srv.Listen(ctx)
	if err != nil {
		cancel()
		t.Fatalf("listen: %v", err)
	}
	go func() {
		for a := range accepts {
			// 只读一帧就关：测试进程不需要连接池语义
			go func(a Accept) {
				defer a.Conn.Close()
				_, _ = a.Conn.Recv()
			}(a)
		}
	}()
	time.Sleep(100 * time.Millisecond) // 让两个 listener 都就绪
	return addr, cancel
}

// dialProto 用真实 Dialer 连（客户端 offer 列表就是生产用的 clientProtos）。
func dialProto(t *testing.T, addr string, mode Kind) *Conn {
	t.Helper()
	d := &Dialer{Addr: addr, ServerName: "localhost", Insecure: true, Mode: mode}
	// QUIC 的 auto 探测超时在这里没必要：直接指定 mode，给足 5s
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := d.Dial(ctx)
	if err != nil {
		t.Fatalf("dial(%s): %v", mode, err)
	}
	return c
}

// TestALPNDowngradeNewClientOldServer 新客户端 × 老服务端 → 协商 v1，不报错。
//
// 这是"自动降级到 dcz"的握手层证据：老服务端只 accept /1，客户端 offer [v2,v1]，
// Go 选中 v1，客户端据此（SupportsDcz2=false）继续用私有 dcz 编码上行。
func TestALPNDowngradeNewClientOldServer(t *testing.T) {
	addr, stop := startProtoServer(t, false)
	defer stop()

	for _, mode := range []Kind{KindTCP, KindQUIC} {
		conn := dialProto(t, addr, mode)
		if got := conn.Proto(); got != ALPNv1 {
			t.Errorf("%s: Proto() = %q，期望 %q（老服务端应降到 v1）", mode, got, ALPNv1)
		}
		if conn.SupportsDcz2() {
			t.Errorf("%s: 老服务端上 SupportsDcz2() 必须为 false", mode)
		}
		conn.Close()
	}
}

// TestALPNNegotiateDcz2 新客户端 × 新服务端 → 协商 v2（dcz2 生效）。
// 双端（客户端 Conn 与服务端 Accept.Conn）都要看到 v2。
func TestALPNNegotiateDcz2(t *testing.T) {
	addr, stop := startProtoServer(t, true)
	defer stop()

	for _, mode := range []Kind{KindTCP, KindQUIC} {
		conn := dialProto(t, addr, mode)
		if got := conn.Proto(); got != ALPNv2 {
			t.Errorf("%s: Proto() = %q，期望 %q", mode, got, ALPNv2)
		}
		if !conn.SupportsDcz2() {
			t.Errorf("%s: SupportsDcz2() 应为 true", mode)
		}
		conn.Close()
	}
}

// TestALPNOldClientNewServer 老客户端（只 offer /1）× 新服务端 → 仍协商 v1。
//
// 老客户端的 offer 列表里没有 v2，服务端的偏好列表 [v2,v1] 会挑到交集 v1 ——
// 服务端不拒连、老客户端行为与今天完全一致（它继续用私有 dcz）。
func TestALPNOldClientNewServer(t *testing.T) {
	addr, stop := startProtoServer(t, true)
	defer stop()

	// TCP：老客户端的裸 TLS 握手
	raw, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	tc := tls.Client(raw, &tls.Config{
		NextProtos: []string{ALPNv1}, ServerName: "localhost", InsecureSkipVerify: true,
	})
	if err := tc.Handshake(); err != nil {
		raw.Close()
		t.Fatalf("老客户端 TCP 握手失败: %v", err)
	}
	if got := tc.ConnectionState().NegotiatedProtocol; got != ALPNv1 {
		t.Errorf("老客户端 TCP 协商到 %q，期望 %q", got, ALPNv1)
	}
	tc.Close()

	// QUIC：老客户端的 offer 列表只有 /1
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	qc, err := quic.DialAddr(ctx, addr, &tls.Config{
		NextProtos: []string{ALPNv1}, ServerName: "localhost", InsecureSkipVerify: true,
	}, &quic.Config{HandshakeIdleTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("老客户端 QUIC 握手失败: %v", err)
	}
	if got := qc.ConnectionState().TLS.NegotiatedProtocol; got != ALPNv1 {
		t.Errorf("老客户端 QUIC 协商到 %q，期望 %q", got, ALPNv1)
	}
	qc.CloseWithError(0, "bye")
}

// TestALPNv2OnlyClientFailsOnOldServer 反面证据：客户端**只** offer /2 时，
// 老服务端直接握手失败（不是降级）。锁住这条，防止有人"优化"掉 clientProtos
// 里的 v1。
func TestALPNv2OnlyClientFailsOnOldServer(t *testing.T) {
	addr, stop := startProtoServer(t, false)
	defer stop()

	raw, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer raw.Close()
	tc := tls.Client(raw, &tls.Config{
		NextProtos: []string{ALPNv2}, ServerName: "localhost", InsecureSkipVerify: true,
	})
	if err := tc.Handshake(); err == nil {
		t.Fatal("只 offer /2 的客户端对老服务端必须握手失败（这正是要带 v1 的理由）")
	} else {
		t.Logf("预期内的握手失败: %v", err)
	}
}

// TestServerProtos 锁住开关语义：零值=只收 v1（与今天一致），显式开启才收 v2。
func TestServerProtos(t *testing.T) {
	if got := serverProtos(false); len(got) != 1 || got[0] != ALPNv1 {
		t.Errorf("serverProtos(false) = %v，期望 [%s]", got, ALPNv1)
	}
	got := serverProtos(true)
	if len(got) != 2 || got[0] != ALPNv2 || got[1] != ALPNv1 {
		t.Errorf("serverProtos(true) = %v，期望 [%s %s]（顺序=服务端偏好）", got, ALPNv2, ALPNv1)
	}
	// 客户端必须两个都 offer，且不接受其它形态
	cp := clientProtos()
	if len(cp) != 2 || cp[0] != ALPNv2 || cp[1] != ALPNv1 {
		t.Errorf("clientProtos() = %v，期望 [%s %s]", cp, ALPNv2, ALPNv1)
	}
	if ALPNv1 != "reply9842/1" || ALPNv2 != "reply9842/2" {
		t.Errorf("ALPN 字面量漂移：v1=%q v2=%q（线上老客户端依赖 v1 的字节不变）", ALPNv1, ALPNv2)
	}
	// 零值 Server 的语义必须是"只收 v1"
	var zero Server
	if zero.Dcz2 {
		t.Error("Server 零值必须是不开启 dcz2")
	}
}

// TestConnProtoZeroValueIsV1 Conn.Proto 的零值语义：空串按 v1。
// 老代码里手工构造的 Conn（测试夹具）不带 proto，不能被误判成支持 dcz2。
func TestConnProtoZeroValueIsV1(t *testing.T) {
	var c Conn
	if c.Proto() != ALPNv1 {
		t.Errorf("零值 Conn.Proto() = %q，期望 %q", c.Proto(), ALPNv1)
	}
	if c.SupportsDcz2() {
		t.Error("零值 Conn 不能声称支持 dcz2")
	}
	c.proto = ALPNv2
	if !c.SupportsDcz2() || c.Proto() != ALPNv2 {
		t.Error("显式置 v2 后应支持 dcz2")
	}
	// 未知版本（未来协议）一律按不支持 dcz2 处理：宁可退回 dcz，也不猜
	c.proto = "reply9842/9"
	if c.SupportsDcz2() {
		t.Error("未知协商结果不能声称支持 dcz2")
	}
}
