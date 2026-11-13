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
	"sort"
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
// 窗口取 2 小时而不是默认 30 分钟：官网要展示「有起伏的曲线」，窗口太窄时
// 刚过零点（按日滚动的日志刚换文件）只剩几分钟的数据，图会看起来像空的。
// 2 小时能把最近一段真实活动都收进 30 个桶里；retain 取 7 天保证扫得到
// 当天的日志。
func realAgg(t *testing.T) stats.Agg {
	t.Helper()
	logPath := relayclient.DefaultLogPath()
	if _, err := os.Stat(logPath); err != nil {
		// 按日滚动的日志在跨零点后会立刻指向一个还不存在的新文件，
		// 而昨天的数据仍然在磁盘上。截图生成器不该因为「刚过零点」
		// 就静默跳过——退回目录里最新的一份日志。
		// stats.Open 对这类带日期的路径按「同前缀全部文件」重建，
		// 所以传昨天的路径仍会读到完整的保留窗口。
		if latest := latestDailyLog(filepath.Dir(logPath)); latest != "" {
			logPath = latest
		} else {
			t.Skipf("没有真实日志 %s：%v", logPath, err)
		}
	}

	c, err := stats.Open(stats.Options{
		LogPath:   logPath,
		StatsPath: filepath.Join(t.TempDir(), "agg.json"), // 别覆盖用户的汇总文件
		Retain:    7 * 24 * time.Hour,
		Window:    2 * time.Hour,
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

// latestDailyLog 返回 dir 下最新的 client-YYYY-MM-DD.jsonl（没有则空串）。
//
// 只用于截图：跨零点时当日文件尚未创建，上一份日志才是「真实的最近数据」。
func latestDailyLog(dir string) string {
	paths, err := filepath.Glob(filepath.Join(dir, "client-*.jsonl"))
	if err != nil || len(paths) == 0 {
		return ""
	}
	sort.Strings(paths) // 文件名里的 ISO 日期天然可排序
	return paths[len(paths)-1]
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

	// 官网截图只用演示值：公开图片里不能出现真实的服务器 host/IP。
	// 监听地址本来就是 127.0.0.1:18899；上游与 SNI 换成 example.com，
	// 端口保留 28443 以维持真实的「非标端口」观感。
	a.listen = relayclient.DefaultListen
	a.server = "tunnel.example.com:28443"
	a.serverName = "tunnel.example.com"

	// 演示两个开关的不同状态：开机自启开启（拇指在右、成功色轨道），
	// 自动更新保持关闭。一张图里同时看到开与关，才能读出新开关的位置语义。
	a.autoStart = true

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
