package transport

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestPoolReuse 验证池确实复用连接而不是每次新建。
//
// 这个测试是「握手成本」修复的核心证据：如果 hits 一直是 0，说明每个请求
// 仍在付 2 秒的握手钱（实测数据见 pool.go 顶部注释）。
func TestPoolReuse(t *testing.T) {
	addr, _, _, stop := startTestServer(t)
	defer stop()

	d := NewPooledDialer(&Dialer{
		Addr: addr, ServerName: "localhost", Insecure: true, Mode: KindTCP,
	}, 4)
	defer d.Close()

	for i := 0; i < 5; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		c, err := d.Get(ctx)
		cancel()
		if err != nil {
			t.Fatalf("第 %d 次 Get: %v", i+1, err)
		}
		// 归还（模拟「请求已完整读完」）
		d.Put(c)
	}

	hits, misses := d.Stats()
	if misses != 1 {
		t.Errorf("新建次数 = %d，期望 1（首次才建连）", misses)
	}
	if hits != 4 {
		t.Errorf("复用次数 = %d，期望 4", hits)
	}
}

// TestPoolNoCrossTalk 串台测试——连接复用最危险的失效模式。
//
// 场景：同一个连接上先后跑两个请求，如果上一条的响应有残留字节没读完，
// 下一个请求会读到脏数据。这个测试用不同的 payload 反复验证每次读到的
// 都是自己那条，绝无串台。
func TestPoolNoCrossTalk(t *testing.T) {
	addr, _, _, stop := startTestServer(t)
	defer stop()

	d := NewPooledDialer(&Dialer{
		Addr: addr, ServerName: "localhost", Insecure: true, Mode: KindTCP,
	}, 2)
	defer d.Close()

	payloads := []string{
		"first-request-body",
		"second-request-body-different-length",
		"third",
		strings4k(),
	}
	for i, want := range payloads {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		c, err := d.Get(ctx)
		cancel()
		if err != nil {
			t.Fatalf("第 %d 次 Get: %v", i+1, err)
		}
		if err := writeAll(c, mustFrame(want)); err != nil {
			d.Drop(c)
			t.Fatalf("第 %d 次 send: %v", i+1, err)
		}
		got, err := c.Recv()
		if err != nil {
			d.Drop(c)
			t.Fatalf("第 %d 次 recv: %v", i+1, err)
		}
		if string(got) != want {
			d.Drop(c)
			t.Fatalf("第 %d 次串台！got %d 字节 %q，want %d 字节 %q",
				i+1, len(got), truncate(got), len(want), want)
		}
		d.Put(c)
	}

	hits, _ := d.Stats()
	if hits == 0 {
		t.Fatal("从未命中复用，串台测试没有真正覆盖复用路径")
	}
}

// TestPoolConcurrent 并发场景：多个 goroutine 同时 Get/Put，验证不崩、不串台。
func TestPoolConcurrent(t *testing.T) {
	addr, _, _, stop := startTestServer(t)
	defer stop()

	d := NewPooledDialer(&Dialer{
		Addr: addr, ServerName: "localhost", Insecure: true, Mode: KindTCP,
	}, 4)
	defer d.Close()

	var wg sync.WaitGroup
	var fail atomic.Int32
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			want := "payload-" + string(rune('a'+n%26)) + "-" + time.Now().Format("150405.000000")
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			c, err := d.Get(ctx)
			cancel()
			if err != nil {
				fail.Add(1)
				return
			}
			if err := writeAll(c, mustFrame(want)); err != nil {
				d.Drop(c)
				fail.Add(1)
				return
			}
			got, err := c.Recv()
			if err != nil {
				d.Drop(c)
				fail.Add(1)
				return
			}
			if string(got) != want {
				d.Drop(c)
				fail.Add(1)
				return
			}
			d.Put(c)
		}(i)
	}
	wg.Wait()
	if n := fail.Load(); n > 0 {
		t.Fatalf("%d/20 个并发请求失败或串台", n)
	}
}

