package transport

// pools.go — 多线路连接池集合（每条线路各自一个 Dialer + PooledDialer）
//
// ── 为什么是「按线路独立池」而不是「一个池装所有线路的连接」 ───────────────
// 池的核心价值是**复用同一条连接的握手成本**（实测本机→香港单次 TCP+TLS
// 握手 0.7–3.2 秒）。连接一旦建立，它的出站路径就被内核钉死了（源地址绑定、
// 代理 CONNECT 的目标、QUIC 的四元组）。所以连接与线路是**一对一**的：
// 一条连接只属于它被建出来的那条线路，永远不能"借"给别的线路用。
//
// 因此结构上必须是 map[routeID]*PooledDialer。把不同线路的连接混在同一个
// 池里，取出来的连接走的可能是别的线路——择路决策会完全失效，且难以察觉
// （请求成功、只是慢）。
//
// ── 为什么键是 string 而不是 route.Route ──────────────────────────────────
// 包依赖方向：relayclient → {transport, route} 都可以，但 transport → route
// 会造成 transport 依赖线路的业务语义（kind 的取值、校验规则）。
// transport 只关心「一批命名好的出站参数」，命名用 string 就够了。
// 归属关系由 relayclient 组装时保证（它把 route.Route 翻译成 DialerSpec）。

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// DialerSpec 一条线路的建连参数（route.Route 在 transport 侧的投影）。
//
// 为什么不让上层直接传 *Dialer：Dialer 有可变状态（prefer/lastKind 的
// atomic），同一个 Dialer 被多条线路共享会让「粘性记忆」互相污染——
// A 线路 QUIC 失败会把 B 线路也标记成"优先 TCP"。每条线路必须自己的实例。
type DialerSpec struct {
	ID         string // 线路标识（与 route.Route.ID 一致，仅作键与日志）
	Addr       string // relay-server 地址 host:port
	ServerName string // TLS SNI
	LocalAddr  string // 绑定源地址（direct 类型）
	Proxy      string // 代理 URL（proxy 类型）
	Mode       Kind   // auto / quic / tcp
	Insecure   bool
}

// NewDialer 按 spec 建一个独立的 Dialer 实例。
func (s DialerSpec) NewDialer() *Dialer {
	return &Dialer{
		Addr:       s.Addr,
		ServerName: s.ServerName,
		LocalAddr:  s.LocalAddr,
		Proxy:      s.Proxy,
		Mode:       s.Mode,
		Insecure:   s.Insecure,
	}
}

// PoolSet 按线路分组的连接池集合。
//
// 并发安全。生命周期：Build 之后可被任意 goroutine 并发 Get/Put，
// Close 后所有 Get 返回错误。
type PoolSet struct {
	mu    sync.RWMutex
	pools map[string]*PooledDialer
	specs map[string]DialerSpec
	order []string // 建池顺序（供 status 输出可复现）
	built bool
}

// ErrNoPool 该线路没有池（未构建或已关闭）。
var ErrNoPool = errors.New("transport: 该线路没有可用的连接池")

// ErrPoolSetClosed 集合已关闭。
var ErrPoolSetClosed = errors.New("transport: 连接池集合已关闭")

// BuildPools 按 specs 建池。重复调用会**保留**已有池（不打断在途连接），
// 只为新增的线路建池，并关闭已从 specs 中移除的线路的池。
//
// 为什么不是「全拆重建」：重建会在每次配置微调时踢掉所有在途连接，
// 而用户改的可能只是某一条线路的名字。
func BuildPools(specs []DialerSpec, maxIdle int) *PoolSet {
	ps := &PoolSet{
		pools: make(map[string]*PooledDialer, len(specs)),
		specs: make(map[string]DialerSpec, len(specs)),
	}

	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.mergeLocked(specs, maxIdle)
	ps.built = true
	return ps
}

// mergeLocked 合并 specs（幂等：已有线路保持原池）。
func (ps *PoolSet) mergeLocked(specs []DialerSpec, maxIdle int) {
	keep := make(map[string]struct{}, len(specs))
	ps.order = ps.order[:0]

	for _, s := range specs {
		if s.ID == "" {
			continue
		}
		keep[s.ID] = struct{}{}
		ps.order = append(ps.order, s.ID)
		if p, ok := ps.pools[s.ID]; ok {
			// 已有池：只在建连参数真的变了时重建，避免无谓地踢连接。
			// 判据是「能否用同样的参数建出等价连接」——比较 DialerSpec 的
			// 全部字段（结构体可比较）。
			if ps.specs[s.ID] == s {
				continue
			}
			p.Close()
		}
		d := s.NewDialer()
		ps.pools[s.ID] = NewPooledDialer(d, maxIdle)
		ps.specs[s.ID] = s
	}

	// 关闭已被移除的线路的池
	for id, p := range ps.pools {
		if _, ok := keep[id]; ok {
			continue
		}
		p.Close()
		delete(ps.pools, id)
		delete(ps.specs, id)
	}
}

// Rebuild 用新 specs 替换（保留未变的线路的池）。
func (ps *PoolSet) Rebuild(specs []DialerSpec, maxIdle int) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.mergeLocked(specs, maxIdle)
	ps.built = true
}

