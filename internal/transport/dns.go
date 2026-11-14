package transport

// dns.go —— 「直连」模式下自己解析域名。
//
// 为什么需要：代理的 TUN 全局模式同时劫持 DNS——系统解析器拿回来的是
// fake-IP（实测 magpie.aiweb.xin → 198.18.x.x）。绑源地址能让 TCP 绕开
// utun，但**解析这一步已经错了**：拿到的 IP 根本不存在。
//
// 所以直连模式自己发一份 DNS 查询，源地址同样绑到物理网卡：包从物理网卡
// 出去、绕开代理的 DNS 拦截，拿到真实 A/AAAA 记录。系统解析器只在没有绑定
// 源地址时使用（那是「系统」模式，出问题也是用户选择的语义）。

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"time"
)

// 公共 DNS：按顺序尝试。223.5.5.5(阿里) / 119.29.29.29(腾讯)。
var dnsServers = []string{"223.5.5.5:53", "119.29.29.29:53"}

// resolveHost 解析域名。host 是 IP 字面量时原样返回。
//
// bindIP 非空时用自建 UDP 查询（源地址绑定到该 IP，绕开 TUN 的 DNS 劫持）；
// 为空则退回系统解析器。
func resolveHost(ctx context.Context, host, bindIP string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	if bindIP == "" {
		return net.DefaultResolver.LookupIP(ctx, "ip", host)
	}

	var lastErr error
	for _, srv := range dnsServers {
		ips, err := dnsQuery(ctx, srv, host, bindIP)
		if err == nil && len(ips) > 0 {
			return ips, nil
		}
		if err != nil {
			lastErr = err
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("dns: %s 无 A/AAAA 记录", host)
	}
	// 自建查询失败时退回系统解析：可能只是这台机器挡住了 53/UDP，
	// 系统解析器（可能走 DoH）反而是通的。
	if ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host); err == nil && len(ips) > 0 {
		return ips, nil
	}
	return nil, lastErr
}

// dnsQuery 发一个 A（必要时 AAAA）查询并解析答案。
func dnsQuery(ctx context.Context, server, host, bindIP string) ([]net.IP, error) {
	for _, qtype := range []uint16{1, 28} { // A, AAAA
		ips, err := dnsQueryType(ctx, server, host, bindIP, qtype)
		if err == nil && len(ips) > 0 {
			return ips, nil
		}
	}
	return nil, fmt.Errorf("dns: %s 在 %s 无结果", host, server)
}

func dnsQueryType(ctx context.Context, server, host, bindIP string, qtype uint16) ([]net.IP, error) {
	var la *net.UDPAddr
	if bindIP != "" {
		la = &net.UDPAddr{IP: net.ParseIP(bindIP)}
	}
	d := net.Dialer{LocalAddr: la, Timeout: 3 * time.Second}
	conn, err := d.DialContext(ctx, "udp", server)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	}

	msg, id, err := buildQuery(host, qtype)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(msg); err != nil {
		return nil, err
	}
	buf := make([]byte, 1500)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, err
	}
	return parseAnswers(buf[:n], id, qtype)
}

func buildQuery(host string, qtype uint16) ([]byte, uint16, error) {
	id := uint16(rand.Intn(1 << 16))
	b := make([]byte, 12, 12+len(host)+6)
	binary.BigEndian.PutUint16(b[0:], id)
	binary.BigEndian.PutUint16(b[2:], 0x0100) // RD
	binary.BigEndian.PutUint16(b[4:], 1)      // QDCOUNT
	for _, label := range splitLabels(host) {
		if len(label) > 63 {
			return nil, 0, errors.New("dns: label 过长")
		}
		b = append(b, byte(len(label)))
		b = append(b, label...)
	}
	b = append(b, 0)
	b = append(b, byte(qtype>>8), byte(qtype))
	b = append(b, 0, 1) // IN
	return b, id, nil
}

func splitLabels(host string) [][]byte {
	var out [][]byte
	start := 0
	for i := 0; i <= len(host); i++ {
		if i == len(host) || host[i] == '.' {
			if i > start {
				out = append(out, []byte(host[start:i]))
			}
			start = i + 1
		}
	}
	return out
}

// parseAnswers 只取 A/AAAA 记录，忽略 CNAME 等其它类型。
func parseAnswers(msg []byte, id, qtype uint16) ([]net.IP, error) {
	if len(msg) < 12 || binary.BigEndian.Uint16(msg[0:]) != id {
		return nil, errors.New("dns: 响应无效")
	}
	if rcode := msg[3] & 0x0F; rcode != 0 {
		return nil, fmt.Errorf("dns: rcode=%d", rcode)
	}
	qd := int(binary.BigEndian.Uint16(msg[4:]))
	an := int(binary.BigEndian.Uint16(msg[6:]))
	off := 12
	for i := 0; i < qd; i++ {
		off = skipName(msg, off)
		if off < 0 || off+4 > len(msg) {
			return nil, errors.New("dns: 问题段截断")
		}
		off += 4
	}
	var out []net.IP
	for i := 0; i < an; i++ {
		off = skipName(msg, off)
		if off < 0 || off+10 > len(msg) {
			return nil, errors.New("dns: 答案段截断")
		}
		typ := binary.BigEndian.Uint16(msg[off:])
		rdlen := int(binary.BigEndian.Uint16(msg[off+8:]))
		off += 10
		if off+rdlen > len(msg) {
			return nil, errors.New("dns: rdata 截断")
		}
		switch typ {
		case 1:
			if rdlen == 4 && qtype == 1 {
				out = append(out, net.IPv4(msg[off], msg[off+1], msg[off+2], msg[off+3]))
			}
		case 28:
			if rdlen == 16 && qtype == 28 {
				ip := make(net.IP, 16)
				copy(ip, msg[off:off+16])
				out = append(out, ip)
			}
		}
		off += rdlen
	}
	return out, nil
}

// skipName 跳过一个（可能带压缩指针的）域名，返回其后第一个字节的偏移。
func skipName(msg []byte, off int) int {
	for {
		if off >= len(msg) {
			return -1
		}
		l := int(msg[off])
		if l == 0 {
			return off + 1
		}
		if l&0xC0 == 0xC0 { // 压缩指针：2 字节
			return off + 2
		}
		off += 1 + l
	}
}
