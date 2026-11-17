package main

// 透传回归：另一台 magpie 的 RFC 9842 dcz 协商（X-Magpie-* 头 + dcz 请求体）
// 必须逐字节无损地走完「本地 HTTP client → 隧道 → relay-server → 上游」全程。
//
// 为什么放在 cmd/server 而不是 internal/e2e：只有 package main 里能看到真实的
// server 结构体（main 包不能被 import）。整条链路全部进程内启动：
//   httptest 假上游 ← 真实 server 结构 + 真实 spool/store/client
//   ← 真实 transport（TCP+TLS，自签证书）← 真实 relayclient.Runner ← 测试用 HTTP 客户端
// CI 里零外部依赖、零手工步骤；QUIC 不参与（Runner 强制 Transport=tcp），
// 避免 UDP 抖动带来的假红。
//
// 覆盖（对应任务清单 2 的四条）：
//   1. 请求头透传   5 个协商头（含 :base64: 冒号值）在上游「规范化后等价」；
//                   同时校验 Content-Length 被重算成转发 body 的长度
//   2. 响应头透传   400 + X-Magpie-Dictionary-Miss / 200 + X-Magpie-Kept-Body
//                   必须原样出现在下游（上游错误响应不被吞、不被改写）
//   3. body 字节一致 伪 dcz（头部合法、正文不可解压）经链路后逐字节一致；
//                   覆盖客户端自身 dcz/zstd 重编码后再由服务端解码的往返
//   4. 大 body      ≥300KB 上行（含客户端 dcz delta 路径）与下行（含 spool
//                   32KB 分片 + 下行 zstd 压缩）仍逐字节一致
//
// 已知破坏路径的「钉住」用例（当前必然失败，不要 t.Skip）见
// passthrough_losses_test.go。

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jixoai/reply9842/internal/codec"
	"github.com/jixoai/reply9842/internal/relayclient"
	"github.com/jixoai/reply9842/internal/spool"
	"github.com/jixoai/reply9842/internal/stats"
	"github.com/jixoai/reply9842/internal/store"
	"github.com/jixoai/reply9842/internal/transport"
)

// ---- 假上游记录 ----

// upstreamHit 上游（假 magpie B）实际收到的请求快照。
type upstreamHit struct {
	Method        string
	URI           string // r.RequestURI：路径 + 原始 query（查 query 丢失用）
	Host          string
	ContentLength int64
	Header        http.Header
	Body          []byte
}

// testRig 一条完整的进程内链路。
type testRig struct {
	t         *testing.T
	upstream  *httptest.Server
	srv       *server
	runner    *relayclient.Runner
	clientLog string
	serverLog string

	mu    sync.Mutex
	hits  []upstreamHit
	reply func(w http.ResponseWriter, r *http.Request)
}

// upstreamHandler 默认行为：回 200 + X-Magpie-Kept-Body（值 = 收到的 body 的
// SHA-256，RFC 8941 byte sequence 形式），这样响应头本身的正确性也能闭环验证。
func (g *testRig) upstreamHandler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)

	g.mu.Lock()
	g.hits = append(g.hits, upstreamHit{
		Method: r.Method, URI: r.RequestURI, Host: r.Host,
		ContentLength: r.ContentLength, Header: r.Header.Clone(), Body: body,
	})
	reply := g.reply
	g.mu.Unlock()

	if reply != nil {
		reply(w, r)
		return
	}
	w.Header().Set("X-Magpie-Kept-Body", byteSequence(body))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ok":true}`))
}

// byteSequence 按 RFC 8941 byte sequence 编码：:base64:
func byteSequence(body []byte) string {
	sum := sha256.Sum256(body)
	return ":" + base64.StdEncoding.EncodeToString(sum[:]) + ":"
}

func (g *testRig) setReply(fn func(w http.ResponseWriter, r *http.Request)) {
	g.mu.Lock()
	g.reply = fn
	g.mu.Unlock()
}

func (g *testRig) hitsSnapshot() []upstreamHit {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]upstreamHit, len(g.hits))
	copy(out, g.hits)
	return out
}

// ---- 链路搭建 ----

