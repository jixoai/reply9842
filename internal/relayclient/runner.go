// Package relayclient — 本地 HTTP 入口 + 隧道客户端的可复用运行体
//
// 正交意图（2 个，2026-11-13 kzf 需求：「把 cmd/relay-client 的主逻辑抽到
// internal/relayclient（一个可复用的 Runner，接受配置、启动 HTTP 监听、暴露统计），
// CLI 和 GUI 都调它」）：
//  1. 一个 Runner 完成「起监听 → 处理请求 → 停监听」的完整生命周期，可反复
//     Start/Stop（GUI 的启动/停止按钮直接按）。
//  2. 统计既落盘（CLI 的 status 子命令读文件），也支持进程内快照（GUI 每秒刷新），
//     两条读路径共用同一个 stats.Agg 结构。
//
// 为什么不把 handler 留在 cmd 里：GUI 与 CLI 必须跑同一份转发逻辑，任何一侧
// 单独演进都会让「GUI 能用、CLI 不能」这类裂缝出现。逻辑只有一份。
package relayclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jixoai/rfc9842-reply/internal/basefind"
	"github.com/jixoai/rfc9842-reply/internal/clientstream"
	"github.com/jixoai/rfc9842-reply/internal/codec"
	"github.com/jixoai/rfc9842-reply/internal/idem"
	"github.com/jixoai/rfc9842-reply/internal/meta"
	"github.com/jixoai/rfc9842-reply/internal/stats"
	"github.com/jixoai/rfc9842-reply/internal/transport"
)

// 默认值集中在一处：CLI 的 flag 默认值、GUI 的输入框初值都从这里取，
// 避免「文档写的默认」与「代码跑的默认」漂移。
const (
	DefaultListen    = "127.0.0.1:18899"
	DefaultServer    = "47.82.76.102:28443"
	DefaultTransport = "auto"
	DefaultMinDelta  = 16 << 10
	DefaultHistoryMB = 64
	DefaultCaptureN  = 30
	DefaultMaxResume = 3
)

// Config 一次运行所需的全部配置（与 CLI flag 一一对应）
type Config struct {
	Listen     string // 本地 HTTP 监听，如 127.0.0.1:18899
	Server     string // relay-server 地址 host:port
	ServerName string // TLS SNI，留空自动推导
	Transport  string // auto | quic | tcp
	Insecure   bool   // 跳过服务端证书校验（自签观察期）
	MinDelta   int    // 小于此字节数的请求体不做 delta
	HistoryMB  int64  // 基线历史总字节预算（MB）
	CaptureDir string // 采样目录，留空不采
	CaptureN   int    // 最多采样多少条
	MaxResume  int    // 断线后最大续传重试次数（0=不续传）
	LogPath    string // 逐请求日志
	StatsPath  string // 汇总统计
}

// Normalize 补齐默认值与派生路径（幂等，可反复调用）
func (c *Config) Normalize() {
	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	if c.Server == "" {
		c.Server = DefaultServer
	}
	if c.Transport == "" {
		c.Transport = DefaultTransport
	}
	if c.MinDelta == 0 {
		c.MinDelta = DefaultMinDelta
	}
	if c.HistoryMB == 0 {
		c.HistoryMB = DefaultHistoryMB
	}
	if c.CaptureN == 0 {
		c.CaptureN = DefaultCaptureN
	}
	if c.MaxResume == 0 {
		c.MaxResume = DefaultMaxResume
	}
	if c.LogPath == "" {
		c.LogPath = DefaultLogPath()
	}
	if c.StatsPath == "" {
		c.StatsPath = filepath.Join(filepath.Dir(c.LogPath), "client-stats.json")
	}
}

// DefaultDir 运行时数据目录（日志与统计同放这里）
func DefaultDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".rfc9842-reply")
}

// DefaultLogPath 当日日志路径（按日期滚动，便于 jq 直读）
func DefaultLogPath() string {
	return filepath.Join(DefaultDir(), "client-"+time.Now().Format("2006-01-02")+".jsonl")
}

// DefaultStatsPath 汇总统计路径
func DefaultStatsPath() string { return filepath.Join(DefaultDir(), "client-stats.json") }

