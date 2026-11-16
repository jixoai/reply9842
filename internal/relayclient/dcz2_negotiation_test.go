package relayclient

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jixoai/reply9842/internal/codec"
	"github.com/jixoai/reply9842/internal/meta"
	"github.com/jixoai/reply9842/internal/transport"
)

// ── 上行编码协商的端到端证据 ─────────────────────────────────────────────
//
// 假的隧道服务端（真 TLS / 真 QUIC，只有上游是假的）：记录每个请求帧里
// **声明**的 Encoding，并按该声明解出明文（dcz/dcz2 各用各的解码器）。
// 于是「新客户端在哪种连接上发哪种编码」与「服务端能不能解」两件事在一条
// 测试里同时被证实——而且用生产同一份 relayclient handler。

// seenReq 一条被观察到的请求
type seenReq struct {
	head *meta.RequestHead
	body []byte // 解出的明文
}

// fakeTunnel 假隧道服务端
type fakeTunnel struct {
	addr     string
	stop     func()
	requests chan seenReq // 供测试按序读取

	mu   sync.Mutex
	seen []seenReq // 同上，供 lookup 按 BLAKE3 找基线（服务端 store 的翻版）
	fail chan string
}

// certFiles 生成自签证书（ECDSA，比 RSA 快得多）
func certFiles(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	dir := t.TempDir()
	certFile = filepath.Join(dir, "c.pem")
	keyFile = filepath.Join(dir, "k.pem")
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
		t.Fatalf("cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("key der: %v", err)
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return certFile, keyFile
}

// startFakeTunnel 起假隧道服务端。dcz2 = 服务端是否 accept reply9842/2。
func startFakeTunnel(t *testing.T, dcz2 bool) *fakeTunnel {
	t.Helper()
	certFile, keyFile := certFiles(t)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	addr := l.Addr().String()
	l.Close()

	ctx, cancel := context.WithCancel(context.Background())
	srv := &transport.Server{Addr: addr, CertFile: certFile, KeyFile: keyFile, Dcz2: dcz2}
	accepts, err := srv.Listen(ctx)
	if err != nil {
		cancel()
		t.Fatalf("listen: %v", err)
	}

	ft := &fakeTunnel{
		addr:     addr,
		stop:     cancel,
		requests: make(chan seenReq, 16),
		fail:     make(chan string, 16),
	}
	go func() {
		for a := range accepts {
			go func(a transport.Accept) {
				defer a.Conn.Close()
				for {
					raw, err := a.Conn.Recv()
					if err != nil {
						return
					}
					ft.handle(a, raw)
				}
			}(a)
		}
	}()
	time.Sleep(100 * time.Millisecond) // 两个 listener 就绪
	return ft
}

// lookup 用 BLAKE3 哈希找已见过的正文（模拟服务端 store：key 恒为 BLAKE3，
// 与线格式是 SHA-256 还是 BLAKE3 无关——这就是「内部寻址不变」）
func (ft *fakeTunnel) lookup(h meta.Hash) []byte {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	for _, r := range ft.seen {
		if codec.Sum(r.body) == codec.Hash(h) {
			return r.body
		}
	}
	return nil
}

// remember 记录一条已解出的请求
func (ft *fakeTunnel) remember(r seenReq) {
	ft.mu.Lock()
	ft.seen = append(ft.seen, r)
	ft.mu.Unlock()
	ft.requests <- r
}

// handle 处理一帧：Ping 回 Ping；Request 解编码 + 校验 + 回最小 200 响应。
func (ft *fakeTunnel) handle(a transport.Accept, raw []byte) {
	ftype, payload, err := meta.ReadFrame(bytes.NewReader(raw))
	if err != nil {
		return
	}
	switch ftype {
	case meta.FramePing:
		if f, e := meta.WrapFrame(meta.FramePing, nil); e == nil {
			_ = transport.WriteFrame(a.Conn, f)
		}
		return
	case meta.FrameRequest:
		// 继续
	default:
		// 本测试不走续传路径；回了 DeltaMiss 以免客户端挂住
		if f, e := meta.WrapFrame(meta.FrameDeltaMiss, nil); e == nil {
			_ = transport.WriteFrame(a.Conn, f)
		}
		return
	}

	head, _, wire, err := meta.DecodeRequest(payload)
	if err != nil {
		ft.fail <- "DecodeRequest: " + err.Error()
		return
	}

	// 按**声明的编码**解出明文。用哪条分支完全由 head.Encoding 决定——
	// 这正是「服务端必须两种都能解」的落点。
	var (
		plain []byte
		derr  error
	)
	switch head.Encoding {
	case meta.EncIdentity:
		plain = wire
	case meta.EncZstd:
		plain, derr = codec.Decompress(wire, nil)
	case meta.EncDcz:
		// 私有 dcz：头里是 BLAKE3(基线)，字典靠帧里的 BaseHash 定位
		dict := ft.lookup(head.BaseHash)
		if dict == nil {
			ft.fail <- fmt.Sprintf("dcz 请求的 BaseHash %s 没有对应基线（先决条件不成立）", head.BaseHash)
			return
		}
		plain, derr = codec.DecodeDcz(wire, dict)
	case meta.EncDcz2:
		// RFC 9842 §5：线格式头里是 SHA-256(字典)；store 侧仍用 BLAKE3(BaseHash)
		dict := ft.lookup(head.BaseHash)
		if dict == nil {
			ft.fail <- fmt.Sprintf("dcz2 请求的 BaseHash %s 没有对应基线（先决条件不成立）", head.BaseHash)
			return
		}
		plain, derr = codec.DecodeDcz2(wire, dict)
	default:
		ft.fail <- fmt.Sprintf("未知编码 %v（服务端没有对应解码分支）", head.Encoding)
		return
	}
	if derr != nil {
		ft.fail <- fmt.Sprintf("按 %v 解编码失败: %v", head.Encoding, derr)
		return
	}
	// 防静默损坏：ResultHash 必须是明文的 BLAKE3
	if !head.ResultHash.IsZero() && convertHash(codec.Sum(plain)) != head.ResultHash {
		ft.fail <- fmt.Sprintf("ResultHash 不符（encoding=%v）", head.Encoding)
		return
	}

	ft.remember(seenReq{head: head, body: plain})

	// 回最小成功响应：响应头 + 一小段 body + Done
	if out, e := meta.EncodeResponse(&meta.ResponseHead{
		Status: 200, Encoding: meta.EncIdentity, BodyLen: -1,
	}, nil, nil); e == nil {
		if f, e2 := meta.WrapFrame(meta.FrameResponse, out); e2 == nil {
			_ = transport.WriteFrame(a.Conn, f)
		}
	}
	if out, e := meta.EncodeResponse(&meta.ResponseHead{
		Status: 0, Encoding: meta.EncIdentity, BodyLen: 4,
	}, nil, []byte("okay")); e == nil {
		if f, e2 := meta.WrapFrame(meta.FrameResponse, out); e2 == nil {
			_ = transport.WriteFrame(a.Conn, f)
		}
	}
	if f, e := meta.WrapFrame(meta.FrameDone, nil); e == nil {
		_ = transport.WriteFrame(a.Conn, f)
	}
}

// bodyBlock 造一段可被 finder 选作基线的正文（远超 4096 的对齐块长）
func bodyBlock(marker string, n int) []byte {
	return []byte(strings.Repeat("{\"role\":\"user\",\"content\":\""+marker+" tail... \"},", n))
}

// waitReq 取一条记录（带超时）
func waitReq(t *testing.T, ft *fakeTunnel, timeout time.Duration) seenReq {
	t.Helper()
	select {
	case got := <-ft.requests:
		return got
	case msg := <-ft.fail:
		t.Fatalf("服务端侧断言失败: %s", msg)
	case <-time.After(timeout):
		t.Fatal("等待请求超时")
	}
	return seenReq{}
}

// postBody 向 Runner 的下游发一条请求并断言 200
func postBody(t *testing.T, addr string, b []byte) {
	t.Helper()
	cl := &http.Client{Timeout: 10 * time.Second}
	resp, err := cl.Post("http://"+addr+"/v1/messages", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("下游状态 = %d（body=%q）", resp.StatusCode, got)
	}
}

// runUplinkPair 起 Runner，向假隧道发两条「相似请求」：
// 第一条没有基线（zstd/identity），第二条应触发 delta 路径——要断言的就是
// 第二条声明的编码。
func runUplinkPair(t *testing.T, ft *fakeTunnel, transportKind string) seenReq {
	t.Helper()
	dir := t.TempDir()
	r := New(Config{
		Listen:     "127.0.0.1:0",
		Server:     ft.addr,
		ServerName: "localhost",
		Insecure:   true,
		Transport:  transportKind,
		LogPath:    filepath.Join(dir, "client.jsonl"),
		StatsPath:  filepath.Join(dir, "stats.json"),
	})
	r.SetLogger(log.New(io.Discard, "", 0))
	if err := r.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = r.Stop(ctx)
		_ = r.Close()
	}()

	base := bodyBlock("base", 600) // ~24KB，超过 MinDelta 门槛
	next := append(append([]byte{}, base...), bodyBlock("delta", 8)...)

	postBody(t, r.Addr(), base)
	first := waitReq(t, ft, 5*time.Second)
	if first.head.Encoding == meta.EncDcz || first.head.Encoding == meta.EncDcz2 {
		t.Fatalf("第一条（无基线）不应是 delta 编码，得到 %v", first.head.Encoding)
	}
	postBody(t, r.Addr(), next)
	return waitReq(t, ft, 5*time.Second)
}

