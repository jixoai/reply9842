package stats

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeRecords 往 path 追加一批记录（每条自带时间戳，模拟真实日志的形态）。
func writeRecords(t *testing.T, path string, at time.Time, recs []Record) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, r := range recs {
		if r.Time == "" {
			r.Time = at.Format(time.RFC3339Nano)
		}
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		f.Write(append(b, '\n'))
	}
}

// TestRebuildFromDisk 核心需求：进程重启后统计必须还在。
//
// 构造方式模拟真实生命周期——第一个 Collector 写日志后关闭（=进程被杀），
// 第二个 Collector 从同一个目录重建。断言的是「数字一模一样」。
func TestRebuildFromDisk(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "client-2026-11-13.jsonl")
	statsPath := filepath.Join(dir, "client-stats.json")

	recs := []Record{
		{Status: 200, Encoding: "dcz(L19)", BaseHit: true, UpRaw: 1_000_000, UpWire: 900, DownRaw: 5000, DownWire: 5100, MsTotal: 120, MsUpstrm: 100, Transport: "tcp"},
		{Status: 200, Encoding: "dcz(L19)", BaseHit: true, UpRaw: 2_000_000, UpWire: 1_800, DownRaw: 7000, DownWire: 7100, MsTotal: 200, MsUpstrm: 180, Transport: "tcp"},
		{Status: 502, Encoding: "identity", UpRaw: 100, UpWire: 100, MsTotal: 50, Err: "boom"},
	}

	// 第一段生命
	c1, err := Open(Options{LogPath: logPath, StatsPath: statsPath, Retain: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	for i := range recs {
		c1.Observe(&recs[i])
	}
	before := c1.Snapshot()
	c1.Close()

	// 第二段生命（= 重启）
	c2, err := Open(Options{LogPath: logPath, StatsPath: statsPath, Retain: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	after := c2.Snapshot()

	if after.Requests != before.Requests || after.Requests != 3 {
		t.Fatalf("重启后 requests=%d，重启前=%d，期望 3", after.Requests, before.Requests)
	}
	if after.UpRaw != before.UpRaw || after.UpWire != before.UpWire {
		t.Fatalf("上行体积未恢复：重启后 %d/%d，重启前 %d/%d",
			after.UpRaw, after.UpWire, before.UpRaw, before.UpWire)
	}
	if after.Errors != 1 || after.BaseHits != 2 {
		t.Errorf("errors=%d base_hits=%d，期望 1/2", after.Errors, after.BaseHits)
	}
	if after.DownRaw != 12000 {
		t.Errorf("down_raw=%d，期望 12000", after.DownRaw)
	}
	if after.P50TTFB == 0 {
		t.Errorf("TTFB 分位在重建后丢了")
	}
	// 柱状图必须有历史：3 条记录都落在同一个桶里
	var n int64
	for _, b := range after.Series {
		n += b.Count
	}
	if n != 3 {
		t.Errorf("重建后时间桶里共 %d 条，期望 3", n)
	}
}

// TestRebuildSkipsOutOfWindow 保留窗口外的记录不许进聚合。
func TestRebuildSkipsOutOfWindow(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "client-x.jsonl")
	now := time.Now()

	writeRecords(t, logPath, now, []Record{
		{Status: 200, UpRaw: 111, UpWire: 111},
		// 两小时前：保留 30 分钟时这条必须被丢弃
		{Status: 200, UpRaw: 222, UpWire: 222, Time: now.Add(-2 * time.Hour).Format(time.RFC3339Nano)},
	})

	c, err := Open(Options{
		LogPath:   logPath,
		StatsPath: filepath.Join(dir, "s.json"),
		Retain:    30 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	agg := c.Snapshot()
	if agg.Requests != 1 {
		t.Fatalf("requests=%d，期望 1（窗口外那条要被丢弃）", agg.Requests)
	}
	if agg.UpRaw != 111 {
		t.Errorf("up_raw=%d，期望 111", agg.UpRaw)
	}
}

// TestRebuildSkipsBrokenLines 一行坏数据只丢这一行，不能毁掉整个重建。
func TestRebuildSkipsBrokenLines(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "client-y.jsonl")
	now := time.Now()

	f, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	// 正常行 + 截断的半行 + 非本格式行 + 正常行
	f.WriteString(`{"t":"` + now.Format(time.RFC3339Nano) + `","status":200,"up_raw":10,"up_wire":5}` + "\n")
	f.WriteString(`{"t":"` + now.Format(time.RFC3339Nano) + `","status":20` + "\n")
	f.WriteString("garbage\n")
	f.WriteString(`{"t":"` + now.Format(time.RFC3339Nano) + `","status":200,"up_raw":20,"up_wire":10}` + "\n")
	f.Close()

	c, err := Open(Options{LogPath: logPath, StatsPath: filepath.Join(dir, "s.json")})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if agg := c.Snapshot(); agg.Requests != 2 {
		t.Fatalf("requests=%d，期望 2（坏行跳过、好行保留）", agg.Requests)
	}
}

// TestBucketSecsPerWindow 六档显示区间各自的桶粒度（需求表格逐行核对）。
func TestBucketSecsPerWindow(t *testing.T) {
	cases := []struct {
		window time.Duration
		want   int64
	}{
		{1 * time.Minute, 2},
		{5 * time.Minute, 10},
		{30 * time.Minute, 60},
		{1 * time.Hour, 120},
		{6 * time.Hour, 720},
		{24 * time.Hour, 2880},
	}
	for _, tc := range cases {
		if got := BucketSecs(tc.window); got != tc.want {
			t.Errorf("BucketSecs(%s) = %d 秒，期望 %d 秒", tc.window, got, tc.want)
		}
		// 桶数恒定：桶粒度 × 30 必须回到窗口
		if int64(tc.window/time.Second) != tc.want*SeriesBuckets {
			t.Errorf("%s 的 %d 桶 × %d 秒 ≠ 窗口长度", tc.window, SeriesBuckets, tc.want)
		}
	}
}

// TestWindowChangeRebuckets 切换显示区间后柱状图横轴跟着变（桶数不变、粒度变）。
func TestWindowChangeRebuckets(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "client-z.jsonl")

	// 造 20 分钟内的记录：30 分钟窗口下它们在几个 1 分钟桶里；
	// 1 小时窗口下会被收进 2 分钟桶，所以非空桶数必须变少。
	now := time.Now()
	var recs []Record
	for i := 0; i < 20; i++ {
		recs = append(recs, Record{
			Status: 200, UpRaw: 1000, UpWire: 100,
			Time: now.Add(-time.Duration(i) * time.Minute).Format(time.RFC3339Nano),
		})
	}
	writeRecords(t, logPath, now, recs)

	c, err := Open(Options{
		LogPath:   logPath,
		StatsPath: filepath.Join(dir, "s.json"),
		Retain:    24 * time.Hour,
		Window:    30 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	fine := c.Snapshot()
	if fine.BucketSec != 60 || len(fine.Series) != SeriesBuckets {
		t.Fatalf("30 分钟窗口下 bucket_sec=%d len=%d，期望 60/%d", fine.BucketSec, len(fine.Series), SeriesBuckets)
	}
	fineBuckets := nonEmpty(fine.Series)

	c.SetRange(24*time.Hour, 24*time.Hour)
	coarse := c.Snapshot()
	if coarse.BucketSec != 2880 {
		t.Fatalf("24 小时窗口下 bucket_sec=%d，期望 2880", coarse.BucketSec)
	}
	coarseBuckets := nonEmpty(coarse.Series)

	if coarseBuckets >= fineBuckets {
		t.Errorf("粗粒度下非空桶 %d 应少于细粒度 %d（20 分钟数据被收进更少的桶）",
			coarseBuckets, fineBuckets)
	}
	// 切换窗口不该改变总量
	if coarse.UpRaw != fine.UpRaw || coarse.Requests != fine.Requests {
		t.Errorf("切窗口改了总量：%d/%d vs %d/%d",
			coarse.UpRaw, coarse.Requests, fine.UpRaw, fine.Requests)
	}
}

func nonEmpty(bs []Bucket) int {
	n := 0
	for _, b := range bs {
		if b.UpRaw > 0 {
			n++
		}
	}
	return n
}

// TestRetainPrunesOldDailyFiles 保留时长会真的删掉整天在窗口外的日志文件。
func TestRetainPrunesOldDailyFiles(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "client-"+time.Now().AddDate(0, 0, -10).Format("2006-01-02")+".jsonl")
	fresh := filepath.Join(dir, "client-"+time.Now().Format("2006-01-02")+".jsonl")
	os.WriteFile(old, []byte(""), 0o644)
	os.WriteFile(fresh, []byte(""), 0o644)

	c, err := Open(Options{
		LogPath:   fresh,
		StatsPath: filepath.Join(dir, "s.json"),
		Retain:    24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("保留 24 小时时，10 天前的日志文件应被删除：%v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("当日日志不该被删：%v", err)
	}
}

// TestCompactDropsOldLines 压实：文件超过阈值时窗口外的行被删掉，窗口内的保留。
//
// 用 Retain=1 分钟 + 手写超过阈值的文件来触发（真实 32MB 太慢）。
func TestCompactDropsOldLines(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "client-2026-11-13.jsonl")
	now := time.Now()

	// 老记录：2 小时前，保留 1 分钟时全部该被删
	var oldRecs []Record
	for i := 0; i < 100; i++ {
		oldRecs = append(oldRecs, Record{
			Status: 200, UpRaw: 4096, UpWire: 4096,
			Time: now.Add(-2 * time.Hour).Format(time.RFC3339Nano),
		})
	}
	writeRecords(t, logPath, now, oldRecs)
	// 新记录：刚刚
	writeRecords(t, logPath, now, []Record{{Status: 200, UpRaw: 77, UpWire: 77}})

	before, err := os.Stat(logPath)
	if err != nil {
		t.Fatal(err)
	}

	c := &Collector{
		dir:       dir,
		prefix:    "client",
		daily:     true,
		logPath:   logPath,
		statsPath: filepath.Join(dir, "s.json"),
		retain:    time.Minute,
		window:    time.Minute,
		agg: &Agg{
			Statuses: map[int]int64{}, Encodings: map[string]int64{}, Transport: map[string]int64{},
		},
	}
	c.mu.Lock()
	if err := c.openLogLocked(); err != nil {
		c.mu.Unlock()
		t.Fatal(err)
	}
	c.compactLocked()
	c.mu.Unlock()
	defer c.Close()

	after, err := os.Stat(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() >= before.Size() {
		t.Fatalf("压实后体积 %d 未减小（原 %d）", after.Size(), before.Size())
	}

	// 压实后重建：只剩窗口内那一条
	c2, err := Open(Options{
		LogPath:   logPath,
		StatsPath: filepath.Join(dir, "s2.json"),
		Retain:    time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	agg := c2.Snapshot()
	if agg.Requests != 1 {
		t.Errorf("压实后 requests=%d，期望 1（只有窗口内那条）", agg.Requests)
	}
	if agg.UpRaw != 77 {
		t.Errorf("压实后 up_raw=%d，期望 77", agg.UpRaw)
	}
}

// TestCompactTriggeredBySize 超过体积阈值时，Observe 会自动触发压实。
//
// 用极小的 compactAt 来测「触发」这件事本身——真的写 32MB 只会让测试慢，
// 而阈值判断是同一个分支。
func TestCompactTriggeredBySize(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "client-size.jsonl")
	now := time.Now()

	// 先塞一批窗口外的老记录（保留 1 分钟 → 它们全是垃圾），把文件撑过阈值
	var recs []Record
	for i := 0; i < 50; i++ {
		recs = append(recs, Record{
			Status: 200, UpRaw: 1024, UpWire: 1024,
			Time: now.Add(-2 * time.Hour).Format(time.RFC3339Nano),
		})
	}
	writeRecords(t, logPath, now, recs)

	c, err := Open(Options{
		LogPath:   logPath,
		StatsPath: filepath.Join(dir, "s.json"),
		Retain:    time.Minute,
		Window:    time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	c.mu.Lock()
	c.compactAt = 1 // 下一次 Observe 必定触发压实
	c.mu.Unlock()

	c.Observe(&Record{Status: 200, UpRaw: 5, UpWire: 5})

	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	// 压实后文件里只该剩刚写的那一条（老记录全部出窗口）
	if got := countLines(b); got != 1 {
		t.Errorf("压实后日志 %d 行，期望 1（窗口外的行被删）", got)
	}

	// 重建必须能在这个文件上正常工作
	c2, err := Open(Options{
		LogPath:   logPath,
		StatsPath: filepath.Join(dir, "s2.json"),
		Retain:    time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if agg := c2.Snapshot(); agg.Requests != 1 {
		t.Errorf("压实后重建 requests=%d，期望 1", agg.Requests)
	}
}

func countLines(b []byte) int {
	n := 0
	for _, c := range b {
		if c == '\n' {
			n++
		}
	}
	return n
}

// TestObserveAppendsAndSnapshotWrites 一次请求必须同时落到 jsonl 与快照。
func TestObserveAppendsAndSnapshotWrites(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "client-a.jsonl")
	statsPath := filepath.Join(dir, "client-stats.json")

	c, err := Open(Options{LogPath: logPath, StatsPath: statsPath})
	if err != nil {
		t.Fatal(err)
	}
	c.Observe(&Record{Status: 200, UpRaw: 1000, UpWire: 10, Encoding: "dcz(L19)", BaseHit: true})
	c.Close()

	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) == 0 || b[len(b)-1] != '\n' {
		t.Fatalf("日志未追加或未以换行结尾：%q", b)
	}
	agg, err := ReadAgg(statsPath)
	if err != nil {
		t.Fatal(err)
	}
	if agg.Requests != 1 || agg.UpRaw != 1000 {
		t.Errorf("快照未落盘：requests=%d up_raw=%d", agg.Requests, agg.UpRaw)
	}
	// 快照里必须带上窗口说明，CLI/前端据此显示区间
	if agg.RetainSec != int64(DefaultRetain/time.Second) || agg.BucketSec == 0 {
		t.Errorf("快照缺少窗口字段：retain=%d bucket=%d", agg.RetainSec, agg.BucketSec)
	}
}
