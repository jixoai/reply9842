package main

// dcz2 的端到端证据（真 server + 真 transport，裸隧道客户端）。
//
// 与 passthrough_test.go 的全链路 rig 互补：那边用真实 relayclient（它总会
// offer [v2,v1]），这里用**裸客户端**精确控制 offer 的 ALPN 与帧里声明的编码，
// 于是「老客户端只 offer v1」「新客户端在 v2 上发 EncDcz2」都能单独钉住，
// 而上游收到的字节是判定 dex 解码是否正确的最终事实来源。
//
// 覆盖：
//   1. 新客户端 × v2 服务端：协商到 v2 → EncDcz2 请求被解码 → 上游逐字节收到
//      （ResultHash 由服务端校验，解码错一位都会被拒）
//   2. 老客户端（只 offer v1）× v2 服务端：仍协商 v1、EncDcz 照旧可解 —— 老客户端
//      互操作不因开关打开而改变
//   3. 回滚：-dcz2=false 的服务端对新客户端 = 协商降级到 v1；此时硬发 EncDcz2
//      会被明确拒绝（不是静默按 dcz 试解）

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jixoai/reply9842/internal/codec"
	"github.com/jixoai/reply9842/internal/meta"
	"github.com/jixoai/reply9842/internal/spool"
	"github.com/jixoai/reply9842/internal/stats"
	"github.com/jixoai/reply9842/internal/store"
	"github.com/jixoai/reply9842/internal/transport"
)

// ---- 链路 ----

type dcz2Rig struct {
	addr     string
	srv      *server
	upstream *httptest.Server
	stop     func()

	mu       sync.Mutex
	lastBody []byte // 上游最近收到的 body
}

func (g *dcz2Rig) lastUpstream() []byte {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]byte{}, g.lastBody...)
}

// dcz2Cert 自签证书（ECDSA）
func dcz2Cert(t *testing.T, dir string) (string, string) {
	t.Helper()
	certFile := filepath.Join(dir, "c.pem")
	keyFile := filepath.Join(dir, "k.pem")
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
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
		t.Fatal(err)
	}
	kd, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

// newDcz2Rig 起 relay-server（真 transport.Server，Dcz2 按参数）+ 假上游。
// 上游把收到的 body 原样回显（body 即证据）。
func newDcz2Rig(t *testing.T, dcz2 bool) *dcz2Rig {
	t.Helper()
	dir := t.TempDir()
	certFile, keyFile := dcz2Cert(t, dir)

	g := &dcz2Rig{}
	g.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		g.mu.Lock()
		g.lastBody = b
		g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write(b)
	}))

	ups, err := parseUpstreams(g.upstream.URL)
	if err != nil {
		t.Fatalf("parseUpstreams: %v", err)
	}
	sp, err := spool.New(spool.Options{
		Dir: filepath.Join(dir, "spool"), TmpBudget: 256 << 20, IdemBudget: 256 << 20,
	})
	if err != nil {
		t.Fatalf("spool.New: %v", err)
	}
	col, err := stats.New(filepath.Join(dir, "server.jsonl"), filepath.Join(dir, "server-stats.json"))
	if err != nil {
		t.Fatalf("stats.New: %v", err)
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	g.addr = addr

	c := &cfg{
		listen: addr, upstream: g.upstream.URL, upURLs: ups,
		upHost: "magpie.upstream.test", upstreamTimout: time.Minute,
		cacheMB: 128, spoolDir: filepath.Join(dir, "spool"), dcz2: dcz2,
	}
	g.srv = &server{
		c: c, store: store.New(c.cacheMB << 20), spool: sp, col: col,
		client: &http.Client{Transport: &http.Transport{
			MaxIdleConns: 20, MaxIdleConnsPerHost: 20,
			DisableCompression: true, DialContext: ups.dial,
		}},
	}

	ctx, cancel := context.WithCancel(context.Background())
	tr := &transport.Server{Addr: addr, CertFile: certFile, KeyFile: keyFile, Dcz2: dcz2}
	accepts, err := tr.Listen(ctx)
	if err != nil {
		cancel()
		t.Fatalf("transport listen: %v", err)
	}
	go func() {
		for a := range accepts {
			go g.srv.serve(a)
		}
	}()
	time.Sleep(100 * time.Millisecond)

	g.stop = func() {
		cancel()
		_ = sp.Close()
		_ = col.Close()
		g.upstream.Close()
	}
	t.Cleanup(g.stop)
	return g
}

// ---- 裸隧道客户端（4 字节大端长度前缀 + meta 帧，与 transport 的封帧一致） ----

type rawTunnel struct {
	c net.Conn
}

func rawDial(t *testing.T, addr string, protos []string) (*rawTunnel, string, error) {
	t.Helper()
	c, err := tls.Dial("tcp", addr, &tls.Config{
		NextProtos: protos, ServerName: "localhost", InsecureSkipVerify: true,
	})
	if err != nil {
		return nil, "", err
	}
	return &rawTunnel{c: c}, c.ConnectionState().NegotiatedProtocol, nil
}

