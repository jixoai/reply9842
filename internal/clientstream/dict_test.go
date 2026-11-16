package clientstream

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jixoai/reply9842/internal/codec"
	"github.com/jixoai/reply9842/internal/meta"
)

// ── 下行字典压缩（dcz/dcz2）的客户端解码 ─────────────────────────────────
//
// 契约（本任务的要求）：下行由服务端选编码，客户端**两种都得能解**；
// 解不了时必须明确失败，绝不把压缩字节按明文写给下游。

// (respFrameEnc / doneFrame 在 zstd_test.go / pump_test.go 里已有：直接复用)

// dictStreamFrames 把一条字典压缩流按给定分片切成续片帧（覆盖「头被切碎」
// 与「头与正文同帧」两种形态）
func dictStreamFrames(t *testing.T, enc meta.Encoding, stream []byte, chunks []int) [][]byte {
	t.Helper()
	frames := [][]byte{respFrameEnc(t, 200, enc, nil)} // 响应头帧（无 body）
	for i, n := 0, 0; i < len(stream); i += n {
		n = chunks[len(frames)%len(chunks)]
		if i+n > len(stream) {
			n = len(stream) - i
		}
		frames = append(frames, respFrameEnc(t, 0, enc, stream[i:i+n]))
	}
	return append(frames, doneFrame(t))
}

// dictBody 造一段够长的明文（跨多个读缓冲）
func dictBody() []byte {
	return []byte(strings.Repeat("{\"id\":\"chatcmpl-1\",\"choices\":[{\"delta\":{\"content\":\"hello \"}}]}\n", 800))
}

// TestDrainDcz2DownlinkWithDict dcz2 下行 + 本地有字典 → 解出完整明文。
// 头 40 字节被切成 7/20/13 三片，验证跨帧攒头的正确性。
func TestDrainDcz2DownlinkWithDict(t *testing.T) {
	dict := []byte(strings.Repeat("{\"model\":\"x\",\"messages\":[...]},", 64))
	plain := dictBody()
	stream, err := codec.EncodeDcz2(plain, 9, dict)
	if err != nil {
		t.Fatalf("EncodeDcz2: %v", err)
	}
	sha := [32]byte(codec.SumSHA256(dict))

	frames := dictStreamFrames(t, meta.EncDcz2, stream, []int{7, 20, 13, 1 << 20})
	conn, closeFn := serveFrames(t, frames)
	defer closeFn()

	rec := httptest.NewRecorder()
	s := &Session{Dict: func(enc meta.Encoding, h [32]byte) ([]byte, bool) {
		if enc != meta.EncDcz2 {
			t.Errorf("解析器收到编码 %v，期望 dcz2", enc)
			return nil, false
		}
		if h != sha {
			t.Errorf("解析器收到哈希 %x，期望 SHA-256(dict) %x", h[:8], sha[:8])
			return nil, false
		}
		return dict, true
	}}
	Drain(conn, rec, s, time.Now())

	if !s.Completed || s.Err != nil {
		t.Fatalf("Completed=%v Err=%v（期望干净完成）", s.Completed, s.Err)
	}
	if got := rec.Body.Bytes(); !bytes.Equal(got, plain) {
		t.Fatalf("下游明文不等：%d 字节 vs %d", len(got), len(plain))
	}
	if s.TTFB <= 0 {
		t.Error("TTFB 未记录")
	}
	if s.Bytes != int64(len(plain)) {
		t.Errorf("Bytes=%d，期望 %d", s.Bytes, len(plain))
	}
}

// TestDrainDcz1DownlinkWithDict 私有 dcz（BLAKE3 头）下行 + 本地有字典 → 解出明文。
func TestDrainDcz1DownlinkWithDict(t *testing.T) {
	dict := []byte(strings.Repeat("{\"model\":\"x\",\"messages\":[...]},", 64))
	plain := dictBody()
	stream, err := codec.EncodeDcz(plain, codec.Sum(dict), 9, dict)
	if err != nil {
		t.Fatalf("EncodeDcz: %v", err)
	}
	b3 := [32]byte(codec.Sum(dict))

	frames := dictStreamFrames(t, meta.EncDcz, stream, []int{40, 1 << 20})
	conn, closeFn := serveFrames(t, frames)
	defer closeFn()

	rec := httptest.NewRecorder()
	s := &Session{Dict: func(enc meta.Encoding, h [32]byte) ([]byte, bool) {
		if enc != meta.EncDcz {
			t.Errorf("解析器收到编码 %v，期望 dcz", enc)
			return nil, false
		}
		if h != b3 {
			t.Errorf("解析器收到哈希 %x，期望 BLAKE3(dict) %x", h[:8], b3[:8])
			return nil, false
		}
		return dict, true
	}}
	Drain(conn, rec, s, time.Now())

	if !s.Completed || s.Err != nil {
		t.Fatalf("Completed=%v Err=%v（期望干净完成）", s.Completed, s.Err)
	}
	if got := rec.Body.Bytes(); !bytes.Equal(got, plain) {
		t.Fatalf("下游明文不等：%d 字节 vs %d", len(got), len(plain))
	}
}

