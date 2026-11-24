package transport

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeConn 造一条可用的假连接：能 Read/Write/Close，且记录来自哪个线路。
func fakeConn(label string, closed *int32) *Conn {
	var once sync.Once
	return &Conn{
		kind:  KindTCP,
		proto: ALPNv1,
		send:  func(b []byte) error { return nil },
		recv:  func() ([]byte, error) { return nil, errors.New("eof") },
		closeFn: func() error {
			if closed != nil {
				once.Do(func() { atomic.AddInt32(closed, 1) })
			}
			return nil
		},
	}
}

// instrumentPool 给某个池注入计数型 dialFn，统计它为哪条线路建了几条连接。
func instrumentPool(ps *PoolSet, id string, counter *int32, closed *int32) {
	ps.mu.Lock()
	p := ps.pools[id]
	ps.mu.Unlock()
	if p == nil {
		return
	}
	p.dialFn = func(ctx context.Context) (*Conn, error) {
		atomic.AddInt32(counter, 1)
		return fakeConn(id, closed), nil
	}
}

// 两条线路各自建池：连接不能串用（A 的连接不能从 B 的池里取出来）。
func TestPoolSetIsolatesRoutes(t *testing.T) {
	var aDials, bDials, aClosed, bClosed int32

	ps := BuildPools([]DialerSpec{
		{ID: "A", Addr: "a:28443", Mode: KindTCP},
		{ID: "B", Addr: "b:28443", Mode: KindTCP},
	}, 0)
	defer ps.Close()

	instrumentPool(ps, "A", &aDials, &aClosed)
	instrumentPool(ps, "B", &bDials, &bClosed)

	ctx := context.Background()

	// 从 A 取一条
	cA, err := ps.Get(ctx, "A")
	if err != nil {
		t.Fatalf("Get A: %v", err)
	}
	// A 的 dial 计数应为 1
	if n := atomic.LoadInt32(&aDials); n != 1 {
		t.Fatalf("A 的 dial 次数 %d，期望 1", n)
	}

	// 从 B 取一条：B 的计数应独立变成 1，A 不变
	cB, err := ps.Get(ctx, "B")
	if err != nil {
		t.Fatalf("Get B: %v", err)
	}
	if n := atomic.LoadInt32(&bDials); n != 1 {
		t.Fatalf("B 的 dial 次数 %d，期望 1", n)
	}
	if n := atomic.LoadInt32(&aDials); n != 1 {
		t.Fatalf("取 B 不应触发 A 的 dial，A 的计数变成了 %d", n)
	}

	// 归还后各自命中自己的池
	ps.Put("A", cA)
	ps.Put("B", cB)

	cA2, err := ps.Get(ctx, "A")
	if err != nil {
		t.Fatalf("Get A(2): %v", err)
	}
	if n := atomic.LoadInt32(&aDials); n != 1 {
		t.Fatalf("A 复用时不该新建连接，dial 次数变成了 %d", n)
	}
	if cA2 != cA {
		t.Fatal("A 复用的应是同一条连接")
	}

	cB2, err := ps.Get(ctx, "B")
	if err != nil {
		t.Fatalf("Get B(2): %v", err)
	}
	if n := atomic.LoadInt32(&bDials); n != 1 {
		t.Fatalf("B 复用时不该新建连接，dial 次数变成了 %d", n)
	}
	if cB2 != cB {
		t.Fatal("B 复用的应是同一条连接（不是 A 的）")
	}
	ps.Put("A", cA2)
	ps.Put("B", cB2)
}

// 一条线路的 dial 持续失败时，另一条线路仍能正常取到连接。
func TestPoolSetOneRouteFailingDoesNotBlockOther(t *testing.T) {
	ps := BuildPools([]DialerSpec{
		{ID: "bad", Addr: "127.0.0.1:1", Mode: KindTCP},
		{ID: "good", Addr: "127.0.0.1:2", Mode: KindTCP},
	}, 0)
	defer ps.Close()

	ps.mu.Lock()
	ps.pools["bad"].dialFn = func(ctx context.Context) (*Conn, error) {
		return nil, errors.New("模拟线路不可用")
	}
	var goodDials int32
	ps.pools["good"].dialFn = func(ctx context.Context) (*Conn, error) {
		atomic.AddInt32(&goodDials, 1)
		return fakeConn("good", nil), nil
	}
	ps.mu.Unlock()

	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, err := ps.Get(ctx, "bad"); err == nil {
			t.Fatalf("第 %d 次：bad 线路应当报错", i)
		}
	}
	// 坏线路的失败不应影响好线路
	c, err := ps.Get(ctx, "good")
	if err != nil {
		t.Fatalf("good 线路应可用，实际: %v", err)
	}
	if atomic.LoadInt32(&goodDials) != 1 {
		t.Fatal("good 线路应只 dial 一次")
	}
	ps.Drop(c)
}

