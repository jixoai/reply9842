// Package meta — 隧道帧格式（QUIC/TCP 共用的二进制协议）
//
// 正交意图（2 个，2026-11-12 kzf 需求：「relay-client 和 relay-server 进行压缩
// 通讯，最后由 relay-server 代理转发」）：
//  1. 请求帧：把一次 HTTP 调用的 method/path/headers/body 编码成单帧，
//     携带 base 哈希与结果哈希（RFC 9842 dcz 语义的隧道化）
//  2. 响应帧：把上游响应流式回传，区分「立即失败」与「正常响应」
//
// 协议设计约束：QUIC stream 与 TCP 连接共享同一套编码，故帧必须自定界
// （长度前缀），不依赖传输层消息边界。
package meta

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// 版本与魔数。变更帧布局必须提升 Version，旧版本一律拒绝（不做兼容）。
const (
	Magic   uint32 = 0x5246_3938 // "RF98"
	Version uint8  = 1
)

// FrameType 帧类型
type FrameType uint8

const (
	FrameRequest    FrameType = 1 // client → server：一次 HTTP 调用
	FrameResponse   FrameType = 2 // server → client：上游响应
	FrameDeltaMiss  FrameType = 3 // server → client：base 未命中，请重发全量
	FrameError      FrameType = 4 // server → client：服务端错误
	FrameAck        FrameType = 5 // 双向：连接级确认（保留）
	FramePing       FrameType = 6 // 双向：保活
	FrameResume     FrameType = 7 // client → server：按 key 续传已有响应
	FrameResumeInfo FrameType = 8 // server → client：续传元信息
	// FrameDone 服务端→客户端：本次响应已完整推完（含 Resume 重放）。
	//
	// 为什么必须有这个帧：客户端靠「连接关闭」判断结束，而连接关闭对 TCP 是
	// 正常 EOF、对 QUIC 是 stream FIN，两者都与「网络抖动/对端消失」在客户端
	// 视角无差别。没有终止帧时，客户端只能把正常结束当成 Err（实测 JSONL 里
	// 3 个 200 全带 err="recv: EOF"，errors 计数虚高），且无法区分「上游完成」
	// 与「连接断了需要续传」，导致对已完成响应白白重连重放。
	FrameDone FrameType = 9
)

// Encoding 请求体的编码方式
type Encoding uint8

const (
	EncIdentity Encoding = 0 // 明文
	EncGzip     Encoding = 1
	EncZstd     Encoding = 2
	EncDcz      Encoding = 3 // RFC 9842 dcz：40 字节头 + zstd 流（基于 BaseHash 字典）
)

func (e Encoding) String() string {
	switch e {
	case EncIdentity:
		return "identity"
	case EncGzip:
		return "gzip"
	case EncZstd:
		return "zstd"
	case EncDcz:
		return "dcz"
	}
	return fmt.Sprintf("enc(%d)", e)
}

// HashSize BLAKE3-256 的长度
const HashSize = 32

// Hash 内容寻址标识。全零表示「无基线」。
type Hash [HashSize]byte

// IsZero 判断是否为空哈希
func (h Hash) IsZero() bool {
	for _, b := range h {
		if b != 0 {
			return false
		}
	}
	return true
}

func (h Hash) String() string { return fmt.Sprintf("%x", h[:8]) }

// RequestHead 请求帧头部（后面紧跟 BodyLen 字节的请求体）
type RequestHead struct {
	Method     string
	Path       string
	BaseHash   Hash // dcz 基线；EncDcz 时有效
	ResultHash Hash // 应用 delta 后应得到的 body 哈希，用于防静默损坏
	Encoding   Encoding
	BodyLen    int64 // 编码后的 body 字节数
	HeaderLen  int32 // 扁平化 headers 的字节数

	// IdemKey 幂等键：客户端按 (method+path+body+callerKeyId) 计算，
	// 用于断线重连时复用服务端已在接收/已完成的响应。
	IdemKey Hash
	// ResumeFrom 续传起点：0 表示从头；>0 表示「我已有前 N 字节，从 N 开始推」。
	ResumeFrom int64
}

// ResponseHead 响应帧头部
type ResponseHead struct {
	Status    int
	Encoding  Encoding
	BodyLen   int64 // -1 表示长度未知（流式）
	HeaderLen int32
}

// ---- 编码 ----

