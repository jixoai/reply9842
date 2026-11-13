// relay-client — 本地 HTTP 入口 + 隧道客户端
//
// 职责（2026-11-12 kzf 需求：「magpie.aiweb.xin 发往 relay-client，然后由
// relay-client 和 relay-server 进行压缩通讯」）：
//  1. 本地监听 HTTP（Agent 配 base_url 指过来）
//  2. 为每个请求体选基线（basefind），做 dcz delta 或 zstd 压缩
//  3. 经 QUIC/TCP 隧道发给 relay-server
//  4. 收到 DeltaMiss 时降级重发全量（单次重试）
//  5. 流式回传：边收边刷给下游，断线时按 IdemKey + 偏移/事件 id 自动续传
//  6. 逐请求记录压缩前后体积、传输耗时、延迟分位
package main

import (
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
	"sync"
	"syscall"
	"time"

	"github.com/jixoai/rfc9842-reply/internal/basefind"
	"github.com/jixoai/rfc9842-reply/internal/clientstream"
	"github.com/jixoai/rfc9842-reply/internal/codec"
	"github.com/jixoai/rfc9842-reply/internal/idem"
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
	historyMB  int64
	captureDir string
	captureN   int
	maxResume  int
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
	flag.Int64Var(&c.historyMB, "history-mb", 64, "基线历史总字节预算（MB）")
	flag.StringVar(&c.captureDir, "capture-dir", "", "采样目录：把请求体原样落盘供离线分析（留空不采）")
	flag.IntVar(&c.captureN, "capture-n", 30, "最多采样多少条")
	flag.IntVar(&c.maxResume, "max-resume", 3, "断线后最大续传重试次数（0=不续传）")
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
	finder := basefind.New(c.historyMB)

	log.Printf("relay-client 启动")
	log.Printf("  本地 HTTP   http://%s", c.listen)
	log.Printf("  隧道上游    %s (%s)", c.server, mode)
	log.Printf("  delta 门槛  >=%d 字节（块复用率>=50%%）", c.minDelta)
	log.Printf("  基线历史    %d MB", c.historyMB)
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

// capture 把请求体原样落盘（按序号命名），用于离线重放分析。
// 只采前 captureN 条，避免无限增长。
func (h *handler) capture(body []byte) {
	h.capMu.Lock()
	n := h.capCount
	if n >= h.c.captureN {
		h.capMu.Unlock()
		return
	}
	h.capCount++
	h.capMu.Unlock()

	os.MkdirAll(h.c.captureDir, 0o755)
	path := filepath.Join(h.c.captureDir, fmt.Sprintf("req-%04d.bin", n))
	if err := os.WriteFile(path, body, 0o600); err != nil {
		log.Printf("capture write err: %v", err)
		return
	}
	log.Printf("captured #%d %d bytes -> %s", n, len(body), path)
}

type handler struct {
	c        *cfg
	dialer   *transport.Dialer
	finder   *basefind.Finder
	col      *stats.Collector
	capMu    sync.Mutex
	capCount int
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

	// 采样（仅在开启时）：原样落盘，供离线分析二进制前缀特征
	if h.c.captureDir != "" {
		h.capture(body)
	}

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
		bh, baseBody := h.finder.Pick(body)
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

	// ---- 幂等键（同内容同键，用于断线续传与去重）----
	callerKey := idem.CallerKeyID(r.Header)
	idemKey := idem.Key(callerKey, r, body)

	// ---- 构造帧 ----
	headers := meta.EncodeHeaders(r.Header)
	reqHead := &meta.RequestHead{
		Method: r.Method, Path: r.URL.Path,
		BaseHash: convertHash(baseHash), ResultHash: convertHash(codec.Sum(body)),
		Encoding: enc, BodyLen: int64(len(payload)),
		IdemKey:    convertHash(idemKeyToCodec(idemKey)),
		ResumeFrom: 0,
	}
	reqPayload, err := meta.EncodeRequest(reqHead, headers, payload)
	if err != nil {
		http.Error(w, "encode: "+err.Error(), 500)
		rec.Status, rec.Err = 500, err.Error()
		h.col.Observe(rec)
		return
	}
	frame, err := meta.WrapFrame(meta.FrameRequest, reqPayload)
	if err != nil {
		http.Error(w, "wrap frame: "+err.Error(), 500)
		rec.Status, rec.Err = 500, err.Error()
		h.col.Observe(rec)
		return
	}

	// ---- 流式传输 + 断线续传 ----
	wireStart := time.Now()
	stream := &clientstream.Session{IdemKey: idemKey}

	conn, err := h.dialer.Dial(r.Context())
	if err != nil {
		http.Error(w, "dial: "+err.Error(), 502)
		rec.Status, rec.Err = 502, err.Error()
		rec.MsTotal = time.Since(start).Milliseconds()
		h.col.Observe(rec)
		return
	}
	if err := transport.WriteFrame(conn, frame); err != nil {
		conn.Close()
		http.Error(w, "send: "+err.Error(), 502)
		rec.Status, rec.Err = 502, err.Error()
		rec.MsTotal = time.Since(start).Milliseconds()
		h.col.Observe(rec)
		return
	}
	clientstream.Drain(conn, w, stream, start)
	conn.Close()

	// DeltaMiss / 断线 → 按需要重发
	if stream.ResumeMiss {
		// 服务端无该 key（store 淘汰或首发失败）→ 全量重发一次
		z, cerr := codec.Compress(body, codec.PickLevel(len(body)), nil)
		if cerr != nil || len(z) >= len(body) {
			z = body
			reqHead.Encoding = meta.EncIdentity
		} else {
			reqHead.Encoding = meta.EncZstd
		}
		reqHead.BaseHash = meta.Hash{}
		reqHead.BodyLen = int64(len(z))
		pl2, e1 := meta.EncodeRequest(reqHead, headers, z)
		if e1 == nil {
			f2, e2 := meta.WrapFrame(meta.FrameRequest, pl2)
			if e2 == nil {
				if c2, e3 := h.dialer.Dial(r.Context()); e3 == nil {
					transport.WriteFrame(c2, f2)
					clientstream.Drain(c2, w, stream, start)
					c2.Close()
				}
			}
		}
		rec.Encoding = "resend-full"
	}

	// 连接中断但上游未完成 → 尝试续传。
	// 关键：若下游（HTTP 客户端）已断开，续传毫无意义——数据没人接收。
	if isDownstreamGone(stream.Err) {
		log.Printf("下游已断开，停止续传 key=%x（条目保留在服务端幂等区）", idemKey[:4])
	}
	// 只有「可恢复的中断」才续传：已完成 / 服务端判定失败 / 下游已走 都不续传
	for att := 0; att < h.c.maxResume && !stream.Completed && !stream.Fatal &&
		stream.Err != nil && !stream.ResumeMiss && !isDownstreamGone(stream.Err); att++ {
		log.Printf("断线续传 #%d：key=%x from=%d event=%q err=%v",
			att+1, idemKey[:4], stream.Bytes, stream.EndEventID, stream.Err)
		time.Sleep(time.Duration(att+1) * 300 * time.Millisecond)

		rf, e := clientstream.BuildResumeFrame(idemKey, stream.Bytes, stream.EndEventID)
		if e != nil {
			break
		}
		c3, e := h.dialer.Dial(r.Context())
		if e != nil {
			continue
		}
		stream.Err = nil
		if e := transport.WriteFrame(c3, rf); e != nil {
			c3.Close()
			stream.Err = e
			continue
		}
		clientstream.Drain(c3, w, stream, start)
		c3.Close()
		if stream.ResumeMiss || stream.Completed || stream.Fatal {
			stream.Err = nil
			break
		}
	}

	rec.MsWire = time.Since(wireStart).Milliseconds()
	rec.Transport = string(h.dialer.LastKind())
	rec.Status = stream.Status
	rec.DownRaw = stream.Bytes
	// down_wire 是隧道实际承载量（含帧头/续片头），不是 body 长度。
	// 流式场景下每 32KiB 续片都带 17 字节帧头，两者必然不等。
	rec.DownWire = stream.WireBytes
	if rec.DownWire == 0 && stream.Bytes > 0 {
		rec.DownWire = stream.Bytes // 兜底：极早期版本路径
	}
	rec.MsTotal = time.Since(start).Milliseconds()
	// TTFB：首个 body 字节抵达下游的时刻（流式路径原先恒为 0）
	rec.MsUpstrm = stream.TTFB.Milliseconds()
	if stream.Err != nil && !stream.Completed {
		rec.Err = stream.Err.Error()
	}
	if rec.Status == 0 {
		rec.Status = 200
	}

	if d := float64(rec.MsWire) / 1000; d > 0 {
		rec.UpBps = float64(rec.UpWire) / d
	}
	if secs := float64(rec.MsTotal) / 1000; secs > 0 {
		rec.DownBps = float64(stream.Bytes) / secs
	}
	h.col.Observe(rec)
}

// isDownstreamGone 判断错误是否来自「写下游失败」（客户端已断开）
//
// 这类错误不应触发续传：下游没人接收，重连只会白等上游。
// 条目会由服务端在 Detach 后保留在幂等区，供客户端**重新发起请求**时复用。
func isDownstreamGone(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, pat := range []string{
		"broken pipe", "connection reset by peer", "write: ",
		"use of closed network connection",
	} {
		if strings.Contains(msg, pat) {
			return true
		}
	}
	return false
}

// idemKeyToCodec [32]byte → codec.Hash（类型别名转换）
func idemKeyToCodec(k [32]byte) codec.Hash {
	var out codec.Hash
	copy(out[:], k[:])
	return out
}

// ---- 哈希/编码类型转换（meta 与 codec 各自定义，避免包依赖倒置） ----

func convertHash(h codec.Hash) meta.Hash {
	var out meta.Hash
	copy(out[:], h[:])
	return out
}

// 保留 json 引用供后续配置扩展
var _ = json.Marshal
