package spool

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// seedIdem 造 n 个「已完成并进幂等区」的条目。
func seedIdem(t *testing.T, m *Manager, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		var k [32]byte
		k[0] = byte(i)
		k[1] = byte(i >> 8)
		k[2] = byte(i >> 16)
		e, existing, err := m.Acquire(k)
		if err != nil || existing {
			t.Fatalf("acquire %d: %v existing=%v", i, err, existing)
		}
		e.SetHead(200, []byte("Content-Type: text/event-stream\n"))
		if err := e.Append([]byte(fmt.Sprintf("id: %d\n\n", i))); err != nil {
			t.Fatal(err)
		}
		e.Finish(nil)
	}
}

func idemCount(t *testing.T, dir string) int {
	t.Helper()
	des, err := os.ReadDir(filepath.Join(dir, "idem"))
	if err != nil {
		return 0
	}
	n := 0
	for _, de := range des {
		if filepath.Ext(de.Name()) == ".dat" {
			n++
		}
	}
	return n
}

// 启动阶段不得扫描幂等区：New 之后内存表必须是空的（否则说明又把两万条
// 全装进来了）。这是 9.5s 重启停顿的根因防线。
func TestNewDoesNotScanIdem(t *testing.T) {
	dir := t.TempDir()
	m, _ := New(Options{Dir: dir, TmpBudget: 1 << 20, IdemBudget: 8 << 20})
	seedIdem(t, m, 300)
	m.Close()
	if got := idemCount(t, dir); got != 300 {
		t.Fatalf("种子里条目数 = %d，想要 300", got)
	}

	start := time.Now()
	m2, err := New(Options{Dir: dir, TmpBudget: 1 << 20, IdemBudget: 8 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	took := time.Since(start)

	m2.mu.Lock()
	n := len(m2.entries)
	m2.mu.Unlock()
	if n != 0 {
		t.Fatalf("New 装入了 %d 个条目：启动不应扫描幂等区", n)
	}
	// 300 条只是结构断言；时间上给一个宽松上限，防止将来又把 IO 塞回启动路径。
	if took > 100*time.Millisecond {
		t.Fatalf("New 用了 %s，启动路径里又出现了逐条目 IO", took)
	}
}

// 冷查：重启后按 key 直接命中幂等区成品，不需要启动扫描。
func TestColdLookupHydratesOnDemand(t *testing.T) {
	dir := t.TempDir()
	m, _ := New(Options{Dir: dir, TmpBudget: 1 << 20, IdemBudget: 8 << 20})
	var k [32]byte
	k[0] = 0xAB
	e, _, _ := m.Acquire(k)
	e.SetHead(201, []byte("X-Test: yes\n"))
	body := []byte("hello cold world")
	if err := e.Append(body); err != nil {
		t.Fatal(err)
	}
	e.Finish(nil)
	m.Close()

	m2, _ := New(Options{Dir: dir, TmpBudget: 1 << 20, IdemBudget: 8 << 20})
	defer m2.Close()

	got, existing, err := m2.Acquire(k)
	if err != nil || !existing {
		t.Fatalf("冷查未命中: err=%v existing=%v", err, existing)
	}
	got.mu.Lock()
	opened := got.file != nil
	st, hdrs, hasHead := got.headStatus, got.headHeaders, got.headSet
	got.mu.Unlock()
	if opened {
		t.Fatal("水合时不应打开数据文件（会重新变成每条约一个 fd）")
	}
	if !hasHead || st != 201 || !bytes.Contains(hdrs, []byte("X-Test")) {
		t.Fatalf("响应头未恢复: status=%d headers=%q", st, hdrs)
	}

	buf, err := got.ReadFrom(0, 64)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf, body) {
		t.Fatalf("读到 %q，想要 %q", buf, body)
	}
	got.mu.Lock()
	opened = got.file != nil
	got.mu.Unlock()
	if !opened {
		t.Fatal("首次读取应当按需打开数据文件")
	}
}

// 补水：把幂等区装进热表但不占 fd；重复 Acquire 不再走文件系统。
func TestHydrateFillsMapWithoutFDs(t *testing.T) {
	dir := t.TempDir()
	m, _ := New(Options{Dir: dir, TmpBudget: 1 << 20, IdemBudget: 8 << 20})
	seedIdem(t, m, 200)
	m.Close()

	m2, _ := New(Options{Dir: dir, TmpBudget: 1 << 20, IdemBudget: 8 << 20})
	defer m2.Close()
	if n := m2.Hydrate(context.Background(), 50, 0); n != 200 {
		t.Fatalf("补水条目数 = %d，想要 200", n)
	}

	m2.mu.Lock()
	n := len(m2.entries)
	opened := 0
	for _, e := range m2.entries {
		e.mu.Lock()
		if e.file != nil {
			opened++
		}
		e.mu.Unlock()
	}
	m2.mu.Unlock()
	if n != 200 {
		t.Fatalf("热表条目数 = %d，想要 200", n)
	}
	if opened != 0 {
		t.Fatalf("补水不应打开任何数据文件，实际开了 %d 个", opened)
	}
}

// 补水必须可取消：进程退出/停机不能卡在扫描里。
func TestHydrateHonorsCancel(t *testing.T) {
	dir := t.TempDir()
	m, _ := New(Options{Dir: dir, TmpBudget: 1 << 20, IdemBudget: 8 << 20})
	seedIdem(t, m, 50)
	m.Close()

	m2, _ := New(Options{Dir: dir, TmpBudget: 1 << 20, IdemBudget: 8 << 20})
	defer m2.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if n := m2.Hydrate(ctx, 10, 0); n != 0 {
		t.Fatalf("ctx 已取消时仍补水了 %d 条", n)
	}
}

// 预算收敛：补水结束后超出预算的部分必须被淘汰（磁盘不会无限涨）。
func TestHydrateConvergesBudget(t *testing.T) {
	dir := t.TempDir()
	m, _ := New(Options{Dir: dir, TmpBudget: 1 << 20, IdemBudget: 8 << 20})
	for i := 0; i < 20; i++ {
		var k [32]byte
		k[0] = byte(i)
		e, _, _ := m.Acquire(k)
		e.SetHead(200, []byte("X: y\n"))
		if err := e.Append(bytes.Repeat([]byte("x"), 4096)); err != nil {
			t.Fatal(err)
		}
		e.Finish(nil)
	}
	m.Close()

	// 预算只留得下 ~5 条
	m2, _ := New(Options{Dir: dir, TmpBudget: 1 << 20, IdemBudget: 20 << 10})
	defer m2.Close()
	m2.Hydrate(context.Background(), 10, 0)

	m2.mu.Lock()
	live := len(m2.entries)
	m2.mu.Unlock()
	if live > 8 {
		t.Fatalf("预算收敛后仍有 %d 条（预算只够 ~5 条）", live)
	}
	if got := idemCount(t, dir); got != live {
		t.Fatalf("磁盘条目 %d 与内存表 %d 不一致", got, live)
	}
}
