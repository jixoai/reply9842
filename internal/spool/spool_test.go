package spool

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	m, err := New(Options{Dir: t.TempDir(), TmpBudget: 1 << 20, IdemBudget: 2 << 20})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

func TestAcquireAndAppend(t *testing.T) {
	m := newTestManager(t)
	var key [32]byte
	key[0] = 1

	e, existing, err := m.Acquire(key)
	if err != nil {
		t.Fatal(err)
	}
	if existing {
		t.Fatal("first acquire should be new")
	}
	if err := e.Append([]byte("hello ")); err != nil {
		t.Fatal(err)
	}
	if err := e.Append([]byte("world")); err != nil {
		t.Fatal(err)
	}
	if got := e.Written(); got != 11 {
		t.Fatalf("written = %d, want 11", got)
	}

	buf, err := e.ReadFrom(0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf) != "hello world" {
		t.Fatalf("read = %q", buf)
	}

	// 从中间读
	buf, err = e.ReadFrom(6, 100)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf) != "world" {
		t.Fatalf("read from 6 = %q", buf)
	}

	// 越界读返回空
	buf, _ = e.ReadFrom(100, 10)
	if len(buf) != 0 {
		t.Fatalf("read past end should be empty, got %d", len(buf))
	}
}

func TestAcquireExistingReuse(t *testing.T) {
	m := newTestManager(t)
	var key [32]byte
	key[0] = 2

	e1, _, _ := m.Acquire(key)
	e1.Append([]byte("data"))

	e2, existing, _ := m.Acquire(key)
	if !existing {
		t.Fatal("second acquire should report existing")
	}
	if e1 != e2 {
		t.Fatal("should return same entry")
	}
	if got := e2.Written(); got != 4 {
		t.Fatalf("written = %d", got)
	}
}

func TestDetachMovesToIdem(t *testing.T) {
	m := newTestManager(t)
	var key [32]byte
	key[0] = 3

	e, _, _ := m.Acquire(key)
	e.Append([]byte("0123456789"))

	tmpPath := e.path
	if filepath.Base(filepath.Dir(tmpPath)) != "tmp" {
		t.Fatalf("initially should be in tmp/, got %s", tmpPath)
	}

	if err := m.Detach(e); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	if e.State() != StateDetached {
		t.Fatalf("state = %v", e.State())
	}
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Fatalf("old tmp file should be gone: %v", err)
	}

	// 断线后仍能继续 Append（上游没停）
	if err := e.Append([]byte("abc")); err != nil {
		t.Fatalf("append after detach: %v", err)
	}
	if got := e.Written(); got != 13 {
		t.Fatalf("written = %d, want 13", got)
	}

	// 数据完整性
	buf, _ := e.ReadFrom(0, 100)
	if string(buf) != "0123456789abc" {
		t.Fatalf("data after rename = %q", buf)
	}

	s := m.Stats()
	if s.Detached != 1 || s.Active != 0 {
		t.Fatalf("stats = %+v", s)
	}
}

func TestEvictionRespectsBudget(t *testing.T) {
	dir := t.TempDir()
	m, _ := New(Options{Dir: dir, TmpBudget: 100, IdemBudget: 100})
	defer m.Close()

	// 造 5 个各 50 字节的条目，全部 detach 后可被淘汰
	var keys [][32]byte
	for i := 0; i < 5; i++ {
		var k [32]byte
		k[0] = byte(i + 10)
		e, _, _ := m.Acquire(k)
		e.Append(bytes.Repeat([]byte("x"), 50))
		e.Finish(nil)
		m.Detach(e)
		keys = append(keys, k)
	}

	s := m.Stats()
	if s.IdemBytes > s.IdemBudget {
		t.Fatalf("idem bytes %d exceeds budget %d", s.IdemBytes, s.IdemBudget)
	}
	if s.Entries >= 5 {
		t.Logf("note: entries=%d (budget %d bytes, each 50B)", s.Entries, s.IdemBudget)
	}
	t.Logf("stats: %+v", s)
}

