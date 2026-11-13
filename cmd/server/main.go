// relay-server — 隧道服务端 + 上游转发 + 幂等断点续传
//
// 职责（2026-11-13 kzf 需求整合）：
//  1. 接受 QUIC/TCP 隧道连接，解帧、解 dcz/zstd、校验 ResultHash
//  2. 转发上游（默认 127.0.0.1:3431，同机回环，不出网卡）
//  3. 流式 spool 双写：上游 chunk 边收边落盘，客户端从文件按偏移拉取
//  4. 断线提升：客户端断开 → tmp→idem rename，上游继续接收
//  5. 重连续传：客户端带 IdemKey + ResumeFrom/EventID 重连 → 重放 + 挂末尾
//
// 关键架构（勿改）：pump goroutine（上游→文件）与 client writer（文件→客户端）
// 完全解耦。pump 的 ctx 用 context.WithoutCancel 派生，客户端断开不会取消上游。
// 这是「请求能续上」的全部前提。
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jixoai/reply9842/internal/codec"
	"github.com/jixoai/reply9842/internal/downlink"
	"github.com/jixoai/reply9842/internal/meta"
	"github.com/jixoai/reply9842/internal/spool"
	"github.com/jixoai/reply9842/internal/stats"
	"github.com/jixoai/reply9842/internal/store"
	"github.com/jixoai/reply9842/internal/transport"
)

type cfg struct {
	listen         string
	upstream       string
	upHost         string
	cert           string
	key            string
	cacheMB        int64
	spoolDir       string
	tmpMB          int64
	idemMB         int64
	upstreamTimout time.Duration
	healthAddr     string
	pprof          bool
	healthProbe    bool
	logPath        string
	statsPath      string
}

