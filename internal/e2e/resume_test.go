package e2e

// 端到端续传验证：直接操作隧道协议，绕过 relay-client 的 HTTP 层。
//
// 前提：relay-server 已在 127.0.0.1:19443 启动，上游为 60 事件的慢速 SSE。
//   E2E=1 go test ./internal/e2e -v

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jixoai/reply9842/internal/codec"
	"github.com/jixoai/reply9842/internal/meta"
	"github.com/jixoai/reply9842/internal/transport"
)

const (
	srvName = "localhost"
	apiPath = "/v1/chat/completions"
	apiBody = `{"stream":true,"k":"resume-e2e"}`
)

// srvAddr 允许用 E2E_ADDR 指向任意 relay-server（默认本地 19443）。
func srvAddr() string {
	if a := os.Getenv("E2E_ADDR"); a != "" {
		return a
	}
	return "127.0.0.1:19443"
}

func dial(t *testing.T) *transport.Conn {
	t.Helper()
	d := &transport.Dialer{
		Addr: srvAddr(), ServerName: srvName, Insecure: true, Mode: transport.KindTCP,
	}
	c, err := d.Dial(context.Background())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return c
}

func toMetaHash(h codec.Hash) meta.Hash {
	var out meta.Hash
	copy(out[:], h[:])
	return out
}