// TestUplinkDcz2OnV2Connection 新客户端 × 新服务端：
// 第二条请求必须声明 EncDcz2，且服务端用 RFC §5 解码器能解出原文。
func TestUplinkDcz2OnV2Connection(t *testing.T) {
	for _, kind := range []string{"tcp", "quic"} {
		t.Run(kind, func(t *testing.T) {
			ft := startFakeTunnel(t, true)
			defer ft.stop()
			got := runUplinkPair(t, ft, kind)
			if got.head.Encoding != meta.EncDcz2 {
				t.Fatalf("v2 连接上第二条请求编码 = %v，期望 dcz2", got.head.Encoding)
			}
			if !bytes.Contains(got.body, []byte("\"delta tail")) {
				t.Fatalf("解出的明文不完整（%d 字节）", len(got.body))
			}
		})
	}
}

// TestUplinkDczOnV1Connection 新客户端 × 老服务端：
// 协商降到 v1，第二条请求必须声明 EncDcz（老编码），且私有 dcz 解码器能解。
// 这条路径同时锁住「老服务端不会被塞 dcz2」。
func TestUplinkDczOnV1Connection(t *testing.T) {
	for _, kind := range []string{"tcp", "quic"} {
		t.Run(kind, func(t *testing.T) {
			ft := startFakeTunnel(t, false)
			defer ft.stop()
			got := runUplinkPair(t, ft, kind)
			if got.head.Encoding == meta.EncDcz2 {
				t.Fatal("老服务端（只 accept v1）收到了 dcz2 —— 编码协商失效")
			}
			if got.head.Encoding != meta.EncDcz {
				t.Fatalf("v1 连接上第二条请求编码 = %v，期望私有 dcz", got.head.Encoding)
			}
			if !bytes.Contains(got.body, []byte("\"delta tail")) {
				t.Fatalf("解出的明文不完整（%d 字节）", len(got.body))
			}
		})
	}
}
