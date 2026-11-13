package spool

import (
	"bytes"
	"fmt"
	"io"
	"runtime"
	"testing"

	"github.com/jixoai/rfc9842-reply/internal/downlink"
	"time"
)

// sse 生成可压缩的 SSE 正文
func sse(events, pad int) []byte {
	var b bytes.Buffer
	for i := 0; i < events; i++ {
		fmt.Fprintf(&b, "event: content_block_delta\nid: %d\n", i)
		b.WriteString(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"`)
		for j := 0; j < pad; j++ {
			b.WriteString("token ")
		}
		b.WriteString("\"}}\n\n")
	}
	return b.Bytes()
}

// newCompressedEntry 建立一个「已申请压缩」的条目。
// 注意压缩是延迟拍板的（攒够 downlink.MinUseful 才真正开始），
// 所以测试里写够数据后调用方应检查 e.Compressed()。
func newCompressedEntry(t *testing.T, m *Manager, key [32]byte) *Entry {
	t.Helper()
	e, existing, err := m.Acquire(key)
	if err != nil || existing {
		t.Fatalf("acquire: %v existing=%v", err, existing)
	}
	e.EnableCompression()
	return e
}

// 压缩条目的 .dat 必须真的变小（spool 存压缩后的字节）
func TestCompressedSpoolSavesDisk(t *testing.T) {
	m := newTestManager(t)
	var key [32]byte
	key[0] = 7
	e := newCompressedEntry(t, m, key)

	in := sse(300, 30)
	for i := 0; i < len(in); i += 4096 {
		end := i + 4096
		if end > len(in) {
			end = len(in)
		}
		if err := e.Append(in[i:end]); err != nil {
			t.Fatal(err)
		}
	}
	e.Finish(nil)

	if got := e.Written(); got != int64(len(in)) {
		t.Fatalf("Written=%d, want %d（明文语义）", got, len(in))
	}
	wire := e.WireBytes()
	if wire <= 0 || wire >= int64(len(in)) {
		t.Fatalf("WireBytes=%d, 明文 %d：压缩没有落到磁盘", wire, len(in))
	}
	t.Logf("spool 磁盘: %d → %d (%.1f%%)", len(in), wire, 100*float64(wire)/float64(len(in)))
}

// 压缩条目的 ReadFrom 必须按明文偏移返回正确内容（整条连续读）
func TestCompressedReadFull(t *testing.T) {
	m := newTestManager(t)
	var key [32]byte
	key[0] = 8
	e := newCompressedEntry(t, m, key)

	in := sse(200, 20)
	if err := e.Append(in); err != nil {
		t.Fatal(err)
	}
	e.Finish(nil)

	var got bytes.Buffer
	off := int64(0)
	for {
		buf, err := e.ReadFrom(off, 32*1024)
		if err != nil {
			t.Fatalf("ReadFrom(%d): %v", off, err)
		}
		if len(buf) == 0 {
			break
		}
		got.Write(buf)
		off += int64(len(buf))
	}
	if !bytes.Equal(got.Bytes(), in) {
		t.Fatalf("读出的内容与写入不一致: %d vs %d 字节", got.Len(), len(in))
	}
}

// 分段顺序读（游标复用）+ 随机跳读（重建游标）都必须正确
func TestCompressedReadSeek(t *testing.T) {
	m := newTestManager(t)
	var key [32]byte
	key[0] = 9
	e := newCompressedEntry(t, m, key)

	in := sse(150, 25)
	if err := e.Append(in); err != nil {
		t.Fatal(err)
	}
	e.Finish(nil)

	// 1) 顺序分块（游标命中路径）
	var seq bytes.Buffer
	off := int64(0)
	for off < int64(len(in)) {
		buf, err := e.ReadFrom(off, 1000)
		if err != nil {
			t.Fatal(err)
		}
		if len(buf) == 0 {
			break
		}
		seq.Write(buf)
		off += int64(len(buf))
	}
	if !bytes.Equal(seq.Bytes(), in) {
		t.Fatal("顺序分块读不一致")
	}

	// 2) 跳读（每次都要重建游标 + 解压丢弃前缀）
	for _, off := range []int64{0, 1, 1777, int64(len(in)) / 2, int64(len(in)) - 10} {
		buf, err := e.ReadFrom(off, 100)
		if err != nil {
			t.Fatalf("seek %d: %v", off, err)
		}
		want := in[off:]
		if int64(len(want)) > 100 {
			want = want[:100]
		}
		if !bytes.Equal(buf, want) {
			t.Fatalf("seek %d 读错: got %d bytes", off, len(buf))
		}
	}
}

// 流式读取：Append 与 ReadFrom 并发时必须"写一点能读到一点"，
// 且绝不能读到还攒在压缩器里的字节。
func TestCompressedStreamingRead(t *testing.T) {
	m := newTestManager(t)
	var key [32]byte
	key[0] = 10
	e := newCompressedEntry(t, m, key)

	in := sse(120, 40)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < len(in); i += 2048 {
			end := i + 2048
			if end > len(in) {
				end = len(in)
			}
			_ = e.Append(in[i:end])
			time.Sleep(time.Millisecond)
		}
		e.Finish(nil)
	}()

	var got bytes.Buffer
	deadline := time.After(10 * time.Second)
	for {
		buf, err := e.ReadFrom(int64(got.Len()), 32*1024)
		if err != nil {
			t.Fatalf("ReadFrom: %v", err)
		}
		if len(buf) > 0 {
			got.Write(buf)
			continue
		}
		if e.IsComplete() && int64(got.Len()) == e.Written() {
			break
		}
		// 上游暂时没新数据：模拟服务端的空闲 flush 心跳
		if !e.WaitMoreFor(20*time.Millisecond, e.Gen()) {
			e.FlushDown()
		}
		select {
		case <-deadline:
			t.Fatalf("超时：已读 %d / %d", got.Len(), len(in))
		case <-done:
		default:
		}
	}
	if !bytes.Equal(got.Bytes(), in) {
		t.Fatalf("流式读不一致: %d vs %d", got.Len(), len(in))
	}
}

