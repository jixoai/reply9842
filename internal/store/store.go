// Package store — 内容寻址的基版本存储（服务端字典缓存）
//
// 正交意图（2 个，2026-11-12 kzf 需求：「客户端和服务端的缓存如何设计，
// 同时也要保持一致，可能需要有额外的带宽用来协商两边的缓存一致性」）：
//  1. hash → body 的 KV，纯内容寻址（无目录、无版本号、无会话状态）
//  2. 字节上限 LRU：淘汰后自然 miss，客户端收到 DeltaMiss 后重发全量
//
// 设计立场（调研结论，勿改）：一致性靠「哈希相等」而非「版本号对齐」。
// RFC 9842 §2.1.3 明文要求服务端必须校验字典哈希、不得信任 ID。
// 因此本 store 没有任何协商协议——查得到就用，查不到就 miss。
package store

import (
	"container/list"
	"sync"

	"github.com/jixoai/reply9842/internal/codec"
)

// Store 内容寻址字节上限 LRU
type Store struct {
	mu      sync.Mutex
	maxByte int64
	curByte int64
	items   map[codec.Hash]*list.Element
	lru     *list.List // front = 最近使用

	// 统计
	hits, misses, evictions, evictedBytes int64
}

type entry struct {
	hash codec.Hash
	body []byte
}

// New 创建 store；maxByte<=0 时用默认 512MB
func New(maxByte int64) *Store {
	if maxByte <= 0 {
		maxByte = 512 << 20
	}
	return &Store{
		maxByte: maxByte,
		items:   make(map[codec.Hash]*list.Element),
		lru:     list.New(),
	}
}

// Put 存入一份 body，返回其哈希
func (s *Store) Put(body []byte) codec.Hash {
	h := codec.Sum(body)
	s.mu.Lock()
	defer s.mu.Unlock()

	if el, ok := s.items[h]; ok {
		s.lru.MoveToFront(el)
		return h
	}
	// 复制一份，避免调用方复用缓冲区
	cp := make([]byte, len(body))
	copy(cp, body)
	el := s.lru.PushFront(&entry{hash: h, body: cp})
	s.items[h] = el
	s.curByte += int64(len(cp))
	s.evictLocked()
	return h
}

// Get 按哈希取 body（返回内部切片，调用方不得修改）
func (s *Store) Get(h codec.Hash) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	el, ok := s.items[h]
	if !ok {
		s.misses++
		return nil, false
	}
	s.lru.MoveToFront(el)
	s.hits++
	return el.Value.(*entry).body, true
}

// Has 判断是否存在
func (s *Store) Has(h codec.Hash) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.items[h]
	return ok
}

func (s *Store) evictLocked() {
	for s.curByte > s.maxByte && s.lru.Len() > 1 {
		el := s.lru.Back()
		if el == nil {
			break
		}
		e := el.Value.(*entry)
		s.lru.Remove(el)
		delete(s.items, e.hash)
		s.curByte -= int64(len(e.body))
		s.evictions++
		s.evictedBytes += int64(len(e.body))
	}
}

// Stats 快照
type Stats struct {
	Items        int   `json:"items"`
	Bytes        int64 `json:"bytes"`
	MaxBytes     int64 `json:"max_bytes"`
	Hits         int64 `json:"hits"`
	Misses       int64 `json:"misses"`
	Evictions    int64 `json:"evictions"`
	EvictedBytes int64 `json:"evicted_bytes"`
}

// Stats 返回当前统计
func (s *Store) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Stats{
		Items: s.lru.Len(), Bytes: s.curByte, MaxBytes: s.maxByte,
		Hits: s.hits, Misses: s.misses,
		Evictions: s.evictions, EvictedBytes: s.evictedBytes,
	}
}
