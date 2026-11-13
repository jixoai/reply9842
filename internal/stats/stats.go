// Package stats — 双向可观测统计（客户端与服务端共用视图结构）
//
// 正交意图（3 个）：
//  1. 逐请求记录（jsonl 追加），字段够细可回溯
//  2. 汇总快照（json 覆盖写），供 CLI status 跨进程读
//  3. **磁盘即事实来源**：启动时从 jsonl 流式重建聚合，重启不丢统计
//
// 为什么不再把聚合当唯一真相（2026-11-13 kzf：「每次重启统计全丢，说明你用了
// 内存；同时也不要浪费内存，利用磁盘来解决这个问题」）：jsonl 本来就是逐请求
// 追加的事实来源，聚合只是它的一个视图。视图可以随时重算，内存里只需要留下
// 「图表要的那 30 个桶」和「分位数要的最近 500 条」——两者都有界，与历史长度无关。
package stats

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

// 时间序列固定 30 个桶：柱宽 = 绘图区宽 / 桶数，桶数变了柱宽就变，30 根是
// 「380px 里每根约 9.6px」这个可读区间的产物。粒度在 2 秒 ~ 48 分钟之间自动
// 适配（见 BucketSecs），用户只需要选「看多长的一段」。
const SeriesBuckets = 30

const (
	// DefaultRetain 磁盘上保留多久的日志（0 值时的兜底）
	DefaultRetain = 24 * time.Hour
	// DefaultWindow 图表默认展示多长的一段
	DefaultWindow = 30 * time.Minute

	// compactBytes 单个日志文件超过这个体积就压实（删掉窗口外的老记录）。
	// 实测单行约 300 字节、一天 1000+ 请求 ≈ 300KB；32MB 相当于 100 天的当日
	// 流量，正常负载永远到不了，只有异常刷屏或超长 err 才会触发。
	compactBytes = 32 << 20

	// recentMax 参与分位数计算的样本上限。分位数本来就是「最近的手感」，
	// 500 条在生产负载下约覆盖数小时，内存约 60KB。
	recentMax = 500
)

// Record 单次请求观测
type Record struct {
	Time      string  `json:"t"`
	Method    string  `json:"method"`
	Path      string  `json:"path"`
	Status    int     `json:"status"`
	Encoding  string  `json:"enc"`
	BaseHit   bool    `json:"base_hit"`
	UpRaw     int64   `json:"up_raw"`  // 明文请求体
	UpWire    int64   `json:"up_wire"` // 隧道上实际传输（编码后）
	DownRaw   int64   `json:"down_raw"`
	DownWire  int64   `json:"down_wire"`
	MsTotal   int64   `json:"ms_total"`
	MsEncode  int64   `json:"ms_encode"`
	MsWire    int64   `json:"ms_wire"` // 隧道往返
	MsDecode  int64   `json:"ms_decode"`
	MsUpstrm  int64   `json:"ms_upstream"` // 上游 TTFB
	UpBps     float64 `json:"up_bps"`
	DownBps   float64 `json:"down_bps"`
	Transport string  `json:"transport"` // quic / tcp
	Err       string  `json:"err,omitempty"`
}

// Bucket 一个时间桶里的上行体积。
//
// 为什么按时间桶而不是「按请求」：请求的到达是不均匀的（空转几分钟后突然
// 一串），按请求画会让横轴撒谎——它看起来像匀速，实际是突发的。
type Bucket struct {
	// At 桶的起点（对齐到桶边界），便于调用方判断这一桶是否落在自己的时间窗里。
	At     time.Time `json:"at"`
	UpRaw  int64     `json:"up_raw"`
	UpWire int64     `json:"up_wire"`
	Count  int64     `json:"count"`
}

