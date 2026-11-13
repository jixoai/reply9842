// Package transport — QUIC / TCP+TLS 双栈隧道
//
// 正交意图（3 个，2026-11-12 kzf 需求：「QUIC 和 TCP+TLS 两路同时做，但我记得
// QUIC 是可以优雅回退成 TCP 传输层才对啊」）：
//  1. 统一 Conn 抽象：QUIC stream 与 TCP 连接在帧层面等价
//  2. auto 模式：客户端先试 QUIC（1s 超时），失败降级 TCP+TLS，并记住结果
//  3. 自签/系统根证书双模式（观察期用自签，正式环境用真证书）
//
// 关于「QUIC 优雅回退」的事实澄清：
//
//	HTTP/3 的回退是「HTTP 协议族」的能力（浏览器用 Alt-Svc 探测，失败退回
//	HTTP/2 over TCP），**不是 QUIC 传输层自带**。quic-go 连不上就是连不上。
//	因此回退逻辑必须在本层实现——即下面的 DialAuto。
package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
)

// Kind 传输类型
type Kind string

const (
	KindQUIC Kind = "quic"
	KindTCP  Kind = "tcp"
	KindAuto Kind = "auto"
)

// ALPN 隧道协议标识（QUIC 与 TLS 共用）
const ALPN = "reply9842/1"

// Dialer 客户端连接工厂
type Dialer struct {
	Addr       string        // host:port
	ServerName string        // TLS SNI；留空则从 Addr 推导
	Insecure   bool          // 跳过证书校验（自签观察期）
	Mode       Kind          // auto / quic / tcp
	QuicProbe  time.Duration // auto 模式下 QUIC 探测超时，默认 1s

	// LocalAddr 绑定出站源地址（如 "192.168.1.106:0" 或 "192.168.1.106"）。
	//
	// 存在的理由：代理软件的 TUN 全局模式会整段接管 IPv4 路由
	// （实测 Clash 系：0.0.0.0/1 + 128.0.0.0/1 两条 UGSc 规则指向 utun），
	// 于是到隧道服务器的流量也被导进代理。代理对 28443 这类非标端口没有
	// 正确的直连规则，TLS 握手会被中途切断，客户端只看到
	// "tcp tls: EOF"——看起来像服务端崩了，实际是本地网络劫持。
	//
	// 绑到物理网卡的真实 IP 后，内核直接走该网卡的 ARP/路由，不再进 utun。
	// 这是用户侧「不想改代理配置」时最省事的解法。
	LocalAddr string

	// auto 模式的粘性记忆：一旦某模式成功，后续优先该模式
	prefer   atomic.Value // Kind
	lastKind atomic.Value // Kind
}

// Conn 统一连接抽象
type Conn struct {
	kind    Kind
	send    func([]byte) error
	recv    func() ([]byte, error)
	closeFn func() error

	// reused 表示这条连接是从池里复用的（true）还是本次新建的（false）。
	// 由 PooledDialer 在 Get 时设置，供上层统计「这次请求是否付了握手代价」。
	reused bool
	// handshake 建立这条连接所花的握手时间（新建才有意义；复用时为 0）。
	handshake time.Duration

	// setReadDeadline 给底层流设读超时。QUIC stream 与 tls.Conn 都有这个方法。
	// 只在**探活**时用短超时，正常请求路径绝不设：LLM 首帧可能等 30–50 秒，
	// 一个固定的读超时会把合法请求误杀。
	setReadDeadline  func(time.Time) error
	setWriteDeadline func(time.Time) error
	// idleAt 这条连接最近一次归还到池里的时刻；0 表示正在被使用。
	idleAt time.Time
	// idleFor 本次取出前闲置了多久（Get 时快照）。
	idleFor time.Duration
}

// Reused 这条连接是否来自连接池复用。
func (c *Conn) Reused() bool { return c.reused }

// Handshake 建立连接所花的握手时间；复用连接返回 0。
func (c *Conn) Handshake() time.Duration { return c.handshake }

// SetReadDeadline 给底层流设读超时；传零值清除。
func (c *Conn) SetReadDeadline(t time.Time) error {
	if c.setReadDeadline == nil {
		return nil
	}
	return c.setReadDeadline(t)
}

// SetWriteDeadline 给底层流设写超时；传零值清除。
//
// QUIC 流写在没有对端 ACK 时会一直阻塞（实测服务端被 kill -9 后写不返回），
// 所以探活必须同时设写超时，否则探活本身就挂在写那一行。
func (c *Conn) SetWriteDeadline(t time.Time) error {
	if c.setWriteDeadline == nil {
		return nil
	}
	return c.setWriteDeadline(t)
}