// newTestRig 起一条完整链路：假上游 + relay-server + 隧道 + relay-client。
// 所有端口都是 127.0.0.1 随机端口，证书运行时生成在 t.TempDir()。
func newTestRig(t *testing.T) *testRig {
	t.Helper()
	dir := t.TempDir()
	certFile, keyFile := writeSelfSignedCert(t, dir)

	g := &testRig{t: t}
	g.upstream = httptest.NewServer(http.HandlerFunc(g.upstreamHandler))

	addr := reserveTCPAddr(t)
	ups, err := parseUpstreams(g.upstream.URL)
	if err != nil {
		t.Fatalf("parseUpstreams(%q): %v", g.upstream.URL, err)
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

	c := &cfg{
		listen: addr, upstream: g.upstream.URL, upURLs: ups,
		upHost: "magpie.upstream.test", upstreamTimout: time.Minute,
		cacheMB: 128, spoolDir: filepath.Join(dir, "spool"),
	}
	g.srv = &server{
		c: c, store: store.New(c.cacheMB << 20), spool: sp, col: col,
		client: &http.Client{Transport: &http.Transport{
			MaxIdleConns: 20, MaxIdleConnsPerHost: 20,
			DisableCompression: true, DialContext: ups.dial,
		}},
	}

	ctx, cancel := context.WithCancel(context.Background())
	tr := &transport.Server{Addr: addr, CertFile: certFile, KeyFile: keyFile}
	accepts, err := tr.Listen(ctx)
	if err != nil {
		cancel()
		t.Fatalf("transport listen %s: %v", addr, err)
	}
	go func() {
		for a := range accepts {
			go g.srv.serve(a)
		}
	}()

	// 客户端入口：真实 relayclient.Runner，监听随机端口。
	g.clientLog = filepath.Join(dir, "client.jsonl")
	g.serverLog = filepath.Join(dir, "server.jsonl")
	g.runner = relayclient.New(relayclient.Config{
		Listen: "127.0.0.1:0", Server: addr, ServerName: "localhost",
		Insecure: true, Transport: string(transport.KindTCP), LogPath: g.clientLog,
	})
	if err := g.runner.Start(); err != nil {
		cancel()
		t.Fatalf("relayclient.Start: %v", err)
	}

	t.Cleanup(func() {
		_ = g.runner.Stop(context.Background())
		_ = g.runner.Close()
		cancel()
		_ = sp.Close()
		_ = col.Close()
		g.upstream.Close()
	})
	return g
}

// post 往下游（relay-client 的本地入口）发一次请求，返回状态码/响应头/响应体。
func (g *testRig) post(uri string, body []byte, hdrs map[string][]string) (int, http.Header, []byte) {
	g.t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://"+g.runner.Addr()+uri, bytes.NewReader(body))
	if err != nil {
		g.t.Fatalf("NewRequest: %v", err)
	}
	for k, vs := range hdrs {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	cl := &http.Client{Timeout: 60 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		g.t.Fatalf("POST %s: %v", uri, err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		g.t.Fatalf("读响应体: %v", err)
	}
	return resp.StatusCode, resp.Header, got
}

// ---- 断言助手 ----

// assertHeaderEqual 断言「规范化后等价」：Go 的 net/http 会把键名 canonical 化
// （x-magpie-keeP-body → X-Magpie-Keep-Body），值的字节必须原样。
func assertHeaderEqual(t *testing.T, h http.Header, key, want string) {
	t.Helper()
	got := h.Values(key) // Values 内部做 canonical 化查找，即「规范化后等价」
	if len(got) != 1 || got[0] != want {
		t.Errorf("头 %s 未逐字节透传（规范化后等价失败）：got %#v want %q", key, got, want)
	}
}

func assertSameBody(t *testing.T, label string, got, want []byte) {
	t.Helper()
	if bytes.Equal(got, want) {
		return
	}
	gs, ws := sha256.Sum256(got), sha256.Sum256(want)
	t.Errorf("%s 字节不一致：len %d vs %d，sha256 %x vs %x",
		label, len(got), len(want), gs[:8], ws[:8])
}

// ---- magpie 协商素材 ----

// magpieRFC9842Headers 造出另一台 magpie 在 dcz 协商中会出现的 5 个头。
// 其中两个（Kept-Body / Dictionary-Miss）正常只出现在响应方向；这里在请求里
// 也各带一份，一次性证明「这 5 个头经过我们的链路一个不少、一个不改」——
// 代理不应关心头的方向语义。
//
// 两个 byte sequence 值取定值、且特意含 `+` `/` `=`（真实 sha256 的 base64
// 也几乎必然含这些标点）：确保头值里的标点不会被任何一层转义、截断或改写。
func magpieRFC9842Headers(body []byte) map[string][]string {
	const (
		keptBody  = ":+/v7+/v7+/v7+/v7+/v7+/v7+/v7+/v7+/v7+/v7+/s=:"
		available = "://////////////////////////////////////////8=:"
	)
	digest := sha256.Sum256(body)
	return map[string][]string{
		"Content-Type":             {"application/json"},
		"Content-Encoding":         {"dcz"},
		"X-Magpie-Keep-Body":       {"1"},
		"X-Magpie-Kept-Body":       {keptBody},
		"Available-Dictionary":     {available},
		"X-Magpie-Body-Digest":     {hex.EncodeToString(digest[:])},
		"X-Magpie-Dictionary-Miss": {"1"},
	}
}

// fakeDczBody 造一段「带合法 dcz 40 字节头、正文却不可解压」的伪 dcz：
// 8 字节 skippable magic + 32 字节字典哈希（RFC 9842 §5），后面接确定性伪随机
// 字节（不是合法 zstd 流）。对隧道而言它必须是不透明的字节流——任何一侧试图
// 按 zstd 解它都会失败；本用例证明没有一侧这么做，且字节没被改动。
func fakeDczBody(n int, seed string) []byte {
	hdr := []byte{0x5e, 0x2a, 0x4d, 0x18, 0x20, 0x00, 0x00, 0x00}
	sum := sha256.Sum256([]byte("fake-dict-" + seed))
	hdr = append(hdr, sum[:]...)

	buf := make([]byte, 0, len(hdr)+n)
	buf = append(buf, hdr...)
	x := uint64(0x9E3779B97F4A7C15) ^ uint64(len(seed))*2654435761
	for i := 0; i < n; i++ {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		buf = append(buf, byte(x))
	}
	return buf
}

// largeJSONBody 造一份 ≥target 字节、高度可压缩、同构的「LLM 请求」body。
// variant 只影响一个数值字段：两份不同 variant 的 body 块复用率 ≈ 100%，
// 足以让客户端 basefind 命中并把第二份走 dcz delta 编码。
func largeJSONBody(target int, variant int) []byte {
	var b strings.Builder
	b.WriteString(`{"model":"test-model","stream":true,"variant":`)
	b.WriteString(strconv.Itoa(variant))
	b.WriteString(`,"messages":[`)
	for i := 0; b.Len() < target; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"role":"user","content":"The quick brown fox jumps over the lazy dog. 0123456789 segment=%d"}`+"\n", i)
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

// patternedBody 造一份 n 字节、可压缩的「SSE 响应」。
func patternedBody(n int) []byte {
	const chunk = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\"}}\n\n"
	var b bytes.Buffer
	for b.Len() < n {
		b.WriteString(chunk)
	}
	out := b.Bytes()
	return out[:n]
}

// ---- 客户端日志 ----

type clientLogRec struct {
	Method   string `json:"method"`
	Path     string `json:"path"`
	Status   int    `json:"status"`
	Enc      string `json:"enc"`
	BaseHit  bool   `json:"base_hit"`
	UpRaw    int64  `json:"up_raw"`
	UpWire   int64  `json:"up_wire"`
	DownRaw  int64  `json:"down_raw"`
	DownWire int64  `json:"down_wire"`
}

// waitClientLog 轮询客户端的逐请求日志，等到 n 条（或超时）。
// 日志是每请求完成时同步追加的，但「响应写给下游」与「Observe 落盘」之间有
// 极短的窗口，所以用轮询而不是 sleep 固定时长。
func (g *testRig) waitClientLog(n int) []clientLogRec {
	g.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		recs := g.readClientLog()
		if len(recs) >= n || time.Now().After(deadline) {
			if len(recs) < n {
				g.t.Fatalf("客户端日志只有 %d 条（期望 %d 条）：%s", len(recs), n, g.clientLog)
			}
			return recs
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (g *testRig) readClientLog() []clientLogRec {
	g.t.Helper()
	raw, err := os.ReadFile(g.clientLog)
	if err != nil {
		return nil
	}
	// 末行可能正在写入（尚无换行结尾）：先丢掉半行，轮询下一轮再读。
	// 与 lastServerRecord 同属「响应完成 → 日志落盘」的窗口竞态。
	if len(raw) > 0 && raw[len(raw)-1] != '\n' {
		if i := bytes.LastIndexByte(raw, '\n'); i >= 0 {
			raw = raw[:i]
		} else {
			return nil
		}
	}
	var out []clientLogRec
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var r clientLogRec
		if err := json.Unmarshal(line, &r); err != nil {
			g.t.Fatalf("客户端日志行无法解析: %v\n%s", err, line)
		}
		out = append(out, r)
	}
	return out
}

// waitStoreHas 等服务端的基版本库收下某份 body（第一条请求的 body 会作为
// 第二条 dcz 的字典）。不做这步时第二条可能因字典未就绪而走 delta-miss 重发，
// 用例就覆盖不到「客户端 dcz → 服务端解码」这条路径。
func (g *testRig) waitStoreHas(h codec.Hash) {
	g.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !g.srv.store.Has(h) {
		if time.Now().After(deadline) {
			g.t.Fatalf("服务端基版本库一直没收到字典 %s", h)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ---- 基础设施 ----

// reserveTCPAddr 拿一个空闲的 127.0.0.1 端口。listen 后立即关闭：transport.Server
// 的 QUIC(UDP) 与 TCP 需要落在同一端口号，而 :0 只让其中一个拿到随机端口。
func reserveTCPAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// writeSelfSignedCert 生成 127.0.0.1 的自签证书，供隧道 TLS 用。
func writeSelfSignedCert(t *testing.T, dir string) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "reply9842-passthrough-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("自签证书: %v", err)
	}
	certFile = filepath.Join(dir, "server.crt")
	keyFile = filepath.Join(dir, "server.key")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("DER 私钥: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatalf("写证书: %v", err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatalf("写私钥: %v", err)
	}
	return certFile, keyFile
}

// ---- 用例 1：请求头透传 ----

// TestMagpieDczNegotiationHeadersPassThrough 五个协商头 + dcz 请求体
// 经 client→server 后在上游「规范化后等价」，响应里的 X-Magpie-Kept-Body
// 原样回到下游。
func TestMagpieDczNegotiationHeadersPassThrough(t *testing.T) {
	g := newTestRig(t)

	body := fakeDczBody(8<<10, "hdr")
	sent := magpieRFC9842Headers(body)
	const uri = "/v1/messages"

	status, respHdr, respBody := g.post(uri, body, sent)
	if status != http.StatusOK {
		t.Fatalf("下游状态 = %d，想要 200", status)
	}

	hits := g.hitsSnapshot()
	if len(hits) != 1 {
		t.Fatalf("上游应收到 1 次请求，实际 %d 次", len(hits))
	}
	hit := hits[0]
	if hit.Method != http.MethodPost || hit.URI != uri {
		t.Errorf("上游收到 %s %s，想要 POST %s", hit.Method, hit.URI, uri)
	}

	// 五个协商头：键名允许 Go canonical 规范化，值必须逐字节一致。
	for _, key := range []string{
		"X-Magpie-Keep-Body", "X-Magpie-Kept-Body", "Available-Dictionary",
		"X-Magpie-Body-Digest", "X-Magpie-Dictionary-Miss",
	} {
		want := sent[key][0]
		assertHeaderEqual(t, hit.Header, key, want)
	}
	assertHeaderEqual(t, hit.Header, "Content-Encoding", "dcz")

	// Content-Length 必须按转发 body 重算（而不是沿用客户端声明的、也不是
	// 隧道内编码后的长度）。
	if hit.ContentLength != int64(len(body)) {
		t.Errorf("上游 ContentLength = %d，想要 %d（应按解码后 body 重算）", hit.ContentLength, len(body))
	}
	if v := hit.Header.Get("Transfer-Encoding"); v != "" {
		t.Errorf("Transfer-Encoding 不应透传到上游，得到 %q", v)
	}

	// 上行 body：逐字节一致（伪 dcz 不可解压，也不许被改）。
	assertSameBody(t, "上行 body（client→server→上游）", hit.Body, body)

	// 响应方向：X-Magpie-Kept-Body 必须原样回到下游（值 = 上游对收到的 body
	// 计算的 :base64 sha256:，闭环验证值没被改写）。
	assertHeaderEqual(t, respHdr, "X-Magpie-Kept-Body", byteSequence(body))
	assertSameBody(t, "下行 body（上游→server→client）", respBody, []byte(`{"ok":true}`))
}

// ---- 用例 2：响应头透传 / 上游错误不被吞 ----

// TestUpstreamDictionaryMissPassesThrough 上游 400 + X-Magpie-Dictionary-Miss: 1
// 必须原样到达下游：状态码不被改写（不是 200/502）、头不丢、错误体不丢。
func TestUpstreamDictionaryMissPassesThrough(t *testing.T) {
	g := newTestRig(t)
	upstreamBody := []byte(`{"error":{"type":"dictionary_miss","message":"please resend full body"}}`)
	g.setReply(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Magpie-Dictionary-Miss", "1")
		w.Header().Set("X-Magpie-Kept-Body", ":AQIDBA==:")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(upstreamBody)
	})

	body := fakeDczBody(4<<10, "miss")
	status, hdr, got := g.post("/v1/messages", body, magpieRFC9842Headers(body))

	if status != http.StatusBadRequest {
		t.Errorf("下游状态 = %d，想要 400（上游错误响应被吞/改写）", status)
	}
	assertHeaderEqual(t, hdr, "X-Magpie-Dictionary-Miss", "1")
	assertHeaderEqual(t, hdr, "X-Magpie-Kept-Body", ":AQIDBA==:")
	assertSameBody(t, "400 错误体", got, upstreamBody)

	// 上游错误响应也不许被写入幂等复用（同 key 重放会固化失败）——服务端
	// 应把它标记为 uncacheable，body 不进基版本库。
	if g.srv.store.Has(codec.Sum(body)) {
		t.Error("400 响应对应的请求体不该进入基版本库（uncacheable 语义）")
	}
}

// TestUpstreamResponseContentEncodingPassThrough 上游响应的 Content-Encoding
// 头与响应体一起原样回下游（与请求方向对称，防止「响应侧被按 zstd 重编码」）。
func TestUpstreamResponseContentEncodingPassThrough(t *testing.T) {
	g := newTestRig(t)
	upstreamBody := patternedBody(32 << 10)
	g.setReply(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "dcz")
		w.Header().Set("X-Magpie-Kept-Body", ":Zm9v:")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(upstreamBody)
	})

	status, hdr, got := g.post("/v1/messages", fakeDczBody(2<<10, "resp-ce"), nil)
	if status != http.StatusOK {
		t.Fatalf("下游状态 = %d，想要 200", status)
	}
	assertHeaderEqual(t, hdr, "Content-Encoding", "dcz")
	assertHeaderEqual(t, hdr, "X-Magpie-Kept-Body", ":Zm9v:")
	assertSameBody(t, "下行 body", got, upstreamBody)
}

// ---- 用例 3：伪 dcz body 字节一致（含客户端自身的 dcz 重编码往返） ----

// TestFakeDczRequestBodyByteIdenticalWithoutDecoding 两段「不可解压」的伪 dcz
// body：第一段走客户端 zstd/identity，第二段（与第一段高度相似）走客户端
// dcz delta 编码——两种路径下上游收到的都必须是原始字节，Content-Encoding
// 也始终原样保留。
func TestFakeDczRequestBodyByteIdenticalWithoutDecoding(t *testing.T) {
	g := newTestRig(t)
	hdrs := map[string][]string{"Content-Type": {"application/json"}, "Content-Encoding": {"dcz"}}

	b1 := fakeDczBody(200<<10, "payload")
	b2 := append([]byte(nil), b1...)
	b2[100] ^= 0xff       // 头部之后的第一块发生变化
	b2[len(b2)-1] ^= 0x0f // 尾部变化：让块复用率仍 ≈100%，且第二条 idem key 不同

	status1, _, _ := g.post("/v1/messages", b1, hdrs)
	if status1 != http.StatusOK {
		t.Fatalf("第一条状态 = %d，想要 200", status1)
	}
	// 等服务端把 b1 收进基版本库，否则第二条可能 delta-miss 重发（覆盖不到 dcz）。
	g.waitStoreHas(codec.Sum(b1))

	status2, _, _ := g.post("/v1/messages", b2, hdrs)
	if status2 != http.StatusOK {
		t.Fatalf("第二条状态 = %d，想要 200", status2)
	}

	hits := g.hitsSnapshot()
	if len(hits) != 2 {
		t.Fatalf("上游应收到 2 次请求，实际 %d 次", len(hits))
	}
	assertSameBody(t, "第一条上行 body", hits[0].Body, b1)
	assertSameBody(t, "第二条上行 body", hits[1].Body, b2)
	assertHeaderEqual(t, hits[0].Header, "Content-Encoding", "dcz")
	assertHeaderEqual(t, hits[1].Header, "Content-Encoding", "dcz")

	logs := g.waitClientLog(2)
	t.Logf("上行编码：req1=%s(wire %d/%d) req2=%s(wire %d/%d, base_hit=%v)",
		logs[0].Enc, logs[0].UpWire, logs[0].UpRaw,
		logs[1].Enc, logs[1].UpWire, logs[1].UpRaw, logs[1].BaseHit)
	if !strings.Contains(logs[1].Enc, "dcz") {
		t.Errorf("第二条请求没走 dcz delta 路径（enc=%q）：本用例没覆盖到「客户端 dcz 编码 → 服务端解码」的往返", logs[1].Enc)
	}
}

// ---- 用例 4：大 body（≥300KB）上行/下行字节一致 ----

// TestLargeBody300KByteIdentical 两条 ≥320KB 的同构 body：第一条 zstd、
// 第二条走客户端 dcz delta；上游响应 ≥350KB，触发 spool 32KB 分片与下行
// zstd 压缩。两个方向上 body 都必须逐字节一致。
func TestLargeBody300KByteIdentical(t *testing.T) {
	g := newTestRig(t)
	respBody := patternedBody(350 << 10)
	g.setReply(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Magpie-Kept-Body", byteSequence(respBody))
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(respBody)
	})

	hdrs := map[string][]string{"Content-Type": {"application/json"}, "Content-Encoding": {"dcz"}}
	b1 := largeJSONBody(320<<10, 1)
	b2 := largeJSONBody(320<<10, 2)
	if len(b1) < 300<<10 || len(b2) < 300<<10 {
		t.Fatalf("测试素材不足 300KB：%d / %d", len(b1), len(b2))
	}

	for i, b := range [][]byte{b1, b2} {
		status, hdr, got := g.post("/v1/messages", b, hdrs)
		if status != http.StatusOK {
			t.Fatalf("第 %d 条状态 = %d，想要 200", i+1, status)
		}
		assertSameBody(t, fmt.Sprintf("第 %d 条下行 body", i+1), got, respBody)
		assertHeaderEqual(t, hdr, "X-Magpie-Kept-Body", byteSequence(respBody))
		if i == 0 {
			// 第二条 dcz 需要第一条先成为服务端字典。
			g.waitStoreHas(codec.Sum(b1))
		}
	}

	hits := g.hitsSnapshot()
	if len(hits) != 2 {
		t.Fatalf("上游应收到 2 次请求，实际 %d 次", len(hits))
	}
	assertSameBody(t, "第一条上行 body（320KB）", hits[0].Body, b1)
	assertSameBody(t, "第二条上行 body（320KB）", hits[1].Body, b2)
	assertHeaderEqual(t, hits[0].Header, "Content-Encoding", "dcz")
	assertHeaderEqual(t, hits[1].Header, "Content-Encoding", "dcz")
	for i, hit := range hits {
		if hit.ContentLength != int64(len(hit.Body)) {
			t.Errorf("第 %d 条上游 ContentLength = %d，想要 %d", i+1, hit.ContentLength, len(hit.Body))
		}
	}

	logs := g.waitClientLog(2)
	for i, rec := range logs {
		t.Logf("第 %d 条：上行 enc=%s raw=%d wire=%d；下行 raw=%d wire=%d",
			i+1, rec.Enc, rec.UpRaw, rec.UpWire, rec.DownRaw, rec.DownWire)
		if rec.UpWire >= rec.UpRaw {
			t.Errorf("第 %d 条上行没被压缩：wire %d >= raw %d", i+1, rec.UpWire, rec.UpRaw)
		}
		if rec.DownWire >= rec.DownRaw {
			t.Errorf("第 %d 条下行没被压缩：wire %d >= raw %d", i+1, rec.DownWire, rec.DownRaw)
		}
	}
	if !strings.Contains(logs[1].Enc, "dcz") {
		t.Errorf("第二条上行没走 dcz delta 路径（enc=%q）", logs[1].Enc)
	}
}

// ---- 用例 5：delta-miss 全量重发路径 ----

// TestDeltaMissFullResendPreservesHeadersAndBody 客户端选了服务端没有的基线
// （前一条请求是 400，body 没进服务端基版本库）时，服务端回 FrameDeltaMiss，
// 客户端会把请求**全量重发**（runner.go 重编码 body、重建帧）。这条重发路径
// 也必须原样带上协商头与 body。
func TestDeltaMissFullResendPreservesHeadersAndBody(t *testing.T) {
	g := newTestRig(t)

	// 第一条：上游 400 → 服务端 MarkUncacheable → body 不进基版本库。
	rejectBody := []byte(`{"error":"rejected"}`)
	g.setReply(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(rejectBody)
	})
	b1 := largeJSONBody(64<<10, 1)
	status1, _, _ := g.post("/v1/messages", b1, magpieRFC9842Headers(b1))
	if status1 != http.StatusBadRequest {
		t.Fatalf("第一条状态 = %d，想要 400", status1)
	}
	if g.srv.store.Has(codec.Sum(b1)) {
		t.Fatal("前提不成立：400 响应后 b1 不该在服务端基版本库里")
	}

	// 第二条：与 b1 高度相似 → 客户端 basefind 选中 b1 → 服务端字典未命中
	// → DeltaMiss → 客户端全量重发（本次覆盖的就是这条重发帧）。
	g.setReply(nil)
	b2 := largeJSONBody(64<<10, 2)
	status2, _, got2 := g.post("/v1/messages", b2, magpieRFC9842Headers(b2))
	if status2 != http.StatusOK {
		t.Fatalf("第二条状态 = %d，想要 200（DeltaMiss 全量重发后应命中默认 200）", status2)
	}
	assertSameBody(t, "第二条下行 body", got2, []byte(`{"ok":true}`))

	hits := g.hitsSnapshot()
	if len(hits) != 2 {
		t.Fatalf("上游应收到 2 次请求（第一条 + 第二条重发），实际 %d 次", len(hits))
	}
	assertSameBody(t, "第一条上行 body", hits[0].Body, b1)
	assertSameBody(t, "第二条上行 body（DeltaMiss 重发）", hits[1].Body, b2)

	want := magpieRFC9842Headers(b2)
	for _, key := range []string{
		"X-Magpie-Keep-Body", "X-Magpie-Kept-Body", "Available-Dictionary",
		"X-Magpie-Body-Digest", "X-Magpie-Dictionary-Miss",
	} {
		if got := hits[1].Header.Values(key); len(got) != 1 || got[0] != want[key][0] {
			t.Errorf("重发帧丢了协商头 %s：got=%q want=%q", key, got, want[key][0])
		}
	}
	assertHeaderEqual(t, hits[1].Header, "Content-Encoding", "dcz")

	logs := g.waitClientLog(2)
	if logs[1].Enc != "resend-full" {
		t.Errorf("第二条没走 DeltaMiss 全量重发路径（enc=%q）：重发帧构造未被覆盖", logs[1].Enc)
	}
}

// ---- 行为记录（非破坏）：Accept-Encoding 被服务端刻意删除 ----

// TestServerDropsAcceptEncodingCharacterization 记录一个刻意行为：
// cmd/server/main.go 在转发到上游前 Del("Accept-Encoding")，配合
// DisableCompression 避免上游对响应体做 gzip（与我们的下行压缩叠床架屋）。
// 代价是「下游的响应压缩偏好不会传达给上游」——语义上近似透明（下游拿到
// 的响应仍由我们自己的压缩/透传决定），但不是逐头透传。
// 若将来决定把 Accept-Encoding 也转发给上游，请同步更新本用例。
func TestServerDropsAcceptEncodingCharacterization(t *testing.T) {
	g := newTestRig(t)
	hdrs := map[string][]string{"Accept-Encoding": {"zstd, gzip"}, "Content-Type": {"application/json"}}

	status, _, _ := g.post("/v1/messages", []byte(`{"hello":"world"}`), hdrs)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，想要 200", status)
	}
	hits := g.hitsSnapshot()
	if len(hits) != 1 {
		t.Fatalf("上游应收到 1 次请求，实际 %d 次", len(hits))
	}
	if got := hits[0].Header.Values("Accept-Encoding"); len(got) != 0 {
		t.Errorf("当前实现会删掉 Accept-Encoding（main.go:668），但这个头到了上游：%q", got)
	}
}