func TestActiveNeverEvicted(t *testing.T) {
	dir := t.TempDir()
	m, _ := New(Options{Dir: dir, TmpBudget: 10, IdemBudget: 10})
	defer m.Close()

	var k [32]byte
	k[0] = 99
	e, _, _ := m.Acquire(k)
	e.Append(bytes.Repeat([]byte("y"), 100)) // 超预算但不该被淘汰

	buf, _ := e.ReadFrom(0, 200)
	if len(buf) != 100 {
		t.Fatalf("active entry data lost: got %d bytes", len(buf))
	}
}

func TestWaitMoreWakesOnAppend(t *testing.T) {
	m := newTestManager(t)
	var k [32]byte
	k[0] = 7
	e, _, _ := m.Acquire(k)

	gen := e.Gen()
	done := make(chan struct{})
	go func() {
		e.WaitMore(nil, gen)
		close(done)
	}()

	e.Append([]byte("x"))

	select {
	case <-done:
	case <-timeoutAfter(1):
		t.Fatal("WaitMore did not wake on Append")
	}
}

func TestEventTracking(t *testing.T) {
	m := newTestManager(t)
	var k [32]byte
	k[0] = 8
	e, _, _ := m.Acquire(k)

	e.Append([]byte("event: message\nid: 42\ndata: hi\n\n"))
	e.Append([]byte("event: message\nid: 43\ndata: yo\n\n"))

	if off := e.EventOffset("42"); off < 0 {
		t.Fatal("event 42 not found")
	} else if off != 25 {
		t.Logf("event 42 offset = %d", off)
	}
	if off := e.EventOffset("43"); off < 0 {
		t.Fatal("event 43 not found")
	}
	if off := e.EventOffset("nope"); off >= 0 {
		t.Fatal("missing event should return -1")
	}
}

// timeoutAfter 轻量计时器
func timeoutAfter(sec int) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		time.Sleep(time.Duration(sec) * time.Second)
		close(ch)
	}()
	return ch
}

// ---- 回归测试：2026-11-13 线上 bug ----

// 线上现象：/var/lib/reply9842/spool/idem 为空，三个已完成响应全在 tmp/。
// 根因：迁移只由 Detach（客户端断开）触发，正常请求从不 Detach。
func TestCompletePromotesToIdem(t *testing.T) {
	dir := t.TempDir()
	m, _ := New(Options{Dir: dir, TmpBudget: 1 << 20, IdemBudget: 1 << 20})
	defer m.Close()

	var k [32]byte
	k[0] = 0xAA
	e, _, _ := m.Acquire(k)
	e.Append([]byte("response-body"))
	e.Finish(nil) // 上游结束，客户端仍在线上

	if e.State() != StateComplete {
		t.Fatalf("state = %v, want complete", e.State())
	}
	if tmp, _ := os.ReadDir(filepath.Join(dir, "tmp")); len(tmp) != 0 {
		t.Errorf("tmp/ 仍有 %d 个文件，完成的条目必须迁出", len(tmp))
	}
	idem, _ := os.ReadDir(filepath.Join(dir, "idem"))
	if len(idem) != 1 {
		t.Fatalf("idem/ 有 %d 个文件，want 1", len(idem))
	}
	if got, _ := e.ReadFrom(0, 100); string(got) != "response-body" {
		t.Errorf("迁移后数据丢失: %q", got)
	}

	// 必须能被同 key 复用（这正是「命中幂等」的定义）
	e2, existing, err := m.Acquire(k)
	if err != nil || !existing || e2 != e {
		t.Fatalf("同 key 未复用: existing=%v err=%v", existing, err)
	}
}

// 线上现象：重启后同 key 命中不了（日志 written=1588 的复用只在单进程内成立）。
// 根因：restoreIdem 只用文件名（前 16 字节 hex）重建 key，尾部补零 ≠ 真实 sha256。
func TestRestoreIdemPreservesFullKeyAndHead(t *testing.T) {
	dir := t.TempDir()
	m1, _ := New(Options{Dir: dir, TmpBudget: 1 << 20, IdemBudget: 1 << 20})

	var k [32]byte
	for i := range k {
		k[i] = byte(i + 1) // 全 32 字节非零，模拟真实哈希
	}
	e, _, _ := m1.Acquire(k)
	e.SetHead(200, []byte("Content-Type: text/event-stream\n"))
	e.Append([]byte("data: hi\n\n"))
	e.Finish(nil)
	m1.Close()

	m2, _ := New(Options{Dir: dir, TmpBudget: 1 << 20, IdemBudget: 1 << 20})
	defer m2.Close()

	e2, existing, err := m2.Acquire(k)
	if err != nil {
		t.Fatal(err)
	}
	if !existing {
		t.Fatal("重启后同 key 未命中：恢复的 key 与真实 key 不等")
	}
	if got, _ := e2.ReadFrom(0, 100); string(got) != "data: hi\n\n" {
		t.Errorf("恢复后 body = %q", got)
	}
	if st, hdrs, ok := e2.Head(); !ok || st != 200 || len(hdrs) == 0 {
		t.Errorf("恢复后响应头丢失: ok=%v status=%d hdrs=%q", ok, st, hdrs)
	}
	if !e2.IsComplete() {
		t.Error("恢复的条目应为 complete")
	}
}

