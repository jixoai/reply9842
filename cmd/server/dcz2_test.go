package main

// dcz2 上行解码的接线测试（-dcz2 开关的两条路径）。
//
// 只测 decodeBody 本身：它是「服务端两种编码都能解」的唯一分派点，
// 不需要起来整条链路就能把开关语义钉住。端到端（真实客户端 + 真隧道）的
// 协商证据在 internal/relayclient 与 internal/transport 的测试里。

import (
	"bytes"
	"strings"
	"testing"

	"github.com/jixoai/reply9842/internal/codec"
	"github.com/jixoai/reply9842/internal/meta"
	"github.com/jixoai/reply9842/internal/store"
)

// newDcz2TestServer 造一个只供 decodeBody 用的最小 server（store 是唯一依赖）
func newDcz2TestServer(t *testing.T, dcz2 bool) (*server, *store.Store) {
	t.Helper()
	st := store.New(8 << 20)
	return &server{c: &cfg{dcz2: dcz2}, store: st}, st
}

// dcz2Fixture 造一份「基版本 + 下一版本」的 dcz2 载荷与声明头
func dcz2Fixture(t *testing.T) (head *meta.RequestHead, wire, plain []byte) {
	t.Helper()
	base := []byte(strings.Repeat("{\"role\":\"user\",\"content\":\"base body \"},", 400))
	next := append(append([]byte{}, base...), []byte("{\"role\":\"user\",\"content\":\"next\"}")...)
	w, err := codec.EncodeDcz2(next, 19, base)
	if err != nil {
		t.Fatalf("EncodeDcz2: %v", err)
	}
	// 字典靠帧里的 BaseHash（BLAKE3）定位：store 的 key 恒为 BLAKE3
	head = &meta.RequestHead{
		Encoding: meta.EncDcz2,
		BaseHash: convertHash(codec.Sum(base)),
		BodyLen:  int64(len(w)),
	}
	return head, w, next
}

// TestDecodeBodyDcz2Enabled 开关开启（默认）：EncDcz2 被正常解出明文。
func TestDecodeBodyDcz2Enabled(t *testing.T) {
	s, st := newDcz2TestServer(t, true)
	head, wire, plain := dcz2Fixture(t)
	st.Put([]byte(strings.Repeat("{\"role\":\"user\",\"content\":\"base body \"},", 400)))

	got, name, miss, err := s.decodeBody(head, wire)
	if err != nil || miss {
		t.Fatalf("decodeBody(dcz2) = miss=%v err=%v，期望成功", miss, err)
	}
	if name != "dcz2" {
		t.Errorf("编码名 = %q，期望 dcz2", name)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("解出的明文不符：%d 字节 vs %d", len(got), len(plain))
	}
}

// TestDecodeBodyDcz2KillSwitchOff 开关关闭（-dcz2=false）：EncDcz2 必须被**明确拒绝**。
//
// 语义是「回到 v0.3.10」：只 accept reply9842/1，新客户端在协商层就回落到私有 dcz，
// 根本不会发出 EncDcz2；万一有人手工构造出这种帧，也不能悄悄按 dcz 试解
// （两种格式互不兼容，试解只会失败或更糟）。
func TestDecodeBodyDcz2KillSwitchOff(t *testing.T) {
	s, st := newDcz2TestServer(t, false)
	head, wire, _ := dcz2Fixture(t)
	st.Put([]byte(strings.Repeat("{\"role\":\"user\",\"content\":\"base body \"},", 400)))

	got, name, miss, err := s.decodeBody(head, wire)
	if err == nil {
		t.Fatalf("开关关闭时 EncDcz2 必须报错，却解出 %d 字节", len(got))
	}
	if miss {
		t.Errorf("拒绝不是 delta-miss（不是让客户端重发，而是明确失败）")
	}
	if name != "dcz2" {
		t.Errorf("编码名 = %q，期望 dcz2（错误信息要能定位到编码）", name)
	}
	if !strings.Contains(err.Error(), "dcz2") {
		t.Errorf("错误信息必须带编码名，得到 %q", err.Error())
	}
}

// TestDecodeBodyDcz2StoreMiss store 里没有该基线 → miss（客户端重发全量），
// 而不是报错或解出垃圾。
func TestDecodeBodyDcz2StoreMiss(t *testing.T) {
	s, _ := newDcz2TestServer(t, true)
	head, wire, _ := dcz2Fixture(t)
	// 故意不 Put 基线
	got, _, miss, err := s.decodeBody(head, wire)
	if err != nil {
		t.Fatalf("基线缺失不应是硬错误（应让客户端重发全量）: %v", err)
	}
	if !miss {
		t.Fatalf("基线缺失应判 miss，却没判（解出 %d 字节）", len(got))
	}
}

// TestDecodeBodyDcz2BaseHashMismatch 声明的 BaseHash 与头里的 SHA-256 不符
// （例如客户端算错/字典被换）→ 不能解出内容，按 miss 让客户端重发。
func TestDecodeBodyDcz2BaseHashMismatch(t *testing.T) {
	s, st := newDcz2TestServer(t, true)
	head, wire, _ := dcz2Fixture(t)
	// 往 store 里放一份**别的**正文，并让 BaseHash 指过去
	other := []byte(strings.Repeat("totally different dictionary content ", 200))
	oh := st.Put(other)
	head.BaseHash = convertHash(oh)

	got, _, miss, err := s.decodeBody(head, wire)
	if err != nil {
		t.Fatalf("不应硬错误: %v", err)
	}
	if !miss {
		t.Fatalf("错字典必须判 miss，却解出 %d 字节（静默损坏风险）", len(got))
	}
}

// TestDecodeBodyDczStillWorks 回滚开关不能影响私有 dcz：老客户端（线上）在
// -dcz2=false 时行为必须与今天逐字节一致。
func TestDecodeBodyDczStillWorks(t *testing.T) {
	for _, dcz2 := range []bool{true, false} {
		s, st := newDcz2TestServer(t, dcz2)
		base := []byte(strings.Repeat("{\"role\":\"user\",\"content\":\"base body \"},", 400))
		next := append(append([]byte{}, base...), []byte("{\"role\":\"user\",\"content\":\"next\"}")...)
		st.Put(base)
		wire, err := codec.EncodeDcz(next, codec.Sum(base), 19, base)
		if err != nil {
			t.Fatal(err)
		}
		head := &meta.RequestHead{
			Encoding: meta.EncDcz,
			BaseHash: convertHash(codec.Sum(base)),
			BodyLen:  int64(len(wire)),
		}
		got, name, miss, derr := s.decodeBody(head, wire)
		if derr != nil || miss {
			t.Fatalf("dcz2=%v: 私有 dcz 解码失败 miss=%v err=%v", dcz2, miss, derr)
		}
		if name != "dcz" || !bytes.Equal(got, next) {
			t.Fatalf("dcz2=%v: 私有 dcz 解出 %d 字节（name=%s）", dcz2, len(got), name)
		}
	}
}