// Agg 汇总视图
type Agg struct {
	StartedAt  string           `json:"started_at"`
	UpdatedAt  string           `json:"updated_at"`
	Requests   int64            `json:"requests"`
	Errors     int64            `json:"errors"`
	BaseHits   int64            `json:"base_hits"`
	UpRaw      int64            `json:"up_raw"`
	UpWire     int64            `json:"up_wire"`
	DownRaw    int64            `json:"down_raw"`
	DownWire   int64            `json:"down_wire"`
	UpRatio    float64          `json:"up_ratio"` // wire/raw
	P50Ms      int64            `json:"p50_ms"`
	P90Ms      int64            `json:"p90_ms"`
	P50Wire    int64            `json:"p50_wire_ms"`
	P90Wire    int64            `json:"p90_wire_ms"`
	P50TTFB    int64            `json:"p50_ttfb_ms"`
	P90TTFB    int64            `json:"p90_ttfb_ms"`
	AvgUpBps   float64          `json:"avg_up_bps"`
	AvgDownBps float64          `json:"avg_down_bps"`
	Statuses   map[int]int64    `json:"statuses"`
	Encodings  map[string]int64 `json:"encodings"`
	Transport  map[string]int64 `json:"transport"`

	// RetainSec / WindowSec / BucketSec 让「这份快照说的是哪段时间」自带说明：
	// 图表横轴、图下那句「最近 30 分钟（每 1 分钟一柱）」都从这里取，
	// 不再假设某个写死的窗口。
	RetainSec int64 `json:"retain_sec"`
	WindowSec int64 `json:"window_sec"`
	BucketSec int64 `json:"bucket_sec"`

	// Series 当前显示区间的分桶（旧 → 新，长度恒为 SeriesBuckets）。
	//
	// json:"-" 是有意的：图表每秒都在变，写进 client-stats.json 只会让快照
	// 又大又无意义（回看历史的人要的是总量，逐条细节在 jsonl 里都有）。
	Series []Bucket `json:"-"`
}

// Options 采集器选项
type Options struct {
	LogPath   string        // 逐请求日志（含日期则以「按日滚动」解释）
	StatsPath string        // 汇总快照
	Retain    time.Duration // 磁盘保留时长 = 重建扫描范围
	Window    time.Duration // 图表显示区间
}

// Collector 采集器。
//
// 内存里只有三样东西：聚合（几个 int64 + 小 map）、最近 recentMax 条样本、
// 30 个时间桶。历史全在磁盘上，随时可以重算。
type Collector struct {
	mu     sync.Mutex
	agg    *Agg
	recent []*Record

	retain time.Duration
	window time.Duration

	// series 环形复用：下标 = (Unix 秒 / 桶秒数) % SeriesBuckets。
	// 复用固定长度而不是 append：桶数固定，长度天然有界，
	// 槽位上的 At 对不上就说明它是过期数据。
	series [SeriesBuckets]Bucket

	// first / last 是内存里的真实时间边界，避免每次累加都解析字符串。
	first, last time.Time

	// 日志文件：dir + prefix 决定「同日滚动」；daily=false 时只用 logPath 单文件。
	dir, prefix, logPath string
	daily                bool
	statsPath            string
	logFile              *os.File
	logSize              int64
	// compactAt 压实阈值（字节）。0 用 compactBytes；测试可以调小以免真写 32MB。
	compactAt int64

	lastWrite time.Time
}

// reDaily 匹配 `client-2026-11-13.jsonl` 这类按日期滚动的文件名。
var reDaily = regexp.MustCompile(`^(.*)-(\d{4}-\d{2}-\d{2})\.jsonl$`)

// New 创建采集器并打开日志文件（追加）。保留时长/显示区间取默认值。
//
// 保留这个两参数签名，是为了 cmd/server 等既有调用点不用改：默认值即
// 「24 小时保留 + 30 分钟窗口」，对服务端同样是合理口径。
func New(logPath, statsPath string) (*Collector, error) {
	return Open(Options{LogPath: logPath, StatsPath: statsPath})
}

// Open 创建采集器：先流式重建聚合，再打开日志文件准备追加。
func Open(o Options) (*Collector, error) {
	if o.Retain <= 0 {
		o.Retain = DefaultRetain
	}
	if o.Window <= 0 {
		o.Window = DefaultWindow
	}
	if o.Window > o.Retain {
		o.Window = o.Retain
	}

	c := &Collector{
		dir:       filepath.Dir(o.LogPath),
		logPath:   o.LogPath,
		statsPath: o.StatsPath,
		retain:    o.Retain,
		window:    o.Window,
	}
	if m := reDaily.FindStringSubmatch(filepath.Base(o.LogPath)); m != nil {
		c.prefix, c.daily = m[1], true
	} else {
		c.prefix = filepath.Base(o.LogPath)
	}
	if c.dir != "" {
		if err := os.MkdirAll(c.dir, 0o755); err != nil {
			return nil, err
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	// 先读后写：重建期间不往文件里追加，否则同一请求会被算两次
	c.rebuildLocked()
	if err := c.openLogLocked(); err != nil {
		return nil, err
	}
	c.writeStatsLocked()
	return c, nil
}

// Close 落盘并关闭
func (c *Collector) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writeStatsLocked()
	if c.logFile != nil {
		err := c.logFile.Close()
		c.logFile = nil
		return err
	}
	return nil
}

// Retain 当前保留时长
func (c *Collector) Retain() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.retain
}