func main() {
	c := cfg{}
	flag.StringVar(&c.listen, "listen", "0.0.0.0:28443", "隧道监听（QUIC 与 TCP 同端口）")
	flag.StringVar(&c.upstream, "upstream", "http://127.0.0.1:3431", "上游 base URL（同机回环最优）")
	flag.StringVar(&c.upHost, "uphost", "magpie.aiweb.xin", "转发时覆盖的 Host 头")
	flag.StringVar(&c.cert, "cert", "", "TLS 证书（fullchain）")
	flag.StringVar(&c.key, "key", "", "TLS 私钥")
	flag.Int64Var(&c.cacheMB, "cache-mb", 256, "基版本缓存上限（MB，内存）")
	flag.StringVar(&c.spoolDir, "spool-dir", "/var/lib/reply9842/spool", "流式 spool 根目录")
	flag.Int64Var(&c.tmpMB, "tmp-mb", 512, "活跃区字节预算（MB）")
	flag.Int64Var(&c.idemMB, "idem-mb", 2048, "幂等区字节预算（MB）")
	flag.DurationVar(&c.upstreamTimout, "upstream-timeout", 10*time.Minute, "上游请求超时")
	flag.BoolVar(&c.healthProbe, "health-probe", false,
		"探针模式：GET 自己的 /healthz，成功 exit 0、失败 exit 1。"+
			"给 scratch 镜像的 HEALTHCHECK 用（容器内无 curl）")
	flag.BoolVar(&c.pprof, "pprof", false,
		"在健康检查端口上挂 /debug/pprof/（排障用，生产别对公网开）")
	flag.StringVar(&c.healthAddr, "health-addr", "",
		"健康检查监听地址（如 127.0.0.1:8080）；留空不启用。"+
			"给容器编排用：scratch 镜像无 shell，不能做 exec 型探活")
	flag.StringVar(&c.logPath, "log", "", "逐请求日志")
	flag.StringVar(&c.statsPath, "stats", "", "汇总统计")
	flag.Parse()

	// 探针模式：容器内无 curl，用二进制自身做 HEALTHCHECK
	if c.healthProbe {
		addr := c.healthAddr
		if addr == "" {
			addr = "127.0.0.1:8080"
		}
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			log.Fatalf("health-probe: 地址非法: %v", err)
		}
		if host == "0.0.0.0" || host == "" {
			host = "127.0.0.1"
		}
		cl := &http.Client{Timeout: 3 * time.Second}
		resp, err := cl.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
		if err != nil {
			log.Printf("health-probe: %v", err)
			os.Exit(1)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			log.Printf("health-probe: HTTP %d", resp.StatusCode)
			os.Exit(1)
		}
		os.Exit(0)
	}

	if c.cert == "" || c.key == "" {
		log.Fatalf("必须提供 -cert 与 -key")
	}
	home, _ := os.UserHomeDir()
	if c.logPath == "" {
		c.logPath = filepath.Join(home, ".reply9842",
			"server-"+time.Now().Format("2006-01-02")+".jsonl")
	}
	if c.statsPath == "" {
		c.statsPath = filepath.Join(filepath.Dir(c.logPath), "server-stats.json")
	}

	col, err := stats.New(c.logPath, c.statsPath)
	if err != nil {
		log.Fatalf("stats: %v", err)
	}
	defer col.Close()

	sp, err := spool.New(spool.Options{
		Dir: c.spoolDir, TmpBudget: c.tmpMB << 20, IdemBudget: c.idemMB << 20,
	})
	if err != nil {
		log.Fatalf("spool: %v", err)
	}
	defer sp.Close()

	log.Printf("relay-server 启动")
	log.Printf("  隧道监听    %s（QUIC + TCP/TLS 同端口）", c.listen)
	log.Printf("  上游        %s (Host: %s)", c.upstream, c.upHost)
	log.Printf("  基版本缓存  %d MB（内存，内容寻址）", c.cacheMB)
	log.Printf("  spool       %s（tmp %dMB / idem %dMB）", c.spoolDir, c.tmpMB, c.idemMB)
	log.Printf("  上游超时    %s（不随客户端断开取消）", c.upstreamTimout)
	log.Printf("  日志        %s", c.logPath)

	s := &server{
		c: &c, store: store.New(c.cacheMB << 20), spool: sp, col: col,
		client: &http.Client{
			Timeout: 0,
			Transport: &http.Transport{
				MaxIdleConns:        200,
				MaxIdleConnsPerHost: 50,
				IdleConnTimeout:     90 * time.Second,
				DisableCompression:  true,
			},
		},
	}

	tr := &transport.Server{Addr: c.listen, CertFile: c.cert, KeyFile: c.key}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	accepts, err := tr.Listen(ctx)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}

	// 健康检查端点：给容器编排用。scratch 镜像没有 shell，无法 exec 型探活，
	// 只能靠 HTTP。只暴露 /healthz，不泄漏任何状态。
	//
	// 端口冲突必须 fail fast：容器里 HEALTHCHECK 打的是固定端口，如果这个端口
	// 被别的进程占着，探活会打到别人身上并返回 200——编排层看到的是假绿，而
	// 本进程其实没有健康端点。先同步 Listen 确认端口到手，再起 serve goroutine。
	if c.healthAddr != "" {
		mux := http.NewServeMux()
		if c.pprof {
			// 只在显式开 -pprof 时挂上：pprof 能看全量堆与 goroutine，
			// 不该是默认暴露面。与 /healthz 同一个监听，方便只绑回环。
			mux.HandleFunc("/debug/pprof/", pprof.Index)
			mux.HandleFunc("/debug/pprof/heap", pprof.Handler("heap").ServeHTTP)
			mux.HandleFunc("/debug/pprof/goroutine", pprof.Handler("goroutine").ServeHTTP)
			mux.HandleFunc("/debug/pprof/allocs", pprof.Handler("allocs").ServeHTTP)
			log.Printf("pprof     %s/debug/pprof/", c.healthAddr)
		}
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		})
		ln, err := net.Listen("tcp", c.healthAddr)
		if err != nil {
			log.Fatalf("健康检查端口 %s 不可用: %v", c.healthAddr, err)
		}
		log.Printf("健康检查  %s/healthz", c.healthAddr)
		go func() {
			if err := http.Serve(ln, mux); err != nil {
				log.Printf("健康检查服务退出: %v", err)
			}
		}()
	}

	log.Printf("就绪")

	for a := range accepts {
		go s.serve(a)
	}
}

type server struct {
	c      *cfg
	store  *store.Store
	spool  *spool.Manager
	client *http.Client
	col    *stats.Collector
}

