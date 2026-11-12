// Package idem — 幂等请求注册表（内容寻址键 + inflight 复用）
//
// 正交意图（2 个，2026-11-13 kzf 需求：「reply 服务如果能提供幂等性……关键是
// 请求能续上，比如服务端仍然在接收 AI 的响应，这时候如果客户端断了，正常情况
// 下，是服务端的请求也要断开。但是如果我们让请求有粘性有幂等性，那么客户端的
// 请求重新接上后，可以直接复用响应」）：
//  1. 键计算：由 (method + path + body + callerKeyId) 派生，同内容同键
//  2. 上游请求脱离客户端生命周期（WithoutCancel），客户端断开不杀上游
//
// 隐私边界（安全约束，勿改）：键必须包含 callerKeyId，否则不同用户发相同内容
// 会互相命中缓存，造成跨用户响应泄露。同 key 内幂等、跨 key 隔离。
package idem

import (
	"crypto/sha256"
	"encoding/binary"
	"io"
	"net/http"
	"sort"
	"strings"
)

// Key 计算幂等键。
//
// 组成（顺序敏感）：
//
//	callerKeyId || method || path || sortedHeaderSubset || body
//
// 只取影响模型输出的请求头（content-type / anthropic-version 等），
// 忽略 authorization（已由 callerKeyId 表达）、user-agent、trace 类噪音头。
func Key(callerKeyID string, req *http.Request, body []byte) [32]byte {
	h := sha256.New()

	writeStr := func(s string) {
		var lb [4]byte
		binary.BigEndian.PutUint32(lb[:], uint32(len(s)))
		h.Write(lb[:])
		h.Write([]byte(s))
	}

	writeStr(callerKeyID)
	writeStr(req.Method)
	writeStr(req.URL.Path)

	// 参与计算的头部白名单（小写）
	keep := []string{
		"content-type", "anthropic-version", "anthropic-beta",
		"openai-beta", "accept",
	}
	var pairs []string
	for _, k := range keep {
		if v := req.Header.Get(k); v != "" {
			pairs = append(pairs, k+"\x00"+v)
		}
	}
	sort.Strings(pairs)
	for _, p := range pairs {
		writeStr(p)
	}

	var bl [8]byte
	binary.BigEndian.PutUint64(bl[:], uint64(len(body)))
	h.Write(bl[:])
	h.Write(body)

	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// CallerKeyID 从 Authorization 头提取 caller key 标识。
//
// 不返回原始 token（避免落进键材料），返回其短哈希前缀。
func CallerKeyID(h http.Header) string {
	auth := h.Get("Authorization")
	if auth == "" {
		auth = h.Get("X-Api-Key")
	}
	if auth == "" {
		return "anon"
	}
	auth = strings.TrimPrefix(auth, "Bearer ")
	auth = strings.TrimSpace(auth)
	if auth == "" {
		return "anon"
	}
	sum := sha256.Sum256([]byte(auth))
	var hexbuf [16]byte
	const hexdigits = "0123456789abcdef"
	for i := 0; i < 8; i++ {
		hexbuf[i*2] = hexdigits[sum[i]>>4]
		hexbuf[i*2+1] = hexdigits[sum[i]&0xf]
	}
	return string(hexbuf[:])
}

// StreamRequested 判断请求是否要求流式响应
func StreamRequested(body []byte) bool {
	// 轻量探测：不解析完整 JSON，只找 "stream":true / "stream": true
	s := string(body)
	for _, pat := range []string{`"stream":true`, `"stream": true`, `"stream" : true`, `"stream" :true`} {
		if strings.Contains(s, pat) {
			return true
		}
	}
	return false
}

// TeeReader 保留 io 引用（后续可能加 body 预读）
var _ = io.Discard