// 不存在的线路：返回 ErrNoPool，而不是 panic 或静默成功。
func TestPoolSetUnknownRoute(t *testing.T) {
	ps := BuildPools([]DialerSpec{{ID: "A", Addr: "a:28443", Mode: KindTCP}}, 0)
	defer ps.Close()

	_, err := ps.Get(context.Background(), "不存在的线路")
	if err == nil {
		t.Fatal("未知线路应当报错")
	}
	if !errors.Is(err, ErrNoPool) {
		t.Fatalf("错误应为 ErrNoPool 族，实际: %v", err)
	}
	// 错误信息里要有线路名（运维排障用）
	if want := "不存在的线路"; !contains(err.Error(), want) {
		t.Fatalf("错误信息应含线路名 %q，实际: %s", want, err)
	}
}

// 关闭后所有 Get 都必须失败（不能返回 nil 连接）。
func TestPoolSetClosed(t *testing.T) {
	ps := BuildPools([]DialerSpec{{ID: "A", Addr: "a:28443", Mode: KindTCP}}, 0)
	ps.Close()

	if _, err := ps.Get(context.Background(), "A"); err == nil {
		t.Fatal("关闭后 Get 应当报错")
	} else if !errors.Is(err, ErrPoolSetClosed) {
		t.Fatalf("错误应为 ErrPoolSetClosed，实际: %v", err)
	}
}

// Rebuild：参数未变的线路必须保留原池（不踢在途连接）；参数变了的必须重建。
func TestPoolSetRebuildKeepsUnchanged(t *testing.T) {
	var aDials, aClosed int32
	ps := BuildPools([]DialerSpec{{ID: "A", Addr: "a:28443", Mode: KindTCP}}, 0)
	defer ps.Close()
	instrumentPool(ps, "A", &aDials, &aClosed)

	ctx := context.Background()
	c, err := ps.Get(ctx, "A")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	ps.Put("A", c)

	// 参数完全相同的 rebuild：不应关池
	ps.Rebuild([]DialerSpec{{ID: "A", Addr: "a:28443", Mode: KindTCP}}, 0)
	if n := atomic.LoadInt32(&aClosed); n != 0 {
		t.Fatalf("参数未变时不该关闭旧池（已关 %d 次）", n)
	}
	got, err := ps.Get(ctx, "A")
	if err != nil {
		t.Fatalf("rebuild 后 Get: %v", err)
	}
	if got != c {
		t.Fatal("rebuild 后应复用原来的连接")
	}
	// 归还：Close 只关 idle 连接（在途连接归调用方负责），不归还就测不出关池
	ps.Put("A", got)

	// 参数变化（Addr 改了）：必须重建
	ps.Rebuild([]DialerSpec{{ID: "A", Addr: "a2:28443", Mode: KindTCP}}, 0)
	if n := atomic.LoadInt32(&aClosed); n == 0 {
		t.Fatal("Addr 变化时应关闭旧池")
	}
}

// Rebuild 移除线路：其池应被关闭，且不再可取。
func TestPoolSetRebuildRemovesRoute(t *testing.T) {
	ps := BuildPools([]DialerSpec{
		{ID: "A", Addr: "a:28443", Mode: KindTCP},
		{ID: "B", Addr: "b:28443", Mode: KindTCP},
	}, 0)
	defer ps.Close()

	ps.Rebuild([]DialerSpec{{ID: "A", Addr: "a:28443", Mode: KindTCP}}, 0)
	if _, err := ps.Get(context.Background(), "B"); err == nil {
		t.Fatal("被移除的线路 B 不应还能取连接")
	}
	if ids := ps.IDs(); len(ids) != 1 || ids[0] != "A" {
		t.Fatalf("IDs 应为 [A]，实际 %v", ids)
	}
}

