// Package clientstream — 把 relay-server 的流式响应转成下游 HTTP 响应
//
// 正交意图（2 个，2026-11-13 kzf 需求：「客户端那边也有自己复杂的点，就是你说的
// SSE lastEvent 的标识问题」）：
//  1. 帧 → HTTP：把 Response 帧（含 Status=0 的续片）写入下游 ResponseWriter
//  2. 续传游标：记录「已成功写给下游的字节偏移」与「最后 SSE event id」，
//     供断线重连时作为 ResumeFrom / EventID
//
// 为什么以「已写给下游的字节」为准而非「已从隧道收到的字节」：只有前者代表
// 下游真正消费到的内容。缓冲区里没刷出去的部分断线后必须重放。
package clientstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/jixoai/rfc9842-reply/internal/meta"
	"github.com/jixoai/rfc9842-reply/internal/transport"

	"github.com/klauspost/compress/zstd"
)

// Session 一次下游请求的流式会话状态，可跨连接复用。
type Session struct {
	IdemKey [32]byte

	Status     int
	HeadDone   bool
	Bytes      int64  // 已成功写给下游的 body 字节（重连时作为 ResumeFrom）
	WireBytes  int64  // 从隧道实际收到的字节（含帧头/续片开销）
	EndEventID string // SSE 最后 event id

	TTFB time.Duration // 从发起上游请求到首个 body 字节写给下游的耗时

	Completed  bool
	ResumeMiss bool // 服务端没有该 key，需完整重发
	Err        error

	// Fatal 错误不可通过续传恢复（如 4xx 业务状态、帧解析失败）。
	// 若为 true，调用方不得再重连重放——重放只会换来同样的结果。
	Fatal bool

	// 下行解压状态不放在 Session 上：解压器生命周期严格等于一次连接
	// （服务端每条连接都从 ResumeFrom 处新起一条 zstd 流），跨连接复用
	// 解压器等于拼接两条无关的流。见 rawSink。
}

// Drain 读取一条连接上的所有帧，写入 w，直到上游完成 / 出错 / 连接断。
//
// 返回时 session 会记录进度。调用方若发现 !Completed && Err != nil，
// 可用 session.Bytes / session.EndEventID 发起 Resume。
//
// start 用于记录 TTFB（首个 body 字节写给下游的时间点）；传零值则以调用
// Drain 的时刻为起点。
//
// 下行解压的生命周期 = 一次连接：服务端每条连接都从客户端请求的明文偏移
// （ResumeFrom）开始压一条**全新**的 zstd 流，所以这里也必须每条连接
// 新建解压器。跨连接复用解压器是把两段无关的流拼接，会立刻解出垃圾。
func Drain(conn *transport.Conn, w http.ResponseWriter, s *Session, start time.Time) {
	if start.IsZero() {
		start = time.Now()
	}
	raw := &rawSink{s: s, t0: start}
	defer raw.close()

	for {
		msg, err := conn.Recv()
		if err != nil {
			// 已收到 FrameDone 之后的连接关闭是正常收尾
			if s.Completed {
				return
			}
			s.Err = fmt.Errorf("recv: %w", err)
			return
		}
		ft, payload, err := meta.ReadFrame(bytes.NewReader(msg))
		if err != nil {
			s.Err = fmt.Errorf("frame: %w", err)
			s.Fatal = true
			return
		}
		// 记录隧道实际承载量（含 10 字节帧头与每次续片头），
		// 与写给下游的 Bytes（纯 body）区分开——两者在流式场景并不相等。
		s.WireBytes += int64(len(msg))

		switch ft {
		case meta.FrameError:
			s.Err = fmt.Errorf("server: %s", string(payload))
			// 服务端已判定失败（解码错误、上游报错、hash 不符）。
			// 续传只会拿回同样的结论，属于不可恢复错误。
			s.Fatal = true
			return

		case meta.FrameDone:
			// 上游已完整推完。这是唯一的「正常结束」信号。
			// 解压器里可能还有未输出的尾字节，必须冲出来再收尾。
			if err := raw.flush(); err != nil {
				s.Err = err
				s.Fatal = true
				return
			}
			s.Completed = true
			s.Err = nil
			return

		case meta.FrameDeltaMiss:
			s.ResumeMiss = true
			return

		case meta.FrameResumeInfo:
			info, derr := meta.DecodeResumeInfo(payload)
			if derr != nil {
				s.Err = derr
				s.Fatal = true
				return
			}
			if info.Status != 0 && !s.HeadDone {
				writeHead(w, info.Status, info.Headers)
				s.Status = info.Status
				s.HeadDone = true
			}
			if info.UpstreamErr != "" && info.Complete {
				s.Err = fmt.Errorf("upstream: %s", info.UpstreamErr)
				s.Fatal = true
			}
			continue

		case meta.FrameResponse:
			rh, hdrs, body, derr := meta.DecodeResponse(payload)
			if derr != nil {
				s.Err = derr
				s.Fatal = true
				return
			}
			if rh.Status != 0 {
				if !s.HeadDone {
					writeHead(w, rh.Status, hdrs)
					s.Status = rh.Status
					s.HeadDone = true
				}
				if len(body) == 0 {
					continue
				}
			}
			if len(body) == 0 {
				continue
			}
			if err := raw.write(w, rh.Encoding, body); err != nil {
				s.Err = err
				if raw.broken {
					// 压缩流损坏：续传会从错误的明文偏移重来，只会重复失败
					s.Fatal = true
				}
				return
			}
			continue
		}
	}
}