// TestDrainDcz2WithoutDictFails 本地没有字典 → 必须明确失败，
// 且**不写给下游任何字节**（把 zstd 字节当明文写就是静默损坏）。
func TestDrainDcz2WithoutDictFails(t *testing.T) {
	dict := []byte(strings.Repeat("dict-content ", 512))
	plain := dictBody()
	stream, err := codec.EncodeDcz2(plain, 9, dict)
	if err != nil {
		t.Fatal(err)
	}
	frames := dictStreamFrames(t, meta.EncDcz2, stream, []int{64, 1 << 20})
	conn, closeFn := serveFrames(t, frames)
	defer closeFn()

	rec := httptest.NewRecorder()
	s := &Session{} // Dict=nil：本地无字典
	Drain(conn, rec, s, time.Now())

	if s.Err == nil {
		t.Fatal("没有字典却解成功了？")
	}
	if !s.Fatal {
		t.Error("字典缺失是不可恢复错误，应标记 Fatal（续传只会重复失败）")
	}
	if s.Completed {
		t.Error("失败不应标记完成")
	}
	if n := rec.Body.Len(); n != 0 {
		t.Fatalf("失败时不得写给下游任何字节，却写了 %d 字节（静默损坏）", n)
	}
	t.Logf("拒绝理由: %v", s.Err)
}

// TestDrainUnknownEncodingIsNotPlaintext 未知编码（未来版本/损坏帧）不得按明文
// 兜底——这正是「老客户端把压缩流当明文写」的历史事故形态。
func TestDrainUnknownEncodingIsNotPlaintext(t *testing.T) {
	unknown := meta.Encoding(200)
	conn, closeFn := serveFrames(t, [][]byte{
		respFrameEnc(t, 200, unknown, []byte("\x28\xb5\x2f\xfd-not-really-plaintext")),
		doneFrame(t),
	})
	defer closeFn()

	rec := httptest.NewRecorder()
	s := &Session{}
	Drain(conn, rec, s, time.Now())

	if s.Err == nil {
		t.Fatal("未知编码必须报错")
	}
	if !s.Fatal {
		t.Error("未知编码应标记 Fatal")
	}
	if n := rec.Body.Len(); n != 0 {
		t.Fatalf("未知编码不得写给下游任何字节，却写了 %d 字节", n)
	}
	t.Logf("拒绝理由: %v", s.Err)
}

// TestDrainDcz2TruncatedHead 流在 40 字节头收齐前结束 → 截断，不能当成功。
func TestDrainDcz2TruncatedHead(t *testing.T) {
	conn, closeFn := serveFrames(t, [][]byte{
		respFrameEnc(t, 200, meta.EncDcz2, nil),
		respFrameEnc(t, 0, meta.EncDcz2, []byte{0x5e, 0x2a, 0x4d, 0x18, 0x20, 0x00, 0x00, 0x00}), // 只有 magic
		doneFrame(t),
	})
	defer closeFn()

	rec := httptest.NewRecorder()
	s := &Session{Dict: func(meta.Encoding, [32]byte) ([]byte, bool) {
		t.Error("头都没攒齐，不应该去问字典")
		return nil, false
	}}
	Drain(conn, rec, s, time.Now())

	if s.Err == nil {
		t.Fatal("截断的 dcz2 流必须报错")
	}
	if s.Completed {
		t.Error("截断流不应标记完成")
	}
	if n := rec.Body.Len(); n != 0 {
		t.Fatalf("截断流不得写给下游任何字节，却写了 %d 字节", n)
	}
	t.Logf("拒绝理由: %v", s.Err)
}
