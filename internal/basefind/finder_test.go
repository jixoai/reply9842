package basefind

import (
	"bytes"
	"testing"

	"github.com/jixoai/reply9842/internal/codec"
)

// 造一个「前缀相同、尾部不同」的请求体（模拟 agent 会话推进）
func makeBody(prefixLen, tailLen int, tail byte) []byte {
	b := bytes.Repeat([]byte{'a'}, prefixLen)
	return append(b, bytes.Repeat([]byte{tail}, tailLen)...)
}

func TestPickFindsPrefixMatch(t *testing.T) {
	f := New(64)
	base := makeBody(65536, 100, 'x')
	f.Remember(base)

	// 新请求：前 1000 字节相同，尾部不同
	next := makeBody(65536, 200, 'y')
	h, body := f.Pick(next)
	if h.IsZero() {
		t.Fatal("should find a base")
	}
	if !bytes.Equal(body, base) {
		t.Fatal("returned wrong base body")
	}
}

func TestPickRejectsLowReuse(t *testing.T) {
	f := New(64)
	// 历史体：前 100 字节 a，后 9000 字节 x
	f.Remember(makeBody(100, 9000, 'x'))

	// 新请求：前 100 字节 a，后 9000 字节 y（内容完全不同）
	// 块命中率接近 0，应拒绝
	next := makeBody(100, 9000, 'y')
	h, _ := f.Pick(next)
	if !h.IsZero() {
		t.Fatal("低复用率应被拒绝")
	}
}

func TestPickNewestFirst(t *testing.T) {
	f := New(64)
	old := makeBody(65536, 100, 'x')
	f.Remember(old)
	newer := makeBody(65536, 100, 'y')
	f.Remember(newer)

	// 两者前缀都能匹配（都是 1000 个 a）。Pick 应从新到旧，返回 newer。
	h, body := f.Pick(makeBody(65536, 300, 'z'))
	if h.IsZero() {
		t.Fatal("should find base")
	}
	if !bytes.Equal(body, newer) {
		t.Fatal("should prefer newest candidate")
	}
}

func TestByteBudgetEviction(t *testing.T) {
	// 预算 1MB，每条约 400KB，最多存 2 条
	f := New(1)
	for i := 0; i < 5; i++ {
		f.Remember(makeBody(400000, 4096, byte('a'+i)))
	}
	if f.Bytes() > 1<<20 {
		t.Fatalf("exceeded budget: %d bytes", f.Bytes())
	}
	if f.Len() > 3 {
		t.Fatalf("too many candidates: %d", f.Len())
	}
	t.Logf("kept %d candidates, %d bytes", f.Len(), f.Bytes())
}

func TestDedupSameBody(t *testing.T) {
	f := New(64)
	b := makeBody(65536, 100, 'x')
	f.Remember(b)
	f.Remember(b)
	f.Remember(b)
	if f.Len() != 1 {
		t.Fatalf("same body should dedup, got %d", f.Len())
	}
}

func TestNoHistory(t *testing.T) {
	f := New(64)
	h, body := f.Pick(makeBody(65536, 100, 'x'))
	if !h.IsZero() || body != nil {
		t.Fatal("empty finder should return zero hash")
	}
}

// 关键场景：基线比新请求短，但前缀被充分复用。
//
// 基线 1000 字节（全 a），新请求 3000 字节（前 1000 是 a，后 2000 是 b）。
// 按「占新请求」算：1000/3000 = 33% → 会误判为不值得
// 按「占基线」算：  1000/1000 = 100% → 正确判定值得（整个字典都被复用）
func TestPickRatioAgainstBaseline(t *testing.T) {
	f := New(64)
	base := bytes.Repeat([]byte{'a'}, 65536)
	f.Remember(base)

	newReq := append(bytes.Repeat([]byte{'a'}, 65536), bytes.Repeat([]byte{'b'}, 131072)...)
	h, body := f.Pick(newReq)
	if h.IsZero() {
		t.Fatal("should accept: entire baseline is reused as prefix")
	}
	if !bytes.Equal(body, base) {
		t.Fatal("wrong base")
	}
}

// 反向场景：基线比新请求长得多，前缀只占基线一小部分 → 应拒绝
func TestPickRejectsWhenBaselineMostlyUnused(t *testing.T) {
	f := New(64)
	// 基线 10000 字节：前 100 是 a，后 9900 是 x
	base := append(bytes.Repeat([]byte{'a'}, 4096), bytes.Repeat([]byte{'x'}, 400000)...)
	f.Remember(base)

	// 新请求：前 4096 是 a，后 8192 是 y
	// 只有 1 块命中 → 复用率 ~1% → 拒绝
	newReq := append(bytes.Repeat([]byte{'a'}, 4096), bytes.Repeat([]byte{'y'}, 8192)...)
	h, _ := f.Pick(newReq)
	if !h.IsZero() {
		t.Fatal("should reject: baseline mostly unused")
	}
}

// Forget 只删指定那条，其它基线保留
func TestForgetKeepsOthers(t *testing.T) {
	f := New(1)
	// 两条内容完全不同、且各自够长（Pick 要求基线 >= blockSize）
	a := makeBody(8192, 0, 'a')
	b := bytes.Repeat([]byte{'b'}, 8192)
	ha := f.Remember(a)
	f.Remember(b)

	if f.Len() != 2 {
		t.Fatalf("应有 2 条，实际 %d", f.Len())
	}
	f.Forget(ha)
	if f.Len() != 1 {
		t.Fatalf("应剩 1 条，实际 %d", f.Len())
	}
	// 用 b 自身当新请求：复用率 100%，应命中 b
	if _, got := f.Pick(b); got == nil {
		t.Fatal("另一条基线不该被误删")
	}
}

// Forget 丢掉指定基线（跨 server 的 delta-miss 会触发它）。
// 真实 e2e 抓出的缺陷：不丢的话会成为「每次都 miss、每次都全量」的稳定劣化。
func TestForgetRemovesBaseline(t *testing.T) {
	f := New(1)
	body := makeBody(1024, 0, 'x')
	h := f.Remember(body)

	if !f.Forget(h) {
		t.Fatal("Forget 应报告找到了并删除")
	}
	if f.Len() != 0 {
		t.Fatalf("删除后应剩 0 条，实际 %d", f.Len())
	}
	if _, b := f.Pick(body); b != nil {
		t.Fatal("删除后不该还能 Pick 到它")
	}
	// 幂等/未知哈希：返回 false 不 panic
	if f.Forget(h) {
		t.Fatal("重复 Forget 应返回 false")
	}
	if f.Forget(codec.Hash{}) {
		t.Fatal("零哈希应返回 false")
	}
}

// Forget 只删指定那条，其它基线保留
