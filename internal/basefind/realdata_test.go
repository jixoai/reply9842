package basefind

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// 用真实采集的请求体做回归验证。
//
// 数据来源：/tmp/rfc2/capture/*.bin（未随仓库分发）。缺失时跳过。
// 期望行为（2026-11-13 实测基准）：同簇配对复用率 ~89%，跨簇 ~2%。
func TestBlockReuseOnRealSamples(t *testing.T) {
	files, _ := filepath.Glob("/tmp/rfc2/capture/req-*.bin")
	if len(files) < 4 {
		t.Skip("需要至少 4 条真实样本（/tmp/rfc2/capture/req-*.bin）")
	}
	sort.Strings(files)

	bodies := make([][]byte, 0, len(files))
	names := make([]string, 0, len(files))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		bodies = append(bodies, b)
		names = append(names, filepath.Base(f))
	}

	// 同簇判定：体积差 < 10%
	sameCluster := func(a, b []byte) bool {
		diff := float64(abs(len(a)-len(b))) / float64(max(len(a), len(b)))
		return diff < 0.10
	}

	var sameVals, crossVals []float64
	for i := 0; i < len(bodies); i++ {
		for j := 0; j < len(bodies); j++ {
			if i == j {
				continue
			}
			r := blockReuseRatio(bodies[i], bodies[j])
			if sameCluster(bodies[i], bodies[j]) {
				sameVals = append(sameVals, r)
				t.Logf("同簇 %s vs %s: 复用率 %.1f%%", names[i], names[j], r*100)
			} else {
				crossVals = append(crossVals, r)
			}
		}
	}

	if len(sameVals) > 0 {
		avg := mean(sameVals)
		t.Logf("同簇平均复用率: %.1f%%（n=%d）", avg*100, len(sameVals))
		if avg < 0.5 {
			t.Errorf("同簇复用率过低: %.1f%%，应 >50%%（实测基准 89%%）", avg*100)
		}
	}
	if len(crossVals) > 0 {
		avg := mean(crossVals)
		t.Logf("跨簇平均复用率: %.1f%%（n=%d）", avg*100, len(crossVals))
		if avg > 0.3 {
			t.Errorf("跨簇复用率过高: %.1f%%，应 <30%%（实测基准 2%%）", avg*100)
		}
	}
}

// TestPickSkipsLeadingDivergence 覆盖真实场景：开头几十字节不同，之后大量相同。
func TestPickSkipsLeadingDivergence(t *testing.T) {
	f := New(64)

	// 构造：前 64 字节不同，之后 200KB 相同
	head1 := bytes.Repeat([]byte{'A'}, 64)
	head2 := bytes.Repeat([]byte{'B'}, 64)
	tail := bytes.Repeat([]byte("payload-data-"), 20000) // ~2.6MB

	base := append(head1, tail...)
	f.Remember(base)

	// 新请求：不同开头 + 相同尾部 + 少量追加
	newBody := append(head2, tail...)
	newBody = append(newBody, []byte("extra")...)

	h, body := f.Pick(newBody)
	if h.IsZero() {
		t.Fatal("应命中：64 字节差异不应阻止匹配（旧的前缀算法会在此失败）")
	}
	if !bytes.Equal(body, base) {
		t.Fatal("返回了错误的基线")
	}
}

// TestPrefixedAlgorithmWouldFail 证明旧算法在此场景下确实会失败（回归护栏）
func TestPrefixedAlgorithmWouldFail(t *testing.T) {
	head1 := bytes.Repeat([]byte{'A'}, 64)
	head2 := bytes.Repeat([]byte{'B'}, 64)
	tail := bytes.Repeat([]byte("x"), 200000)
	base := append(head1, tail...)
	newBody := append(head2, tail...)

	// 旧逻辑：从头算公共前缀
	cp := 0
	for cp < len(base) && cp < len(newBody) && base[cp] == newBody[cp] {
		cp++
	}
	oldRatio := float64(cp) / float64(len(base))
	if oldRatio >= 0.5 {
		t.Fatalf("前提不成立：旧算法在此样本上应判定失败，实际 %.1f%%", oldRatio*100)
	}
	t.Logf("旧算法（前缀）: %.2f%% → 会拒绝", oldRatio*100)

	// 新逻辑：块命中率
	newRatio := blockReuseRatio(base, newBody)
	t.Logf("新算法（块命中）: %.1f%% → 会接受", newRatio*100)
	if newRatio < 0.9 {
		t.Fatalf("新算法应给出高复用率，实际 %.1f%%", newRatio*100)
	}
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func mean(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	var s float64
	for _, x := range v {
		s += x
	}
	return s / float64(len(v))
}