func sendRequest(t *testing.T, c *transport.Conn, key [32]byte) {
	t.Helper()
	var mh meta.Hash
	copy(mh[:], key[:])
	h := &meta.RequestHead{
		Method: "POST", Path: apiPath,
		Encoding:   meta.EncIdentity,
		ResultHash: toMetaHash(codec.Sum([]byte(apiBody))),
		BodyLen:    int64(len(apiBody)),
		IdemKey:    mh,
		// 与真实客户端一致：声明能解压下行压缩
		AcceptDownZstd: true,
	}
	hdr := meta.EncodeHeaders(map[string][]string{"Content-Type": {"application/json"}})
	payload, err := meta.EncodeRequest(h, hdr, []byte(apiBody))
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

func sendResume(t *testing.T, c *transport.Conn, key [32]byte, from int64, ev string) {
	t.Helper()
	rq := &meta.ResumeReq{IdemKey: key, From: from, EventID: ev, AcceptDownZstd: true}
	frame, err := meta.WrapFrame(meta.FrameResume, meta.EncodeResumeReq(rq))
	if err != nil {
		t.Fatalf("wrap resume: %v", err)
	}
	if err := transport.WriteFrame(c, frame); err != nil {
		t.Fatalf("send resume: %v", err)
	}
}

// readN 读至多 maxFrames 帧，返回 body、最后事件 id、帧数
func readN(t *testing.T, c *transport.Conn, maxFrames int) ([]byte, string, int) {
	t.Helper()
	var body []byte
	var lastEvent string
	frames := 0
	for frames < maxFrames {
		raw, err := c.Recv()
		if err != nil {
			return body, lastEvent, frames
		}
		ft, payload, err := meta.ReadFrame(strings.NewReader(string(raw)))
		if err != nil {
			return body, lastEvent, frames
		}
		frames++
		switch ft {
		case meta.FrameResumeInfo:
			info, _ := meta.DecodeResumeInfo(payload)
			t.Logf("  ResumeInfo status=%d written=%d complete=%v", info.Status, info.Written, info.Complete)
		case meta.FrameResponse:
			rh, _, b, _ := meta.DecodeResponse(payload)
			if rh.Status != 0 {
				t.Logf("  head status=%d", rh.Status)
			}
			body = append(body, b...)
			if id := lastID(b); id != "" {
				lastEvent = id
			}
		case meta.FrameError:
			t.Logf("  Error: %s", payload)
			return body, lastEvent, frames
		case meta.FrameDeltaMiss:
			t.Logf("  DeltaMiss (resume miss)")
			return body, lastEvent, frames
		}
	}
	return body, lastEvent, frames
}

func lastID(p []byte) string {
	var last string
	start := 0
	for i := 0; i < len(p); i++ {
		if p[i] != '\n' {
			continue
		}
		line := p[start:i]
		start = i + 1
		if len(line) > 3 && string(line[:3]) == "id:" {
			last = strings.TrimSpace(string(line[3:]))
		}
	}
	return last
}

func TestResumeAfterDisconnect(t *testing.T) {
	if os.Getenv("E2E") == "" {
		t.Skip("需要 E2E=1 且 relay-server 在 19443 运行")
	}

	// 每次运行用不同 key：固定 key 会在第二次运行时命中上一轮遗留的
	// 已完成条目（幂等复用生效），导致「阶段 1 就读完整」而无数据可续传。
	var key [32]byte
	copy(key[:], []byte(fmt.Sprintf("resume-e2e-%d", time.Now().UnixNano())))

	// 阶段 1：发请求，读 12 帧后断开
	c1 := dial(t)
	sendRequest(t, c1, key)
	part1, ev1, n1 := readN(t, c1, 12)
	t.Logf("阶段1：%d 帧 %d 字节，最后事件=%q", n1, len(part1), ev1)
	c1.Close()
	if len(part1) == 0 {
		t.Fatal("阶段1 无数据")
	}

	time.Sleep(1500 * time.Millisecond) // 等服务端 Detach

	// 阶段 2：Resume 续传
	c2 := dial(t)
	sendResume(t, c2, key, int64(len(part1)), ev1)
	part2, ev2, n2 := readN(t, c2, 500)
	t.Logf("阶段2：%d 帧 %d 字节，最后事件=%q", n2, len(part2), ev2)
	c2.Close()

	if len(part2) == 0 {
		t.Fatal("续传无数据")
	}

	ids := extractIDs(append(append([]byte{}, part1...), part2...))
	t.Logf("拼接后 %d 个事件", len(ids))
	for i := 1; i < len(ids); i++ {
		if ids[i] != ids[i-1]+1 {
			t.Errorf("事件 id 不连续: %d → %d", ids[i-1], ids[i])
		}
	}
	if len(ids) < 55 {
		t.Errorf("事件数偏少: %d（期望 ~60）", len(ids))
	}
}

func extractIDs(b []byte) []int {
	var out []int
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "id: ") {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(strings.TrimSpace(line[4:]), "%d", &n); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// readAll 读到 FrameDone 为止（新协议的正常结束信号）。
func readAll(t *testing.T, c *transport.Conn) ([]byte, bool) {
	t.Helper()
	var body []byte
	for {
		raw, err := c.Recv()
		if err != nil {
			return body, false
		}
		ft, payload, err := meta.ReadFrame(strings.NewReader(string(raw)))
		if err != nil {
			return body, false
		}
		switch ft {
		case meta.FrameDone:
			return body, true
		case meta.FrameResponse:
			rh, _, b, _ := meta.DecodeResponse(payload)
			if rh.Status != 0 {
				t.Logf("  head status=%d", rh.Status)
			}
			body = append(body, b...)
		case meta.FrameError:
			t.Logf("  Error: %s", payload)
			return body, false
		case meta.FrameDeltaMiss:
			t.Logf("  DeltaMiss")
			return body, false
		}
	}
}

// 线上现象：同 body 第二次请求，服务端日志说「复用已有条目」，客户端却等了
// 12.5 秒（第二次甚至比第一次 10.0 秒更慢）。
//
// 根因：服务端从不发送终止帧。客户端靠连接关闭判断结束，而「上游推完」与
// 「连接断了」在客户端视角完全一样，于是正常完成被记成 err="recv: EOF"，
// 且无法区分完成与断线，对已完成响应仍会重连重放（每轮 +300ms 退避）。
//
// 修复后：第一轮正常完成（Completed + 无 Err），第二轮命中幂等必须显著更快，
// 且收到的 body 与第一轮完全一致。
func TestReuseCompletedEntryIsFast(t *testing.T) {
	if os.Getenv("E2E") == "" {
		t.Skip("需要 E2E=1 且 relay-server 在 19443 运行")
	}

	// 每次跑用独立 key，避免与其它用例互相干扰
	var key [32]byte
	copy(key[:], []byte("reuse-fast-e2e-0123456789abcdef"))

	// 第一轮：完整读完（下游 60 事件 × 100ms ≈ 6s）
	c1 := dial(t)
	sendRequest(t, c1, key)
	t0 := time.Now()
	body1, ok1 := readAll(t, c1)
	d1 := time.Since(t0)
	c1.Close()
	if !ok1 {
		t.Fatalf("第一轮未收到 FrameDone（正常结束信号缺失），body=%d 字节", len(body1))
	}
	t.Logf("第一轮：%d 字节，耗时 %v", len(body1), d1)

	// 第二轮：同 key 同 body → 应命中幂等，从磁盘重放
	time.Sleep(300 * time.Millisecond)
	c2 := dial(t)
	sendRequest(t, c2, key)
	t0 = time.Now()
	body2, ok2 := readAll(t, c2)
	d2 := time.Since(t0)
	c2.Close()
	if !ok2 {
		t.Fatalf("第二轮未收到 FrameDone，body=%d 字节", len(body2))
	}
	t.Logf("第二轮（幂等命中）：%d 字节，耗时 %v", len(body2), d2)

	if len(body2) != len(body1) {
		t.Errorf("复用响应长度不一致: %d vs %d", len(body2), len(body1))
	}
	if string(body2) != string(body1) {
		t.Error("复用响应内容与第一轮不一致（幂等语义被破坏）")
	}
	// 上游本身要 6s；命中幂等应远快于此。给 3s 余量覆盖隧道与磁盘抖动。
	if d2 > 3*time.Second {
		t.Errorf("幂等命中耗时 %v，应远小于上游生成耗时（~6s）——说明没有真正复用，仍在等上游", d2)
	}
	if d2 >= d1 {
		t.Errorf("幂等命中 (%v) 不比首轮 (%v) 快，复用未生效", d2, d1)
	}
}
