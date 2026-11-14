package netif

import (
	"net"
	"strings"
	"testing"
)

// TestPrimaryIPv4ReturnsPhysical 探测结果必须是物理网卡上的地址。
func TestPrimaryIPv4ReturnsPhysical(t *testing.T) {
	ip, iface, err := PrimaryIPv4()
	if err != nil {
		t.Skipf("本机没有可用物理网卡：%v", err)
	}
	parsed := net.ParseIP(ip)
	if parsed == nil || parsed.To4() == nil {
		t.Fatalf("返回的不是 IPv4：%q", ip)
	}
	if isVirtual(iface) {
		t.Fatalf("返回了虚拟网卡 %q（TUN/容器网卡不能当出口）", iface)
	}
}

// TestIsVirtual 虚拟网卡识别：漏一个就会把出站绑到 utun 上，等于没绕过代理。
func TestIsVirtual(t *testing.T) {
	virtual := []string{"utun5", "tun0", "tap0", "docker0", "veth1a2b", "vmnet8", "tailscale0", "wg0"}
	real := []string{"en0", "en1", "eth0", "wlan0", "Wi-Fi", "Ethernet"}
	for _, n := range virtual {
		if !isVirtual(n) {
			t.Errorf("%q 应判为虚拟网卡", n)
		}
	}
	for _, n := range real {
		if isVirtual(n) {
			t.Errorf("%q 不应判为虚拟网卡", n)
		}
	}
}

// TestPriorityPhysicalFirst 真实网卡优先级高于其它（多网卡时选有线/无线）。
func TestPriorityPhysicalFirst(t *testing.T) {
	if prioOf("en0") <= prioOf("somethingelse") {
		t.Errorf("en0 优先级应高于未知网卡")
	}
	if !strings.HasPrefix("en0", "en") {
		t.Fatal("sanity")
	}
}
