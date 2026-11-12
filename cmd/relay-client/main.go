// relay-client — 本地 HTTP 入口 + 隧道客户端
//
// 职责（2026-11-12 kzf 需求：「magpie.aiweb.xin 发往 relay-client，然后由
// relay-client 和 relay-server 进行压缩通讯」）：
//   1. 本地监听 HTTP（Agent 配 base_url 指过来）
//   2. 为每个请求体选基线（basefind），做 dcz delta 或 zstd 压缩
//   3. 经 QUIC/TCP 隧道发给 relay-server
//   4. 收到 DeltaMiss 时降级重发全量（单次重试）
//   5. 逐请求记录压缩前后体积、传输耗时、延迟分位
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jixoai/rfc9842-reply/internal/basefind"
	"github.com/jixoai/rfc9842-reply/internal/codec"
	"github.com/jixoai/rfc9842-reply/internal/meta"
	"github.com/jixoai/rfc9842-reply/internal/stats"
	"github.com/jixoai/rfc9842-reply/internal/transport"
)

type cfg struct {
	listen     string
	server     string
	serverName string
	transport  string
	insecure   bool
	minDelta   int
	minPrefix  float64
	historyN   int
	logPath    string
	statsPath  string
}

func main() {
	c := cfg{}
	flag.StringVar(&c.listen, "listen", "127.0.0.1:8899", "本地 HTTP 监听")
	flag.StringVar(&c.server, "server", "127.0.0.1:9000", "relay-server 地址 host:port")
	flag.StringVar(&c.serverName, "server-name", "", "TLS SNI（留空自动推导）")
	flag.StringVar(&c.transport, "transport", "auto", "传输: auto|quic|tcp")
	flag.BoolVar(&c.insecure, "insecure", false, "跳过服务端证书校验（自签观察期）")
	flag.IntVar(&c.minDelta, "min-delta", 16<<10, "小于此字节数的请求体不做 delta")
	flag.Float64Var(&c.minPrefix, "min-prefix", 0.5, "公共前缀占比低于此值不做 delta")
	flag.IntVar(&c.historyN, "history", 8, "保留的历史基线条数")
	flag.StringVar(&c.logPath, "log", "", "逐请求日志（默认 ~/.rfc9842-reply/client-<date>.jsonl）")
	flag.StringVar(&c.statsPath, "stats", "", "汇总统计（默认同目录 stats.json）")
	flag.Parse()

	home, _ := os.UserHomeDir()
	if c.logPath == "" {
		c.logPath = filepath.Join(home, ".rfc9842-reply",
			"client-"+time.Now().Format("2006-01-02")+".jsonl")
	}
	if c.statsPath == "" {
		c.statsPath = filepath.Join(filepath.Dir(c.logPath), "client-stats.json")
	}

	col, err := stats.New(c.logPath, c.statsPath)
	if err != nil {
		log.Fatalf("stats: %v", err)
	}
	defer col.Close()

	mode := transport.Kind(strings.ToLower(c.transport))
	dialer := &transport.Dialer{
		Addr: c.server, ServerName: c.serverName,
		Insecure: c.insecure, Mode: mode,
	}
	finder := basefind.New(c.historyN)

	log.Printf("relay-client 启动")
	log.Printf("  本地 HTTP   http://%s", c.listen)
	log.Printf("  隧道上游    %s (%s)", c.server, mode)
	log.Printf("  delta 门槛  >=%d 字节 且 前缀>=%.0f%%", c.minDelta, c.minPrefix*100)
	log.Printf("  日志        %s", c.logPath)

	h := &handler{c: &c, dialer: dialer, finder: finder, col: col}
	srv := &http.Server{
		Addr:              c.listen,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
		// 不设 WriteTimeout：SSE 长连接需要
	}

	// 优雅退出
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		log.Printf("收到退出信号，正在关闭")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("listen: %v", err)
	}
}

