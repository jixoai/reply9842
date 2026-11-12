// relay-server — 隧道服务端 + 上游转发
//
// 职责（2026-11-12 kzf 需求：「最后由 relay-server 代理转发」）：
//   1. 接受 QUIC/TCP 隧道连接
//   2. 解帧、按 base 哈希从内容寻址 store 取字典、解 dcz/zstd
//   3. 校验 ResultHash（防静默损坏，这是本方案最关键的安全网）
//   4. 转发到上游（默认 127.0.0.1:3431，同机直连，不出网卡）
//   5. 把上游响应回传；把本次 body 存入 store 供后续请求当基线
//
// 关键设计（勿改）：store 是纯内容寻址，无会话状态、无目录、无版本号。
// base 未命中即回 DeltaMiss，客户端重发全量——这是唯一的一致性协商路径。
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
	"github.com/jixoai/rfc9842-reply/internal/stats"
	"github.com/jixoai/rfc9842-reply/internal/store"
	"github.com/jixoai/rfc9842-reply/internal/transport"
)

type cfg struct {
	listen     string
	upstream   string
	upHost     string
	cert       string
	key        string
	cacheMB    int64
	logPath    string
	statsPath  string
	maxRetries int
}

func main() {
	c := cfg{}
	flag.StringVar(&c.listen, "listen", "0.0.0.0:9000", "隧道监听（QUIC 与 TCP 同端口）")
	flag.StringVar(&c.upstream, "upstream", "http://127.0.0.1:3431", "上游 base URL（同机回环最优）")
	flag.StringVar(&c.upHost, "uphost", "magpie.aiweb.xin", "转发时覆盖的 Host 头")
	flag.StringVar(&c.cert, "cert", "", "TLS 证书（fullchain）")
	flag.StringVar(&c.key, "key", "", "TLS 私钥")
	flag.Int64Var(&c.cacheMB, "cache-mb", 512, "基版本缓存上限（MB）")
	flag.StringVar(&c.logPath, "log", "", "逐请求日志")
	flag.StringVar(&c.statsPath, "stats", "", "汇总统计")
	flag.Parse()

	if c.cert == "" || c.key == "" {
		log.Fatalf("必须提供 -cert 与 -key（自签可用 openssl 生成）")
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

	st := store.New(c.cacheMB << 20)

	log.Printf("relay-server 启动")
	log.Printf("  隧道监听    %s（QUIC + TCP/TLS 同端口）", c.listen)
	log.Printf("  上游        %s (Host: %s)", c.upstream, c.upHost)
	log.Printf("  基版本缓存  %d MB", c.cacheMB)
	log.Printf("  日志        %s", c.logPath)

	s := &server{
		c: &c, store: st, col: col,
		client: &http.Client{
			Timeout: 0, // SSE 流式需要无超时；由 ctx 控制
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
	client *http.Client
	col    *stats.Collector
}

func (s *server) serve(a transport.Accept) {
	defer a.Conn.Close()
	rec := &stats.Record{Transport: string(a.Kind)}
	start := time.Now()

	log.Printf("[%s] 新连接", a.Kind)
	raw, err := a.Conn.Recv()
	if err != nil {
		log.Printf("[%s] recv err: %v", a.Kind, err)
		return
	}
	log.Printf("[%s] 收到 %d 字节", a.Kind, len(raw))
	ft, payload, err := meta.ReadFrame(bytes.NewReader(raw))
	if err != nil {
		log.Printf("[%s] meta.ReadFrame err: %v (raw %d bytes, head=%x)", a.Kind, err, len(raw), raw[:min(12,len(raw))])
		s.fail(a.Conn, "bad frame: "+err.Error())
		return
	}
	if ft != meta.FrameRequest {
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
		// base 未命中：告知客户端重发全量
		if f, e := wrapFrame(meta.FrameDeltaMiss, nil); e == nil {
			transport.WriteFrame(a.Conn, f)
		}
		rec.Encoding, rec.BaseHit = "delta-miss", false
		rec.Status = 409
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
	rec.Encoding, rec.BaseHit = encName, reqHead.Encoding == meta.EncDcz
	rec.UpRaw = int64(len(body))

	// ---- 防静默损坏：校验 ResultHash ----
	if !reqHead.ResultHash.IsZero() {
		got := codec.Sum(body)
		if convertHash(got) != reqHead.ResultHash {
			s.fail(a.Conn, "result hash mismatch (silent corruption guard)")
			rec.Status, rec.Err = 422, "result hash mismatch"
			rec.MsTotal = time.Since(start).Milliseconds()
			s.col.Observe(rec)
			return
		}
	}

	// ---- 转发上游 ----
	upStart := time.Now()
	status, respHdr, respBody, err := s.forward(reqHead, hdrRaw, body)
	rec.MsUpstrm = time.Since(upStart).Milliseconds()
	if err != nil {
		s.fail(a.Conn, "upstream: "+err.Error())
		rec.Status, rec.Err = 502, err.Error()
		rec.MsTotal = time.Since(start).Milliseconds()
		s.col.Observe(rec)
		return
	}

	// ---- 存入 store（供后续请求当基线）----
	s.store.Put(body)

	// ---- 响应帧 ----
	rh := &meta.ResponseHead{Status: status, Encoding: meta.EncIdentity,
		BodyLen: int64(len(respBody)), HeaderLen: int32(len(respHdr))}
	out, err := meta.EncodeResponse(rh, respHdr, respBody)
	if err != nil {
		s.fail(a.Conn, "encode response: "+err.Error())
		return
	}
	frame, err := wrapFrame(meta.FrameResponse, out)
	if err != nil {
		s.fail(a.Conn, "wrap: "+err.Error())
		return
	}
	if err := transport.WriteFrame(a.Conn, frame); err != nil {
		return
	}

	rec.Status = status
	rec.DownRaw = int64(len(respBody))
	rec.DownWire = rec.DownRaw
	rec.MsTotal = time.Since(start).Milliseconds()
	if d := float64(rec.MsWire) / 1000; d > 0 {
		rec.UpBps = float64(rec.UpWire) / d
	}
	log.Printf("[%s] 完成 status=%d up=%d down=%d", a.Kind, rec.Status, rec.UpRaw, rec.DownRaw)
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
		// 兼容阶段 0（客户端仍可能发 gzip）；用 zstd 库解不了，交给上层 415
		return nil, "gzip", false, fmt.Errorf("gzip decoding not enabled on server")

	case meta.EncDcz:
		// 先看头里的哈希，再从 store 精确取字典
		declared, e := codec.PeekDczHash(wire)
		if e != nil {
			return nil, "dcz", false, e
		}
		dict, ok := s.store.Get(declared)
		if !ok {
			return nil, "dcz", true, nil // → DeltaMiss
		}
		b, e := codec.DecodeDcz(wire, dict)
		if e != nil {
			// 字典在但解不开：可能是字典内容被替换（哈希碰撞或 store 损坏）
			// 保守处理：当作 miss，让客户端重发全量
			return nil, "dcz", true, nil
		}
		return b, "dcz", false, nil
	}
	return nil, "unknown", false, fmt.Errorf("unknown encoding %d", h.Encoding)
}

// forward 转发到上游，返回 status/headers/body
func (s *server) forward(h *meta.RequestHead, hdrRaw, body []byte) (int, []byte, []byte, error) {
	u := strings.TrimSuffix(s.c.upstream, "/") + h.Path
	req, err := http.NewRequest(h.Method, u, bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, err
	}
	// 复制客户端 headers（跳过 hop-by-hop）
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
	req.Header.Del("Accept-Encoding") // 由本程序如实接收
	req.ContentLength = int64(len(body))

	resp, err := s.client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, nil, err
	}
	outHdr := meta.EncodeHeaders(resp.Header)
	return resp.StatusCode, outHdr, respBody, nil
}

func (s *server) fail(c *transport.Conn, msg string) {
	frame, _ := wrapFrame(meta.FrameError, []byte(msg))
	transport.WriteFrame(c, frame)
}

func wrapFrame(ft meta.FrameType, payload []byte) ([]byte, error) {
	var buf bytes.Buffer
	if err := meta.WriteFrame(&buf, ft, payload); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
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

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
