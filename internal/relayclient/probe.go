// probe.go — 「测试这条线路能不能用」。
//
// 存在的理由（owner 2026-11-24）：「我能不能先编辑表单、测试线路，最后再
// 确认添加？」——不能就先让人加进去再等它失败，是很差的交互：用户面对一条
// 无响应的线路，既不知道该改哪儿，也不知道是不是自己填错了。
//
// 探测只做**连接层**验证：拨号（含 TLS/代理/源地址绑定）→ 发一个 Ping 帧
// → 等回显。它回答的是「这条线路通不通、握手多久、往返多久」，不是
// 「传一个大请求要多久」（后者要靠真实请求的样本，见 route.Model）。
package relayclient

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jixoai/reply9842/internal/route"
	"github.com/jixoai/reply9842/internal/transport"
)

// ProbeResult 一次线路探测的结果。
type ProbeResult struct {
	// HandshakeMS 建连耗时（DNS + TCP + TLS 握手；QUIC 则是 QUIC 握手）。
	// 它是「固定开销」的粗略下限，不等于吞吐模型里的 a。
	HandshakeMS int64
	// RTTMS Ping→Pong 往返。只说明链路活着与大致延迟，**不作为择路依据**
	// （择路看预计完成时间）。
	RTTMS int64
	// Transport 实际协商到的传输方式（"tcp" / "quic"）。auto 模式下这条
	// 信息很有用：用户以为在跑 QUIC，实际早退回 TCP 了。
	Transport string
	// Dcz2 对端是否支持 dcz2（RFC 9842 §5）。旧服务端只认私有 dcz。
	Dcz2 bool
}

// ProbeRoute 用一条线路真实拨号并 Ping 一次。
//
// ctx 应带超时：这是「测试」按钮的后台任务，用户随时可能点取消或关窗口。
// 拨号成功后**必定关闭连接**——探测不该在池里留一条没人用的连接。
func ProbeRoute(ctx context.Context, r route.Route, insecure bool) (ProbeResult, error) {
	n := r.Normalize()
	if err := n.Validate(); err != nil {
		return ProbeResult{}, err
	}

	spec := transport.DialerSpec{
		ID:         n.ID,
		Addr:       n.Server,
		ServerName: n.ServerName,
		LocalAddr:  n.LocalAddr,
		Proxy:      n.Proxy,
		Mode:       transport.KindAuto,
		Insecure:   insecure,
	}
	// transport 的模式命名与 route.Kind 不同（那边是 KindAuto/KindTCP/KindQUIC），
	// 这里按 transport 字段映射。
	switch n.Transport {
	case "tcp":
		spec.Mode = transport.KindTCP
	case "quic":
		spec.Mode = transport.KindQUIC
	}

	d := spec.NewDialer()

	start := time.Now()
	conn, err := d.Dial(ctx)
	if err != nil {
		return ProbeResult{}, explainDialErr(n, err)
	}
	defer conn.Close()

	res := ProbeResult{
		HandshakeMS: time.Since(start).Milliseconds(),
		Transport:   string(conn.Kind()),
		Dcz2:        conn.SupportsDcz2(),
	}

	// Ping 与握手分开计时：用户看「通了但很慢」和「连都连不上」是两件事。
	pingStart := time.Now()
	if err := pingConn(conn); err != nil {
		return res, fmt.Errorf("握手成功（%d ms）但 Ping 失败：%w", res.HandshakeMS, err)
	}
	res.RTTMS = time.Since(pingStart).Milliseconds()
	return res, nil
}

// explainDialErr 给常见失败补一句「该改哪儿」。
//
// 为什么值得单独写：这两类错误的信息量几乎为零，用户看到只会以为是网络坏了，
// 而实际是配置缺字段——我们自己在多线路上踩过两次。
func explainDialErr(r route.Route, err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "no IP SANs") || strings.Contains(msg, "cannot validate certificate"):
		return fmt.Errorf("%w\n→ 这条线路指向 IP 但证书只签了域名：请在「证书域名」里填证书上的域名", err)
	case strings.Contains(msg, "tls: EOF") || strings.Contains(msg, "timeout"):
		return fmt.Errorf("%w\n→ 若本机开着代理的 TUN 全局模式，需在「源地址」绑定物理网卡 IP", err)
	case strings.Contains(msg, "missing port"):
		return fmt.Errorf("%w\n→ 服务器地址要写成 host:port", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w（探测超时：目标不可达或被中间设备阻断）", err)
	}
	return err
}