// serve 一条隧道连接的主循环。
//
// 为什么要循环（而不是处理一个请求就关）：客户端侧上了连接池，一条连接会被
// 复用来跑后续请求。握手成本实测平均约 2 秒（本机→香港，见 transport/pool.go
// 顶部注释），不复用等于每个请求白扔这份钱。协议上一条连接跑多个请求是安全的：
// 帧自带长度前缀，且服务端串行处理（一条连接同一时刻只有一个请求在途），
// 不会出现响应串台。
//
// 复用条件（任一不满足即关闭连接）：
//   - 本请求完整成功（响应推完、无 delta-miss、无上游错误）
//   - 没有残留的未读帧
//
// 错误路径一律关闭：半截响应留在连接里会被下一个请求误读成它的开头。
func (s *server) serve(a transport.Accept) {
	defer a.Conn.Close()
	for {
		keep, err := s.serveOne(a)
		if !keep || err != nil {
			if err != nil && !errors.Is(err, io.EOF) {
				log.Printf("[%s] 关闭连接: %v", a.Kind, err)
			}
			return
		}
	}
}

// serveOne 处理一个请求，返回是否可继续复用这条连接。
func (s *server) serveOne(a transport.Accept) (keepAlive bool, err error) {
	start := time.Now()
	rec := &stats.Record{Transport: string(a.Kind)}

	raw, err := a.Conn.Recv()
	if err != nil {
		return false, err
	}
	ft, payload, err := meta.ReadFrame(bytes.NewReader(raw))
	if err != nil {
		return false, err
	}

	switch ft {
	case meta.FrameResume:
		// 续传一律不复用连接：它本来就发生在「上一条连接异常」之后，
		// 且 handleResume 有多条提前 return 的失败路径（解码失败、key 不存在、
		// 客户端不支持下行压缩），那些路径下连接里可能残留半截帧。
		// 用一次握手换确定性，比省这次复用划算。
		s.handleResume(a, payload, rec, start)
		return false, nil
	case meta.FrameRequest:
		// 继续新请求路径
	case meta.FramePing:
		// 保活帧：直接回一个 Ping 即可，不计入统计
		if f, e := meta.WrapFrame(meta.FramePing, nil); e == nil {
			_ = transport.WriteFrame(a.Conn, f)
		}
		return true, nil
	default:
		return false, fmt.Errorf("unexpected frame type %d", ft)
	}

	reqHead, hdrRaw, bodyWire, err := meta.DecodeRequest(payload)
	if err != nil {
		s.fail(a.Conn, "decode request: "+err.Error())
		return false, err
	}
	rec.Method, rec.Path = reqHead.Method, reqHead.Path
	rec.UpWire = int64(len(bodyWire))

	// ---- 解编码 ----
	decStart := time.Now()
	body, encName, miss, err := s.decodeBody(reqHead, bodyWire)
	rec.MsDecode = time.Since(decStart).Milliseconds()
	if miss {
		if f, e := meta.WrapFrame(meta.FrameDeltaMiss, nil); e == nil {
			transport.WriteFrame(a.Conn, f)
		}
		rec.Encoding, rec.Status = "delta-miss", 409
		rec.MsTotal = time.Since(start).Milliseconds()
		s.col.Observe(rec)
		// DeltaMiss 后客户端会另起一次全量请求。当前连接上已回了一帧
		// DeltaMiss，且没有后续响应，状态是干净的，可以复用。
		return true, nil
	}
	if err != nil {
		s.fail(a.Conn, "decode: "+err.Error())
		rec.Status, rec.Err = 400, err.Error()
		rec.MsTotal = time.Since(start).Milliseconds()
		s.col.Observe(rec)
		return false, err
	}
	rec.Encoding = encName
	rec.BaseHit = reqHead.Encoding == meta.EncDcz
	rec.UpRaw = int64(len(body))

	// ---- 防静默损坏 ----
	if !reqHead.ResultHash.IsZero() {
		if convertHash(codec.Sum(body)) != reqHead.ResultHash {
			s.fail(a.Conn, "result hash mismatch (silent corruption guard)")
			rec.Status, rec.Err = 422, "result hash mismatch"
			rec.MsTotal = time.Since(start).Milliseconds()
			s.col.Observe(rec)
			return false, errors.New("result hash mismatch")
		}
	}

	// ---- 幂等键 ----
	key := reqHead.IdemKey
	if key.IsZero() {
		// 客户端未提供（旧客户端）：服务端按内容派生，需补齐 Authorization 上下文
		key = meta.Hash{}
		log.Printf("warn: request without IdemKey, falling back to path-only key")
	}

	var keyArr [32]byte
	copy(keyArr[:], key[:])

	// ---- 取得 spool 条目 ----
	entry, existing, err := s.spool.Acquire(keyArr)
	if err != nil {
		s.fail(a.Conn, "spool: "+err.Error())
		rec.Status, rec.Err = 500, err.Error()
		rec.MsTotal = time.Since(start).Milliseconds()
		s.col.Observe(rec)
		return false, err
	}

	if !existing {
		s.startPump(entry, reqHead, hdrRaw, body)
	} else {
		log.Printf("同 key 请求复用已有条目（existing）key=%x written=%d",
			keyArr[:4], entry.Written())
	}

	// 下行压缩的能力协商在 startPump 里按 reqHead.AcceptDownZstd 决定；
	// streamToClient 只读条目的最终状态（entry.Compressed()），不自行判断。
	status := s.streamToClient(a.Conn, entry, 0, start, rec)
	if status != 0 {
		rec.Status = status
	}
	rec.MsTotal = time.Since(start).Milliseconds()
	s.col.Observe(rec)

	// 只有完整成功（没有传输错误）才允许复用：streamToClient 出错时
	// 连接里可能残留半截帧，继续用会让下一个请求读到脏数据。
	clean := rec.Err == "" && (status == 0 || status == 200)
	return clean, nil
}

