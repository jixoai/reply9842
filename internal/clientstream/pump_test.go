package clientstream

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jixoai/rfc9842-reply/internal/meta"
	"github.com/jixoai/rfc9842-reply/internal/transport"
)

// selfSigned 生成自签证书（transport.Dialer 走 DNS 名之外的 TLS 握手）。
func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("cert: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// serveFrames 起一个 TLS 监听，把预置帧序列原样写给第一个连上来的客户端。
func serveFrames(t *testing.T, frames [][]byte) (*transport.Conn, func()) {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{selfSigned(t)},
		NextProtos:   []string{transport.ALPN},
		MinVersion:   tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		for _, f := range frames {
			// transport 层是「4 字节大端长度 + payload」的封帧，
			// 直接写 meta 帧会被 Recv 当成超大长度。
			var lb [4]byte
			n := uint32(len(f))
			lb[0], lb[1], lb[2], lb[3] = byte(n>>24), byte(n>>16), byte(n>>8), byte(n)
			if _, err := c.Write(lb[:]); err != nil {
				return
			}
			if _, err := c.Write(f); err != nil {
				return
			}
		}
		// 让客户端读到 EOF：连接关闭 = 无终止帧的「断线」场景
	}()

	d := &transport.Dialer{
		Addr: ln.Addr().String(), ServerName: "localhost",
		Mode: transport.KindTCP, Insecure: true,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := d.Dial(ctx)
	if err != nil {
		ln.Close()
		t.Fatalf("dial: %v", err)
	}
	return conn, func() { conn.Close(); ln.Close(); <-done }
}

func respFrame(t *testing.T, status int, body []byte) []byte {
	t.Helper()
	payload, err := meta.EncodeResponse(&meta.ResponseHead{
		Status: status, Encoding: meta.EncIdentity,
		BodyLen: int64(len(body)), HeaderLen: 0,
	}, nil, body)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	f, err := meta.WrapFrame(meta.FrameResponse, payload)
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	return f
}

func doneFrame(t *testing.T) []byte {
	t.Helper()
	f, err := meta.WrapFrame(meta.FrameDone, nil)
	if err != nil {
		t.Fatalf("wrap done: %v", err)
	}
	return f
}

// 线上现象：3 个 200 全部带 err="recv: EOF"。
// 根因：连接关闭是唯一的结束信号，客户端无法区分「正常完成」和「断线」。
// 修复：服务端发 FrameDone，客户端收到即 Completed=true 且 Err=nil。
func TestDrainFrameDoneCompletesCleanly(t *testing.T) {
	conn, closeFn := serveFrames(t, [][]byte{
		respFrame(t, 200, nil),
		respFrame(t, 0, []byte("hello")),
		doneFrame(t),
	})
	defer closeFn()

	rec := httptest.NewRecorder()
	s := &Session{}
	Drain(conn, rec, s, time.Now())

	if !s.Completed {
		t.Fatalf("Completed=false（FrameDone 未被识别）, Err=%v", s.Err)
	}
	if s.Err != nil {
		t.Fatalf("正常完成不应有 Err, got %v", s.Err)
	}
	if s.Status != 200 {
		t.Errorf("Status=%d, want 200", s.Status)
	}
	if got := rec.Body.String(); got != "hello" {
		t.Errorf("body=%q, want hello", got)
	}
	if s.Bytes != 5 {
		t.Errorf("Bytes=%d, want 5", s.Bytes)
	}
	if s.TTFB <= 0 {
		t.Error("TTFB 未记录（流式路径恒 0 的 bug）")
	}
}

// 连接在没有 FrameDone 的情况下断开 → 应报错且 Completed=false，
// 调用方据此发起续传。
func TestDrainConnCloseWithoutDoneIsResumable(t *testing.T) {
	conn, closeFn := serveFrames(t, [][]byte{respFrame(t, 200, nil)})
	defer closeFn()

	s := &Session{}
	Drain(conn, httptest.NewRecorder(), s, time.Now())

	if s.Completed {
		t.Fatal("没有 FrameDone 不应标记完成")
	}
	if s.Err == nil {
		t.Fatal("中断应有 Err，供上层续传判断")
	}
	if s.Fatal {
		t.Error("连接中断是可恢复错误，不应标记 Fatal")
	}
}

// FrameError 来自服务端判定失败（解码/上游/hash），重连重放只会得到同样结果。
func TestDrainFrameErrorIsFatal(t *testing.T) {
	fErr, _ := meta.WrapFrame(meta.FrameError, []byte("upstream: 500 boom"))
	conn, closeFn := serveFrames(t, [][]byte{fErr})
	defer closeFn()

	s := &Session{}
	Drain(conn, httptest.NewRecorder(), s, time.Now())

	if s.Err == nil {
		t.Fatal("FrameError 应设置 Err")
	}
	if !s.Fatal {
		t.Fatal("FrameError 应标记 Fatal，否则会白白重放 maxResume 次")
	}
	if s.Completed {
		t.Error("失败不应标记完成")
	}
}