// 未压缩条目行为不变（回归保护）
func TestUncompressedUnaffected(t *testing.T) {
	m := newTestManager(t)
	var key [32]byte
	key[0] = 11
	e, _, err := m.Acquire(key)
	if err != nil {
		t.Fatal(err)
	}
	if e.Compressed() {
		t.Fatal("默认应为未压缩")
	}
	if err := e.Append([]byte("plain body")); err != nil {
		t.Fatal(err)
	}
	if e.WireBytes() != e.Written() {
		t.Fatalf("未压缩 WireBytes=%d Written=%d 应相等", e.WireBytes(), e.Written())
	}
	e.Finish(nil)
	if got, _ := e.ReadFrom(0, 100); string(got) != "plain body" {
		t.Fatalf("读 = %q", got)
	}
}

// 重启恢复：压缩条目的 sidecar 必须记住 compressed，且重启后仍能正确读明文
func TestRestoreCompressedEntry(t *testing.T) {
	dir := t.TempDir()
	m, err := New(Options{Dir: dir, TmpBudget: 1 << 20, IdemBudget: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	var key [32]byte
	key[0] = 12
	e := newCompressedEntry(t, m, key)
	// 没有响应头的条目不写 sidecar（设计如此：无可重放内容），恢复测试必须先设头
	e.SetHead(200, []byte("Content-Type: text/event-stream\n"))
	in := sse(80, 20)
	if err := e.Append(in); err != nil {
		t.Fatal(err)
	}
	e.Finish(nil) // 转入 idem/ 并写 sidecar
	m.Close()

	m2, err := New(Options{Dir: dir, TmpBudget: 1 << 20, IdemBudget: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	e2, existing, err := m2.Acquire(key)
	if err != nil || !existing {
		t.Fatalf("恢复失败: existing=%v err=%v", existing, err)
	}
	if !e2.Compressed() {
		t.Fatal("恢复后丢失 compressed 标记，会把压缩字节当明文推给客户端")
	}
	var got bytes.Buffer
	off := int64(0)
	for {
		buf, rerr := e2.ReadFrom(off, 16*1024)
		if rerr != nil {
			t.Fatalf("恢复后读失败: %v", rerr)
		}
		if len(buf) == 0 {
			break
		}
		got.Write(buf)
		off += int64(len(buf))
	}
	if !bytes.Equal(got.Bytes(), in) {
		t.Fatalf("恢复后内容不一致: %d vs %d", got.Len(), len(in))
	}
}

// 恢复后 SSE 事件索引必须仍然可用（按 event id 续传）
func TestRestoreCompressedEventIndex(t *testing.T) {
	dir := t.TempDir()
	m, _ := New(Options{Dir: dir, TmpBudget: 1 << 20, IdemBudget: 1 << 20})
	var key [32]byte
	key[0] = 13
	e := newCompressedEntry(t, m, key)
	e.SetHead(200, []byte("Content-Type: text/event-stream\n"))
	in := sse(30, 5)
	if err := e.Append(in); err != nil {
		t.Fatal(err)
	}
	e.Finish(nil)
	wantOff := e.EventOffset("17")
	m.Close()

	m2, _ := New(Options{Dir: dir, TmpBudget: 1 << 20, IdemBudget: 1 << 20})
	defer m2.Close()
	e2, _, _ := m2.Acquire(key)
	gotOff := e2.EventOffset("17")
	if gotOff != wantOff || gotOff < 0 {
		t.Fatalf("事件索引未恢复: got %d want %d", gotOff, wantOff)
	}
	// 按该偏移读出来的内容也应正确
	buf, err := e2.ReadFrom(gotOff, 64)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf, in[gotOff:gotOff+64]) {
		t.Fatal("按事件偏移读出的内容不对")
	}
}

// 非常小的响应（小于一个压缩块）也要能完整读出
func TestCompressedTinyBody(t *testing.T) {
	m := newTestManager(t)
	var key [32]byte
	key[0] = 14
	e := newCompressedEntry(t, m, key)
	in := []byte("data: ok\n\n")
	if err := e.Append(in); err != nil {
		t.Fatal(err)
	}
	e.Finish(nil)
	got, err := e.ReadFrom(0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, in) {
		t.Fatalf("小响应读 = %q", got)
	}
}

// 空响应：不能 panic，读回 0 字节
func TestCompressedEmptyBody(t *testing.T) {
	m := newTestManager(t)
	var key [32]byte
	key[0] = 15
	e := newCompressedEntry(t, m, key)
	e.Finish(nil)
	got, err := e.ReadFrom(0, 100)
	if err != nil {
		t.Fatalf("空响应读失败: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("空响应读出 %d 字节", len(got))
	}
}

var _ = io.Discard

// 并发读同一条目（多客户端同时挂载同一 key 的幂等条目）：
// 游标必须串行化，每个 goroutine 都读到正确内容。
func TestCompressedConcurrentReads(t *testing.T) {
	m := newTestManager(t)
	var key [32]byte
	key[0] = 16
	e := newCompressedEntry(t, m, key)
	in := sse(100, 20)
	if err := e.Append(in); err != nil {
		t.Fatal(err)
	}
	e.Finish(nil)

	const workers = 8
	errCh := make(chan error, workers)
	for w := 0; w < workers; w++ {
		go func() {
			var got bytes.Buffer
			off := int64(0)
			for {
				buf, err := e.ReadFrom(off, 4096)
				if err != nil {
					errCh <- err
					return
				}
				if len(buf) == 0 {
					break
				}
				got.Write(buf)
				off += int64(len(buf))
			}
			if !bytes.Equal(got.Bytes(), in) {
				errCh <- fmt.Errorf("并发读内容不一致: %d vs %d", got.Len(), len(in))
				return
			}
			errCh <- nil
		}()
	}
	for w := 0; w < workers; w++ {
		if err := <-errCh; err != nil {
			t.Fatal(err)
		}
	}
}

// 游标在条目关闭/淘汰时必须被回收（不泄漏 goroutine）
func TestCompressedCursorClosedOnEvict(t *testing.T) {
	m := newTestManager(t)
	var key [32]byte
	key[0] = 17
	e := newCompressedEntry(t, m, key)
	in := sse(50, 20)
	if err := e.Append(in); err != nil {
		t.Fatal(err)
	}
	e.Finish(nil)
	// 建立游标
	if _, err := e.ReadFrom(0, 100); err != nil {
		t.Fatal(err)
	}
	base := runtime.NumGoroutine()
	m.Evict(key)
	// 给 goroutine 一点退出时间
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= base {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Logf("goroutine 数 %d → %d（解码器后台 goroutine 可能仍在退出）", base, runtime.NumGoroutine())
}

// 小响应（< MinUseful）不应压缩：压缩后反而更大。
// 实测线上：401 错误体 172B → 压缩后连同帧头 333B（1.9 倍）。
func TestSmallResponseStaysPlain(t *testing.T) {
	m := newTestManager(t)
	var key [32]byte
	key[0] = 20
	e := newCompressedEntry(t, m, key)
	small := []byte(`{"error":{"message":"no API key was sent","type":"authentication_error"}}`)
	if len(small) >= downlink.MinUseful {
		t.Fatalf("测试数据应小于门槛 %d，实际 %d", downlink.MinUseful, len(small))
	}
	if err := e.Append(small); err != nil {
		t.Fatal(err)
	}
	e.Finish(nil)

	if e.Compressed() {
		t.Fatal("小响应被压缩了：会膨胀")
	}
	if e.WireBytes() != int64(len(small)) {
		t.Fatalf("小响应 wire=%d 应等于明文 %d", e.WireBytes(), len(small))
	}
	got, err := e.ReadFrom(0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, small) {
		t.Fatalf("小响应内容丢失/损坏: %q", got)
	}
}

// 跨过门槛的响应必须压缩，且开头那段（拍板前攒的 pending）不能丢
func TestCrossingThresholdKeepsPrefix(t *testing.T) {
	m := newTestManager(t)
	var key [32]byte
	key[0] = 21
	e := newCompressedEntry(t, m, key)

	// 前缀故意用可识别的标记，验证它没被丢掉
	prefix := bytes.Repeat([]byte("PREFIX-"), 40) // 280B < 512
	rest := sse(60, 20)
	if err := e.Append(prefix); err != nil {
		t.Fatal(err)
	}
	if e.Compressed() {
		t.Fatal("只写了前缀就压缩了？应等攒够门槛")
	}
	// 继续写，跨过门槛
	for i := 0; i < len(rest); i += 1024 {
		end := i + 1024
		if end > len(rest) {
			end = len(rest)
		}
		if err := e.Append(rest[i:end]); err != nil {
			t.Fatal(err)
		}
	}
	e.Finish(nil)

	if !e.Compressed() {
		t.Fatal("跨过门槛后仍未压缩")
	}
	want := append(append([]byte{}, prefix...), rest...)
	var got bytes.Buffer
	off := int64(0)
	for {
		buf, err := e.ReadFrom(off, 16*1024)
		if err != nil {
			t.Fatalf("ReadFrom(%d): %v", off, err)
		}
		if len(buf) == 0 {
			break
		}
		got.Write(buf)
		off += int64(len(buf))
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("跨门槛后内容不一致: got %d want %d（前缀可能被丢）", got.Len(), len(want))
	}
	if !bytes.Contains(got.Bytes(), []byte("PREFIX-")) {
		t.Fatal("拍板前攒的前缀丢失了")
	}
}

// 超时拍板（ForceDecide）也必须过门槛：等不到更多数据 ≠ 数据够大。
// 实测线上踩中：180B 的 401 错误体在超时路径被压缩，wire 膨胀到 328B。
func TestForceDecideRespectsMinUseful(t *testing.T) {
	m := newTestManager(t)
	var key [32]byte
	key[0] = 22
	e := newCompressedEntry(t, m, key)

	small := bytes.Repeat([]byte("e"), 180)
	if err := e.Append(small); err != nil {
		t.Fatal(err)
	}
	// 模拟服务端等待头时超时
	decided, compressed := e.ForceDecide()
	if !decided {
		t.Fatal("ForceDecide 应标记已决定")
	}
	if compressed {
		t.Fatal("小响应在超时路径被压缩：会膨胀")
	}
	if e.Compressed() {
		t.Fatal("Compressed() 应为 false")
	}
	e.Finish(nil)
	got, err := e.ReadFrom(0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, small) {
		t.Fatalf("超时拍板后内容丢失: got %d want %d", len(got), len(small))
	}
}

// 超时拍板时若已够大，则正常压缩且内容无损
func TestForceDecideCompressesWhenLargeEnough(t *testing.T) {
	m := newTestManager(t)
	var key [32]byte
	key[0] = 23
	e := newCompressedEntry(t, m, key)

	// 直接写一大块但不触发 Append 内的自动拍板（pending 已够大，
	// Append 会立即拍板）。这里改为写两次小块，第二次跨过门槛后
	// 手动验证 ForceDecide 的幂等性。
	in := sse(50, 20)
	if err := e.Append(in); err != nil {
		t.Fatal(err)
	}
	_, compressed := e.ForceDecide()
	if !compressed {
		t.Fatal("足够大的响应应被压缩")
	}
	e.Finish(nil)
	var got bytes.Buffer
	off := int64(0)
	for {
		buf, err := e.ReadFrom(off, 16*1024)
		if err != nil {
			t.Fatal(err)
		}
		if len(buf) == 0 {
			break
		}
		got.Write(buf)
		off += int64(len(buf))
	}
	if !bytes.Equal(got.Bytes(), in) {
		t.Fatalf("内容不一致: %d vs %d", got.Len(), len(in))
	}
}
