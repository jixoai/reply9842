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
	"bufio"
	"encoding/json"
	"image/png"
	"os"
	"path/filepath"
	"regexp"
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
	t.Logf("真实数据：%d 请求 / 上行 %d → %d / p50 %d ms / %d 个非空桶 / %d 个有流量桶",
		agg.Requests, agg.UpRaw, agg.UpWire, agg.P50Ms,
		nonEmpty(agg.Series), withTraffic(agg.Series))

	renderWith(t, agg, tabPanel, filepath.Join(dir, "panel-running.png"))
	renderWith(t, agg, tabSettings, filepath.Join(dir, "settings.png"))
}

// realAgg 从真实日志重建聚合。
//
// 窗口取 1 小时：它是设置页「显示区间」里真实存在的档位，截图里两处的取值
// 必须一致（复核抓到过「设置页写 30 分钟、图上写最近 2 小时」的跨图矛盾）。
// 1 小时 / 30 桶 = 每桶 2 分钟，实测这段负载下 30 桶全非空。
//
// 窗口的**位置**不取「最近」而取「日志里最忙的连续 1 小时」（见 busiestSeries）：
// 真实日志有很长的空闲段（夜里可能一小时只有几分钟有请求），固定取最近窗口
// 时绘图区会大片空白，读者看到的不是「真实的安静」而是「图画坏了」。
// 挑窗口只是换一个切片位置，桶里的每个数字仍然是从日志逐条累加出来的。
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

	retain := 7 * 24 * time.Hour
	window := time.Hour
	c, err := stats.Open(stats.Options{
		LogPath:   logPath,
		StatsPath: filepath.Join(t.TempDir(), "agg.json"), // 别覆盖用户的汇总文件
		Retain:    retain,
		Window:    window,
	})
	if err != nil {
		t.Fatalf("stats.Open: %v", err)
	}
	defer c.Close()

	agg := c.Snapshot()
	if agg.Requests == 0 {
		t.Skip("日志里没有窗口内的请求，先跑一会儿 relay-client")
	}
	if series, from, to := busiestSeries(t, filepath.Dir(logPath), retain, window, stats.SeriesBuckets); series != nil {
		agg.Series = series
		agg.WindowSec = int64(window / time.Second)
		agg.BucketSec = stats.BucketSecs(window)
		t.Logf("窗口：%s → %s（%d 分钟一桶，非空 %d/%d 桶）",
			from.Format("2006-01-02 15:04"), to.Format("15:04"),
			agg.BucketSec/60, nonEmpty(series), len(series))
	}
	return agg
}