// Window 当前显示区间
func (c *Collector) Window() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.window
}

// SetRange 同时改保留时长与显示区间，只重建一次。
//
// 两条设置是一起改的（设置页里它们互相约束），分两次调会白扫两遍磁盘。
// 约束在这里兜底：显示区间不能超过保留时长——UI 没拦住时数据也不能撒谎。
func (c *Collector) SetRange(retain, window time.Duration) {
	if retain <= 0 {
		retain = DefaultRetain
	}
	if window <= 0 {
		window = DefaultWindow
	}
	if window > retain {
		window = retain
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if retain == c.retain && window == c.window {
		return
	}
	c.retain, c.window = retain, window
	c.rebuildLocked()
	c.writeStatsLocked()
}

// Observe 记录一次请求
func (c *Collector) Observe(r *Record) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	r.Time = now.Format(time.RFC3339Nano)
	c.accumulateLocked(r, now)
	c.recomputeLocked()

	c.appendLocked(r)
	if c.logSize >= c.compactThreshold() {
		c.compactLocked()
	}
	// 快照最多每秒写一次：每个请求覆盖写一份 JSON 是纯浪费，
	// 崩溃恢复靠的是 jsonl，不是这份快照。
	if time.Since(c.lastWrite) >= time.Second {
		c.writeStatsLocked()
	}
}

// Snapshot 返回当前汇总的副本，供进程内读者（GUI 每秒刷新）使用。
//
// 为什么必须深拷贝：Agg 里的 map 是原地更新的，直接把值交出去会让 GUI
// 在无锁状态下读到正在被请求 goroutine 写的 map——Go 的 map 并发读写是
// 直接 fatal（不是竞态报告）。拷贝成本是几个小 map，每秒一次可忽略。
func (c *Collector) Snapshot() Agg {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := *c.agg
	out.Statuses = cloneIntMap(c.agg.Statuses)
	out.Encodings = cloneStrMap(c.agg.Encodings)
	out.Transport = cloneStrMap(c.agg.Transport)
	out.RetainSec = int64(c.retain / time.Second)
	out.WindowSec = int64(c.window / time.Second)
	out.BucketSec = BucketSecs(c.window)
	out.Series = c.seriesLocked(time.Now())
	return out
}

// ---- 累加与派生（c.mu 必须已持有）----

// accumulateLocked 把一条记录算进聚合、时间桶与样本。
func (c *Collector) accumulateLocked(r *Record, t time.Time) {
	a := c.agg
	if c.first.IsZero() || t.Before(c.first) {
		c.first = t
		a.StartedAt = t.Format(time.RFC3339)
	}
	if t.After(c.last) {
		c.last = t
		a.UpdatedAt = t.Format(time.RFC3339)
	}

	a.Requests++
	if r.Status >= 400 || r.Err != "" {
		a.Errors++
	}
	if r.BaseHit {
		a.BaseHits++
	}
	a.UpRaw += r.UpRaw
	a.UpWire += r.UpWire
	a.DownRaw += r.DownRaw
	a.DownWire += r.DownWire
	if a.UpRaw > 0 {
		a.UpRatio = float64(a.UpWire) / float64(a.UpRaw)
	}
	a.Statuses[r.Status]++
	a.Encodings[r.Encoding]++
	if r.Transport != "" {
		a.Transport[r.Transport]++
	}

	c.bucketLocked(t, r)

	c.recent = append(c.recent, r)
	if len(c.recent) > recentMax {
		c.recent = c.recent[len(c.recent)-recentMax:]
	}
	// 分位数由调用方在「一批累加完之后」统一重算：重建时逐行算会把
	// O(log n) 的排序做上千遍，而结果只在最后那次有意义。
}