// TestPoolIdleTimeout 超龄空闲连接必须被丢弃，不能复用一条对端早已关闭的连接。
func TestPoolIdleTimeout(t *testing.T) {
	addr, _, _, stop := startTestServer(t)
	defer stop()

	d := NewPooledDialer(&Dialer{
		Addr: addr, ServerName: "localhost", Insecure: true, Mode: KindTCP,
	}, 2)
	defer d.Close()
	// 把超时压到极短，避免测试睡很久
	d.timeout = 30 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	c, err := d.Get(ctx)
	cancel()
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	d.Put(c)

	time.Sleep(80 * time.Millisecond)

	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	c2, err := d.Get(ctx2)
	cancel2()
	if err != nil {
		t.Fatalf("Get after idle: %v", err)
	}
	defer d.Drop(c2)

	hits, misses := d.Stats()
	if hits != 0 {
		t.Errorf("超龄连接被复用了（hits=%d），期望丢弃后新建", hits)
	}
	if misses != 2 {
		t.Errorf("misses=%d，期望 2（一次超龄丢弃 + 新建）", misses)
	}
}

// TestPoolMaxIdle 超过 maxIdle 的连接应被关闭而不是无限堆积。
func TestPoolMaxIdle(t *testing.T) {
	addr, _, _, stop := startTestServer(t)
	defer stop()

	d := NewPooledDialer(&Dialer{
		Addr: addr, ServerName: "localhost", Insecure: true, Mode: KindTCP,
	}, 2) // 最多留 2 条
	defer d.Close()

	var conns []*Conn
	for i := 0; i < 5; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		c, err := d.Get(ctx)
		cancel()
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		conns = append(conns, c)
	}
	for _, c := range conns {
		d.Put(c)
	}

	d.mu.Lock()
	n := len(d.idle)
	d.mu.Unlock()
	if n > 2 {
		t.Fatalf("空闲连接 %d 条，超过 maxIdle=2", n)
	}
}

// TestPoolClosed Get 在 Close 之后必须报错，不能返回空连接。
func TestPoolClosed(t *testing.T) {
	d := NewPooledDialer(&Dialer{Addr: "127.0.0.1:1"}, 2)
	d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := d.Get(ctx); err == nil {
		t.Fatal("池已关闭，Get 应报错")
	}
}

func truncate(b []byte) string {
	if len(b) > 40 {
		return string(b[:40]) + "…"
	}
	return string(b)
}

func strings4k() string {
	b := make([]byte, 4096)
	for i := range b {
		b[i] = byte('A' + i%26)
	}
	return string(b)
}

// TestPoolIdleForSurvivesGet 锁住一个真实的踩坑：
//
// IdleFor 必须在 Get 时快照。第一版是在上层现算的（Get 之后 idleAt 已被清零），
// 于是「闲置多久」永远是 0，探活条件 `IdleFor() > 5s` 一次都没成立过——
// 表现为服务端重启后客户端仍然挂满 120 秒（QUIC MaxIdleTimeout）。
func TestPoolIdleForSurvivesGet(t *testing.T) {
	p := NewPooledDialer(&Dialer{}, 4)
	c := &Conn{}
	p.Put(c)
	time.Sleep(30 * time.Millisecond)

	got, err := p.Get(context.Background())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != c {
		t.Fatalf("Get 没拿到刚归还的连接")
	}
	if d := got.IdleFor(); d < 25*time.Millisecond {
		t.Errorf("IdleFor() = %v，应在 Get 时快照到约 30ms", d)
	}
	if !got.reused {
		t.Errorf("从池里取出的连接必须标记 reused")
	}
}

// TestConnDeadlinesReachTransport 验证 SetReadDeadline 真的传到了底层流：
// 探活靠它把「死连接上的阻塞读」变成 2.5 秒的 deadline exceeded。
func TestConnDeadlinesReachTransport(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	defer client.Close()

	c := &Conn{
		send:             func(b []byte) error { _, err := client.Write(b); return err },
		recv:             func() ([]byte, error) { return readFrame(client) },
		setReadDeadline:  client.SetReadDeadline,
		setWriteDeadline: client.SetWriteDeadline,
	}
	if err := c.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	start := time.Now()
	if _, err := c.Recv(); err == nil {
		t.Fatalf("对端不写数据时 Recv 应超时")
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("读超时用了 %v，deadline 没生效", d)
	}
}
