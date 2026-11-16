package spool

import "testing"

// 2026-11-16：重启恢复曾经对每个 idem 条目全量读盘并解压来重建 SSE 事件
// 索引，2GB 的 spool 让端口 90 秒才起来。事件索引只服务「按 event id 续传」，
// 绝大多数恢复条目永远不会被续传，所以必须惰性化：
//   - New() 之后不得有索引（没扫过盘）
//   - 第一次 EventOffset 时才补齐，且结果与扫盘建索引一致
func TestRestoreIdemDefersEventScan(t *testing.T) {
	dir := t.TempDir()
	m, err := New(Options{Dir: dir, TmpBudget: 1 << 20, IdemBudget: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	var key [32]byte
	key[0] = 21
	e, existing, err := m.Acquire(key)
	if err != nil || existing {
		t.Fatalf("acquire: %v existing=%v", err, existing)
	}
	e.SetHead(200, []byte("Content-Type: text/event-stream\n"))
	if err := e.Append(sse(10, 3)); err != nil {
		t.Fatal(err)
	}
	e.Finish(nil)
	wantOff := e.EventOffset("7")
	if wantOff < 0 {
		t.Fatal("活跃条目的事件索引应当已经建好")
	}
	m.Close()

	m2, err := New(Options{Dir: dir, TmpBudget: 1 << 20, IdemBudget: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()

	e2, _, err := m2.Acquire(key)
	if err != nil {
		t.Fatal(err)
	}
	e2.mu.Lock()
	scanned := len(e2.events)
	e2.mu.Unlock()
	if scanned != 0 {
		t.Fatalf("恢复时不应扫盘建索引，实际有 %d 条事件标记", scanned)
	}

	if got := e2.EventOffset("7"); got != wantOff {
		t.Fatalf("惰性重建的偏移 = %d，想要 %d", got, wantOff)
	}
	e2.mu.Lock()
	n := len(e2.events)
	e2.mu.Unlock()
	if n == 0 {
		t.Fatal("首次按 event id 续传时应当补齐索引")
	}
}