// recomputeLocked 从 recent 重算分位数与均值。
//
// TTFB 为 0 表示「没测到首字节」（如 401、请求在写响应前就失败），
// 属于缺失样本而非「0 毫秒」。混进分位会把 p50 拉成 0——这正是线上
// p50_ttfb_ms/p90_ttfb_ms 恒为 0 的成因之一。
func (c *Collector) recomputeLocked() {
	a := c.agg
	totals := make([]int64, 0, len(c.recent))
	wires := make([]int64, 0, len(c.recent))
	ttfbs := make([]int64, 0, len(c.recent))
	var sumUp, sumDown float64
	var nUp, nDown int
	for _, x := range c.recent {
		totals = append(totals, x.MsTotal)
		wires = append(wires, x.MsWire)
		if x.MsUpstrm > 0 {
			ttfbs = append(ttfbs, x.MsUpstrm)
		}
		if x.UpBps > 0 {
			sumUp += x.UpBps
			nUp++
		}
		if x.DownBps > 0 {
			sumDown += x.DownBps
			nDown++
		}
	}
	sort.Slice(totals, func(i, j int) bool { return totals[i] < totals[j] })
	sort.Slice(wires, func(i, j int) bool { return wires[i] < wires[j] })
	sort.Slice(ttfbs, func(i, j int) bool { return ttfbs[i] < ttfbs[j] })
	a.P50Ms, a.P90Ms = q(totals, .5), q(totals, .9)
	a.P50Wire, a.P90Wire = q(wires, .5), q(wires, .9)
	a.P50TTFB, a.P90TTFB = q(ttfbs, .5), q(ttfbs, .9)
	if nUp > 0 {
		a.AvgUpBps = sumUp / float64(nUp)
	}
	if nDown > 0 {
		a.AvgDownBps = sumDown / float64(nDown)
	}
}

// BucketSecs 每个桶覆盖的秒数：显示区间 / 30，至少 1 秒。
//
//	1 分钟 → 2 秒      5 分钟 → 10 秒    30 分钟 → 1 分钟
//	1 小时 → 2 分钟    6 小时 → 12 分钟  24 小时 → 48 分钟
//
// 桶数恒定，粒度自动适配；空桶留空位（不填充），空转期在图上是可见的空白。
func BucketSecs(window time.Duration) int64 {
	s := int64(window / time.Second / SeriesBuckets)
	if s < 1 {
		s = 1
	}
	return s
}

// bucketLocked 把一次请求记进它所属的桶（c.mu 必须已持有）。
func (c *Collector) bucketLocked(t time.Time, r *Record) {
	bsec := BucketSecs(c.window)
	at := bucketStart(t, bsec)
	slot := &c.series[int(at.Unix()/bsec)%SeriesBuckets]
	if !slot.At.Equal(at) {
		// 槽位上躺的是同一圈之前的旧桶（或者从未写过）：整槽重置。
		// 这一步就是「环形」的全部逻辑——不需要清空整个数组。
		*slot = Bucket{At: at}
	}
	slot.UpRaw += r.UpRaw
	slot.UpWire += r.UpWire
	slot.Count++
}

// seriesLocked 取最近 SeriesBuckets 个桶，旧 → 新，长度恒为 SeriesBuckets。
//
// 槽位是「桶序号取模」，所以从最后一个桶往前逐桶回读，就自动得到正确顺序——
// 不需要排序，也不会因为跨过数组边界而错位。空桶返回 UpRaw=0（而不是跳过），
// 这样横轴的时间间隔是恒定的：空转期在图上是可见的空白，而不是被压缩掉的假密度。
func (c *Collector) seriesLocked(now time.Time) []Bucket {
	bsec := BucketSecs(c.window)
	last := bucketStart(now, bsec)
	out := make([]Bucket, SeriesBuckets)
	for i := range out {
		at := last.Add(time.Duration(i-(SeriesBuckets-1)) * time.Duration(bsec) * time.Second)
		b := c.series[int(at.Unix()/bsec)%SeriesBuckets]
		if b.At.Equal(at) {
			out[i] = b
		} else {
			out[i] = Bucket{At: at}
		}
	}
	return out
}