// Runner 可反复启动/停止的客户端运行体
type Runner struct {
	mu sync.Mutex
	c  Config

	srv    *http.Server
	ln     net.Listener
	col    *stats.Collector
	dialer *transport.Dialer
	finder *basefind.Finder

	running   bool
	startedAt time.Time
	lastErr   error

	// Logger 运行日志出口；为 nil 时用 log.Default()（CLI 走标准输出，
	// GUI 没有终端，因此 GUI 会把它设成一个环状缓冲，供界面显示）。
	logger *log.Logger
	// OnEvent 每次启动/停止/出错时回调（GUI 用来触发重绘）。可为 nil。
	OnEvent func()
}

// New 创建 Runner（不启动）
func New(c Config) *Runner {
	c.Normalize()
	return &Runner{c: c, logger: log.Default()}
}

// SetLogger 替换日志出口（GUI 用内存环形缓冲，CLI 用标准输出）
func (r *Runner) SetLogger(l *log.Logger) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logger = l
}

// Config 当前生效配置（已 Normalize）
func (r *Runner) Config() Config {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.c
}

// SetConfig 停止状态下更新配置。运行中调用无效（返回错误），
// 避免「改了参数但旧监听还在跑」的错觉。
func (r *Runner) SetConfig(c Config) error {
	c.Normalize()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running {
		return errors.New("运行中不能改配置，请先停止")
	}
	r.c = c
	return nil
}

// Running 是否正在监听
func (r *Runner) Running() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running
}

// Addr 实际监听地址（启动后才有意义，例如端口填 0 时返回真实端口）
func (r *Runner) Addr() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ln == nil {
		return r.c.Listen
	}
	return r.ln.Addr().String()
}

// Err 最近一次启动失败原因（成功后清空）
func (r *Runner) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastErr
}

// StartedAt 本次启动时刻（未运行为零值）
func (r *Runner) StartedAt() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.startedAt
}

// Start 启动监听。先 bind 再返回，因此返回 nil 意味着端口已经真实可用。
//
// 锁的纪律（踩过一次）：本函数只在「读/写共享状态」的短片段里持锁，
// 中途的 bind、建目录、写日志一律不持锁。早期版本用 defer Unlock 罩住整个
// 函数，而 logf 也要拿 mu——结果是 Start 自死锁：端口已经 bind 成功（所以
// `lsof` 能看到在听），但 Start 永远不返回，界面就一直显示「未连接」。
func (r *Runner) Start() error {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return errors.New("已在运行")
	}
	c := r.c
	logger := r.logger
	r.mu.Unlock()

	col, err := stats.New(c.LogPath, c.StatsPath)
	if err != nil {
		r.mu.Lock()
		r.lastErr = fmt.Errorf("stats: %w", err)
		r.mu.Unlock()
		return fmt.Errorf("stats: %w", err)
	}

	mode := transport.Kind(strings.ToLower(c.Transport))
	switch mode {
	case transport.KindQUIC, transport.KindTCP, transport.KindAuto:
	default:
		col.Close()
		err := fmt.Errorf("未知传输方式 %q（可选 auto|quic|tcp）", c.Transport)
		r.mu.Lock()
		r.lastErr = err
		r.mu.Unlock()
		return err
	}

	// 先 bind 再置 running：bind 失败时 Runner 保持「未运行」，GUI 的
	// 状态栏能直接显示原因
	ln, err := net.Listen("tcp", c.Listen)
	if err != nil {
		col.Close()
		err = fmt.Errorf("listen %s: %w", c.Listen, err)
		r.mu.Lock()
		r.lastErr = err
		r.mu.Unlock()
		return err
	}

	dialer := &transport.Dialer{
		Addr: c.Server, ServerName: c.ServerName,
		Insecure: c.Insecure, Mode: mode,
	}
	finder := basefind.New(c.HistoryMB)
	h := &handler{cfg: c, dialer: dialer, finder: finder, col: col, logger: logger}
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
		// 不设 WriteTimeout：SSE 长连接需要
	}

	r.mu.Lock()
	r.col, r.ln, r.dialer, r.finder, r.srv = col, ln, dialer, finder, srv
	r.running = true
	r.startedAt = time.Now()
	r.lastErr = nil
	r.mu.Unlock()

	if logger != nil {
		logger.Printf("relay-client 启动")
		logger.Printf("  本地 HTTP   http://%s", ln.Addr())
		logger.Printf("  隧道上游    %s (%s)", c.Server, mode)
		logger.Printf("  delta 门槛  >=%d 字节（块复用率>=50%%）", c.MinDelta)
		logger.Printf("  基线历史    %d MB", c.HistoryMB)
		logger.Printf("  日志        %s", c.LogPath)
	}

	go func(srv *http.Server, ln net.Listener) {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			r.mu.Lock()
			r.lastErr = fmt.Errorf("serve: %w", err)
			r.running = false
			r.mu.Unlock()
			if logger != nil {
				logger.Printf("监听异常退出: %v", err)
			}
			r.notify()
		}
	}(srv, ln)

	r.notify()
	return nil
}