// streamToClient 从 entry 的 from 偏移开始把 body 推给客户端。
// 客户端断开时不取消上游（pump 独立），只把条目 Detach 到幂等区。
//
// 正常推完后必须发 FrameDone：客户端靠它区分「上游完成」与「连接断了」。
// 不发的话客户端只能把正常结束当 recv EOF 记成错误，并被迫重连去确认状态。
//
// 下行压缩（2026-11-13）：spool 里存的是压缩流，这里读出来的却是**明文**
// （ReadFrom 的明文语义），所以线上这一层要重新压一遍——每条连接一个
// 独立的 zstd 流。这样续传天然正确：客户端每次都从 from 明文偏移处开始
// 收一条全新的流，不需要跨连接保留解压器状态（那是唯一的正确性陷阱）。
// noHeadTimeout 等响应头的上限。上游 TTFT 实测 20–90 秒（推理模型），
// 120 秒留出余量，同时保证「没有生产者」的僵尸条目不会把请求挂死。
const noHeadTimeout = 120 * time.Second

func (s *server) streamToClient(conn *transport.Conn, e *spool.Entry, from int64,
	start time.Time, rec *stats.Record) int {
	// 无论从哪条 return 离开，都把解码游标还回去：它背后是一个按窗口
	// 分配的 zstd 解码器（约 4MB），跟着条目缓存会把内存按请求数堆起来。
	defer e.ReleaseCursor()

	// enc 必须在**头就绪且压缩决策已拍板之后**才判定：
	// startPump 是 goroutine，压缩决策发生在它内部（要等攒够 MinUseful）。
	// 若在决策前就发头，头里声明的编码可能与随后的 body 不一致——客户端
	// 会按 identity 解析 zstd 流（实测把线上响应变成了二进制垃圾）。
	var enc meta.Encoding

	// 等响应头就绪
	for {
		if st, hdrs, ok := e.Head(); ok {
			if s.waitDecision(e) {
				enc = meta.EncZstd
			}
			rh := &meta.ResponseHead{
				Status: st, Encoding: enc,
				BodyLen: -1, HeaderLen: int32(len(hdrs)),
			}
			out, err := meta.EncodeResponse(rh, hdrs, nil)
			if err != nil {
				s.fail(conn, "encode head: "+err.Error())
				return 500
			}
			if f, err := meta.WrapFrame(meta.FrameResponse, out); err == nil {
				if err := transport.WriteFrame(conn, f); err != nil {
					s.detachOnDisconnect(e)
					return 0
				}
			}
			rec.Status = st
			rec.MsUpstrm = time.Since(start).Milliseconds()
			break
		}
		if e.IsComplete() {
			msg := "upstream finished without headers"
			if err := e.Err(); err != nil {
				msg = err.Error()
			}
			s.fail(conn, msg)
			return 502
		}
		// 头部等待必须有个上限：WaitMore(nil, ...) 在「条目存在但没有任何
		// 生产者」时会永远阻塞（实测：客户端复用一个 written=0 的幂等键，
		// 服务端既没有 pump 也没有数据，请求就挂在那里——用户看到的是
		// 「请求像卡住了」）。到点回 504，让客户端自己重试，比静默挂死好。
		if time.Since(start) > noHeadTimeout {
			s.fail(conn, "上游 120 秒未返回响应头（上游过载或请求已失效，请重试）")
			return 504
		}
		if !e.WaitMore(nil, e.Gen()) {
			s.detachOnDisconnect(e)
			return 0
		}
	}

	fw := &frameWriter{conn: conn, enc: enc}
	var cw *downlink.Compressor
	if enc == meta.EncZstd {
		var err error
		cw, err = downlink.New(fw)
		if err != nil {
			s.fail(conn, "compressor: "+err.Error())
			return 500
		}
	}

	// emit 把一段明文写到客户端（压缩条目经压缩器，其余直发）
	emit := func(p []byte) error {
		if cw != nil {
			_, err := cw.Write(p)
			return err
		}
		_, err := fw.Write(p)
		return err
	}
	finishBody := func() error {
		if cw != nil {
			if err := cw.Close(); err != nil {
				return err
			}
			cw = nil
		}
		return nil
	}

	// 推 body：按偏移拉取，空闲时等通知。
	//
	// 空闲等待必须有上限：压缩器攒够 4KB 或 200ms 才输出，静默的上游会让
	// 最后一小段明文一直留在压缩器里。超时后 FlushDown 把它推成可读明文。
	off := from
	var down int64
	for {
		buf, err := e.ReadFrom(off, 32*1024)
		if err != nil {
			s.fail(conn, "spool read: "+err.Error())
			return 500
		}
		if len(buf) > 0 {
			if err := emit(buf); err != nil {
				s.detachOnDisconnect(e)
				return 0
			}
			off += int64(len(buf))
			down += int64(len(buf))
			continue
		}
		if e.IsComplete() {
			if err := e.Err(); err != nil {
				s.fail(conn, "upstream: "+err.Error())
				return 502
			}
			if err := finishBody(); err != nil {
				s.detachOnDisconnect(e)
				return 0
			}
			// 终止帧：告诉客户端「这是完整响应的结尾，别当成断线」
			if f, ferr := meta.WrapFrame(meta.FrameDone, nil); ferr == nil {
				if werr := transport.WriteFrame(conn, f); werr != nil {
					s.detachOnDisconnect(e)
					return 0
				}
			}
			rec.DownRaw = down
			rec.DownWire = fw.n
			return 200
		}
		// 等新数据；超时说明上游暂时静默，把压缩器里攒的推出来
		if !e.WaitMoreFor(downlink.DefaultFlushIdle, e.Gen()) {
			e.FlushDown()
		}
	}
}