// bucketStart 把时刻对齐到它所属桶的起点。
//
// 保留本地时区：Bucket.At 会进图表，也可能被调用方拿去显示，用 UTC 会让
// 「14:23:46 +08:00」变成「06:23:46」，对比日志时容易误判。
func bucketStart(t time.Time, bsec int64) time.Time {
	sec := t.Unix() / bsec * bsec
	return time.Unix(sec, 0).In(t.Location())
}

// ---- 磁盘重建（流式）----

// rebuildLocked 从日志重建聚合、时间桶与样本。
//
// 流式：bufio.Scanner 逐行解析，只有落在保留窗口内的记录才进累加器，
// 窗口外的直接丢弃——所以内存不随日志增长，只随「窗口内的请求数」增长。
func (c *Collector) rebuildLocked() {
	cutoff := time.Now().Add(-c.retain)
	c.resetLocked()
	for _, p := range c.filesLocked(cutoff) {
		c.scanFileLocked(p, cutoff)
	}
	if c.agg.StartedAt == "" {
		// 没有任何历史：窗口起点说成「现在」，比留空更好读
		c.agg.StartedAt = time.Now().Format(time.RFC3339)
	}
	c.recomputeLocked()
	c.pruneLocked(cutoff)
}

func (c *Collector) resetLocked() {
	c.agg = &Agg{
		Statuses:  map[int]int64{},
		Encodings: map[string]int64{},
		Transport: map[string]int64{},
	}
	c.recent = nil
	c.series = [SeriesBuckets]Bucket{}
	c.first, c.last = time.Time{}, time.Time{}
}

// scanFileLocked 流式扫描单个日志文件。
//
// 一行坏数据（崩溃截断的半行、手工编辑、别的格式）只跳过这一行，
// 不影响整个重建——「一行坏掉就全丢」是最糟的失败方式。
func (c *Collector) scanFileLocked(path string, cutoff time.Time) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	// 默认 64KB 的 token 上限对异常长的 err 字段可能不够，放宽到 1MB。
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var r Record
		if err := json.Unmarshal(line, &r); err != nil {
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, r.Time)
		if err != nil {
			continue
		}
		if t.Before(cutoff) {
			continue // 窗口外：不累加、不进桶，也不读进内存
		}
		rec := r
		c.accumulateLocked(&rec, t)
	}
}

// filesLocked 列出保留窗口内的日志文件，按时间从旧到新。
//
// 只按文件名的日期做粗筛（整天窗口外的文件直接跳过），比逐行读时间戳便宜。
func (c *Collector) filesLocked(cutoff time.Time) []string {
	if !c.daily {
		if _, err := os.Stat(c.logPath); err != nil {
			return nil
		}
		return []string{c.logPath}
	}
	ents, err := os.ReadDir(c.dir)
	if err != nil {
		return nil
	}
	dayCut := time.Date(cutoff.Year(), cutoff.Month(), cutoff.Day(), 0, 0, 0, 0, cutoff.Location())
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		m := reDaily.FindStringSubmatch(e.Name())
		if m == nil || m[1] != c.prefix {
			continue
		}
		day, err := time.ParseInLocation("2006-01-02", m[2], time.Local)
		if err != nil || day.Before(dayCut) {
			continue
		}
		out = append(out, filepath.Join(c.dir, e.Name()))
	}
	sort.Strings(out)
	return out
}

// pruneLocked 删掉整天都在保留窗口之外的历史日志文件。
//
// 「保留时长」的语义就是「磁盘上留多久」，所以这里真的删——只读不删的话文件
// 会按月累积，用户改设置也看不到磁盘效果。
func (c *Collector) pruneLocked(cutoff time.Time) {
	if !c.daily {
		return
	}
	ents, err := os.ReadDir(c.dir)
	if err != nil {
		return
	}
	dayCut := time.Date(cutoff.Year(), cutoff.Month(), cutoff.Day(), 0, 0, 0, 0, cutoff.Location())
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		m := reDaily.FindStringSubmatch(e.Name())
		if m == nil || m[1] != c.prefix {
			continue
		}
		day, err := time.ParseInLocation("2006-01-02", m[2], time.Local)
		if err != nil || !day.Before(dayCut) {
			continue
		}
		p := filepath.Join(c.dir, e.Name())
		if p == c.logPath {
			continue // 当前正在写的文件绝不删（跨天瞬间的边界保护）
		}
		os.Remove(p)
	}
}

