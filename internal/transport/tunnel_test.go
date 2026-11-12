package transport

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 生成临时自签证书（测试用）
func testCert(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	dir := t.TempDir()
	certFile = filepath.Join(dir, "c.pem")
	keyFile = filepath.Join(dir, "k.pem")
	// 用既有 testdata；若不存在则跳过
	t.Logf("cert dir: %s", dir)
	return "", ""
}

// TestTCPRoundTrip 只测 TCP 隧道的帧往返，隔离上层协议问题
func TestTCPRoundTrip(t *testing.T) {
	certFile := os.Getenv("TEST_CERT")
	keyFile := os.Getenv("TEST_KEY")
	if certFile == "" || keyFile == "" {
		t.Skip("需要 TEST_CERT / TEST_KEY 环境变量")
	}

	addr := "127.0.0.1:19100"
	srv := &Server{Addr: addr, CertFile: certFile, KeyFile: keyFile}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	accepts, err := srv.Listen(ctx)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	// 服务端回显
	go func() {
		for a := range accepts {
			go func(a Accept) {
				defer a.Conn.Close()
				b, err := a.Conn.Recv()
				if err != nil {
					t.Logf("server recv err: %v", err)
					return
				}
				t.Logf("server got %d bytes", len(b))
				// 回显：套一层 transport 帧
				if err := WriteFrame(a.Conn, b); err != nil {
					t.Logf("server send err: %v", err)
				}
			}(a)
		}
	}()

	time.Sleep(200 * time.Millisecond)

	d := &Dialer{Addr: addr, ServerName: "localhost", Insecure: true, Mode: KindTCP}
	conn, err := d.Dial(ctx)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	payload := []byte("hello tunnel")
	if err := WriteFrame(conn, payload); err != nil {
		t.Fatalf("send: %v", err)
	}
	got, err := conn.Recv()
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("echo mismatch: %q vs %q", got, payload)
	}
}

// TestQUICRoundTrip 同上但走 QUIC
func TestQUICRoundTrip(t *testing.T) {
	certFile := os.Getenv("TEST_CERT")
	keyFile := os.Getenv("TEST_KEY")
	if certFile == "" || keyFile == "" {
		t.Skip("需要 TEST_CERT / TEST_KEY 环境变量")
	}

	addr := "127.0.0.1:19101"
	srv := &Server{Addr: addr, CertFile: certFile, KeyFile: keyFile}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	accepts, err := srv.Listen(ctx)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		for a := range accepts {
			go func(a Accept) {
				defer a.Conn.Close()
				b, err := a.Conn.Recv()
				if err != nil {
					t.Logf("server recv err: %v", err)
					return
				}
				WriteFrame(a.Conn, b)
			}(a)
		}
	}()

	time.Sleep(300 * time.Millisecond)

	d := &Dialer{Addr: addr, ServerName: "localhost", Insecure: true, Mode: KindQUIC}
	conn, err := d.Dial(ctx)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	payload := []byte("hello quic")
	if err := WriteFrame(conn, payload); err != nil {
		t.Fatalf("send: %v", err)
	}
	got, err := conn.Recv()
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("echo mismatch: %q vs %q", got, payload)
	}
}
