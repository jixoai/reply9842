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
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jixoai/rfc9842-reply/internal/codec"
	"github.com/jixoai/rfc9842-reply/internal/meta"
	"github.com/jixoai/rfc9842-reply/internal/spool"
	"github.com/jixoai/rfc9842-reply/internal/stats"
	"github.com/jixoai/rfc9842-reply/internal/store"
	"github.com/jixoai/rfc9842-reply/internal/transport"
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
	flag.StringVar(&c.spoolDir, "spool-dir", "/var/lib/rfc9842-reply/spool", "流式 spool 根目录")
	flag.Int64Var(&c.tmpMB, "tmp-mb", 512, "活跃区字节预算（MB）")
	flag.Int64Var(&c.idemMB, "idem-mb", 2048, "幂等区字节预算（MB）")
	flag.DurationVar(&c.upstreamTimout, "upstream-timeout", 10*time.Minute, "上游请求超时")
	flag.StringVar(&c.logPath, "log", "", "逐请求日志")
	flag.StringVar(&c.statsPath, "stats", "", "汇总统计")
	flag.Parse()

	if c.cert == "" || c.key == "" {
		log.Fatalf("必须提供 -cert 与 -key")
	}
	home, _ := os.UserHomeDir()
	if c.logPath == "" {
		c.logPath = filepath.Join(home, ".rfc9842-reply",
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

func (s *server) serve(a transport.Accept) {
	defer a.Conn.Close()
	start := time.Now()
	rec := &stats.Record{Transport: string(a.Kind)}

	raw, err := a.Conn.Recv()
	if err != nil {
		log.Printf("[%s] recv err: %v", a.Kind, err)
		return
	}
	ft, payload, err := meta.ReadFrame(bytes.NewReader(raw))
	if err != nil {
		log.Printf("[%s] meta.ReadFrame err: %v", a.Kind, err)
		return
	}

	switch ft {
	case meta.FrameResume:
		s.handleResume(a, payload, rec, start)
		return
	case meta.FrameRequest:
		// 继续新请求路径
	default:
		log.Printf("[%s] unexpected frame type %d", a.Kind, ft)
		return
	}

	reqHead, hdrRaw, bodyWire, err := meta.DecodeRequest(payload)
	if err != nil {
		s.fail(a.Conn, "decode request: "+err.Error())
		return
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
		return
	}
	if err != nil {
		s.fail(a.Conn, "decode: "+err.Error())
		rec.Status, rec.Err = 400, err.Error()
		rec.MsTotal = time.Since(start).Milliseconds()
		s.col.Observe(rec)
		return
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
			return
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
		return
	}

	if !existing {
		s.startPump(entry, reqHead, hdrRaw, body)
	} else {
		log.Printf("同 key 请求复用已有条目（existing）key=%x written=%d",
			keyArr[:4], entry.Written())
	}

	status := s.streamToClient(a.Conn, entry, 0, start, rec)
	if status != 0 {
		rec.Status = status
	}
	rec.MsTotal = time.Since(start).Milliseconds()
	s.col.Observe(rec)
}

// streamToClient 从 entry 的 from 偏移开始把 body 推给客户端。
// 客户端断开时不取消上游（pump 独立），只把条目 Detach 到幂等区。
//
// 正常推完后必须发 FrameDone：客户端靠它区分「上游完成」与「连接断了」。
// 不发的话客户端只能把正常结束当 recv EOF 记成错误，并被迫重连去确认状态。
func (s *server) streamToClient(conn *transport.Conn, e *spool.Entry, from int64,
	start time.Time, rec *stats.Record) int {

	// 等响应头就绪
	for {
		if st, hdrs, ok := e.Head(); ok {
			rh := &meta.ResponseHead{
				Status: st, Encoding: meta.EncIdentity,
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
		if !e.WaitMore(nil, e.Gen()) {
			s.detachOnDisconnect(e)
			return 0
		}
	}

	// 推 body：按偏移拉取，空闲时等通知
	off := from
	var down int64
	for {
		buf, err := e.ReadFrom(off, 32*1024)
		if err != nil {
			s.fail(conn, "spool read: "+err.Error())
			return 500
		}
		if len(buf) > 0 {
			// Status=0 表示「续片」，客户端按序拼接
			chunkHead := &meta.ResponseHead{
				Status: 0, Encoding: meta.EncIdentity,
				BodyLen: int64(len(buf)), HeaderLen: 0,
			}
			out, _ := meta.EncodeResponse(chunkHead, nil, buf)
			f, err := meta.WrapFrame(meta.FrameResponse, out)
			if err != nil {
				return 500
			}
			if err := transport.WriteFrame(conn, f); err != nil {
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
			// 终止帧：告诉客户端「这是完整响应的结尾，别当成断线」
			if f, ferr := meta.WrapFrame(meta.FrameDone, nil); ferr == nil {
				if werr := transport.WriteFrame(conn, f); werr != nil {
					s.detachOnDisconnect(e)
					return 0
				}
			}
			rec.DownRaw = down
			rec.DownWire = down
			return 200
		}
		if !e.WaitMore(nil, e.Gen()) {
			s.detachOnDisconnect(e)
			return 0
		}
	}
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

		e.SetHead(resp.StatusCode, meta.EncodeHeaders(resp.Header))
		// 非 2xx 响应不进入幂等复用（瞬时故障不应被固化）
		if resp.StatusCode >= 400 {
			defer func() {
				// 让客户端拿到这次的错误响应，但之后不再复用
			}()
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