func (r *rawTunnel) send(payload []byte) error {
	var lb [4]byte
	binary.BigEndian.PutUint32(lb[:], uint32(len(payload)))
	if _, err := r.c.Write(lb[:]); err != nil {
		return err
	}
	_, err := r.c.Write(payload)
	return err
}

func (r *rawTunnel) recv() ([]byte, error) {
	var lb [4]byte
	if _, err := io.ReadFull(r.c, lb[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(lb[:])
	if n > 256<<20 {
		return nil, fmt.Errorf("frame too large: %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r.c, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func (r *rawTunnel) close() { _ = r.c.Close() }

// request 发一帧请求（AcceptDownZstd 故意留 false：下行保持明文，断言直接可比）
//
// wire 是按 head.Encoding 编好的 body；plain 是它的明文（用于声明 ResultHash ——
// 服务端会拿它校验解码结果，一位不对就是 422，这也是测试的最后一道自检）。
func (r *rawTunnel) request(t *testing.T, seq byte, head *meta.RequestHead, wire, plain []byte) {
	t.Helper()
	head.Method, head.Path = http.MethodPost, "/v1/messages"
	head.BodyLen = int64(len(wire))
	head.IdemKey[31] = seq // 幂等键必须互不相同（同键会被服务端当作重复请求复用）
	head.ResultHash = convertHash(codec.Sum(plain))
	pl, err := meta.EncodeRequest(head, meta.EncodeHeaders(http.Header{
		"Content-Type": []string{"application/json"},
	}), wire)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	f, err := meta.WrapFrame(meta.FrameRequest, pl)
	if err != nil {
		t.Fatalf("WrapFrame: %v", err)
	}
	if err := r.send(f); err != nil {
		t.Fatalf("send: %v", err)
	}
}

// resp 读完整响应（head + 续片 + Done），返回 (status, body, 错误文本)
func (r *rawTunnel) resp(t *testing.T) (int, []byte, string) {
	t.Helper()
	var (
		status int
		body   []byte
	)
	for {
		payload, err := r.recv()
		if err != nil {
			return status, body, "recv: " + err.Error()
		}
		ft, pl, err := meta.ReadFrame(bytes.NewReader(payload))
		if err != nil {
			return status, body, "frame: " + err.Error()
		}
		switch ft {
		case meta.FrameResponse:
			rh, _, b, derr := meta.DecodeResponse(pl)
			if derr != nil {
				return status, body, "decode resp: " + derr.Error()
			}
			if rh.Status != 0 {
				status = rh.Status
			}
			if rh.Encoding != meta.EncIdentity && len(b) > 0 {
				return status, body, fmt.Sprintf("下行编码 %v 未预期（本测试不声明 AcceptDownZstd）", rh.Encoding)
			}
			body = append(body, b...)
		case meta.FrameDone:
			return status, body, ""
		case meta.FrameError:
			return status, body, "server: " + string(pl)
		case meta.FrameDeltaMiss:
			return status, body, "delta-miss"
		}
	}
}

// waitStore 等某份正文进到服务端 store（pump 结束后 Put 是异步的）
func waitStore(t *testing.T, s *server, body []byte) {
	t.Helper()
	h := codec.Sum(body)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s.store.Has(h) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("store 里始终没有这份基线（%s）", h)
}

// dcz2Body 造一份够大的正文（> MinDelta，且块对齐可复用）
func dcz2Body(marker string, n int) []byte {
	return []byte(strings.Repeat("{\"role\":\"user\",\"content\":\""+marker+" content...\"},", n))
}

// TestDcz2E2ENewClientV2Server 新客户端 × 开启 dcz2 的服务端：
// 协商 v2 → EncDcz2 上行被解码 → 上游逐字节收到 → 响应回到下游。
func TestDcz2E2ENewClientV2Server(t *testing.T) {
	g := newDcz2Rig(t, true)
	rt, proto, err := rawDial(t, g.addr, []string{transport.ALPNv2, transport.ALPNv1})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer rt.close()
	if proto != transport.ALPNv2 {
		t.Fatalf("协商到 %q，期望 %q", proto, transport.ALPNv2)
	}

	base := dcz2Body("base", 600)
	next := append(append([]byte{}, base...), dcz2Body("next", 8)...)

	// 1) 先把 base 送进服务端（identity），它会被存成基线
	rt.request(t, 1, &meta.RequestHead{Encoding: meta.EncIdentity}, base, base)
	if st, body, errs := rt.resp(t); errs != "" || st != 200 || !bytes.Equal(body, base) {
		t.Fatalf("第一次请求失败：status=%d err=%q body=%d", st, errs, len(body))
	}
	waitStore(t, g.srv, base)

	// 2) dcz2：字典靠 BaseHash（BLAKE3）定位，头里是 SHA-256(base)
	wire, err := codec.EncodeDcz2(next, 19, base)
	if err != nil {
		t.Fatalf("EncodeDcz2: %v", err)
	}
	rt.request(t, 2, &meta.RequestHead{
		Encoding: meta.EncDcz2,
		BaseHash: convertHash(codec.Sum(base)),
	}, wire, next)

	st, body, errs := rt.resp(t)
	if errs != "" {
		t.Fatalf("dcz2 请求失败：status=%d err=%s", st, errs)
	}
	if st != 200 {
		t.Fatalf("status=%d，期望 200", st)
	}
	if !bytes.Equal(body, next) {
		t.Fatalf("下游响应 %d 字节，期望 %d（上游回显了它收到的 body）", len(body), len(next))
	}
	if got := g.lastUpstream(); !bytes.Equal(got, next) {
		t.Fatalf("上游收到的 body %d 字节，期望 %d —— dcz2 解码不正确", len(got), len(next))
	}
}

// TestDcz2E2EOldClientV1OnNewServer 老客户端（只 offer reply9842/1）× 开启
// dcz2 的服务端：协商仍是 v1，私有 dcz 照旧可解 —— 老客户端互操作零变化。
func TestDcz2E2EOldClientV1OnNewServer(t *testing.T) {
	g := newDcz2Rig(t, true)
	rt, proto, err := rawDial(t, g.addr, []string{transport.ALPNv1}) // 老客户端的 offer
	if err != nil {
		t.Fatalf("老客户端握手失败（不能拒连）: %v", err)
	}
	defer rt.close()
	if proto != transport.ALPNv1 {
		t.Fatalf("协商到 %q，期望 %q", proto, transport.ALPNv1)
	}

	base := dcz2Body("legacy", 600)
	next := append(append([]byte{}, base...), []byte(`{"legacy":true}`)...)

	rt.request(t, 11, &meta.RequestHead{Encoding: meta.EncIdentity}, base, base)
	if st, _, errs := rt.resp(t); errs != "" || st != 200 {
		t.Fatalf("老客户端首次请求失败：status=%d err=%q", st, errs)
	}
	waitStore(t, g.srv, base)

	// 老编码：私有 dcz（BLAKE3 头 + 私有字典 ID）
	wire, err := codec.EncodeDcz(next, codec.Sum(base), 19, base)
	if err != nil {
		t.Fatalf("EncodeDcz: %v", err)
	}
	rt.request(t, 12, &meta.RequestHead{
		Encoding: meta.EncDcz,
		BaseHash: convertHash(codec.Sum(base)),
	}, wire, next)
	if st, body, errs := rt.resp(t); errs != "" || st != 200 || !bytes.Equal(body, next) {
		t.Fatalf("老客户端 dcz 请求失败：status=%d err=%q body=%d", st, errs, len(body))
	}
	if got := g.lastUpstream(); !bytes.Equal(got, next) {
		t.Fatalf("上游收到的 body %d 字节，期望 %d", len(got), len(next))
	}
}

// TestDcz2E2EKillSwitchDowngradeAndReject -dcz2=false：
//   - 新客户端（offer [v2,v1]）协商降级到 v1，不报错；
//   - 硬发 EncDcz2 被明确拒绝（错误信息带 dcz2）。
func TestDcz2E2EKillSwitchDowngradeAndReject(t *testing.T) {
	g := newDcz2Rig(t, false)
	rt, proto, err := rawDial(t, g.addr, []string{transport.ALPNv2, transport.ALPNv1})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer rt.close()
	if proto != transport.ALPNv1 {
		t.Fatalf("关闭 dcz2 时协商到 %q，期望降级到 %q", proto, transport.ALPNv1)
	}

	base := dcz2Body("kill", 600)
	next := append(append([]byte{}, base...), dcz2Body("next", 4)...)

	rt.request(t, 21, &meta.RequestHead{Encoding: meta.EncIdentity}, base, base)
	if st, _, errs := rt.resp(t); errs != "" || st != 200 {
		t.Fatalf("首次请求失败：status=%d err=%q", st, errs)
	}
	waitStore(t, g.srv, base)

	wire, err := codec.EncodeDcz2(next, 19, base)
	if err != nil {
		t.Fatal(err)
	}
	rt.request(t, 22, &meta.RequestHead{
		Encoding: meta.EncDcz2,
		BaseHash: convertHash(codec.Sum(base)),
	}, wire, next)
	st, body, errs := rt.resp(t)
	if errs == "" {
		t.Fatalf("关闭 dcz2 时 EncDcz2 必须被拒，却回了 status=%d body=%d 字节", st, len(body))
	}
	if !strings.Contains(errs, "dcz2") {
		t.Fatalf("拒绝原因必须带编码名，得到 %q", errs)
	}
	t.Logf("拒绝理由: %s", errs)
}