// Stop 停止监听并落盘统计。未运行时是 no-op（GUI 上「停止」按钮可以随便按）。
func (r *Runner) Stop(ctx context.Context) error {
	r.mu.Lock()
	if !r.running {
		r.mu.Unlock()
		return nil
	}
	srv, col := r.srv, r.col
	r.running = false
	r.srv, r.col, r.ln = nil, nil, nil
	r.mu.Unlock()

	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := srv.Shutdown(ctx)
	// Shutdown 之后仍有在途请求，Close 兜底
	_ = srv.Close()
	if col != nil {
		col.Close()
	}
	r.logf("已停止监听")
	r.notify()
	return err
}

// Snapshot 进程内统计快照。未运行时返回零值 Agg（GUI 显示全 0 而不是上次残留）。
func (r *Runner) Snapshot() stats.Agg {
	r.mu.Lock()
	col := r.col
	r.mu.Unlock()
	if col == nil {
		return stats.Agg{}
	}
	return col.Snapshot()
}

func (r *Runner) logf(format string, args ...any) {
	r.mu.Lock()
	l := r.logger
	r.mu.Unlock()
	if l != nil {
		l.Printf(format, args...)
	}
}

func (r *Runner) notify() {
	r.mu.Lock()
	fn := r.OnEvent
	r.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// ---- 转发 handler ----

type handler struct {
	cfg      Config
	dialer   *transport.Dialer
	finder   *basefind.Finder
	col      *stats.Collector
	logger   *log.Logger
	capMu    sync.Mutex
	capCount int
}

func (h *handler) logf(format string, args ...any) {
	if h.logger != nil {
		h.logger.Printf(format, args...)
	}
}

// capture 把请求体原样落盘（按序号命名），用于离线重放分析。
// 只采前 captureN 条，避免无限增长。
func (h *handler) capture(body []byte) {
	h.capMu.Lock()
	n := h.capCount
	if n >= h.cfg.CaptureN {
		h.capMu.Unlock()
		return
	}
	h.capCount++
	h.capMu.Unlock()

	os.MkdirAll(h.cfg.CaptureDir, 0o755)
	path := filepath.Join(h.cfg.CaptureDir, fmt.Sprintf("req-%04d.bin", n))
	if err := os.WriteFile(path, body, 0o600); err != nil {
		h.logf("capture write err: %v", err)
		return
	}
	h.logf("captured #%d %d bytes -> %s", n, len(body), path)
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
	if h.cfg.CaptureDir != "" {
		h.capture(body)
	}

	// ---- 选择编码 ----
	enc := meta.EncIdentity
	encName := "identity"
	var payload []byte
	var baseHash codec.Hash

	encStart := time.Now()
	switch {
	case len(body) < h.cfg.MinDelta:
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
		// 声明能解压压缩的下行响应。服务端据此决定是否压缩；
		// 不发这一位的老客户端会被服务端当作「不支持」而退回明文。
		AcceptDownZstd: true,
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
		h.logf("下游已断开，停止续传 key=%x（条目保留在服务端幂等区）", idemKey[:4])
	}
	// 只有「可恢复的中断」才续传：已完成 / 服务端判定失败 / 下游已走 都不续传
	for att := 0; att < h.cfg.MaxResume && !stream.Completed && !stream.Fatal &&
		stream.Err != nil && !stream.ResumeMiss && !isDownstreamGone(stream.Err); att++ {
		h.logf("断线续传 #%d：key=%x from=%d event=%q err=%v",
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
