//go:build darwin

// site_shots_test.go 用**真实运行数据**生成官网截图。
//
// 为什么不复用 TestSnapshotFrames：它用 fakeSeries 造数据，适合回归断言，
// 但官网要「真机上跑出来的样子」。
//
// 关键机制：图表的 Series（30 个时间桶）标了 `json:"-"`，不落盘；但
// stats.Open() 会从 JSONL 日志**重建**这些桶（rebuildLocked 流式扫日志）。
// 所以只要指向真实的 client-<date>.jsonl，就能拿到真实的曲线形状。
//
// 用法：
//
//	SITE_SHOTS_DIR=/tmp/site go test ./cmd/gui -run TestSiteShots -v
//
// 没有真实日志时自动跳过。
package main

import (
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/jixoai/reply9842/internal/relayclient"
	"github.com/jixoai/reply9842/internal/stats"
)

// TestSiteShots 用真实日志重建统计，产出面板与设置页截图。
func TestSiteShots(t *testing.T) {
	dir := os.Getenv("SITE_SHOTS_DIR")
	if dir == "" {
		t.Skip("设置 SITE_SHOTS_DIR 才生成")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	agg := realAgg(t)
	t.Logf("真实数据：%d 请求 / 上行 %d → %d / p50 %d ms / %d 个非空桶",
		agg.Requests, agg.UpRaw, agg.UpWire, agg.P50Ms, nonEmpty(agg.Series))

	renderWith(t, agg, tabPanel, filepath.Join(dir, "panel-running.png"))
	renderWith(t, agg, tabSettings, filepath.Join(dir, "settings.png"))
}

// realAgg 从真实日志重建聚合。
//
// 窗口取 1 小时而不是默认 30 分钟：官网要展示「有起伏的曲线」，
// 1 小时能让最近的活动都落在图里。retain 取 7 天保证能扫到今天的日志。
func realAgg(t *testing.T) stats.Agg {
	t.Helper()
	logPath := relayclient.DefaultLogPath()
	if _, err := os.Stat(logPath); err != nil {
		t.Skipf("没有真实日志 %s：%v", logPath, err)
	}

	c, err := stats.Open(stats.Options{
		LogPath:   logPath,
		StatsPath: filepath.Join(t.TempDir(), "agg.json"), // 别覆盖用户的汇总文件
		Retain:    7 * 24 * time.Hour,
		Window:    1 * time.Hour,
	})
	if err != nil {
		t.Fatalf("stats.Open: %v", err)
	}
	defer c.Close()

	agg := c.Snapshot()
	if agg.Requests == 0 {
		t.Skip("日志里没有窗口内的请求，先跑一会儿 relay-client")
	}
	return agg
}

func nonEmpty(bs []stats.Bucket) int {
	n := 0
	for _, b := range bs {
		if b.Count > 0 {
			n++
		}
	}
	return n
}

func renderWith(t *testing.T, agg stats.Agg, tab int, path string) {
	t.Helper()
	a := newTestApp(t)
	setRunning(a, true)
	a.tab = tab
	a.snap = func() stats.Agg { return agg }

	tt := ui.NewTester(a.view, 560, 660)
	img := tt.Image()
	if img == nil {
		t.Fatalf("%s: 渲染为空", path)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatalf("%s: png.Encode: %v", path, err)
	}
	t.Logf("wrote %s", path)
}
