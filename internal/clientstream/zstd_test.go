package clientstream

import (
	"bytes"
	"fmt"
	"io"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jixoai/reply9842/internal/meta"

	"github.com/klauspost/compress/zstd"
)

// sseBody 生成可压缩 SSE 正文（含 id 行以验证事件游标）
func sseBody(events int, pad int) []byte {
	var b bytes.Buffer
	for i := 0; i < events; i++ {
		fmt.Fprintf(&b, "event: content_block_delta\nid: %d\n", i)
		b.WriteString(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"`)
		for j := 0; j < pad; j++ {
			b.WriteString("tok ")
		}
		b.WriteString("\"}}\n\n")
	}
	return b.Bytes()
}

// compressFrames 把明文字节按 chunk 压缩成若干 zstd 输出块（模拟服务端边收边压）
func compressFrames(t *testing.T, plain []byte, chunk int) [][]byte {
	t.Helper()
	var out [][]byte
	var buf bytes.Buffer
	w, err := zstd.NewWriter(&buf,
		zstd.WithEncoderLevel(zstd.SpeedFastest),
		zstd.WithEncoderCRC(true),
		zstd.WithEncoderConcurrency(1),
	)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(plain); i += chunk {
		end := i + chunk
		if end > len(plain) {
			end = len(plain)
		}
		if _, err := w.Write(plain[i:end]); err != nil {
			t.Fatal(err)
		}
		w.Flush()
		if buf.Len() > 0 {
			out = append(out, append([]byte{}, buf.Bytes()...))
			buf.Reset()
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if buf.Len() > 0 {
		out = append(out, append([]byte{}, buf.Bytes()...))
	}
	return out
}

// zstdFrames 组装帧序列：头帧（EncZstd）+ N 个续片 + Done
func zstdFrames(t *testing.T, status int, chunks [][]byte) [][]byte {
	t.Helper()
	out := [][]byte{respFrameEnc(t, status, meta.EncZstd, nil)}
	for _, c := range chunks {
		out = append(out, respFrameEnc(t, 0, meta.EncZstd, c))
	}
	out = append(out, doneFrame(t))
	return out
}

func respFrameEnc(t *testing.T, status int, enc meta.Encoding, body []byte) []byte {
	t.Helper()
	payload, err := meta.EncodeResponse(&meta.ResponseHead{
		Status: status, Encoding: enc,
		BodyLen: int64(len(body)), HeaderLen: 0,
	}, nil, body)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	f, err := meta.WrapFrame(meta.FrameResponse, payload)
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	return f
}

// 压缩流 roundtrip：客户端解出的明文与原始一致
func TestDrainZstdRoundTrip(t *testing.T) {
	plain := sseBody(200, 30)
	chunks := compressFrames(t, plain, 4096)
	if len(chunks) < 2 {
		t.Fatalf("样本应切成多块，实际 %d", len(chunks))
	}

	conn, closeFn := serveFrames(t, zstdFrames(t, 200, chunks))
	defer closeFn()

	rec := httptest.NewRecorder()
	s := &Session{}
	Drain(conn, rec, s, time.Now())

	if !s.Completed {
		t.Fatalf("未完成, Err=%v", s.Err)
	}
	if s.Err != nil {
		t.Fatalf("Err=%v", s.Err)
	}
	if got := rec.Body.Bytes(); !bytes.Equal(got, plain) {
		t.Fatalf("解压后内容不一致: %d vs %d 字节", len(got), len(plain))
	}
	if s.Bytes != int64(len(plain)) {
		t.Fatalf("Bytes=%d, want %d（应按明文记账）", s.Bytes, len(plain))
	}
	// wire 字节应明显小于明文（压缩真的生效）
	t.Logf("明文 %d → wire %d (%.1f%%)", len(plain), s.WireBytes, 100*float64(s.WireBytes)/float64(len(plain)))
	if s.WireBytes >= int64(len(plain)) {
		t.Errorf("wire %d >= 明文 %d：压缩没生效", s.WireBytes, len(plain))
	}
}

// 未压缩路径必须保持不变（旧服务端兼容）
func TestDrainIdentityUnchanged(t *testing.T) {
	plain := []byte("plain identity body")
	conn, closeFn := serveFrames(t, [][]byte{
		respFrame(t, 200, nil),
		respFrame(t, 0, plain),
		doneFrame(t),
	})
	defer closeFn()

	rec := httptest.NewRecorder()
	s := &Session{}
	Drain(conn, rec, s, time.Now())

	if !s.Completed || s.Err != nil {
		t.Fatalf("Completed=%v Err=%v", s.Completed, s.Err)
	}
	if got := rec.Body.String(); got != string(plain) {
		t.Fatalf("body=%q", got)
	}
}

// SSE 事件 id 游标在压缩路径下必须正确（续传要靠它）
func TestDrainZstdTracksEventID(t *testing.T) {
	plain := sseBody(40, 5)
	conn, closeFn := serveFrames(t, zstdFrames(t, 200, compressFrames(t, plain, 1024)))
	defer closeFn()

	rec := httptest.NewRecorder()
	s := &Session{}
	Drain(conn, rec, s, time.Now())

	if s.EndEventID != "39" {
		t.Fatalf("EndEventID=%q, want 39", s.EndEventID)
	}
}

// 续传完整性（核心）：断开 → 按已写偏移续传 → 拼接结果与完整响应一致。
//
// 服务端行为：收到 Resume 后从客户端声明的明文偏移处**重压一条新流**。
// 这里用两次独立的帧序列模拟，第 2 段只含 plain[from:] 的压缩数据。
func TestResumeAcrossConnectionsIsLossless(t *testing.T) {
	plain := sseBody(300, 25)
	chunks := compressFrames(t, plain, 4096)

	// 阶段 1：只发前 3 块（模拟中途断开，无 FrameDone）
	conn1, close1 := serveFrames(t, func() [][]byte {
		fr := [][]byte{respFrameEnc(t, 200, meta.EncZstd, nil)}
		for _, c := range chunks[:3] {
			fr = append(fr, respFrameEnc(t, 0, meta.EncZstd, c))
		}
		return fr // 不含 doneFrame → 连接断 = 可续传中断
	}())
	rec := httptest.NewRecorder()
	s := &Session{}
	Drain(conn1, rec, s, time.Now())
	close1()

	if s.Completed {
		t.Fatal("阶段 1 不应完成（无 FrameDone）")
	}
	if s.Err == nil {
		t.Fatal("阶段 1 应记录中断错误")
	}
	part1 := append([]byte{}, rec.Body.Bytes()...)
	if len(part1) == 0 {
		t.Fatal("阶段 1 没写出任何明文")
	}
	if s.Bytes != int64(len(part1)) {
		t.Fatalf("Bytes=%d 与已写明文 %d 不一致（续传偏移会错）", s.Bytes, len(part1))
	}

	// 阶段 2：从 s.Bytes 处续传（服务端重压剩余部分）
	rest := plain[len(part1):]
	conn2, close2 := serveFrames(t, zstdFrames(t, 200, compressFrames(t, rest, 4096)))
	defer close2()
	rec2 := httptest.NewRecorder()
	Drain(conn2, rec2, s, time.Now())

	if !s.Completed || s.Err != nil {
		t.Fatalf("阶段 2 未完成: Completed=%v Err=%v", s.Completed, s.Err)
	}
	joined := append(part1, rec2.Body.Bytes()...)
	if !bytes.Equal(joined, plain) {
		t.Fatalf("续传拼接不一致: %d vs %d 字节", len(joined), len(plain))
	}
	t.Logf("续传完整性 OK：%d + %d = %d 字节", len(part1), rec2.Body.Len(), len(plain))
}

// 续传在中间任意偏移都必须无损（跨压缩块边界、非块对齐）
func TestResumeAtArbitraryOffsets(t *testing.T) {
	plain := sseBody(120, 20)
	for _, frac := range []float64{0.1, 0.33, 0.5, 0.77, 0.95} {
		cut := int(float64(len(plain)) * frac)
		head := compressFrames(t, plain[:cut], 2048)
		headFrames := [][]byte{respFrameEnc(t, 200, meta.EncZstd, nil)}
		for _, c := range head {
			headFrames = append(headFrames, respFrameEnc(t, 0, meta.EncZstd, c))
		}
		conn1, close1 := serveFrames(t, headFrames)
		rec1 := httptest.NewRecorder()
		s := &Session{}
		Drain(conn1, rec1, s, time.Now())
		close1()

		conn2, close2 := serveFrames(t, zstdFrames(t, 200, compressFrames(t, plain[s.Bytes:], 2048)))
		rec2 := httptest.NewRecorder()
		s.Err = nil
		Drain(conn2, rec2, s, time.Now())
		close2()

		joined := append(append([]byte{}, rec1.Body.Bytes()...), rec2.Body.Bytes()...)
		if !bytes.Equal(joined, plain) {
			t.Fatalf("偏移 %.0f%%（%d）续传不一致: 得到 %d 字节", frac*100, s.Bytes, len(joined))
		}
	}
	t.Log("任意偏移续传均无损")
}

// 解压流损坏必须标记 Fatal（否则会按错误的明文偏移无限重试）
func TestDrainCorruptZstdIsFatal(t *testing.T) {
	plain := sseBody(50, 20)
	chunks := compressFrames(t, plain, 4096)
	// 破坏第 2 块：改成随机字节
	bad := make([]byte, len(chunks[1]))
	for i := range bad {
		bad[i] = 0xFF
	}
	chunks[1] = bad

	conn, closeFn := serveFrames(t, zstdFrames(t, 200, chunks))
	defer closeFn()

	rec := httptest.NewRecorder()
	s := &Session{}
	Drain(conn, rec, s, time.Now())

	if s.Err == nil {
		t.Fatal("损坏的压缩流应报错")
	}
	if !s.Fatal {
		t.Fatal("压缩流损坏应标记 Fatal：续传只会按错误偏移重复失败")
	}
}

// 头帧带 body 的情况（服务端第一条就带数据）也必须解压
func TestDrainZstdHeadWithBody(t *testing.T) {
	plain := sseBody(20, 10)
	chunks := compressFrames(t, plain, 1<<20)
	// 第一块塞进头帧，其余（含 Close 写出的帧尾）作为续片
	frames := [][]byte{respFrameEnc(t, 200, meta.EncZstd, chunks[0])}
	for _, c := range chunks[1:] {
		frames = append(frames, respFrameEnc(t, 0, meta.EncZstd, c))
	}
	frames = append(frames, doneFrame(t))

	conn, closeFn := serveFrames(t, frames)
	defer closeFn()

	rec := httptest.NewRecorder()
	s := &Session{}
	Drain(conn, rec, s, time.Now())
	if !s.Completed || s.Err != nil {
		t.Fatalf("Completed=%v Err=%v", s.Completed, s.Err)
	}
	if !bytes.Equal(rec.Body.Bytes(), plain) {
		t.Fatalf("头帧带 body 解压不一致: %d vs %d", rec.Body.Len(), len(plain))
	}
}

var _ = io.Discard