// busiestSeries 在日志里挑「最忙的连续窗口」，并按同样的桶宽重建它的桶。
//
// 选择规则：先比窗口内「有请求的桶个数」，再比请求总数。桶宽与桶数由 span/buckets
// 决定（2 小时 / 30 = 4 分钟），与 stats.BucketSecs 同一口径，所以产出的
// Series 可以直接喂给视图。
//
// 只读扫描、不回写：采集器的窗口语义（锚在「现在」）不改，这里只挑一个历史
// 切片。没有日志时返回 nil，调用方沿用快照里的最近窗口。
func busiestSeries(t *testing.T, dir string, retain, span time.Duration, buckets int) ([]stats.Bucket, time.Time, time.Time) {
	t.Helper()
	bsec := int64(span/time.Second) / int64(buckets)
	if bsec < 1 {
		bsec = 1
	}
	cutoff := time.Now().Add(-retain)

	sum := map[int64]*stats.Bucket{}
	var first, last int64
	seen := false
	for _, p := range logsSince(dir, cutoff) {
		f, err := os.Open(p)
		if err != nil {
			continue // 一份打不开的日志不该让整张图消失
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			var r stats.Record
			if json.Unmarshal(sc.Bytes(), &r) != nil || r.Time == "" {
				continue // 一行坏数据只丢这一行（与 stats 的重建同一条规矩）
			}
			at, err := time.Parse(time.RFC3339Nano, r.Time)
			if err != nil || at.Before(cutoff) {
				continue
			}
			idx := at.Unix() / bsec
			b := sum[idx]
			if b == nil {
				b = &stats.Bucket{At: time.Unix(idx*bsec, 0)}
				sum[idx] = b
			}
			// 累加规则与 stats.Collector.bucketLocked 逐字段一致：
			// 这里造的是「同一个采集器在另一段时间里会得到的那份 Series」。
			b.UpRaw += r.UpRaw
			b.UpWire += r.UpWire
			b.DownRaw += r.DownRaw
			b.DownWire += r.DownWire
			b.Count++
			b.MsSum += r.MsTotal
			b.TTFbSum += r.MsUpstrm
			if r.MsHandshake > 0 {
				b.HsSum += r.MsHandshake
				b.HsCount++
			}
			if !seen || idx < first {
				first = idx
			}
			if !seen || idx > last {
				last = idx
			}
			seen = true
		}
		f.Close()
	}
	if !seen {
		return nil, time.Time{}, time.Time{}
	}

	best, bestNonEmpty, bestCount := first, -1, int64(-1)
	for start := first; start+int64(buckets) <= last+1; start++ {
		nEmpty, count := 0, int64(0)
		for i := int64(0); i < int64(buckets); i++ {
			if b := sum[start+i]; b != nil && b.Count > 0 {
				nEmpty++
				count += b.Count
			}
		}
		if nEmpty > bestNonEmpty || (nEmpty == bestNonEmpty && count > bestCount) {
			best, bestNonEmpty, bestCount = start, nEmpty, count
		}
	}

	out := make([]stats.Bucket, buckets)
	for i := range out {
		at := time.Unix((best+int64(i))*bsec, 0)
		if b := sum[best+int64(i)]; b != nil {
			out[i] = *b
		} else {
			out[i] = stats.Bucket{At: at}
		}
	}
	from := time.Unix(best*bsec, 0)
	return out, from, from.Add(span)
}

// reLogDay 匹配按日滚动日志文件名里的日期（用于按保留期粗筛文件，
// 免得为了 2 小时的窗口把 30 天的日志全读一遍）。
var reLogDay = regexp.MustCompile(`(\d{4}-\d{2}-\d{2})\.jsonl$`)

// logsSince 保留期内的日志文件（按文件名日期粗筛，旧 → 新）。
func logsSince(dir string, since time.Time) []string {
	paths, err := filepath.Glob(filepath.Join(dir, "client-*.jsonl"))
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(paths))
	day := since.Format("2006-01-02")
	for _, p := range paths {
		if m := reLogDay.FindStringSubmatch(filepath.Base(p)); m != nil && m[1] < day {
			continue
		}
		out = append(out, p)
	}
	sort.Strings(out) // 文件名里的 ISO 日期天然可排序
	return out
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

// withTraffic 有柱子的桶数（第二张图只画有字节的桶）。
func withTraffic(bs []stats.Bucket) int {
	n := 0
	for _, b := range bs {
		if b.UpWire+b.DownWire > 0 {
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

	// 显示区间与图必须说同一段时间：图表用的是「日志里最忙的连续 2 小时」，
	// 设置页就选 2 小时；否则同一张官网图里一处写「最近 2 小时」、
	// 另一处写「30 分钟」，读者会以为图看的是别的时间段。
	for i, d := range relayclient.WindowOptions {
		if d == time.Hour {
			a.windowIdx = i
		}
	}

	// 演示两个开关的不同状态：开机自启开启（拇指在右、成功色轨道），
	// 自动更新保持关闭。一张图里同时看到开与关，才能读出新开关的位置语义。
	a.autoStart = true

	tt := ui.NewTester(a.view, defaultWinWidth, defaultWinHeight)
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
