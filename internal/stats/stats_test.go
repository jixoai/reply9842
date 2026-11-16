package stats

import (
	"path/filepath"
	"testing"
	"time"
)

// TestLastTransportFollowsMostRecentRecord GUI 底栏要显示「实际生效的传输」：
// Transport 计数答不出「最近一次」，LastTransport 必须答得出，且 JSONL 重建
// 路径（进程重启后）同样成立。
func TestLastTransportFollowsMostRecentRecord(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "client-"+time.Now().Format("2006-01-02")+".jsonl")
	now := time.Now()
	writeRecords(t, logPath, now, []Record{
		{Status: 200, UpRaw: 1, UpWire: 1, Transport: "tcp"},
		{Status: 200, UpRaw: 1, UpWire: 1, Transport: "quic", Time: now.Add(time.Second).Format(time.RFC3339Nano)},
		// 最后一条没有 transport（旧版本日志的形态）：不该把最近一次有效值抹掉
		{Status: 200, UpRaw: 1, UpWire: 1, Time: now.Add(2 * time.Second).Format(time.RFC3339Nano)},
	})

	c, err := Open(Options{LogPath: logPath, StatsPath: filepath.Join(dir, "s.json")})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// 重建路径：按文件/行序从旧到新扫描，最后写入的是最新一条带传输的记录。
	if got := c.Snapshot().LastTransport; got != "quic" {
		t.Errorf("重建后 LastTransport=%q，期望 quic", got)
	}
	// 计数仍照常（两种都出现过）
	if m := c.Snapshot().Transport; m["tcp"] != 1 || m["quic"] != 1 {
		t.Errorf("Transport 计数 = %v，期望 tcp/quic 各 1", m)
	}
	// 实时路径：Observe 一条 tcp，最近一次必须跟着变
	c.Observe(&Record{Status: 200, UpRaw: 1, UpWire: 1, Transport: "tcp"})
	if got := c.Snapshot().LastTransport; got != "tcp" {
		t.Errorf("Observe 后 LastTransport=%q，期望 tcp", got)
	}
}

// 线上现象：client-stats.json 里 p50_ttfb_ms/p90_ttfb_ms 恒为 0。
// 根因之一：流式路径从不复写 MsUpstrm（恒 0），而 0 被当有效样本参与分位。
// 修复：0 视为「未测到」，不进入 TTFB 分位。
func TestTTFBPercentileIgnoresMissingSamples(t *testing.T) {
	dir := t.TempDir()
	c, err := New(filepath.Join(dir, "x.jsonl"), filepath.Join(dir, "stats.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// 三条没有 TTFB 的请求（streaming 旧路径 / 401）
	for i := 0; i < 3; i++ {
		c.Observe(&Record{Status: 200, MsTotal: 100})
	}
	if got := c.agg.P50TTFB; got != 0 {
		t.Fatalf("无样本时 P50TTFB=%d, want 0", got)
	}

	// 两条有 TTFB 的请求
	c.Observe(&Record{Status: 200, MsTotal: 900, MsUpstrm: 800})
	c.Observe(&Record{Status: 200, MsTotal: 1000, MsUpstrm: 1200})

	if c.agg.P50TTFB == 0 {
		t.Fatal("BUG: 有 TTFB 样本时 P50TTFB 仍为 0（缺样本把分位拉到 0）")
	}
	// 中位应落在两个真实样本之间，而不是被三个 0 拉低
	if c.agg.P50TTFB < 800 || c.agg.P50TTFB > 1200 {
		t.Errorf("P50TTFB=%d, want 800..1200", c.agg.P50TTFB)
	}
	t.Logf("P50TTFB=%d P90TTFB=%d", c.agg.P50TTFB, c.agg.P90TTFB)
}

// 线上现象：errors=4 但其中 3 个是 200。根因是客户端把正常结束记成 recv EOF。
// Collector 侧的语义是「err 非空即错误」，所以调用方必须保证正常完成不写 Err。
// 这里锁定 Collector 的判定不变，避免有人为了“凑数”去改判定。
func TestErrorsCountedByStatusAndErr(t *testing.T) {
	dir := t.TempDir()
	c, err := New(filepath.Join(dir, "x.jsonl"), filepath.Join(dir, "stats.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	c.Observe(&Record{Status: 200})              // ok
	c.Observe(&Record{Status: 401})              // 4xx
	c.Observe(&Record{Status: 200, Err: "boom"}) // 带错
	if c.agg.Errors != 2 {
		t.Fatalf("Errors=%d, want 2", c.agg.Errors)
	}
}
