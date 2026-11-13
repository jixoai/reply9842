package e2e

// 下行压缩的端到端验证：真实 relay-server + 真实 SSE 上游，走完整隧道协议。
//
// 前置：E2E=1 且 relay-server 在 19443 运行、上游为 60 事件慢速 SSE。
//   E2E=1 go test ./internal/e2e -run Downlink -v

import (
	"fmt"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jixoai/rfc9842-reply/internal/clientstream"
	"github.com/jixoai/rfc9842-reply/internal/codec"
	"github.com/jixoai/rfc9842-reply/internal/meta"
	"github.com/jixoai/rfc9842-reply/internal/transport"
)

func newKey(prefix string) [32]byte {
	var key [32]byte
	copy(key[:], []byte(fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())))
	return key
}

// sendReq 发一次完整请求帧
func sendReq(t *testing.T, c *transport.Conn, key [32]byte, body, path string) {
	t.Helper()
	var mh meta.Hash
	copy(mh[:], key[:])
	h := &meta.RequestHead{
		Method: "POST", Path: path,
		Encoding:   meta.EncIdentity,
		ResultHash: toMetaHash(codec.Sum([]byte(body))),
		BodyLen:    int64(len(body)),
		IdemKey:    mh,
		AcceptDownZstd: true,
	}
	hdr := meta.EncodeHeaders(map[string][]string{"Content-Type": {"application/json"}})
	payload, err := meta.EncodeRequest(h, hdr, []byte(body))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	frame, err := meta.WrapFrame(meta.FrameRequest, payload)
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	if err := transport.WriteFrame(c, frame); err != nil {
		t.Fatalf("send: %v", err)
	}
}

// TestDownlinkCompressionOnWire：下行压缩必须在真实链路上生效且无损
func TestDownlinkCompressionOnWire(t *testing.T) {
	if os.Getenv("E2E") == "" {
		t.Skip("需要 E2E=1 且 relay-server 在 19443 运行")
	}
	key := newKey("downlink-wire")
	c := dial(t)
	sendReq(t, c, key, apiBody, apiPath)

	rec := httptest.NewRecorder()
	s := &clientstream.Session{IdemKey: key}
	clientstream.Drain(c, rec, s, time.Now())
	c.Close()

	if !s.Completed {
		t.Fatalf("未完成: Err=%v", s.Err)
	}
	raw := rec.Body.Bytes()
	if len(raw) == 0 {
		t.Fatal("没有收到 body")
	}
	ratio := float64(s.WireBytes) / float64(len(raw))
	t.Logf("明文 %d → wire %d (%.1f%%，含帧头)", len(raw), s.WireBytes, ratio*100)
	if ratio >= 1 {
		t.Errorf("wire %.1f%% >= 100%%：下行没有压缩", ratio*100)
	}
	if ratio > 0.6 {
		t.Errorf("压缩率 %.1f%% 过高（期望 <60%%）", ratio*100)
	}
	ids := extractIDs(raw)
	if len(ids) < 55 {
		t.Errorf("事件数偏少: %d", len(ids))
	}
	for i := 1; i < len(ids); i++ {
		if ids[i] != ids[i-1]+1 {
			t.Fatalf("事件 id 不连续: %d → %d", ids[i-1], ids[i])
		}
	}
}

// TestDownlinkResumeLossless：断开 → 按明文偏移续传 → 拼接与完整响应一致
func TestDownlinkResumeLossless(t *testing.T) {
	if os.Getenv("E2E") == "" {
		t.Skip("需要 E2E=1 且 relay-server 在 19443 运行")
	}
	key := newKey("downlink-resume")

	// 阶段 1：收一部分就主动断连接（真实断线场景）
	c1 := dial(t)
	sendReq(t, c1, key, apiBody, apiPath)
	rec1 := httptest.NewRecorder()
	s := &clientstream.Session{IdemKey: key}

	done := make(chan struct{})
	go func() {
		clientstream.Drain(c1, rec1, s, time.Now())
		close(done)
	}()
	time.Sleep(1200 * time.Millisecond) // 收一段
	c1.Close()                          // 断线
	<-done

	part1 := append([]byte{}, rec1.Body.Bytes()...)
	if len(part1) == 0 {
		t.Fatal("阶段 1 没有收到明文")
	}
	if s.Completed {
		t.Skip("上游已完成，无法验证续传（上游太慢/连接太快）")
	}
	if s.Bytes != int64(len(part1)) {
		t.Fatalf("Bytes=%d 与已写下游 %d 不一致（续传偏移会错）", s.Bytes, len(part1))
	}
	t.Logf("阶段 1：%d 明文后断开，err=%v", len(part1), s.Err)

	time.Sleep(1500 * time.Millisecond) // 等服务端 Detach

	// 阶段 2：按明文偏移续传
	c2 := dial(t)
	rq := &meta.ResumeReq{IdemKey: key, From: int64(len(part1)), AcceptDownZstd: true}
	rf, err := meta.WrapFrame(meta.FrameResume, meta.EncodeResumeReq(rq))
	if err != nil {
		t.Fatalf("wrap resume: %v", err)
	}
	if err := transport.WriteFrame(c2, rf); err != nil {
		t.Fatalf("send resume: %v", err)
	}
	rec2 := httptest.NewRecorder()
	s.Err = nil
	clientstream.Drain(c2, rec2, s, time.Now())
	c2.Close()

	if !s.Completed {
		t.Fatalf("续传未完成: Err=%v", s.Err)
	}
	joined := append(part1, rec2.Body.Bytes()...)
	ids := extractIDs(joined)
	t.Logf("拼接后 %d 字节 / %d 个事件（首段 %d）", len(joined), len(ids), len(part1))
	if len(ids) < 55 {
		t.Fatalf("事件数偏少: %d", len(ids))
	}
	for i := 1; i < len(ids); i++ {
		if ids[i] != ids[i-1]+1 {
			t.Fatalf("拼接后事件 id 不连续: %d → %d（续传丢/重了数据）", ids[i-1], ids[i])
		}
	}
}