// ---- 写入与压实 ----

// appendLocked 追加一条记录，并按需滚到新一天的日志文件。
func (c *Collector) appendLocked(r *Record) {
	b, err := json.Marshal(r)
	if err != nil {
		return
	}
	b = append(b, '\n')

	// 跨天滚动：进程活过零点时不能继续往昨天的文件里写，否则「按日期分文件」
	// 只对当天启动的进程成立。
	if c.daily {
		if want := c.dailyPath(time.Now()); want != c.logPath {
			if c.logFile != nil {
				c.logFile.Close()
				c.logFile = nil
			}
			c.logPath = want
			if err := c.openLogLocked(); err != nil {
				return
			}
		}
	}
	if c.logFile == nil {
		return
	}
	if n, err := c.logFile.Write(b); err == nil {
		c.logSize += int64(n)
	}
}

// dailyPath 当日日志路径（prefix 在构造时从 logPath 提取）
func (c *Collector) dailyPath(t time.Time) string {
	if !c.daily {
		return c.logPath
	}
	return filepath.Join(c.dir, c.prefix+"-"+t.Format("2006-01-02")+".jsonl")
}

// openLogLocked 以追加方式打开当日日志，并记下当前体积。
func (c *Collector) openLogLocked() error {
	if c.daily {
		c.logPath = c.dailyPath(time.Now())
	}
	f, err := os.OpenFile(c.logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	c.logFile = f
	c.logSize = 0
	if st, err := f.Stat(); err == nil {
		c.logSize = st.Size()
	}
	return nil
}

// compactThreshold 压实阈值：默认 32MB，测试可覆盖。
func (c *Collector) compactThreshold() int64 {
	if c.compactAt > 0 {
		return c.compactAt
	}
	return compactBytes
}

// compactLocked 压实当前日志：只保留保留窗口内的行，原子替换原文件。
//
// 触发条件只有一个——当前文件超过 compactBytes。不做「按时间自动压实」：
// 正常负载一天才 300KB，被时间触发只会白耗 IO；真正会撑大文件的是异常
// （刷屏、超长 err），此时按体积触发才精准。
func (c *Collector) compactLocked() {
	path := c.logPath
	if c.logFile != nil {
		c.logFile.Close()
		c.logFile = nil
	}
	tmp := path + ".compact"
	cutoff := time.Now().Add(-c.retain)

	// 失败就照常续写：压实是磁盘治理，不是正确性依赖。
	if err := c.rewriteKeeping(tmp, path, cutoff); err == nil {
		if os.Rename(tmp, path) != nil {
			os.Remove(tmp)
		}
	} else {
		os.Remove(tmp)
	}
	_ = c.openLogLocked()
}

// rewriteKeeping 把 src 里窗口内的行流式写到 dst。
func (c *Collector) rewriteKeeping(dst, src string, cutoff time.Time) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	w := bufio.NewWriter(out)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var r Record
		if err := json.Unmarshal(line, &r); err != nil {
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, r.Time)
		if err != nil || t.Before(cutoff) {
			continue // 窗口外：这一行就是压实要删掉的东西
		}
		w.Write(line)
		w.WriteByte('\n')
	}
	if err := w.Flush(); err != nil {
		return err
	}
	return sc.Err()
}

func (c *Collector) writeStatsLocked() {
	c.lastWrite = time.Now()
	if c.statsPath == "" {
		return
	}
	c.agg.RetainSec = int64(c.retain / time.Second)
	c.agg.WindowSec = int64(c.window / time.Second)
	c.agg.BucketSec = BucketSecs(c.window)
	if b, err := json.MarshalIndent(c.agg, "", "  "); err == nil {
		os.WriteFile(c.statsPath, b, 0o644)
	}
}

// ---- 小工具 ----

func cloneStrMap(m map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func cloneIntMap(m map[int]int64) map[int]int64 {
	out := make(map[int]int64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// ReadAgg 读取落盘的汇总快照（CLI 的 status 子命令用）。
// 文件不存在时返回 os.ErrNotExist，由调用方决定如何提示。
func ReadAgg(statsPath string) (*Agg, error) {
	b, err := os.ReadFile(statsPath)
	if err != nil {
		return nil, err
	}
	var a Agg
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

func q(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(float64(len(sorted)) * p)
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}
