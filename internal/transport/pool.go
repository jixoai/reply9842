package transport

// 连接复用（串行池）
//
// ── 为什么需要 ────────────────────────────────────────────────────────────
// 实测（2026-11-13，本机 → 香港 47.82.76.102，绑物理网卡绕开代理 TUN）：
// 单次 TCP+TLS 握手耗时 747ms / 2330ms / 2821ms / 773ms / 3188ms，
// 平均约 2 秒。而每个请求原本都新建一条连接、用完即关，等于每个请求
// 白扔 1–3 秒在握手上——这比隧道省下的带宽值钱得多。
//
// ── 为什么是「串行」而不是多路复用 ────────────────────────────────────────
// 完整多路复用（一条连接并发跑多个请求，按 stream id 分发）需要在协议层
// 引入请求 ID、乱序响应、流量控制，改动面覆盖 meta/frame、clientstream、
// 服务端 accept 循环，出错就是数据串台（最坏情况：A 的响应写给 B）。
// 而串行复用——一条连接同一时刻只跑一个请求，跑完归还、下一个接着用——
// 已经能吃掉握手开销的绝大部分：只要请求不是严格并发，命中率就接近 100%。
// 代价是并发请求会各占一条连接（等价于当前行为），不会比现在更差。
//
// 结论：先做串行复用，把 2 秒的握手成本降到接近零；多路复用留作后续优化。

import (
	"context"
	"sync"
	"time"
)

// 池的默认参数。
//
// MaxIdle 取 4 而非 1：客户端可能是多线程 Agent，少量并发很常见；
// 但也不需要更多——空闲连接本身占内存（QUIC 连接尤甚），且服务端
// 每条连接一个 goroutine。
const (
	defaultMaxIdle     = 4
	defaultIdleTimeout = 90 * time.Second
)

// PooledDialer 在 Dialer 之上加一层空闲连接池。
//
// 用法的关键约束：Get 拿到的连接必须在使用完毕后调用 Put 归还（或 Close 丢弃）。
// 归还前必须确保上一条请求的响应已完整读完——否则残留字节会被下一个请求读到，
// 造成串台。调用方（relayclient）在 Drain 完成、且没有未决错误时才归还。
type PooledDialer struct {
	*Dialer

	mu      sync.Mutex
	idle    []*idleConn
	maxIdle int
	timeout time.Duration
	closed  bool

	// 统计：便于观测复用率，排查「为什么还是很慢」
	hits   int64
	misses int64
}

type idleConn struct {
	c     *Conn
	since time.Time
}

// NewPooledDialer 包装一个 Dialer。maxIdle<=0 用默认值。
func NewPooledDialer(d *Dialer, maxIdle int) *PooledDialer {
	if maxIdle <= 0 {
		maxIdle = defaultMaxIdle
	}
	return &PooledDialer{
		Dialer:  d,
		maxIdle: maxIdle,
		timeout: defaultIdleTimeout,
	}
}

// Stats 返回 (复用次数, 新建次数)。用于日志与诊断。
func (p *PooledDialer) Stats() (hits, misses int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hits, p.misses
}

// Get 取一条可用连接：优先复用空闲连接，没有则新建。
func (p *PooledDialer) Get(ctx context.Context) (*Conn, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, errPoolClosed
	}
	// 从尾部取（LIFO）：最近归还的连接最可能仍然健康
	for len(p.idle) > 0 {
		ic := p.idle[len(p.idle)-1]
		p.idle = p.idle[:len(p.idle)-1]
		// 超龄的空闲连接直接丢弃，避免复用一个对端早已关闭的连接
		if time.Since(ic.since) > p.timeout {
			p.mu.Unlock()
			ic.c.Close()
			p.mu.Lock()
			if p.closed {
				p.mu.Unlock()
				return nil, errPoolClosed
			}
			continue
		}
		p.hits++
		p.mu.Unlock()
		ic.c.reused = true
		return ic.c, nil
	}
	p.misses++
	p.mu.Unlock()
	c, err := p.Dial(ctx)
	if c != nil {
		c.reused = false // 新建连接：调用方按「这次付了握手钱」记录
	}
	return c, err
}

// Put 归还一条连接。
//
// 调用方必须保证：
//  1. 上一条请求的响应已经完整读完（否则残留字节会被下一个请求误读）
//  2. 连接未处于错误状态
//
// 不满足任一条就不要 Put，直接 Close。
func (p *PooledDialer) Put(c *Conn) {
	if c == nil {
		return
	}
	p.mu.Lock()
	if p.closed || len(p.idle) >= p.maxIdle {
		p.mu.Unlock()
		c.Close()
		return
	}
	p.idle = append(p.idle, &idleConn{c: c, since: time.Now()})
	p.mu.Unlock()
}

// Drop 丢弃一条连接（有错误时用），等价于 Close 但不进池。
func (p *PooledDialer) Drop(c *Conn) {
	if c != nil {
		c.Close()
	}
}

// Close 关闭池并释放全部空闲连接。
func (p *PooledDialer) Close() error {
	p.mu.Lock()
	p.closed = true
	conns := p.idle
	p.idle = nil
	p.mu.Unlock()
	for _, ic := range conns {
		ic.c.Close()
	}
	return nil
}

// Reap 清理超龄的空闲连接。由调用方定期调用（比如每秒一次）。
func (p *PooledDialer) Reap() {
	now := time.Now()
	p.mu.Lock()
	keep := p.idle[:0]
	for _, ic := range p.idle {
		if now.Sub(ic.since) > p.timeout {
			ic.c.Close()
			continue
		}
		keep = append(keep, ic)
	}
	p.idle = keep
	p.mu.Unlock()
}

// errPoolClosed 池已关闭
var errPoolClosed = errStr("transport: pool closed")

type errStr string

func (e errStr) Error() string { return string(e) }
