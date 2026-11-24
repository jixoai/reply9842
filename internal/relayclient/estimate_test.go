package relayclient

// estimate_test.go — 择路体积估计的护栏。
//
// 这个估计的价值不在于精确，而在于**量级正确**：量级错了，择路会把大请求
// 交给窄带线路，用户直接感知为「总耗时变慢」——而那正是 owner 要解决的问题。
// 下面三条覆盖早已出现过的真实形态。

import (
	"bytes"
	"testing"

	"github.com/jixoai/reply9842/internal/basefind"
)

// 小基线 + 大新增：LLM 上下文滚雪球的日常形态。
//
// 对抗审查指出的缺陷：复用率的分母是基线长度，所以「64 KB 基线 + 1.4 MB 新增」
// 算出 100% 命中，若按「命中 = 4 KB」估，会把 1.5 MB 的请求当成小请求。
// 正确的估计必须反映「那 1.4 MB 仍要传」。
func TestEstimateWireSmallBaseLargeAddition(t *testing.T) {
	f := basefind.New(64)

	// 基线：64 KB
	base := bytes.Repeat([]byte("abcdefg"), 64*1024/7+1)[:64*1024]
	f.Remember(base)

	// 新请求：完整基线前缀 + 1.4 MB 新增
	body := append(append([]byte{}, base...),
		bytes.Repeat([]byte("ZYXWVUT"), 1400*1024/7+1)[:1400*1024]...)

	est := estimateWireBytes(f, body, 16<<10)

	// 未覆盖部分 ≈ 1.4 MB，按 0.2 估 ≈ 280 KB。
	// 断言只要求「显著大于 4 KB 的下限」——量级正确即可，不锁死经验比。
	if est < 200<<10 {
		t.Fatalf("估计 %d 字节过低：1.4MB 未覆盖内容被当成小请求（会把大请求塞进窄带线路）", est)
	}
	if est > int64(len(body)) {
		t.Fatalf("估计 %d 超过明文 %d：不该高估到这种程度", est, len(body))
	}
}

// 完整命中（新请求就是基线的微改）：应估成很小的值（几百字节量级）
func TestEstimateWireFullHit(t *testing.T) {
	f := basefind.New(64)
	base := bytes.Repeat([]byte("abcdefg"), 1024*1024/7+1)[:1024*1024]
	f.Remember(base)

	// 几乎相同（末尾改 8 字节）
	body := append([]byte{}, base...)
	copy(body[len(body)-8:], []byte("CHANGED!"))

	est := estimateWireBytes(f, body, 16<<10)
	if est > 64<<10 {
		t.Fatalf("几乎完整命中的请求估计 %d 字节，太大了（应是小值）", est)
	}
	if est < 1024 {
		t.Fatalf("估计 %d 低于下限 1KB", est)
	}
}

// 无基线的大请求：按 0.2 经验比估
func TestEstimateWireNoBase(t *testing.T) {
	f := basefind.New(1) // 极小预算，不 Remember 任何东西
	body := bytes.Repeat([]byte("N"), 500*1024)
	est := estimateWireBytes(f, body, 16<<10)
	if est < 64<<10 || est > int64(len(body)) {
		t.Fatalf("无基线 500KB 请求估计 %d 字节，应在 [64KB, 500KB] 区间", est)
	}
	// 应接近 100KB（0.2×500KB）
	if est < 80<<10 || est > 130<<10 {
		t.Logf("提示：估计 %d 偏离 0.2 经验比较多（未阻塞，仅记录）", est)
	}
}

// 小请求（低于 minDelta）：直接按明文估，不做 delta
func TestEstimateWireSmallRequest(t *testing.T) {
	f := basefind.New(64)
	f.Remember(bytes.Repeat([]byte("x"), 1024*1024))
	body := bytes.Repeat([]byte("y"), 8*1024) // 8KB < 16KB minDelta
	est := estimateWireBytes(f, body, 16<<10)
	if est != int64(len(body)) {
		t.Fatalf("小请求应按明文估 %d，实际 %d", len(body), est)
	}
}
