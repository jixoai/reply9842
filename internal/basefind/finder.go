// Package basefind — 客户端侧基线选择（哪个历史 body 当 dcz 字典）
//
// 正交意图（2 个，2026-11-12 kzf 需求：「客户端上起转发器，把 AI 请求拆成块，
// 每块算哈希……增量的部分用哈希再加 1 个 JSON-op 来发送」「技术的难点在于
// 客户端和服务端的缓存如何设计」）：
//   1. 历史环形表：记录最近发出的 N 个 body + 哈希（客户端自己的「我有什么」）
//   2. 选基线：用「最长公共前缀 + 体积接近」挑出最可能高压缩的 base
//
// 为什么不做块级去重（调研实测结论，勿改）：
//   FastCDC 在 292KB agent 请求上 23 块、19 块可复用，成本约 17KB；
//   整文档 zstd delta 仅 1297B，差 13 倍。LLM 会话是尾部追加，
//   LZ77 的变长 match 比固定块粒度更细。故此处只选「整份历史 body」。
package basefind

import (
	"bytes"
	"sync"

	"github.com/jixoai/rfc9842-reply/internal/codec"
)

// Candidate 一份历史 body
type Candidate struct {
	Hash codec.Hash
	Body []byte
	// 记录被选中的次数，用于将来自适应（暂未用于打分）
	picked int
}

// Finder 客户端历史窗口
type Finder struct {
	mu    sync.Mutex
	max   int
	items []*Candidate // 新→旧
}

// New 创建，max 为保留条数（建议 8：足以覆盖 10 并发窗口）
func New(max int) *Finder {
	if max <= 0 {
		max = 8
	}
	return &Finder{max: max}
}

// Remember 记录一次已发出的 body，返回其哈希
func (f *Finder) Remember(body []byte) codec.Hash {
	h := codec.Sum(body)
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.items {
		if c.Hash == h {
			return h
		}
	}
	cp := make([]byte, len(body))
	copy(cp, body)
	f.items = append([]*Candidate{{Hash: h, Body: cp}}, f.items...)
	if len(f.items) > f.max {
		f.items = f.items[:f.max]
	}
	return h
}

// Pick 为 newBody 选一个基线。
//
// 评分：公共前缀占比为主，体积接近度为辅（配合 LZ77 的匹配倾向）。
// 返回零哈希表示「找不到合适基线，应发全量」。
//
// minPrefix 建议 0.5：前缀不足一半时 delta 收益不如直接压。
func (f *Finder) Pick(newBody []byte, minPrefix float64) (codec.Hash, []byte) {
	f.mu.Lock()
	cands := make([]*Candidate, len(f.items))
	copy(cands, f.items)
	f.mu.Unlock()

	if len(cands) == 0 || len(newBody) == 0 {
		return codec.Hash{}, nil
	}

	var best *Candidate
	bestScore := 0.0
	for _, c := range cands {
		p := commonPrefixLen(c.Body, newBody)
		if p == 0 {
			continue
		}
		pfx := float64(p) / float64(len(newBody))
		// 体积接近度：|Δ| 越小越好，映射到 [0,1]
		sizeRatio := float64(min(len(c.Body), len(newBody))) / float64(max(len(c.Body), len(newBody)))
		score := pfx*0.85 + sizeRatio*0.15
		if score > bestScore {
			bestScore = score
			best = c
		}
	}
	if best == nil || bestScore < minPrefix {
		return codec.Hash{}, nil
	}
	best.picked++
	return best.Hash, best.Body
}

// Len 当前候选数
func (f *Finder) Len() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.items)
}

// Clear 清空历史（会话切换时用）
func (f *Finder) Clear() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items = nil
}

func commonPrefixLen(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	// 分块比较，比逐字节快
	const chunk = 256
	i := 0
	for i+chunk <= n {
		if !bytes.Equal(a[i:i+chunk], b[i:i+chunk]) {
			break
		}
		i += chunk
	}
	for i < n && a[i] == b[i] {
		i++
	}
	return i
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