// 线上现象：tmp/ 里既有活跃请求也有完成请求，预算形同虚设。
// 根因：Append 不记账 + Detach 用 e.Written() 反减，导致 tmpBytes 变负。
func TestBudgetAccountingNoDrift(t *testing.T) {
	m := newTestManager(t)
	var k [32]byte
	k[0] = 1

	e, _, _ := m.Acquire(k)
	e.Append(make([]byte, 1000))

	if s := m.Stats(); s.TmpBytes != 1000 {
		t.Errorf("Append 后 TmpBytes=%d, want 1000", s.TmpBytes)
	}

	m.Detach(e) // 未完成就断线
	if s := m.Stats(); s.TmpBytes != 0 || s.IdemBytes != 1000 {
		t.Errorf("Detach 后 tmp=%d idem=%d, want 0/1000", s.TmpBytes, s.IdemBytes)
	}

	// 再 Append：仍应计入 idem 区
	e.Append(make([]byte, 500))
	if s := m.Stats(); s.TmpBytes != 0 || s.IdemBytes != 1500 {
		t.Errorf("Detach 后 Append: tmp=%d idem=%d, want 0/1500", s.TmpBytes, s.IdemBytes)
	}
}

// 完成后再 Detach 不能把状态降级为 detached（否则会被误判为「上游仍在收」）
func TestDetachAfterCompleteKeepsComplete(t *testing.T) {
	m := newTestManager(t)
	var k [32]byte
	k[0] = 42

	e, _, _ := m.Acquire(k)
	e.Append([]byte("abc"))
	e.Finish(nil)
	if err := m.Detach(e); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	if e.State() != StateComplete {
		t.Fatalf("state = %v, want complete（不允许降级）", e.State())
	}
	if err := m.Detach(e); err != nil {
		t.Fatalf("second Detach: %v", err)
	}
}

// 没有 sidecar 的 .dat 是不可用残骸（旧版本遗留），恢复时应清理
func TestRestoreDropsLegacyOrphanDat(t *testing.T) {
	dir := t.TempDir()
	m, _ := New(Options{Dir: dir, TmpBudget: 1 << 20, IdemBudget: 1 << 20})

	var k [32]byte
	k[0] = 9
	e, _, _ := m.Acquire(k)
	e.SetHead(200, []byte("X: y\n"))
	e.Append([]byte("x"))
	e.Finish(nil)
	m.Close()

	// 删掉 sidecar，模拟旧版本留下的孤儿
	metaPath := filepath.Join(dir, "idem", fmt.Sprintf("%x.meta", k[:16]))
	if err := os.Remove(metaPath); err != nil {
		t.Fatalf("remove sidecar: %v", err)
	}

	m2, _ := New(Options{Dir: dir, TmpBudget: 1 << 20, IdemBudget: 1 << 20})
	defer m2.Close()

	if _, existing, _ := m2.Acquire(k); existing {
		t.Error("孤儿 .dat 不应被恢复成可用条目")
	}
	if ents, _ := os.ReadDir(filepath.Join(dir, "idem")); len(ents) != 0 {
		t.Errorf("孤儿文件未被清理: %v", ents)
	}
}