// WriteFrame 写入一个完整帧：magic(4) + version(1) + type(1) + payloadLen(4) + payload
func WriteFrame(w io.Writer, ft FrameType, payload []byte) error {
	var hdr [10]byte
	binary.BigEndian.PutUint32(hdr[0:4], Magic)
	hdr[4] = Version
	hdr[5] = byte(ft)
	binary.BigEndian.PutUint32(hdr[6:10], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

// ReadFrame 读取一个完整帧（payload 由调用方释放）
func ReadFrame(r io.Reader) (FrameType, []byte, error) {
	var hdr [10]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	if binary.BigEndian.Uint32(hdr[0:4]) != Magic {
		return 0, nil, errors.New("meta: bad magic")
	}
	if hdr[4] != Version {
		return 0, nil, fmt.Errorf("meta: unsupported version %d", hdr[4])
	}
	n := binary.BigEndian.Uint32(hdr[6:10])
	if n > 256<<20 { // 256MB 上限，防御性
		return 0, nil, fmt.Errorf("meta: frame too large: %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, nil, err
	}
	return FrameType(hdr[5]), buf, nil
}

// EncodeRequest 把 RequestHead 与 body/headers 序列化成单帧 payload
//
// 布局：methodLen(1) method | pathLen(2) path | base(32) | result(32)
//
//	encoding(1) bodyLen(8) headerLen(4) | headers | body
func EncodeRequest(h *RequestHead, headers, body []byte) ([]byte, error) {
	if len(h.Method) > 255 {
		return nil, errors.New("meta: method too long")
	}
	if len(h.Path) > 0xFFFF {
		return nil, errors.New("meta: path too long")
	}
	size := 1 + len(h.Method) + 2 + len(h.Path) + HashSize*2 + 1 + 8 + 4 + HashSize + 8 + len(headers) + len(body)
	buf := make([]byte, 0, size)

	buf = append(buf, byte(len(h.Method)))
	buf = append(buf, h.Method...)
	var tmp2 [2]byte
	binary.BigEndian.PutUint16(tmp2[:], uint16(len(h.Path)))
	buf = append(buf, tmp2[:]...)
	buf = append(buf, h.Path...)
	buf = append(buf, h.BaseHash[:]...)
	buf = append(buf, h.ResultHash[:]...)
	buf = append(buf, byte(h.Encoding))
	var tmp8 [8]byte
	binary.BigEndian.PutUint64(tmp8[:], uint64(len(body)))
	buf = append(buf, tmp8[:]...)
	var tmp4 [4]byte
	binary.BigEndian.PutUint32(tmp4[:], uint32(len(headers)))
	buf = append(buf, tmp4[:]...)
	buf = append(buf, h.IdemKey[:]...)
	var tmpRF [8]byte
	binary.BigEndian.PutUint64(tmpRF[:], uint64(h.ResumeFrom))
	buf = append(buf, tmpRF[:]...)
	buf = append(buf, headers...)
	buf = append(buf, body...)
	return buf, nil
}

// DecodeRequest 解析请求帧 payload，返回 head 与 headers/body 切片（指向同一底层数组）
func DecodeRequest(b []byte) (*RequestHead, []byte, []byte, error) {
	off := 0
	need := func(n int) error {
		if off+n > len(b) {
			return errors.New("meta: truncated request frame")
		}
		return nil
	}
	if err := need(1); err != nil {
		return nil, nil, nil, err
	}
	ml := int(b[off])
	off++
	if err := need(ml); err != nil {
		return nil, nil, nil, err
	}
	method := string(b[off : off+ml])
	off += ml

	if err := need(2); err != nil {
		return nil, nil, nil, err
	}
	pl := int(binary.BigEndian.Uint16(b[off : off+2]))
	off += 2
	if err := need(pl); err != nil {
		return nil, nil, nil, err
	}
	path := string(b[off : off+pl])
	off += pl

	if err := need(HashSize * 2); err != nil {
		return nil, nil, nil, err
	}
	var base, result Hash
	copy(base[:], b[off:off+HashSize])
	off += HashSize
	copy(result[:], b[off:off+HashSize])
	off += HashSize

	if err := need(1 + 8 + 4); err != nil {
		return nil, nil, nil, err
	}
	enc := Encoding(b[off])
	off++
	bodyLen := int64(binary.BigEndian.Uint64(b[off : off+8]))
	off += 8
	hdrLen := int64(binary.BigEndian.Uint32(b[off : off+4]))
	off += 4

	if err := need(HashSize + 8); err != nil {
		return nil, nil, nil, err
	}
	var idem Hash
	copy(idem[:], b[off:off+HashSize])
	off += HashSize
	resumeFrom := int64(binary.BigEndian.Uint64(b[off : off+8]))
	off += 8

	if hdrLen < 0 || bodyLen < 0 || off+int(hdrLen)+int(bodyLen) > len(b) {
		return nil, nil, nil, errors.New("meta: bad lengths in request frame")
	}
	headers := b[off : off+int(hdrLen)]
	off += int(hdrLen)
	body := b[off : off+int(bodyLen)]

	return &RequestHead{
		Method: method, Path: path, BaseHash: base, ResultHash: result,
		Encoding: enc, BodyLen: bodyLen, HeaderLen: int32(hdrLen),
		IdemKey: idem, ResumeFrom: resumeFrom,
	}, headers, body, nil
}

// EncodeResponse 序列化响应帧 payload
func EncodeResponse(h *ResponseHead, headers, body []byte) ([]byte, error) {
	size := 4 + 1 + 8 + 4 + len(headers) + len(body)
	buf := make([]byte, 0, size)
	var t4 [4]byte
	binary.BigEndian.PutUint32(t4[:], uint32(h.Status))
	buf = append(buf, t4[:]...)
	buf = append(buf, byte(h.Encoding))
	var t8 [8]byte
	binary.BigEndian.PutUint64(t8[:], uint64(h.BodyLen))
	buf = append(buf, t8[:]...)
	var t4b [4]byte
	binary.BigEndian.PutUint32(t4b[:], uint32(len(headers)))
	buf = append(buf, t4b[:]...)
	buf = append(buf, headers...)
	buf = append(buf, body...)
	return buf, nil
}

// DecodeResponse 解析响应帧 payload
func DecodeResponse(b []byte) (*ResponseHead, []byte, []byte, error) {
	off := 0
	if len(b) < 4+1+8+4 {
		return nil, nil, nil, errors.New("meta: truncated response frame")
	}
	status := int(binary.BigEndian.Uint32(b[off : off+4]))
	off += 4
	enc := Encoding(b[off])
	off++
	bodyLen := int64(binary.BigEndian.Uint64(b[off : off+8]))
	off += 8
	hdrLen := int64(binary.BigEndian.Uint32(b[off : off+4]))
	off += 4
	if hdrLen < 0 || off+int(hdrLen) > len(b) {
		return nil, nil, nil, errors.New("meta: bad header length in response frame")
	}
	headers := b[off : off+int(hdrLen)]
	off += int(hdrLen)
	body := b[off:]
	return &ResponseHead{Status: status, Encoding: enc, BodyLen: bodyLen, HeaderLen: int32(hdrLen)},
		headers, body, nil
}

// EncodeHeaders / DecodeHeaders 简化的 headers 扁平化：k:v\nk:v\n
// 只保留首值（多值头在 LLM API 场景极少见），大小写原样保留。
func EncodeHeaders(h map[string][]string) []byte {
	var buf []byte
	for k, vs := range h {
		if len(vs) == 0 {
			continue
		}
		buf = append(buf, k...)
		buf = append(buf, ':')
		buf = append(buf, vs[0]...)
		buf = append(buf, '\n')
	}
	return buf
}

// DecodeHeaders 解析扁平化 headers
func DecodeHeaders(b []byte) map[string][]string {
	out := make(map[string][]string, 8)
	start := 0
	for i := 0; i < len(b); i++ {
		if b[i] != '\n' {
			continue
		}
		line := b[start:i]
		start = i + 1
		for j := 0; j < len(line); j++ {
			if line[j] == ':' {
				k := string(line[:j])
				v := string(line[j+1:])
				out[k] = append(out[k], v)
				break
			}
		}
	}
	return out
}

// ---- 续传帧（FrameResume / FrameResumeInfo）----
//
// FrameResume 由客户端发出，告诉服务端「我要按某个 key 续传」。
// 服务端若命中，回 FrameResumeInfo 告知 status/headers/已有字节数/是否完成；
// 未命中则回 FrameDeltaMiss（复用同一语义：请重新发起完整请求）。

// ResumeReq 续传请求载荷
type ResumeReq struct {
	IdemKey [32]byte
	// From 客户端已有的字节偏移（-1 表示按 EventID 定位）
	From    int64
	EventID string // SSE 事件 id；非空则优先于 From
}

// ResumeInfo 续传响应载荷
type ResumeInfo struct {
	Status      int
	Written     int64 // 服务端已落盘字节数
	Complete    bool  // 上游是否已结束
	HeaderLen   int32
	Headers     []byte
	UpstreamErr string
}

// EncodeResumeReq 序列化续传请求
func EncodeResumeReq(r *ResumeReq) []byte {
	buf := make([]byte, 0, 32+8+2+len(r.EventID))
	buf = append(buf, r.IdemKey[:]...)
	var t8 [8]byte
	binary.BigEndian.PutUint64(t8[:], uint64(r.From))
	buf = append(buf, t8[:]...)
	if len(r.EventID) > 0xFFFF {
		r.EventID = r.EventID[:0xFFFF]
	}
	var t2 [2]byte
	binary.BigEndian.PutUint16(t2[:], uint16(len(r.EventID)))
	buf = append(buf, t2[:]...)
	buf = append(buf, r.EventID...)
	return buf
}

// DecodeResumeReq 反序列化续传请求
func DecodeResumeReq(b []byte) (*ResumeReq, error) {
	if len(b) < 32+8+2 {
		return nil, errors.New("meta: short resume req")
	}
	r := &ResumeReq{}
	copy(r.IdemKey[:], b[:32])
	r.From = int64(binary.BigEndian.Uint64(b[32:40]))
	n := int(binary.BigEndian.Uint16(b[40:42]))
	if 42+n > len(b) {
		return nil, errors.New("meta: bad resume req length")
	}
	r.EventID = string(b[42 : 42+n])
	return r, nil
}

// EncodeResumeInfo 序列化续传响应
func EncodeResumeInfo(i *ResumeInfo) []byte {
	buf := make([]byte, 0, 4+8+1+4+len(i.Headers)+2+len(i.UpstreamErr))
	var t4 [4]byte
	binary.BigEndian.PutUint32(t4[:], uint32(i.Status))
	buf = append(buf, t4[:]...)
	var t8 [8]byte
	binary.BigEndian.PutUint64(t8[:], uint64(i.Written))
	buf = append(buf, t8[:]...)
	if i.Complete {
		buf = append(buf, 1)
	} else {
		buf = append(buf, 0)
	}
	var t4b [4]byte
	binary.BigEndian.PutUint32(t4b[:], uint32(len(i.Headers)))
	buf = append(buf, t4b[:]...)
	buf = append(buf, i.Headers...)
	e := i.UpstreamErr
	if len(e) > 0xFFFF {
		e = e[:0xFFFF]
	}
	var t2 [2]byte
	binary.BigEndian.PutUint16(t2[:], uint16(len(e)))
	buf = append(buf, t2[:]...)
	buf = append(buf, e...)
	return buf
}

// DecodeResumeInfo 反序列化续传响应
func DecodeResumeInfo(b []byte) (*ResumeInfo, error) {
	if len(b) < 4+8+1+4 {
		return nil, errors.New("meta: short resume info")
	}
	i := &ResumeInfo{}
	i.Status = int(binary.BigEndian.Uint32(b[0:4]))
	i.Written = int64(binary.BigEndian.Uint64(b[4:12]))
	i.Complete = b[12] == 1
	hl := int(binary.BigEndian.Uint32(b[13:17]))
	off := 17
	if off+hl > len(b) {
		return nil, errors.New("meta: bad resume info header length")
	}
	i.Headers = b[off : off+hl]
	off += hl
	if off+2 > len(b) {
		return nil, errors.New("meta: missing resume info error field")
	}
	n := int(binary.BigEndian.Uint16(b[off : off+2]))
	off += 2
	if off+n > len(b) {
		return nil, errors.New("meta: bad resume info error length")
	}
	i.UpstreamErr = string(b[off : off+n])
	return i, nil
}

// WrapFrame 把 payload 包成带 magic 的完整帧（含 10 字节头）
func WrapFrame(ft FrameType, payload []byte) ([]byte, error) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, ft, payload); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Hash 便捷判断
func (h Hash) Bytes() [32]byte {
	var out [32]byte
	copy(out[:], h[:])
	return out
}