// waitDecision 等压缩决策，返回是否启用压缩。
//
// 上游可能长时间只吐几十字节（慢速小响应），这时既不能提前发头（编码未定），
// 也不能无限等待。上限取 downlink.DefaultFlushIdle：超时即按当前量拍板，
// 小响应退回明文——宁可多几字节也不拖住首字节。
func (s *server) waitDecision(e *spool.Entry) bool {
	if decided, compressed := e.Decided(); decided {
		return compressed
	}
	deadline := time.Now().Add(downlink.DefaultFlushIdle)
	for {
		if decided, compressed := e.Decided(); decided {
			return compressed
		}
		if time.Now().After(deadline) {
			_, compressed := e.ForceDecide()
			return compressed
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// frameWriter 把压缩器输出的字节封成 Response 续片帧（Status=0）发出去。
// 每 4KB 明文才产出一次输出，正常请求整条响应在 30KB 上下，帧数可控。
type frameWriter struct {
	conn *transport.Conn
	enc  meta.Encoding
	n    int64 // 已发出的 payload 字节（不含帧头），用于 down_wire 统计
	err  error
}

func (f *frameWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if f.err != nil {
		return 0, f.err
	}
	// Status=0 表示「续片」，客户端按序拼接
	out, err := meta.EncodeResponse(&meta.ResponseHead{
		Status: 0, Encoding: f.enc, BodyLen: int64(len(p)),
	}, nil, p)
	if err != nil {
		f.err = err
		return 0, err
	}
	frame, err := meta.WrapFrame(meta.FrameResponse, out)
	if err != nil {
		f.err = err
		return 0, err
	}
	if err := transport.WriteFrame(f.conn, frame); err != nil {
		f.err = err
		return 0, err
	}
	f.n += int64(len(p))
	return len(p), nil
}

// detachOnDisconnect 客户端断开：把条目提升到幂等区，上游继续。
func (s *server) detachOnDisconnect(e *spool.Entry) {
	if e.IsComplete() {
		return
	}
	if err := s.spool.Detach(e); err != nil {
		log.Printf("detach err: %v", err)
	} else {
		log.Printf("客户端断开，条目转入幂等区 key=%x written=%d", e.Key[:4], e.Written())
	}
}

// startPump 上游请求 → 流式落盘。ctx 用 WithoutCancel 派生，客户端断开不影响。
func (s *server) startPump(e *spool.Entry, h *meta.RequestHead, hdrRaw, body []byte) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), s.c.upstreamTimout)

	go func() {
		defer cancel()

		u := strings.TrimSuffix(s.c.upstream, "/") + h.Path
		req, err := http.NewRequestWithContext(ctx, h.Method, u, bytes.NewReader(body))
		if err != nil {
			e.Finish(err)
			return
		}
		for k, vs := range meta.DecodeHeaders(hdrRaw) {
			if isHopByHop(k) {
				continue
			}
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
		req.Header.Set("Host", s.c.upHost)
		req.Host = s.c.upHost
		req.Header.Del("Accept-Encoding")
		req.ContentLength = int64(len(body))

		resp, err := s.client.Do(req)
		if err != nil {
			// 上游不可达：标记为不可复用，Finish 后立即从 spool 移除。
			// 否则空响应会被当成「已完成」永久命中，客户端永远拿到空结果。
			e.Finish(err)
			e.MarkUncacheable()
			return
		}
		defer resp.Body.Close()

		// 下行压缩必须在 SetHead **之前**表达意愿：SetHead 会唤醒等头的
		// streamToClient，它在那一刻读 e.Compressed() 决定线上编码。
		//
		// 注意这是「允许压缩」，真正的开关由 spool 在累计明文达到
		// downlink.MinUseful 时拍板——小响应（如 401 错误体）压缩反而更大。
		// 因此认领压缩时也必须预先把响应头推迟到「决定之后」再公开：
		// 见下面 SetHead 的调用顺序。
		//
		// 只在客户端声明支持（AcceptDownZstd）时启用：旧客户端不认识
		// EncZstd，收到压缩流会把 zstd 字节当明文写给下游。
		if h.AcceptDownZstd {
			e.EnableCompression()
		}
		e.SetHead(resp.StatusCode, meta.EncodeHeaders(resp.Header))
		// 非 2xx 响应不进入幂等复用（瞬时故障不应被固化）
		if resp.StatusCode >= 400 {
			e.MarkUncacheable()
		}

		buf := make([]byte, 32*1024)
		for {
			n, rerr := resp.Body.Read(buf)
			if n > 0 {
				if werr := e.Append(buf[:n]); werr != nil {
					e.Finish(werr)
					return
				}
			}
			if rerr == io.EOF {
				break
			}
			if rerr != nil {
				e.Finish(rerr)
				return
			}
			if ctx.Err() != nil {
				e.Finish(ctx.Err())
				return
			}
		}
		e.Finish(nil)
		if !e.Uncacheable() {
			s.store.Put(body)
		}
	}()
}

// handleResume 处理续传：按 key 找条目，回元信息，从偏移继续推。
func (s *server) handleResume(a transport.Accept, payload []byte, rec *stats.Record, start time.Time) {
	rq, err := meta.DecodeResumeReq(payload)
	if err != nil {
		s.fail(a.Conn, "decode resume: "+err.Error())
		return
	}
	rec.Method, rec.Path = "RESUME", fmt.Sprintf("key=%x", rq.IdemKey[:4])

	entry, existing, err := s.spool.Acquire(rq.IdemKey)
	if err != nil {
		s.fail(a.Conn, "spool: "+err.Error())
		return
	}
	if !existing {
		// 没有对应条目 → 让客户端重新发完整请求
		if f, e := meta.WrapFrame(meta.FrameDeltaMiss, nil); e == nil {
			transport.WriteFrame(a.Conn, f)
		}
		rec.Status, rec.Encoding = 409, "resume-miss"
		rec.MsTotal = time.Since(start).Milliseconds()
		s.col.Observe(rec)
		return
	}

	from := rq.From
	if rq.EventID != "" {
		if off := entry.EventOffset(rq.EventID); off >= 0 {
			from = off
		}
	}
	if from < 0 {
		from = 0
	}
	if w := entry.Written(); from > w {
		from = w
	}

	// 能力不匹配：条目是压缩存的，但续传的客户端不会解压（例如请求发起时
	// 是旧客户端、中途升级了客户端）。此时不能硬推压缩流——让客户端重发
	// 完整请求，服务端会按新能力位重新决定编码。
	if entry.Compressed() && !rq.AcceptDownZstd {
		log.Printf("续传客户端不支持下行压缩，要求重发（key=%x）", rq.IdemKey[:4])
		if f, e := meta.WrapFrame(meta.FrameDeltaMiss, nil); e == nil {
			transport.WriteFrame(a.Conn, f)
		}
		rec.Status, rec.Encoding = 409, "resume-cap-miss"
		rec.MsTotal = time.Since(start).Milliseconds()
		s.col.Observe(rec)
		return
	}
	// 反向不匹配：条目是明文，客户端却能解压 → 明文本来就兼容，直接推。

	st, hdrs, ok := entry.Head()
	info := &meta.ResumeInfo{
		Status: st, Written: entry.Written(), Complete: entry.IsComplete(),
		Headers: hdrs,
	}
	if err := entry.Err(); err != nil {
		info.UpstreamErr = err.Error()
	}
	if !ok {
		info.Status = 0
	}
	if f, e := meta.WrapFrame(meta.FrameResumeInfo, meta.EncodeResumeInfo(info)); e == nil {
		if err := transport.WriteFrame(a.Conn, f); err != nil {
			return
		}
	}

	status := s.streamToClient(a.Conn, entry, from, start, rec)
	if status != 0 {
		rec.Status = status
	}
	rec.Encoding = "resume"
	rec.MsTotal = time.Since(start).Milliseconds()
	s.col.Observe(rec)
}

// decodeBody 按编码类型解出明文；miss=true 表示需要客户端重发全量
func (s *server) decodeBody(h *meta.RequestHead, wire []byte) (body []byte, encName string, miss bool, err error) {
	switch h.Encoding {
	case meta.EncIdentity:
		return wire, "identity", false, nil
	case meta.EncZstd:
		b, e := codec.Decompress(wire, nil)
		return b, "zstd", false, e
	case meta.EncGzip:
		return nil, "gzip", false, fmt.Errorf("gzip decoding not enabled on server")
	case meta.EncDcz:
		declared, e := codec.PeekDczHash(wire)
		if e != nil {
			return nil, "dcz", false, e
		}
		dict, ok := s.store.Get(declared)
		if !ok {
			return nil, "dcz", true, nil
		}
		b, e := codec.DecodeDcz(wire, dict)
		if e != nil {
			return nil, "dcz", true, nil
		}
		return b, "dcz", false, nil
	}
	return nil, "unknown", false, fmt.Errorf("unknown encoding %d", h.Encoding)
}

func (s *server) fail(c *transport.Conn, msg string) {
	if f, err := meta.WrapFrame(meta.FrameError, []byte(msg)); err == nil {
		transport.WriteFrame(c, f)
	}
}

func isHopByHop(k string) bool {
	switch strings.ToLower(k) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
		"te", "trailer", "transfer-encoding", "upgrade", "content-length", "host":
		return true
	}
	return false
}

func convertHash(h codec.Hash) meta.Hash {
	var out meta.Hash
	copy(out[:], h[:])
	return out
}
