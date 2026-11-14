// Package netif —— 「直连」模式要用的本机出口地址探测。
//
// 存在的理由：代理软件的 TUN 全局模式会把整个 IPv4 路由接管到虚拟网卡
// （macOS 实测：route get 47.82.76.102 → 198.18.0.1 / utun5），于是连隧道
// 服务器的包也进了代理，代理对 28443 这类非标端口没有正确规则，TLS 被切断
// （客户端只看到 "tcp tls: EOF" 或 QUIC 超时）。
//
// 解法是把出站源地址绑到**物理网卡**的 IP：内核随后直接走该网卡的 ARP/路由，
// 不再进 utun。要绑哪个 IP 不该让用户去查——这就是本包做的事。
package netif

import (
	"fmt"
	"net"
	"strings"
)

// 虚拟/隧道网卡的前缀与关键字：这些接口上的地址不能当出口。
var virtualPrefixes = []string{
	"utun", "tun", "tap", "ipsec", "ppp", "awdl", "llw", "bridge", "gif", "stf",
	"docker", "veth", "vmnet", "vboxnet", "tailscale", "zt", "wg", "flannel", "cni",
}

// 真实网卡前缀（优先级从高到低）：有线/无线优先于其它。
var realPrefixes = []string{"en", "eth", "wlan", "wl", "Ethernet", "Wi-Fi"}

// PrimaryIPv4 返回最适合做出口的物理网卡 IPv4（点分十进制）。
//
// 规则（按顺序）：
//  1. 跳过 down / loopback / 点对点接口
//  2. 跳过虚拟网卡（utun/tun/docker/vmnet/... 见 virtualPrefixes）
//  3. 只要 IPv4、非回环、非链路本地（169.254/16）
//  4. 真实网卡前缀优先；同优先级取第一个 up 的（多网卡时通常 en0 在前）
//
// 找不到时返回错误，调用方应退回「系统默认路由」而不是硬绑一个错地址。
func PrimaryIPv4() (ip string, iface string, err error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", "", fmt.Errorf("netif: 枚举网卡: %w", err)
	}

	type cand struct {
		ip    string
		iface string
		prio  int
	}
	var best *cand
	for _, in := range ifaces {
		if in.Flags&net.FlagUp == 0 || in.Flags&net.FlagLoopback != 0 {
			continue
		}
		if in.Flags&net.FlagPointToPoint != 0 {
			continue
		}
		if isVirtual(in.Name) {
			continue
		}
		addrs, aerr := in.Addrs()
		if aerr != nil {
			continue
		}
		for _, a := range addrs {
			var ipnet *net.IPNet
			switch v := a.(type) {
			case *net.IPNet:
				ipnet = v
			case *net.IPAddr:
				ipnet = &net.IPNet{IP: v.IP, Mask: net.CIDRMask(32, 32)}
			default:
				continue
			}
			v4 := ipnet.IP.To4()
			if v4 == nil || v4.IsLoopback() || v4.IsLinkLocalUnicast() {
				continue
			}
			c := cand{ip: v4.String(), iface: in.Name, prio: prioOf(in.Name)}
			if best == nil || c.prio > best.prio {
				best = &c
			}
			break
		}
	}
	if best == nil {
		return "", "", fmt.Errorf("netif: 找不到可用的物理网卡 IPv4")
	}
	return best.ip, best.iface, nil
}

func isVirtual(name string) bool {
	low := strings.ToLower(name)
	for _, p := range virtualPrefixes {
		if strings.HasPrefix(low, p) {
			return true
		}
	}
	return false
}

func prioOf(name string) int {
	for i, p := range realPrefixes {
		if strings.HasPrefix(name, p) {
			return len(realPrefixes) - i
		}
	}
	return 0
}