type handler struct {
	c      *cfg
	dialer *transport.Dialer
	finder *basefind.Finder
	col    *stats.Collector
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rec := &stats.Record{Method: r.Method, Path: r.URL.Path}

	// ---- 读请求体 ----
	body, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		http.Error(w, "read body: "+err.Error(), 400)
		rec.Status, rec.Err = 400, err.Error()
		h.col.Observe(rec)
		return
	}
	rec.UpRaw = int64(len(body))

	// ---- 选择编码 ----
	enc := meta.EncIdentity
	encName := "identity"
	var payload []byte
	var baseHash codec.Hash

	encStart := time.Now()
	switch {
	case len(body) < h.c.minDelta:
		// 小请求：直接 zstd，不折腾 delta
		if len(body) > 1024 {
			z, err := codec.Compress(body, 19, nil)
			if err == nil && len(z) < len(body) {
				payload, enc = z, meta.EncZstd
				encName = "zstd"
			}
		}
		if payload == nil {
			payload = body
		}
	default:
		// 大请求：尝试 delta
		bh, baseBody := h.finder.Pick(body, h.c.minPrefix)
		if !bh.IsZero() {
			lvl := codec.PickLevel(len(body))
			z, err := codec.EncodeDcz(body, bh, lvl, baseBody)
			if err == nil {
				payload, enc, baseHash = z, meta.EncDcz, bh
				encName = fmt.Sprintf("dcz(L%d)", lvl)
				rec.BaseHit = true
			}
		}
		if payload == nil {
			// 无合适基线 → 普通 zstd
			lvl := codec.PickLevel(len(body))
			if z, err := codec.Compress(body, lvl, nil); err == nil && len(z) < len(body) {
				payload, enc = z, meta.EncZstd
				encName = fmt.Sprintf("zstd(L%d)", lvl)
			} else {
				payload = body
			}
		}
	}
	rec.MsEncode = time.Since(encStart).Milliseconds()
	rec.Encoding = encName
	rec.UpWire = int64(len(payload))

	// 记住这份 body 供下次当基线
	h.finder.Remember(body)

	// ---- 构造帧 ----
	headers := meta.EncodeHeaders(r.Header)
	reqHead := &meta.RequestHead{
		Method: r.Method, Path: r.URL.Path,
		BaseHash: convertHash(baseHash), ResultHash: convertHash(codec.Sum(body)),
		Encoding: enc, BodyLen: int64(len(payload)),
	}
	reqPayload, err := meta.EncodeRequest(reqHead, headers, payload)
	if err != nil {
		http.Error(w, "encode: "+err.Error(), 500)
		rec.Status, rec.Err = 500, err.Error()
		h.col.Observe(rec)
		return
	}
	// 包成带 magic 的完整 meta 帧
	var frameBuf bytes.Buffer
	if err := meta.WriteFrame(&frameBuf, meta.FrameRequest, reqPayload); err != nil {
		http.Error(w, "wrap frame: "+err.Error(), 500)
		rec.Status, rec.Err = 500, err.Error()
		h.col.Observe(rec)
		return
	}
	frame := frameBuf.Bytes()

	// ---- 隧道往返（含一次 DeltaMiss 重试）----
	wireStart := time.Now()
	resp, respBody, retryEnc, err := h.roundTrip(r.Context(), frame, reqHead, body, headers)
	rec.MsWire = time.Since(wireStart).Milliseconds()
	if retryEnc != "" {
		rec.Encoding = retryEnc
	}
	if err != nil {
		http.Error(w, "tunnel: "+err.Error(), 502)
		rec.Status, rec.Err = 502, err.Error()
		rec.MsTotal = time.Since(start).Milliseconds()
		h.col.Observe(rec)
		return
	}
	rec.Transport = string(h.dialer.LastKind())

	// ---- 回写 ----
	decStart := time.Now()
	if resp.Status == 0 {
		resp.Status = 200
	}
	for k, vs := range resp.HeadersDecoded() {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Del("Content-Length")
	w.WriteHeader(resp.Status)
	n, _ := w.Write(respBody)
	rec.MsDecode = time.Since(decStart).Milliseconds()

	rec.Status = resp.Status
	rec.DownRaw = int64(len(respBody))
	rec.DownWire = int64(len(respBody))
	rec.MsTotal = time.Since(start).Milliseconds()
	rec.MsUpstrm = resp.MsUpstream

	// 速率（KB/s → B/s）
	if d := float64(rec.MsWire) / 1000; d > 0 {
		rec.UpBps = float64(rec.UpWire) / d
	}
	if secs := float64(rec.MsTotal) / 1000; secs > 0 {
		rec.DownBps = float64(n) / secs
	}
	h.col.Observe(rec)
}

