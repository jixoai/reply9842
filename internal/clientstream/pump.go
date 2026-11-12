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
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/jixoai/rfc9842-reply/internal/meta"
	"github.com/jixoai/rfc9842-reply/internal/transport"
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
}

// Drain 读取一条连接上的所有帧，写入 w，直到上游完成 / 出错 / 连接断。
//
// 返回时 session 会记录进度。调用方若发现 !Completed && Err != nil，
// 可用 session.Bytes / session.EndEventID 发起 Resume。
//
// start 用于记录 TTFB（首个 body 字节写给下游的时间点）；传零值则以调用
// Drain 的时刻为起点。
func Drain(conn *transport.Conn, w http.ResponseWriter, s *Session, start time.Time) {
	if start.IsZero() {
		start = time.Now()
	}
	for {
		raw, err := conn.Recv()
		if err != nil {
			// 已收到 FrameDone 之后的连接关闭是正常收尾
			if s.Completed {
				return
			}
			s.Err = fmt.Errorf("recv: %w", err)
			return
		}
		ft, payload, err := meta.ReadFrame(bytes.NewReader(raw))
		if err != nil {
			s.Err = fmt.Errorf("frame: %w", err)
			s.Fatal = true
			return
		}
		// 记录隧道实际承载量（含 10 字节帧头与每次续片头），
		// 与写给下游的 Bytes（纯 body）区分开——两者在流式场景并不相等。
		s.WireBytes += int64(len(raw))

		switch ft {
		case meta.FrameError:
			s.Err = fmt.Errorf("server: %s", string(payload))
			// 服务端已判定失败（解码错误、上游报错、hash 不符）。
			// 续传只会拿回同样的结论，属于不可恢复错误。
			s.Fatal = true
			return

		case meta.FrameDone:
			// 上游已完整推完。这是唯一的「正常结束」信号。
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
			if len(body) > 0 {
				if s.TTFB == 0 {
					s.TTFB = time.Since(start)
				}
				if err := writeBody(w, body, s); err != nil {
					s.Err = err
					return
				}
			}
			continue
		}
	}
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

func writeBody(w http.ResponseWriter, p []byte, s *Session) error {
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
func BuildResumeFrame(key [32]byte, from int64, eventID string) ([]byte, error) {
	rq := &meta.ResumeReq{IdemKey: key, From: from, EventID: eventID}
	return meta.WrapFrame(meta.FrameResume, meta.EncodeResumeReq(rq))
}

// 保留 io 引用
var _ io.Reader = (*bytes.Reader)(nil)
var _ = json.Marshal
