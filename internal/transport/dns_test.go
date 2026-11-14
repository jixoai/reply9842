package transport

import (
	"encoding/binary"
	"net"
	"testing"
)

// buildResp 造一个带压缩指针的 A 记录响应，验证解析器能跳过 CNAME 与指针名。
func buildResp(id uint16, name string, ips [][4]byte) []byte {
	b := make([]byte, 12)
	binary.BigEndian.PutUint16(b[0:], id)
	binary.BigEndian.PutUint16(b[2:], 0x8180) // 标准响应，RD+RA
	binary.BigEndian.PutUint16(b[4:], 1)      // QDCOUNT
	binary.BigEndian.PutUint16(b[6:], uint16(len(ips)+1))
	// 问题段
	for _, l := range splitLabels(name) {
		b = append(b, byte(len(l)))
		b = append(b, l...)
	}
	b = append(b, 0, 0, 1, 0, 1) // type A, class IN
	// 一条 CNAME（用压缩指针指向问题段的名字）
	b = append(b, 0xC0, 12)
	b = append(b, 0, 5, 0, 1, 0, 0, 0, 60, 0, 2)
	b = append(b, 0xC0, 12)
	// A 记录
	for _, ip := range ips {
		b = append(b, 0xC0, 12)
		b = append(b, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4)
		b = append(b, ip[:]...)
	}
	return b
}

// TestParseAnswersSkipsCNAME 解析要拿到 A 记录，且能跳过 CNAME 与压缩指针。
func TestParseAnswersSkipsCNAME(t *testing.T) {
	resp := buildResp(0x1234, "relay.example.com", [][4]byte{{47, 82, 76, 102}, {1, 2, 3, 4}})
	binary.BigEndian.PutUint16(resp[6:], 3) // 2 条 A + 1 条 CNAME
	ips, err := parseAnswers(resp, 0x1234, 1)
	if err != nil {
		t.Fatalf("parseAnswers: %v", err)
	}
	if len(ips) != 2 {
		t.Fatalf("期望 2 个 A 记录，得到 %v", ips)
	}
	if !ips[0].Equal(net.IPv4(47, 82, 76, 102)) {
		t.Errorf("第一个 IP = %v", ips[0])
	}
}

// TestParseAnswersRejectsBadID 事务 ID 不匹配的响应必须丢弃（防投毒）。
func TestParseAnswersRejectsBadID(t *testing.T) {
	resp := buildResp(0x1234, "relay.example.com", [][4]byte{{1, 1, 1, 1}})
	if _, err := parseAnswers(resp, 0x9999, 1); err == nil {
		t.Fatal("ID 不匹配应当报错")
	}
}

// TestResolveHostPassesIPLiteral IP 字面量不该走 DNS。
func TestResolveHostPassesIPLiteral(t *testing.T) {
	ips, err := resolveHost(t.Context(), "47.82.76.102", "")
	if err != nil || len(ips) != 1 || !ips[0].Equal(net.ParseIP("47.82.76.102")) {
		t.Fatalf("resolveHost(IP) = %v, %v", ips, err)
	}
}
