package codec

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// 构造贴近真实 agent 请求体的负载：长 system + tools schema + 多轮历史
func makePayload(turns int, seed string) []byte {
	var b strings.Builder
	b.WriteString(`{"model":"deepseek-v4.1-flash","system":"You are a coding agent. Follow rules carefully. `)
	b.WriteString(strings.Repeat("Rule text here. ", 400))
	b.WriteString(`","tools":[`)
	for i := 0; i < 30; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"type":"function","function":{"name":"tool_`)
		b.WriteString(seed)
		b.WriteString(`_`)
		b.WriteString(strings.Repeat("x", 1))
		b.WriteString(`","description":"`)
		b.WriteString(strings.Repeat("Does something useful. ", 20))
		b.WriteString(`"}}`)
	}
	b.WriteString(`],"messages":[`)
	for i := 0; i < turns; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"role":"user","content":"analyze this file `)
		b.WriteString(strings.Repeat("detail ", 50))
		b.WriteString(`"},{"role":"assistant","content":"`)
		b.WriteString(strings.Repeat("Here is my analysis. ", 60))
		b.WriteString(`"},{"role":"tool","content":"`)
		b.WriteString(strings.Repeat("line of source code here\n", 200))
		b.WriteString(`"}`)
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

func TestSumDeterministic(t *testing.T) {
	a := []byte("hello world")
	if Sum(a) != Sum(a) {
		t.Fatal("hash not deterministic")
	}
	if Sum(a) == Sum([]byte("hello worle")) {
		t.Fatal("different content produced same hash")
	}
}

func TestZstdRoundTrip(t *testing.T) {
	body := makePayload(5, "a")
	for _, lvl := range []int{1, 9, 19} {
		z, err := Compress(body, lvl, nil)
		if err != nil {
			t.Fatalf("level %d compress: %v", lvl, err)
		}
		got, err := Decompress(z, nil)
		if err != nil {
			t.Fatalf("level %d decompress: %v", lvl, err)
		}
		if !bytes.Equal(body, got) {
			t.Fatalf("level %d roundtrip mismatch: %d vs %d bytes", lvl, len(body), len(got))
		}
		t.Logf("level %2d: %d -> %d bytes (%.1f%%)", lvl, len(body), len(z),
			float64(len(z))/float64(len(body))*100)
	}
}

func TestDczRoundTrip(t *testing.T) {
	base := makePayload(5, "a")
	// 下一轮：尾部追加（真实 agent 形态）
	next := append([]byte{}, base[:len(base)-1]...)
	next = append(next, []byte(`,{"role":"user","content":"continue"}]}`)...)

	baseHash := Sum(base)
	z, err := EncodeDcz(next, baseHash, 19, base)
	if err != nil {
		t.Fatalf("encode dcz: %v", err)
	}
	if len(z) < HeaderLen {
		t.Fatalf("dcz payload too short: %d", len(z))
	}

	got, err := DecodeDcz(z, base)
	if err != nil {
		t.Fatalf("decode dcz: %v", err)
	}
	if !bytes.Equal(next, got) {
		t.Fatalf("dcz roundtrip mismatch")
	}

	// 关键收益：dcz 应远小于普通 zstd
	plain, _ := Compress(next, 19, nil)
	t.Logf("next=%d  dcz=%d  zstd=%d  ratio_vs_plain=%.4f",
		len(next), len(z), len(plain), float64(len(z))/float64(len(next)))
	if len(z) >= len(plain) {
		t.Errorf("dcz (%d) should be smaller than plain zstd (%d)", len(z), len(plain))
	}
}

func TestDczRejectsWrongDict(t *testing.T) {
	base := makePayload(5, "a")
	next := append([]byte{}, base...)
	next = append(next, []byte("x")...)

	z, err := EncodeDcz(next, Sum(base), 19, base)
	if err != nil {
		t.Fatal(err)
	}

	// 用错误的字典解码：必须失败，不能静默产出错数据
	wrong := makePayload(5, "b")
	if _, err := DecodeDcz(z, wrong); err == nil {
		t.Fatal("decoding with wrong dictionary must fail")
	}
}

func TestEncodeDczRejectsHashMismatch(t *testing.T) {
	base := makePayload(3, "a")
	// 声明一个错的 baseHash
	var wrong Hash
	wrong[0] = 0xFF
	if _, err := EncodeDcz([]byte("hello"), wrong, 19, base); err == nil {
		t.Fatal("expected hash mismatch error")
	}
}

func TestPeekDczHash(t *testing.T) {
	base := makePayload(3, "a")
	z, err := EncodeDcz([]byte("payload"), Sum(base), 19, base)
	if err != nil {
		t.Fatal(err)
	}
	h, err := PeekDczHash(z)
	if err != nil {
		t.Fatal(err)
	}
	if h != Sum(base) {
		t.Fatalf("peeked hash %s != expected %s", h, Sum(base))
	}
}

func TestPickLevel(t *testing.T) {
	cases := []struct{ size, want int }{
		{100 << 10, 19},
		{2 << 20, 19},
		{2<<20 + 1, 12},
		{8 << 20, 12},
		{8<<20 + 1, 9},
		{40 << 20, 9},
	}
	for _, c := range cases {
		if got := PickLevel(c.size); got != c.want {
			t.Errorf("PickLevel(%d) = %d, want %d", c.size, got, c.want)
		}
	}
}

// TestEncoderPoolBounded 编码器池必须是常数规模。
//
// key 里带字典哈希，而每轮会话的基版本都不同 —— 无限缓存等价于每个请求留一个
// level-19 编码器（单个可达数十 MB）。2026-11-14 实测另一台机器内存涨到 5GB，
// 来源就是这里。这条测试用 40 个不同字典压一轮，断言池不超过上限。
func TestEncoderPoolBounded(t *testing.T) {
	p := newEncPool()
	body := bytes.Repeat([]byte("abc"), 1000)
	for i := 0; i < 40; i++ {
		dict := []byte(strings.Repeat("d", 100+i))
		e, k, err := p.acquire(19, dict)
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		_ = e.EncodeAll(body, nil)
		p.release(k, e)
	}
	if n := p.poolSize(); n > encPoolCap {
		t.Fatalf("池里有 %d 个实例，上限是 %d", n, encPoolCap)
	}
}

// TestCompressConcurrentSameDict 同一个字典的并发压缩不能共享编码器实例。
//
// zstd.Encoder 的 EncodeAll 不保证并发安全：两个 goroutine 同时用同一个实例
// 会踩内部缓冲区（输出损坏 / 崩溃）。池必须交出独占实例。
func TestCompressConcurrentSameDict(t *testing.T) {
	dict := bytes.Repeat([]byte("shared-dict-"), 500)
	body := bytes.Repeat([]byte("payload-"), 3000)
	want, err := Compress(body, 19, dict)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				got, err := Compress(body, 19, dict)
				if err != nil {
					errs <- err
					return
				}
				if !bytes.Equal(got, want) {
					errs <- fmt.Errorf("并发压缩结果不一致：%d vs %d 字节", len(got), len(want))
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}
