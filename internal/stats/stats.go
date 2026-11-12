// Package stats — 双向可观测统计（客户端与服务端共用视图结构）
//
// 正交意图（2 个，2026-11-12 kzf 需求：「你要做好统计，比如数据包压缩前后的
// 体积，传输速度等等，请求的时间等等」）：
//   1. 逐请求记录（jsonl 追加），字段够细可回溯
//   2. 汇总快照（json 覆盖写），供 watch 实时看
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
	UpRaw     int64   `json:"up_raw"`      // 明文请求体
	UpWire    int64   `json:"up_wire"`     // 隧道上实际传输（编码后）
	DownRaw   int64   `json:"down_raw"`
	DownWire  int64   `json:"down_wire"`
	MsTotal   int64   `json:"ms_total"`
	MsEncode  int64   `json:"ms_encode"`
	MsWire    int64   `json:"ms_wire"`     // 隧道往返
	MsDecode  int64   `json:"ms_decode"`
	MsUpstrm  int64   `json:"ms_upstream"` // 上游 TTFB
	UpBps     float64 `json:"up_bps"`
	DownBps   float64 `json:"down_bps"`
	Transport string  `json:"transport"` // quic / tcp
	Err       string  `json:"err,omitempty"`
}

// Agg 汇总视图
type Agg struct {
	StartedAt  string       `json:"started_at"`
	UpdatedAt  string       `json:"updated_at"`
	Requests   int64        `json:"requests"`
	Errors     int64        `json:"errors"`
	BaseHits   int64        `json:"base_hits"`
	UpRaw      int64        `json:"up_raw"`
	UpWire     int64        `json:"up_wire"`
	DownRaw    int64        `json:"down_raw"`
	DownWire   int64        `json:"down_wire"`
	UpRatio    float64      `json:"up_ratio"` // wire/raw
	P50Ms      int64        `json:"p50_ms"`
	P90Ms      int64        `json:"p90_ms"`
	P50Wire    int64        `json:"p50_wire_ms"`
	P90Wire    int64        `json:"p90_wire_ms"`
	P50TTFB    int64        `json:"p50_ttfb_ms"`
	P90TTFB    int64        `json:"p90_ttfb_ms"`
	AvgUpBps   float64      `json:"avg_up_bps"`
	AvgDownBps float64      `json:"avg_down_bps"`
	Statuses   map[int]int64 `json:"statuses"`
	Encodings  map[string]int64 `json:"encodings"`
	Transport  map[string]int64 `json:"transport"`
}

// Collector 采集器
type Collector struct {
	mu      sync.Mutex
	agg     *Agg
	recent  []*Record
	logPath string
	statsPath string

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
			StartedAt:  time.Now().Format(time.RFC3339),
			Statuses:   map[int]int64{},
			Encodings:  map[string]int64{},
			Transport:  map[string]int64{},
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
	c.recent = append(c.recent, r)
	if len(c.recent) > 500 {
		c.recent = c.recent[len(c.recent)-500:]
	}

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
		ttfbs = append(ttfbs, x.MsUpstrm)
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
