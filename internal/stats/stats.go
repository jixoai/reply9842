// Package stats — 双向可观测统计（客户端与服务端共用视图结构）
//
// 正交意图（2 个，2026-11-12 kzf 需求：「你要做好统计，比如数据包压缩前后的
// 体积，传输速度等等，请求的时间等等」）：
//  1. 逐请求记录（jsonl 追加），字段够细可回溯
//  2. 汇总快照（json 覆盖写），供 watch 实时看
package stats

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
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

// 时间序列的窗口：最近 30 分钟，按分钟聚合 = 30 个桶。
//
// 为什么是分钟而不是秒：实测日志（14.5 小时 / 1124 条请求）里，最近 30 分钟
// 只有 46 个请求、18 个非空分钟。按秒聚合时 60 秒内常常只有 1-2 个请求，
// 柱子稀稀落落到读不出任何趋势；按分钟聚合同样这 46 个请求会落进 18 根
// 柱子，形状立刻清楚。
//
// 为什么是 30 分钟而不是更长：这是「刚才那一段」与「今天的全貌」之间的
// 折中——足够看出一次任务（agent 连续跑几十个请求）的起伏，又短到不需要
// 横轴刻度（左端就是半小时前）。
const (
	SeriesSeconds = 30 * 60
	SeriesBuckets = 30
	// SeriesBucket 每个桶覆盖的时长（秒）
	SeriesBucket = SeriesSeconds / SeriesBuckets // 60 秒 = 1 分钟
)

// Bucket 一个时间桶里的上行体积（每桶 SeriesBucket 秒 = 1 分钟）。
//
// 为什么按时间桶而不是「按请求」：请求的到达是不均匀的（空转几分钟后突然
// 一串），按请求画会让横轴撒谎——它看起来像匀速，实际是突发的。
type Bucket struct {
	// At 桶的起点（整秒），便于调用方判断这一桶是否落在自己的时间窗里。
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

	// Series 最近一分钟的按秒分桶（旧 → 新），只给进程内的图表用。
	//
	// json:"-" 是有意的：这个字段每秒都在变，写进 client-stats.json 只会
	// 让那份快照变得又大又无意义（回看历史的人要的是总量，不是最后一分钟的
	// 秒级细节；秒级细节在 jsonl 里逐条都有）。
	Series []Bucket `json:"-"`
}

// Collector 采集器
type Collector struct {
	mu        sync.Mutex
	agg       *Agg
	recent    []*Record
	logPath   string
	statsPath string

	// series 是最近 30 分钟的分桶，环形复用：下标 = (Unix 秒 / 桶秒数) % SeriesBuckets。
	//
	// 为什么在内存里攒而不是回头读 jsonl：图表每秒要一次，读文件是 IO + 解析
	// 一整天的日志；而这里的数据本来就都在 Observe 的入参里，顺手记一笔即可。
	// 为什么复用固定长度而不是 append：桶数固定，长度天然有界，
	// 不需要淘汰逻辑（槽位上的 At 对不同就说明它过期了）。
	series [SeriesBuckets]Bucket

	logFile *os.File
}

// New 创建采集器并打开日志文件（追加）
func New(logPath, statsPath string) (*Collector, error) {
	if dir := filepath.Dir(logPath); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &Collector{
		agg: &Agg{
			StartedAt: time.Now().Format(time.RFC3339),
			Statuses:  map[int]int64{},
			Encodings: map[string]int64{},
			Transport: map[string]int64{},
		},
		logPath:   logPath,
		statsPath: statsPath,
		logFile:   f,
	}, nil
}

// Close 落盘并关闭
func (c *Collector) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writeStatsLocked()
	if c.logFile != nil {
		return c.logFile.Close()
	}
	return nil
}

// Observe 记录一次请求
func (c *Collector) Observe(r *Record) {
	c.mu.Lock()
	defer c.mu.Unlock()

	r.Time = time.Now().Format(time.RFC3339Nano)
	now := time.Now()
	c.recent = append(c.recent, r)
	if len(c.recent) > 500 {
		c.recent = c.recent[len(c.recent)-500:]
	}
	c.bucketLocked(now, r)

	a := c.agg
	a.Requests++
	a.UpdatedAt = time.Now().Format(time.RFC3339)
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

	// 分位（最近 500 条）
	totals := make([]int64, 0, len(c.recent))
	wires := make([]int64, 0, len(c.recent))
	ttfbs := make([]int64, 0, len(c.recent))
	var sumUp, sumDown float64
	var nUp, nDown int
	for _, x := range c.recent {
		totals = append(totals, x.MsTotal)
		wires = append(wires, x.MsWire)
		// TTFB 为 0 表示「没测到首字节」（如 401、请求在写响应前就失败），
		// 属于缺失样本而非「0 毫秒」。混进分位会把 p50 拉成 0——这正是
		// 线上 p50_ttfb_ms/p90_ttfb_ms 恒为 0 的成因之一。
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

	// 追加日志
	if c.logFile != nil {
		if b, err := json.Marshal(r); err == nil {
			c.logFile.Write(append(b, '\n'))
		}
	}
	c.writeStatsLocked()
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
	out.Series = c.seriesLocked(time.Now())
	return out
}

// bucketLocked 把一次请求记进它所属的桶（c.mu 必须已持有）。
func (c *Collector) bucketLocked(now time.Time, r *Record) {
	at := bucketStart(now)
	slot := &c.series[int(at.Unix()/SeriesBucket)%SeriesBuckets]
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
	last := bucketStart(now)
	out := make([]Bucket, SeriesBuckets)
	for i := range out {
		at := last.Add(time.Duration(i-(SeriesBuckets-1)) * SeriesBucket * time.Second)
		b := c.series[int(at.Unix()/SeriesBucket)%SeriesBuckets]
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
// 保留本地时区：Bucket.At 会进图表的调试输出，也可能被调用方拿去显示，
// 用 UTC 会让「14:23:46 +08:00」变成「06:23:46」，对比日志时容易误判。
func bucketStart(t time.Time) time.Time {
	sec := t.Unix() / SeriesBucket * SeriesBucket
	return time.Unix(sec, 0).In(t.Location())
}

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

func (c *Collector) writeStatsLocked() {
	if c.statsPath == "" {
		return
	}
	if b, err := json.MarshalIndent(c.agg, "", "  "); err == nil {
		os.WriteFile(c.statsPath, b, 0o644)
	}
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