// 线上现象：进程重启后 tmp/ 里的旧文件永久滞留（活跃请求不跨进程存活）。
// 修复：启动时清空 tmp/，但绝不能碰 idem/。
func TestStartupResetsTmpKeepsIdem(t *testing.T) {
	dir := t.TempDir()
	m1, _ := New(Options{Dir: dir, TmpBudget: 1 << 20, IdemBudget: 1 << 20})

	// 一个已完成的条目 → idem
	var done [32]byte
	for i := range done {
		done[i] = 0x11
	}
	ed, _, _ := m1.Acquire(done)
	ed.SetHead(200, []byte("A: b\n"))
	ed.Append([]byte("keep-me"))
	ed.Finish(nil)

	// 一个仍在进行中的条目 → 留在 tmp
	var live [32]byte
	live[0] = 0x22
	el, _, _ := m1.Acquire(live)
	el.Append([]byte("orphan"))
	m1.Close() // 模拟进程被杀：live 没走 Finish

	m2, _ := New(Options{Dir: dir, TmpBudget: 1 << 20, IdemBudget: 1 << 20})
	defer m2.Close()

	if ents, _ := os.ReadDir(filepath.Join(dir, "tmp")); len(ents) != 0 {
		t.Errorf("启动后 tmp/ 应为空，实际 %d 个文件", len(ents))
	}
	if _, existing, _ := m2.Acquire(live); existing {
		t.Error("孤儿活跃条目不应被恢复")
	}
	e2, existing, _ := m2.Acquire(done)
	if !existing {
		t.Fatal("idem 条目必须在重启后保留")
	}
	if got, _ := e2.ReadFrom(0, 100); string(got) != "keep-me" {
		t.Errorf("idem 数据丢失: %q", got)
	}
}

// 上游失败产生的空条目必须可标记为不可复用，并从 spool 移除
func TestUncacheableEviction(t *testing.T) {
	m := newTestManager(t)
	var k [32]byte
	k[0] = 0xAA

	e, _, _ := m.Acquire(k)
	// 模拟上游失败：0 字节 + error
	e.Finish(errTestUpstream)
	e.MarkUncacheable()

	if !e.Uncacheable() {
		t.Fatal("should be marked uncacheable")
	}
	if !e.IsComplete() {
		t.Fatal("should be complete")
	}

	// 回收后再次 Acquire 应该得到新条目（existing=false）
	m.Evict(k)
	_, existing, err := m.Acquire(k)
	if err != nil {
		t.Fatal(err)
	}
	if existing {
		t.Fatal("evicted entry should not be reused: 失败的响应被固化会导致客户端永远拿到空结果")
	}
}

var errTestUpstream = errStr("upstream unreachable")

type errStr string

func (e errStr) Error() string { return string(e) }

// TestReleaseCursorFreesDecoder 读完必须把解码游标还回去。
//
// 每个游标背后是一个 zstd.Decoder，history 环形缓冲按帧窗口分配（实测
// ~4MB）。2026-11-14 生产环境跑 5 小时 RSS 993MB，heap profile 里 95% 在
// zstd.(*history).ensureBlockRing —— 就是「游标跟着 entry 一直缓存」造成的。
// 这条测试锁住「读完释放」这个契约，并用 HeapInuse 兜住规模。
func TestReleaseCursorFreesDecoder(t *testing.T) {
	m, err := New(Options{Dir: t.TempDir(), TmpBudget: 64 << 20, IdemBudget: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	body := bytes.Repeat([]byte("hello-spool-"), 40000) // ~480KB
	var key [32]byte
	key[0] = 1

	e, _, err := m.Acquire(key)
	if err != nil {
		t.Fatal(err)
	}
	e.SetHead(200, nil)
	if err := e.Append(body); err != nil {
		t.Fatal(err)
	}
	e.Finish(nil)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	for i := 0; i < 40; i++ {
		if _, err := e.ReadFrom(0, 64<<10); err != nil {
			t.Fatalf("ReadFrom: %v", err)
		}
		e.ReleaseCursor()
		if e.cur != nil {
			t.Fatal("ReleaseCursor 之后仍然缓存着解码游标")
		}
	}

	runtime.GC()
	runtime.ReadMemStats(&after)
	grown := int64(after.HeapInuse) - int64(before.HeapInuse)
	// 40 次「建解码器 + 释放」不应该留下 40×4MB；留 32MB 余量吸收噪声。
	if grown > 32<<20 {
		t.Fatalf("循环后 HeapInuse 增长 %d bytes（>32MB），解码器没被释放", grown)
	}
}