// IdleFor 这条连接在这次使用前闲置了多久。
//
// 由连接池在 Get 时快照（而不是每次现算）：Get 之后 idleAt 就清零了，
// 上层若现算拿到的永远是 0——第一版就是这么写的，探活因此从未触发过。
func (c *Conn) IdleFor() time.Duration { return c.idleFor }

// Kind 当前连接使用的传输
func (c *Conn) Kind() Kind { return c.kind }

// Send 发送一个完整帧（长度前缀由上层 meta 负责）
func (c *Conn) Send(b []byte) error { return c.send(b) }

// Write 实现 io.Writer，便于把 meta.WriteFrame 直接写到连接上
func (c *Conn) Write(b []byte) (int, error) {
	if err := c.send(b); err != nil {
		return 0, err
	}
	return len(b), nil
}

// Recv 接收一个完整帧
func (c *Conn) Recv() ([]byte, error) { return c.recv() }

// Close 关闭
func (c *Conn) Close() error { return c.closeFn() }

// connectResult 内部结果
type connectResult struct {
	conn *Conn
	err  error
}

// Dial 建立连接。Mode=auto 时按 prefer → quic → tcp 顺序尝试。
func (d *Dialer) Dial(ctx context.Context) (*Conn, error) {
	// 不要写回 d.QuicProbe：Dial 会被多个请求 goroutine 并发调用，写共享字段
	// 是数据竞争（-race 实测抓到过）。用局部值，语义完全一样。
	probe := d.QuicProbe
	if probe <= 0 {
		probe = time.Second
	}

	var order []Kind
	switch d.Mode {
	case KindQUIC:
		order = []Kind{KindQUIC}
	case KindTCP:
		order = []Kind{KindTCP}
	default: // auto
		order = []Kind{KindQUIC, KindTCP}
		if p, ok := d.prefer.Load().(Kind); ok && p == KindTCP {
			// 上次 QUIC 失败过，先试 TCP（但仍会给 QUIC 一次机会在下次连接）
			order = []Kind{KindTCP, KindQUIC}
		}
	}

	var lastErr error
	for _, k := range order {
		var (
			c   *Conn
			err error
		)
		switch k {
		case KindQUIC:
			// auto 模式必须给 QUIC 一个明确的探测超时，否则握手失败会无限等
			cctx, cancel := context.WithTimeout(ctx, probe)
			c, err = d.dialQUIC(cctx)
			cancel()
			if err != nil && d.Mode == KindAuto {
				// QUIC 失败不影响后续 TCP 尝试，只记录
				d.prefer.Store(KindTCP)
			}
		case KindTCP:
			c, err = d.dialTCP(ctx)
		}
		if err == nil {
			d.lastKind.Store(c.kind)
			return c, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("transport: no usable transport")
	}
	return nil, lastErr
}

// LastKind 最近一次成功的传输类型（供统计）
func (d *Dialer) LastKind() Kind {
	if v, ok := d.lastKind.Load().(Kind); ok {
		return v
	}
	return ""
}

func (d *Dialer) dialQUIC(ctx context.Context) (*Conn, error) {
	tlsCfg := &tls.Config{
		NextProtos:         []string{ALPN},
		ServerName:         d.serverName(),
		InsecureSkipVerify: d.Insecure,
		MinVersion:         tls.VersionTLS13,
	}
	// 握手计时：只统计到 QUIC 连接建立为止（不含后面的 OpenStreamSync），
	// 因为用户感知的「连接慢」就是这一段。
	hsStart := time.Now()
	// quic-go v0.65 的 *quic.Conn 内部 ctx 派生自 DialAddr 的入参 ctx
	// （connection.go: context.WithCancelCause(context.WithoutCancel(ctx))
	// → 实际用 WithoutCancel 脱钩，但 OpenStreamSync/AcceptStream 等仍吃传进去的 ctx）。
	// 这里 dial 用调用方 ctx（auto 模式的 1s 探测超时），但 stream 操作必须
	// 用连接自身生命周期 ctx，否则探测超时一到就把已建立的连接判死。
	//
	// 需要绑源地址时不能走 DialAddr（它会自己建 UDP socket），改用
	// net.ListenUDP 先拿到绑定好的 PacketConn 再交给 quic.Dial。
	var qc *quic.Conn
	var err error
	if d.LocalAddr != "" {
		la, e := net.ResolveUDPAddr("udp", func() string {
			if _, _, se := net.SplitHostPort(d.LocalAddr); se != nil {
				return net.JoinHostPort(d.LocalAddr, "0")
			}
			return d.LocalAddr
		}())
		if e != nil {
			return nil, fmt.Errorf("quic local addr: %w", e)
		}
		pc, e := net.ListenUDP("udp", la)
		if e != nil {
			return nil, fmt.Errorf("quic listen udp: %w", e)
		}
		ra, e := net.ResolveUDPAddr("udp", d.Addr)
		if e != nil {
			pc.Close()
			return nil, fmt.Errorf("quic remote addr: %w", e)
		}
		qc, err = quic.Dial(ctx, pc, ra, tlsCfg, &quic.Config{
			HandshakeIdleTimeout: 5 * time.Second,
			MaxIdleTimeout:       120 * time.Second,
			KeepAlivePeriod:      30 * time.Second,
		})
		if err != nil {
			pc.Close()
		}
	} else {
		qc, err = quic.DialAddr(ctx, d.Addr, tlsCfg, &quic.Config{
			HandshakeIdleTimeout: 5 * time.Second,
			MaxIdleTimeout:       120 * time.Second,
			KeepAlivePeriod:      30 * time.Second,
		})
	}
	if err != nil {
		return nil, fmt.Errorf("quic dial: %w", err)
	}
	// 建连成功后必须脱钩 dial ctx：auto 模式的 ctx 只带 1s 探测超时，
	// 拿它去做后续 stream 操作会把已建立的连接误判为超时。qc.Context()
	// 是连接自身的生命周期 ctx（quic-go 内部用 context.WithoutCancel
	// 派生，仅在连接关闭时 cancel），既不受探测超时影响，也不会永久悬挂。
	st, err := qc.OpenStreamSync(qc.Context())
	if err != nil {
		qc.CloseWithError(1, "open stream")
		return nil, fmt.Errorf("quic stream: %w", err)
	}
	return &Conn{
		kind:             KindQUIC,
		handshake:        time.Since(hsStart),
		send:             func(b []byte) error { return writeAll(st, b) },
		recv:             func() ([]byte, error) { return readFrame(st) },
		setReadDeadline:  st.SetReadDeadline,
		setWriteDeadline: st.SetWriteDeadline,
		closeFn: func() error {
			st.Close()
			return qc.CloseWithError(0, "bye")
		},
	}, nil
}

func (d *Dialer) dialTCP(ctx context.Context) (*Conn, error) {
	tlsCfg := &tls.Config{
		NextProtos:         []string{ALPN},
		ServerName:         d.serverName(),
		InsecureSkipVerify: d.Insecure,
		MinVersion:         tls.VersionTLS12,
	}
	la, err := d.localAddr()
	if err != nil {
		return nil, fmt.Errorf("tcp local addr: %w", err)
	}
	hsStart := time.Now()
	nd := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second, LocalAddr: la}
	raw, err := nd.DialContext(ctx, "tcp", d.Addr)
	if err != nil {
		return nil, fmt.Errorf("tcp dial: %w", err)
	}
	tc := tls.Client(raw, tlsCfg)
	if err := tc.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, fmt.Errorf("tcp tls: %w", err)
	}
	return &Conn{
		kind:             KindTCP,
		handshake:        time.Since(hsStart),
		send:             func(b []byte) error { return writeAll(tc, b) },
		recv:             func() ([]byte, error) { return readFrame(tc) },
		setReadDeadline:  tc.SetReadDeadline,
		setWriteDeadline: tc.SetWriteDeadline,
		closeFn:          tc.Close,
	}, nil
}