// roundTrip 发一帧并收响应；DeltaMiss 时用全量重发一次
func (h *handler) roundTrip(ctx context.Context, frame []byte, reqHead *meta.RequestHead,
	body []byte, headers []byte) (*responseResult, []byte, string, error) {

	res, err := h.exchange(ctx, frame)
	if err != nil {
		return nil, nil, "", err
	}
	if res.kind == meta.FrameDeltaMiss {
		// 全量重发（zstd，不用 delta）
		z, cerr := codec.Compress(body, codec.PickLevel(len(body)), nil)
		if cerr != nil || len(z) >= len(body) {
			z = body
		} else {
			reqHead.Encoding = meta.EncZstd
		}
		reqHead.BaseHash = meta.Hash{}
		reqHead.BodyLen = int64(len(z))
		payload2, eerr := meta.EncodeRequest(reqHead, headers, z)
		if eerr != nil {
			return nil, nil, "", eerr
		}
		var fb bytes.Buffer
		if err := meta.WriteFrame(&fb, meta.FrameRequest, payload2); err != nil {
			return nil, nil, "", err
		}
		frame2 := fb.Bytes()
		res, err = h.exchange(ctx, frame2)
		if err != nil {
			return nil, nil, "", err
		}
		encName := "zstd"
		if len(z) == len(body) {
			encName = "identity"
		}
		return res, res.body, encName, nil
	}
	return res, res.body, "", nil
}

type responseResult struct {
	kind       meta.FrameType
	Status     int
	MsUpstream int64
	headerRaw  []byte
	body       []byte
}

func (r *responseResult) HeadersDecoded() map[string][]string {
	return meta.DecodeHeaders(r.headerRaw)
}

func (h *handler) exchange(ctx context.Context, frame []byte) (*responseResult, error) {
	conn, err := h.dialer.Dial(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	// 两层结构：transport 4 字节长度前缀 + meta 帧（含自身 10 字节头）
	log.Printf("[%s] 发送 %d 字节", conn.Kind(), len(frame))
	if err := transport.WriteFrame(conn, frame); err != nil {
		log.Printf("[%s] send err: %v", conn.Kind(), err)
		return nil, err
	}
	raw, err := conn.Recv()
	if err != nil {
		log.Printf("[%s] recv err: %v", conn.Kind(), err)
		return nil, err
	}
	log.Printf("[%s] 收到 %d 字节", conn.Kind(), len(raw))
	ft, payload, err := meta.ReadFrame(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	switch ft {
	case meta.FrameError:
		return nil, fmt.Errorf("server error: %s", string(payload))
	case meta.FrameDeltaMiss:
		return &responseResult{kind: meta.FrameDeltaMiss}, nil
	case meta.FrameResponse:
		rh, hdr, body, err := meta.DecodeResponse(payload)
		if err != nil {
			return nil, err
		}
		return &responseResult{
			kind: meta.FrameResponse, Status: rh.Status,
			MsUpstream: 0, headerRaw: hdr, body: body,
		}, nil
	}
	return nil, fmt.Errorf("unexpected frame type %d", ft)
}

// ---- 哈希/编码类型转换（meta 与 codec 各自定义，避免包依赖倒置） ----

func convertHash(h codec.Hash) meta.Hash {
	var out meta.Hash
	copy(out[:], h[:])
	return out
}

// 保留 json 引用供后续配置扩展
var _ = json.Marshal