// Get 从指定线路的池取一条连接。池不存在时返回 ErrNoPool。
//
// 注意：这里**不做**跨线路回退。回退是上层（relayclient 的 handler）的
// 职责，因为只有它知道「这次请求该换哪条线路」——它手里有 Picker 的评分。
// 在 transport 层做静默回退会让「择路为什么选了这条」变得不可解释。
func (ps *PoolSet) Get(ctx context.Context, id string) (*Conn, error) {
	ps.mu.RLock()
	p, ok := ps.pools[id]
	closed := !ps.built
	ps.mu.RUnlock()

	if closed {
		return nil, ErrPoolSetClosed
	}
	if !ok {
		return nil, errNoSuchPool(id)
	}
	return p.Get(ctx)
}

// Put 归还连接到对应线路的池。
func (ps *PoolSet) Put(id string, c *Conn) {
	if c == nil {
		return
	}
	ps.mu.RLock()
	p, ok := ps.pools[id]
	ps.mu.RUnlock()
	if !ok {
		c.Close() // 线路已被移除：连接无处可归，直接关掉
		return
	}
	p.Put(c)
}

// Drop 丢弃连接（不进池）。
func (ps *PoolSet) Drop(c *Conn) {
	if c != nil {
		c.Close()
	}
}

// Dialer 返回某条线路的 Dialer（给「续传」路径用：它不走池）。
func (ps *PoolSet) Dialer(id string) (*Dialer, bool) {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	p, ok := ps.pools[id]
	if !ok {
		return nil, false
	}
	return p.Dialer, true
}

// MustFirstDialer 返回第一条线路的 Dialer；没有线路时返回 nil。
//
// 用途：续传路径需要一个「不经过池」的建连函数，而它对线路选择不敏感
// （续传是按 server 侧的 key 找回响应，与走哪条线路无关）。
func (ps *PoolSet) MustFirstDialer() *Dialer {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	if len(ps.order) == 0 {
		return nil
	}
	p, ok := ps.pools[ps.order[0]]
	if !ok {
		return nil
	}
	return p.Dialer
}

// IDs 返回当前有池的线路 ID（按建池顺序）。
func (ps *PoolSet) IDs() []string {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	out := make([]string, len(ps.order))
	copy(out, ps.order)
	return out
}

// PoolStats 一条线路的池统计（供 status / 诊断）。
type PoolStats struct {
	ID     string `json:"id"`
	Addr   string `json:"addr"`
	Hits   int64  `json:"hits"`   // 复用次数
	Misses int64  `json:"misses"` // 新建次数
}

// Stats 返回全部池的统计。
func (ps *PoolSet) Stats() []PoolStats {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	out := make([]PoolStats, 0, len(ps.pools))
	for _, id := range ps.order {
		p, ok := ps.pools[id]
		if !ok {
			continue
		}
		h, m := p.Stats()
		out = append(out, PoolStats{ID: id, Addr: ps.specs[id].Addr, Hits: h, Misses: m})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Reap 清理所有池的超龄空闲连接（调用方定期调用）。
func (ps *PoolSet) Reap() {
	ps.mu.RLock()
	pools := make([]*PooledDialer, 0, len(ps.pools))
	for _, p := range ps.pools {
		pools = append(pools, p)
	}
	ps.mu.RUnlock()
	for _, p := range pools {
		p.Reap()
	}
}

// Close 关闭全部池。
func (ps *PoolSet) Close() error {
	ps.mu.Lock()
	pools := make([]*PooledDialer, 0, len(ps.pools))
	for _, p := range ps.pools {
		pools = append(pools, p)
	}
	ps.pools = nil
	ps.specs = nil
	ps.order = nil
	ps.built = false
	ps.mu.Unlock()

	for _, p := range pools {
		p.Close()
	}
	return nil
}

// errNoSuchPool 让错误信息带上线路名，便于运维定位
func errNoSuchPool(id string) error {
	return fmt.Errorf("%w: %q", ErrNoPool, id)
}

// HandshakeCost 估算「每条线路握手一次」的平均成本（用于 UI 提示）。
//
// 不是必需的运行时逻辑，但**排障时非常有用**：用户抱怨「加了线路还是很慢」
// 时，先看这条——如果握手成本高而池命中率低，问题在池而不是在选路。
func (ps *PoolSet) HandshakeCost() (avg time.Duration) {
	ps.mu.RLock()
	pools := make([]*PooledDialer, 0, len(ps.pools))
	for _, p := range ps.pools {
		pools = append(pools, p)
	}
	ps.mu.RUnlock()

	var sum time.Duration
	var n int
	for _, p := range pools {
		h, m := p.Stats()
		if h+m == 0 {
			continue
		}
		// 新建次数越多，握手成本越"重"，用它做加权
		sum += time.Duration(m) * defaultHandshakeEstimate
		n += int(m)
	}
	if n == 0 {
		return 0
	}
	return sum / time.Duration(n)
}

// defaultHandshakeEstimate 单次握手的经验值（本机→香港实测 0.7–3.2s 的中位）
const defaultHandshakeEstimate = 2 * time.Second
