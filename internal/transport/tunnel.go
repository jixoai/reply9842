// Package transport — QUIC / TCP+TLS 双栈隧道
//
// 正交意图（3 个，2026-11-12 kzf 需求：「QUIC 和 TCP+TLS 两路同时做，但我记得
// QUIC 是可以优雅回退成 TCP 传输层才对啊」）：
//   1. 统一 Conn 抽象：QUIC stream 与 TCP 连接在帧层面等价
//   2. auto 模式：客户端先试 QUIC（1s 超时），失败降级 TCP+TLS，并记住结果
//   3. 自签/系统根证书双模式（观察期用自签，正式环境用真证书）
//
// 关于「QUIC 优雅回退」的事实澄清：
//   HTTP/3 的回退是「HTTP 协议族」的能力（浏览器用 Alt-Svc 探测，失败退回
//   HTTP/2 over TCP），**不是 QUIC 传输层自带**。quic-go 连不上就是连不上。
//   因此回退逻辑必须在本层实现——即下面的 DialAuto。
package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
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
const ALPN = "rfc9842-reply/1"

// Dialer 客户端连接工厂
type Dialer struct {
	Addr      string // host:port
	ServerName string // TLS SNI；留空则从 Addr 推导
	Insecure  bool   // 跳过证书校验（自签观察期）
	Mode      Kind   // auto / quic / tcp
	QuicProbe time.Duration // auto 模式下 QUIC 探测超时，默认 1s

	// auto 模式的粘性记忆：一旦某模式成功，后续优先该模式
	prefer atomic.Value // Kind
	lastKind atomic.Value // Kind
}

// Conn 统一连接抽象
type Conn struct {
	kind   Kind
	send   func([]byte) error
	recv   func() ([]byte, error)
	closeFn func() error
}

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
	if d.QuicProbe <= 0 {
		d.QuicProbe = time.Second
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
			cctx, cancel := context.WithTimeout(ctx, d.QuicProbe)
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
	qc, err := quic.DialAddr(ctx, d.Addr, tlsCfg, &quic.Config{
		HandshakeIdleTimeout: 5 * time.Second,
		MaxIdleTimeout:       120 * time.Second,
		KeepAlivePeriod:      30 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("quic dial: %w", err)
	}
	st, err := qc.OpenStreamSync(ctx)
	if err != nil {
		qc.CloseWithError(1, "open stream")
		return nil, fmt.Errorf("quic stream: %w", err)
	}
	return &Conn{
		kind: KindQUIC,
		send: func(b []byte) error { _, err := st.Write(b); return err },
		recv: func() ([]byte, error) { return readFrame(st) },
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
	nd := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
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
		kind: KindTCP,
		send: func(b []byte) error { _, err := tc.Write(b); return err },
		recv: func() ([]byte, error) { return readFrame(tc) },
		closeFn: tc.Close,
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
			// 一条 QUIC 连接可承载多个 stream；每个 stream 处理一次请求
			go func(qc *quic.Conn) {
				defer qc.CloseWithError(0, "done")
				for {
					st, err := qc.AcceptStream(ctx)
					if err != nil {
						return
					}
					go func(st *quic.Stream) {
						out <- Accept{Kind: KindQUIC, Conn: &Conn{
							kind: KindQUIC,
							send: func(b []byte) error { _, err := st.Write(b); return err },
							recv: func() ([]byte, error) { return readFrame(st) },
							closeFn: func() error { st.Close(); return nil },
						}}
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
				out <- Accept{Kind: KindTCP, Conn: &Conn{
					kind: KindTCP,
					send: func(b []byte) error { _, err := c.Write(b); return err },
					recv: func() ([]byte, error) { return readFrame(c) },
					closeFn: c.Close,
				}}
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
func readFrame(r interface{ Read([]byte) (int, error) }) ([]byte, error) {
	var lb [4]byte
	if _, err := readFull(r, lb[:]); err != nil {
		return nil, err
	}
	n := uint32(lb[0])<<24 | uint32(lb[1])<<16 | uint32(lb[2])<<8 | uint32(lb[3])
	if n > 256<<20 {
		return nil, fmt.Errorf("transport: frame too large: %d", n)
	}
	buf := make([]byte, n)
	if _, err := readFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func readFull(r interface{ Read([]byte) (int, error) }, b []byte) (int, error) {
	got := 0
	for got < len(b) {
		n, err := r.Read(b[got:])
		got += n
		if err != nil {
			return got, err
		}
	}
	return got, nil
}

// WriteFrame 写长度前缀帧（meta.WriteFrame 之外的传输层封装）
func WriteFrame(c *Conn, payload []byte) error {
	var lb [4]byte
	n := len(payload)
	lb[0], lb[1], lb[2], lb[3] = byte(n>>24), byte(n>>16), byte(n>>8), byte(n)
	if err := c.Send(lb[:]); err != nil {
		return err
	}
	return c.Send(payload)
}