// rawSink 把 wire 字节（zstd 流片段）转成明文字节写给下游。
//
// zstd 的解压器需要一个阻塞式 io.Reader，而隧道给的是一帧一帧的字节，
// 所以用 io.Pipe 做桥：Drain 把帧写进管道（写满即阻塞，天然背压），
// 单独的 goroutine 从解码器读出明文写下游。明文一到就写出去，因此保持
// 流式——不会等到整条响应收完才开始出字。
type rawSink struct {
	s      *Session
	t0     time.Time // 用于 TTFB：首个明文字节真正写给下游的时刻
	w      http.ResponseWriter
	dec    *zstd.Decoder
	pw     *io.PipeWriter
	done   chan error
	closed bool
	broken bool
}

// start 建立解压管道（每条连接一次）
func (r *rawSink) start(w http.ResponseWriter) error {
	r.w = w
	pr, pw := io.Pipe()
	d, err := zstd.NewReader(pr, zstd.WithDecoderConcurrency(1))
	if err != nil {
		_ = pr.CloseWithError(err)
		return err
	}
	r.dec, r.pw = d, pw
	r.done = make(chan error, 1)

	go func() {
		var err error
		buf := make([]byte, 32*1024)
		for {
			n, rerr := d.Read(buf)
			if n > 0 {
				r.markTTFB()
				if werr := writeBody(r.s, buf[:n], r.w); werr != nil {
					// 下游写失败：结束解码，并把错误透出去
					err = werr
					break
				}
			}
			if rerr != nil {
				if rerr != io.EOF {
					err = rerr
				}
				break
			}
		}
		// 必须关闭读端：解码既然结束（无论成功/出错），后续 pw.Write 都要
		// 立刻失败而不是永久阻塞——否则 Drain 会卡死在管道上。
		_ = pr.CloseWithError(err)
		r.done <- err
	}()
	return nil
}

func (r *rawSink) write(w http.ResponseWriter, enc meta.Encoding, p []byte) error {
	if enc != meta.EncZstd {
		// 未压缩（服务端压缩启用失败时的降级路径）
		r.markTTFB()
		return writeBody(r.s, p, w)
	}
	if r.dec == nil {
		if err := r.start(w); err != nil {
			r.broken = true
			return err
		}
	}
	if _, err := r.pw.Write(p); err != nil {
		r.broken = true
		return fmt.Errorf("downlink: %w", err)
	}
	return nil
}

// markTTFB 记录首个明文字节写给下游的时刻（只看第一个）。
func (r *rawSink) markTTFB() {
	if r.s.TTFB == 0 {
		r.s.TTFB = time.Since(r.t0)
	}
}

// flush 由 FrameDone 触发：关管道让解码器读到 EOF，等它把尾巴写完。
func (r *rawSink) flush() error {
	if r.dec == nil || r.closed {
		return nil
	}
	r.closed = true
	_ = r.pw.Close()
	err := <-r.done
	if err != nil {
		r.broken = true
	}
	return err
}

// close 连接异常结束（没收到 FrameDone）：丢弃解压器。
// 已解出的明文已经写给下游并计入 s.Bytes，续传从那里继续。
func (r *rawSink) close() {
	if r.dec == nil {
		return
	}
	if !r.closed {
		r.closed = true
		_ = r.pw.CloseWithError(errors.New("downlink: connection closed"))
		<-r.done
	}
	r.dec.Close()
	r.dec = nil
}

func writeHead(w http.ResponseWriter, status int, hdrRaw []byte) {
	for k, vs := range meta.DecodeHeaders(hdrRaw) {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Del("Content-Length")
	w.WriteHeader(status)
	flush(w)
}

// writeBody 把一段明文写下游并记账。
//
// 必须记账「真正写成功的字节」而非「收到的字节」：解压后的明文长度和 wire
// 长度不再相等，续传偏移只能以写下游的明文为准，否则重连会重复或丢数据。
func writeBody(s *Session, p []byte, w http.ResponseWriter) error {
	n, err := w.Write(p)
	s.Bytes += int64(n)
	flush(w)
	if err != nil {
		return err
	}
	if id := lastEventID(p); id != "" {
		s.EndEventID = id
	}
	return nil
}

func flush(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// lastEventID 找字节流里最后一个 SSE "id: xxx"
func lastEventID(p []byte) string {
	var last string
	start := 0
	for i := 0; i < len(p); i++ {
		if p[i] != '\n' {
			continue
		}
		line := p[start:i]
		start = i + 1
		if len(line) > 3 && line[0] == 'i' && line[1] == 'd' && line[2] == ':' {
			v := line[3:]
			for len(v) > 0 && v[0] == ' ' {
				v = v[1:]
			}
			last = string(v)
		}
	}
	return last
}

// BuildResumeFrame 构造续传请求帧
//
// 必须带上 AcceptDownZstd：服务端靠它判断能不能推压缩流。不带的话，
// 「自身支持解压的客户端」续传压缩条目会被服务端拒绝（cap-miss），白跑一轮。
func BuildResumeFrame(key [32]byte, from int64, eventID string) ([]byte, error) {
	rq := &meta.ResumeReq{IdemKey: key, From: from, EventID: eventID, AcceptDownZstd: true}
	return meta.WrapFrame(meta.FrameResume, meta.EncodeResumeReq(rq))
}

// 保留 io 引用
var _ io.Reader = (*bytes.Reader)(nil)
var _ = json.Marshal
