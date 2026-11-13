package downlink

import (
	"bytes"
	"io"
	"math/rand"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

// decodeAll 用一次性解码器还原整条流
func decodeAll(t *testing.T, b []byte) []byte {
	t.Helper()
	d, err := zstd.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("decoder: %v", err)
	}
	defer d.Close()
	out, err := io.ReadAll(d)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

// sseBody 生成贴近真实的 SSE 响应（重复的 JSON 字段，压缩率高）
func sseBody(events, pad int) []byte {
	var b bytes.Buffer
	for i := 0; i < events; i++ {
		b.WriteString("event: content_block_delta\n")
		b.WriteString(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"`)
		for j := 0; j < pad; j++ {
			b.WriteString("token ")
		}
		b.WriteString(`"}}`)
		b.WriteString("\n\n")
	}
	return b.Bytes()
}

func TestRoundTripSmall(t *testing.T) {
	var buf bytes.Buffer
	c, err := New(&buf)
	if err != nil {
		t.Fatal(err)
	}
	in := []byte("hello downlink")
	if _, err := c.Write(in); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if got := decodeAll(t, buf.Bytes()); string(got) != string(in) {
		t.Fatalf("roundtrip = %q, want %q", got, in)
	}
}

// 150KB 级真实尺寸响应：压缩生效且无损
func TestRoundTripLarge(t *testing.T) {
	in := sseBody(400, 40) // ≈ 150KB
	if len(in) < 100_000 {
		t.Fatalf("样本太小: %d", len(in))
	}
	var buf bytes.Buffer
	c, err := New(&buf)
	if err != nil {
		t.Fatal(err)
	}
	// 按 8KB 分块写，模拟上游 chunk
	for i := 0; i < len(in); i += 8192 {
		e := i + 8192
		if e > len(in) {
			e = len(in)
		}
		if _, err := c.Write(in[i:e]); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if got := decodeAll(t, buf.Bytes()); !bytes.Equal(got, in) {
		t.Fatalf("150KB roundtrip 内容不一致: got %d bytes, want %d", len(got), len(in))
	}
	ratio := float64(buf.Len()) / float64(len(in))
	t.Logf("150KB SSE: %d → %d (%.1f%%)", len(in), buf.Len(), ratio*100)
	if ratio > 0.3 {
		t.Errorf("压缩率 %.1f%% 过高（SSE 重复度高，应远低于 30%%）", ratio*100)
	}
}

// 流式实时性：写入一小段后必须能被解出，不能等 Close 才吐
func TestStreamingDoesNotBatch(t *testing.T) {
	pr, pw := io.Pipe()

	type firstRead struct {
		n   int
		err error
	}
	first := make(chan firstRead, 1)
	// 解码 goroutine 持续读（否则管道写会阻塞，测试自己把自己卡住）
	go func() {
		d, err := zstd.NewReader(pr, zstd.WithDecoderConcurrency(1))
		if err != nil {
			first <- firstRead{0, err}
			return
		}
		defer d.Close()
		buf := make([]byte, 4096)
		n, rerr := d.Read(buf)
		first <- firstRead{n, rerr}
		io.Copy(io.Discard, d) // 读干净，让写端能结束
	}()

	c, err := New(pw)
	if err != nil {
		t.Fatal(err)
	}
	chunk := bytes.Repeat([]byte("event: message\ndata: 实时性测试\n\n"), 200)
	if len(chunk) < DefaultFlushBytes {
		t.Fatalf("测试块应大于 FlushBytes: %d", len(chunk))
	}
	if _, err := c.Write(chunk); err != nil {
		t.Fatal(err)
	}

	select {
	case r := <-first:
		if r.err != nil {
			t.Fatalf("解压读失败: %v", r.err)
		}
		if r.n == 0 {
			t.Fatal("写入超过阈值后解压端仍读不到数据（攒批了）")
		}
		t.Logf("阈值触发即时可读: %d 字节", r.n)
	case <-time.After(2 * time.Second):
		t.Fatal("解压端 2s 内没有拿到数据：压缩器在攒批，破坏流式语义")
	}

	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	pw.Close()
}

// 时间阈值触发：只有小数据、且距上次 flush 超过 FlushIdle 时必须自动 flush
func TestIdleFlush(t *testing.T) {
	var buf bytes.Buffer
	c, err := New(&buf)
	if err != nil {
		t.Fatal(err)
	}
	c.FlushIdle = 20 * time.Millisecond

	small := []byte("event: ping\n\n")
	if _, err := c.Write(small); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Logf("写入后立刻有 %d 字节输出（阈值触发）", buf.Len())
	}
	time.Sleep(30 * time.Millisecond)
	// 再写一次，此时应因空闲超时自动 flush
	if _, err := c.Write(small); err != nil {
		t.Fatal(err)
	}
	if buf.Len() == 0 {
		t.Fatal("空闲超时后仍未 flush，小响应会一直卡在压缩器里")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if got := decodeAll(t, buf.Bytes()); !bytes.Equal(got, append(append([]byte{}, small...), small...)) {
		t.Fatalf("内容不一致: %q", got)
	}
}

// Flush 的输出必须能被后续 Flush 接续（同一条流），不能被当成新流
func TestFlushKeepsStreamContinuous(t *testing.T) {
	var buf bytes.Buffer
	c, err := New(&buf)
	if err != nil {
		t.Fatal(err)
	}
	c.FlushBytes = 1 << 20 // 关闭字节阈值
	parts := [][]byte{
		[]byte("first-part|"),
		[]byte("second-part|"),
		[]byte("third-part"),
	}
	for _, p := range parts {
		if _, err := c.Write(p); err != nil {
			t.Fatal(err)
		}
		if err := c.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	want := bytes.Join(parts, nil)
	if got := decodeAll(t, buf.Bytes()); !bytes.Equal(got, want) {
		t.Fatalf("flush 之间流不连续: got %q want %q", got, want)
	}
}

// 不可压数据（随机）不应显著膨胀
func TestIncompressibleOverhead(t *testing.T) {
	rnd := rand.New(rand.NewSource(1))
	in := make([]byte, 64<<10)
	rnd.Read(in)

	var buf bytes.Buffer
	c, _ := New(&buf)
	c.FlushBytes = 1 << 20
	if _, err := c.Write(in); err != nil {
		t.Fatal(err)
	}
	c.Close()
	ratio := float64(buf.Len()) / float64(len(in))
	t.Logf("随机 64KB: %d → %d (%.1f%%)", len(in), buf.Len(), ratio*100)
	if ratio > 1.01 {
		t.Errorf("不可压数据膨胀到 %.2f%%", ratio*100)
	}
	if got := decodeAll(t, buf.Bytes()); !bytes.Equal(got, in) {
		t.Fatal("随机数据 roundtrip 不一致")
	}
}