func (d *Dialer) serverName() string {
	if d.ServerName != "" {
		return d.ServerName
	}
	h, _, err := net.SplitHostPort(d.Addr)
	if err != nil {
		return d.Addr
	}
	return h
}

// localAddr 把 LocalAddr 规整成 *net.TCPAddr（空则返回 nil = 让内核选）。
// 允许只给 IP（自动补 :0），也允许给完整 host:port。
func (d *Dialer) localAddr() (*net.TCPAddr, error) {
	if d.LocalAddr == "" {
		return nil, nil
	}
	s := d.LocalAddr
	if _, _, err := net.SplitHostPort(s); err != nil {
		s = net.JoinHostPort(s, "0")
	}
	return net.ResolveTCPAddr("tcp", s)
}

// ---- 服务端 ----

// Server 双栈监听
type Server struct {
	Addr     string
	CertFile string
	KeyFile  string
	Insecure bool // 自签证书时仍要提供证书；此字段仅记录语义

	tlsCfg *tls.Config
}

// Accept 句柄
type Accept struct {
	Kind Kind
	Conn *Conn
}

// Listen 同时启动 QUIC 与 TCP+TLS 监听，返回统一 accept 通道
//
// 关闭 cancel 即停止两个 listener。
func (s *Server) Listen(ctx context.Context) (<-chan Accept, error) {
	cert, err := tls.LoadX509KeyPair(s.CertFile, s.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load cert: %w", err)
	}
	s.tlsCfg = &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{ALPN},
		MinVersion:   tls.VersionTLS12,
	}

	out := make(chan Accept, 16)

	// QUIC
	ln, err := quic.ListenAddr(s.Addr, s.tlsCfg, &quic.Config{
		HandshakeIdleTimeout: 5 * time.Second,
		MaxIdleTimeout:       120 * time.Second,
		KeepAlivePeriod:      30 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("quic listen: %w", err)
	}
	go func() {
		for {
			qc, err := ln.Accept(ctx)
			if err != nil {
				return
			}
			// 一条 QUIC 连接可承载多个 stream；每个 stream 处理一次请求。
			// 每条连接用自己的生命周期 ctx：AcceptStream 不能被 listener ctx
			// 提前掐断，也不该让某条连接的退出影响其它连接。
			go func(qc *quic.Conn) {
				connCtx, connCancel := context.WithCancel(ctx)
				defer connCancel()
				// 连接被对端关闭时，AcceptStream 会返回错误而退出循环
				defer qc.CloseWithError(0, "done")
				for {
					st, err := qc.AcceptStream(connCtx)
					if err != nil {
						return
					}
					go func(st *quic.Stream) {
						c := &Conn{
							kind:    KindQUIC,
							send:    func(b []byte) error { return writeAll(st, b) },
							recv:    func() ([]byte, error) { return readFrame(st) },
							closeFn: func() error { st.Close(); return nil },
						}
						select {
						case out <- Accept{Kind: KindQUIC, Conn: c}:
						case <-connCtx.Done():
							st.Close()
						}
					}(st)
				}
			}(qc)
		}
	}()

	// TCP+TLS
	tln, err := tls.Listen("tcp", s.Addr, s.tlsCfg)
	if err != nil {
		ln.Close()
		return nil, fmt.Errorf("tcp listen: %w", err)
	}
	go func() {
		for {
			tc, err := tln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				// 与 QUIC 侧对称：用 send/recv 闭包封装，避免半包写入
				cc := &Conn{
					kind:    KindTCP,
					send:    func(b []byte) error { return writeAll(c, b) },
					recv:    func() ([]byte, error) { return readFrame(c) },
					closeFn: c.Close,
				}
				select {
				case out <- Accept{Kind: KindTCP, Conn: cc}:
				case <-ctx.Done():
					c.Close()
				}
			}(tc)
		}
	}()

	go func() {
		<-ctx.Done()
		ln.Close()
		tln.Close()
	}()

	return out, nil
}