// 给已被移除线路的连接调用 Put：应安全关闭而不是泄漏。
func TestPoolSetPutToRemovedRoute(t *testing.T) {
	var aClosed, bClosed int32
	ps := BuildPools([]DialerSpec{
		{ID: "A", Addr: "a:28443", Mode: KindTCP},
		{ID: "B", Addr: "b:28443", Mode: KindTCP},
	}, 0)
	defer ps.Close()
	instrumentPool(ps, "B", new(int32), &bClosed)

	c, err := ps.Get(context.Background(), "B")
	if err != nil {
		t.Fatalf("Get B: %v", err)
	}
	ps.Rebuild([]DialerSpec{{ID: "A", Addr: "a:28443", Mode: KindTCP}}, 0)

	ps.Put("B", c) // B 已不存在
	if n := atomic.LoadInt32(&bClosed); n != 1 {
		t.Fatalf("归还到已移除线路的连接应被关闭（关 %d 次）", n)
	}
	_ = aClosed
}

// Stats 要能反映每条线路的复用/新建次数（排障用）。
func TestPoolSetStats(t *testing.T) {
	ps := BuildPools([]DialerSpec{{ID: "A", Addr: "a:28443", Mode: KindTCP}}, 0)
	defer ps.Close()
	instrumentPool(ps, "A", new(int32), new(int32))

	ctx := context.Background()
	c, _ := ps.Get(ctx, "A")
	ps.Put("A", c)
	if _, err := ps.Get(ctx, "A"); err != nil {
		t.Fatalf("Get: %v", err)
	}

	st := ps.Stats()
	if len(st) != 1 {
		t.Fatalf("stats 条数 %d，期望 1", len(st))
	}
	if st[0].Misses != 1 {
		t.Fatalf("新建次数 %d，期望 1", st[0].Misses)
	}
	if st[0].Hits != 1 {
		t.Fatalf("复用次数 %d，期望 1", st[0].Hits)
	}
	if st[0].Addr != "a:28443" {
		t.Fatalf("Addr 字段 %q", st[0].Addr)
	}
}

// Dialer 按线路取：续传路径要走这条，必须拿到对应线路的 Dialer。
func TestPoolSetDialer(t *testing.T) {
	ps := BuildPools([]DialerSpec{
		{ID: "A", Addr: "a:28443", Mode: KindQUIC},
		{ID: "B", Addr: "b:28443", Mode: KindTCP, Proxy: "socks5://p:1080"},
	}, 0)
	defer ps.Close()

	dA, ok := ps.Dialer("A")
	if !ok || dA.Addr != "a:28443" || dA.Mode != KindQUIC {
		t.Fatalf("A 的 Dialer 不对: ok=%v d=%+v", ok, dA)
	}
	dB, ok := ps.Dialer("B")
	if !ok || dB.Addr != "b:28443" || dB.Proxy != "socks5://p:1080" {
		t.Fatalf("B 的 Dialer 不对: ok=%v d=%+v", ok, dB)
	}
	// 必须是不同实例（共享会污染 prefer 粘性状态）
	if dA == dB {
		t.Fatal("不同线路必须各自独立的 Dialer 实例")
	}

	if _, ok := ps.Dialer("不存在"); ok {
		t.Fatal("未知线路不应返回 Dialer")
	}
}

// 并发 Get/Put 不得 panic 或串台（-race 下必须干净）。
func TestPoolSetConcurrent(t *testing.T) {
	ps := BuildPools([]DialerSpec{
		{ID: "A", Addr: "a:28443", Mode: KindTCP},
		{ID: "B", Addr: "b:28443", Mode: KindTCP},
	}, 4)
	defer ps.Close()

	for _, id := range []string{"A", "B"} {
		id := id
		ps.mu.Lock()
		p := ps.pools[id]
		ps.mu.Unlock()
		p.dialFn = func(ctx context.Context) (*Conn, error) { return fakeConn(id, nil), nil }
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := context.Background()
			id := "A"
			if i%2 == 0 {
				id = "B"
			}
			c, err := ps.Get(ctx, id)
			if err != nil {
				return // 并发下池可能刚被 Reap，忽略
			}
			time.Sleep(time.Millisecond)
			ps.Put(id, c)
		}(i)
	}
	wg.Wait()
}

// 空 specs：Get 应报 ErrNoPool（保持 ErrNoPool 族，便于上层区分）
func TestPoolSetEmpty(t *testing.T) {
	ps := BuildPools(nil, 0)
	defer ps.Close()
	if _, err := ps.Get(context.Background(), "任意"); !errors.Is(err, ErrNoPool) {
		t.Fatalf("空集合应返回 ErrNoPool，实际: %v", err)
	}
	if len(ps.IDs()) != 0 {
		t.Fatal("空集合的 IDs 应为空")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