// readFrame 读一个长度前缀帧（4 字节大端长度 + payload）
//
// 必须用 io.ReadFull，不能手写「循环 Read，遇 err 立即返回」的等价物：
// io.Reader 允许**同时**返回数据和 io.EOF，QUIC 的 ReceiveStream 在对端
// 写完数据即 FIN 时的最后一帧正是 (n>0, io.EOF)。手写循环遇到 err != nil
// 立刻返回，会把这一次刚读到的 n 字节整段丢掉——这正是 QUIC 回环「服务端
// 明明 write 成功、客户端却读到 EOF」的根因。io.ReadFull 在恰好读满目标
// 长度时忽略 EOF，语义才是「读满或失败」。
func readFrame(r io.Reader) ([]byte, error) {
	var lb [4]byte
	if _, err := io.ReadFull(r, lb[:]); err != nil {
		return nil, err
	}
	n := uint32(lb[0])<<24 | uint32(lb[1])<<16 | uint32(lb[2])<<8 | uint32(lb[3])
	if n > 256<<20 {
		return nil, fmt.Errorf("transport: frame too large: %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// writeAll 完整写入 b：io.Writer 允许返回 (n < len(b), nil)，只看 err 会把
// 半包当成功。QUIC stream 在设置写 deadline 时就会走这条路径。
func writeAll(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		b = b[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

// WriteFrame 写长度前缀帧（meta.WriteFrame 之外的传输层封装）
//
// 头与 payload 合成一次写入：分两次 Write 在对端看来可能落在不同 QUIC
// STREAM 帧里，虽然流内有序不影响正确性，但多一次 syscall/帧开销。
func WriteFrame(c *Conn, payload []byte) error {
	n := len(payload)
	buf := make([]byte, 4+n)
	buf[0], buf[1], buf[2], buf[3] = byte(n>>24), byte(n>>16), byte(n>>8), byte(n)
	copy(buf[4:], payload)
	return c.Send(buf)
}
