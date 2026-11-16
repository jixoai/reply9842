package main

// 透传回归：两条曾被「失败测试」钉住的破坏路径，现已修复。
//
// 背景（2026-11-16）：magpie↔magpie 的 RFC 9842 dcz 协商要求链路逐字节透传
// 一组自定义头与 `Content-Encoding: dcz` 的 body。透传普查发现两处**既有**
// 实现缺陷，当时按"先钉住、后修复"（不允许 t.Skip）的方式留下必然失败的
// 用例；实现修复后本文件的用例全部转绿，作为长期回归保护。
//
//  1. 多值头截断：internal/meta/frame.go 的 EncodeHeaders 只写每个头的 vs[0]，
//     任何重复头（Set-Cookie、X-Magpie-Kept-Body、多值 Accept…）都会在
//     client↔server 两个方向静默丢值。修复：每个值各写一行（DecodeHeaders
//     本就会 append 成多值）。
//
//  2. query string 丢失：客户端构造帧时只带 r.URL.Path，服务端拼上游 URL
//     时 `POST /v1/messages?beta=true&alt=sse` 变成 `/v1/messages`；幂等键
//     也只含 path，不同 query 会撞键。修复：帧里带 RequestURI，idem.Key
//     纳入 RawQuery。
//
// 相关实现位置：internal/meta/frame.go、internal/relayclient/runner.go、
// internal/idem/key.go、cmd/server/main.go（statusservername 记录）。

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"testing"
)

// TestMultiValueHeaderPreserved_Request 请求方向：
// 重复出现的协商头（同一头两个值）必须原样到达上游。
func TestMultiValueHeaderPreserved_Request(t *testing.T) {
	g := newTestRig(t)

	dictA := sha256.Sum256([]byte("dictionary-one"))
	dictB := sha256.Sum256([]byte("dictionary-two"))
	vA := ":" + base64.StdEncoding.EncodeToString(dictA[:]) + ":"
	vB := ":" + base64.StdEncoding.EncodeToString(dictB[:]) + ":"
	digestA := sha256.Sum256([]byte("body-digest-one"))
	digestB := sha256.Sum256([]byte("body-digest-two"))

	hdrs := map[string][]string{
		"Content-Type":         {"application/json"},
		"Content-Encoding":     {"dcz"},
		"Available-Dictionary": {vA, vB},
		"X-Magpie-Body-Digest": {hex.EncodeToString(digestA[:]), hex.EncodeToString(digestB[:])},
	}
	status, _, _ := g.post("/v1/messages", fakeDczBody(4<<10, "multi-req"), hdrs)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，想要 200", status)
	}
	hits := g.hitsSnapshot()
	if len(hits) != 1 {
		t.Fatalf("上游应收到 1 次请求，实际 %d 次", len(hits))
	}
	for key, want := range map[string][]string{
		"Available-Dictionary": {vA, vB},
		"X-Magpie-Body-Digest": {hex.EncodeToString(digestA[:]), hex.EncodeToString(digestB[:])},
	} {
		got := hits[0].Header.Values(key)
		if !slices.Equal(got, want) {
			t.Errorf("多值头在 client→server 方向被截断：%s got=%q want=%q（EncodeHeaders 只写 vs[0]，见 internal/meta/frame.go）",
				key, got, want)
		}
	}
}

// TestMultiValueHeaderPreserved_Response 响应方向：
// 上游重复的响应头（如两行 X-Magpie-Kept-Body）必须都在下游出现。
func TestMultiValueHeaderPreserved_Response(t *testing.T) {
	g := newTestRig(t)
	g.setReply(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Add("X-Magpie-Kept-Body", ":b25l:")
		w.Header().Add("X-Magpie-Kept-Body", ":dHdv:")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	})

	status, hdr, _ := g.post("/v1/messages", fakeDczBody(2<<10, "multi-resp"), nil)
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，想要 200", status)
	}
	want := []string{":b25l:", ":dHdv:"}
	if got := hdr.Values("X-Magpie-Kept-Body"); !slices.Equal(got, want) {
		t.Errorf("多值响应头在 server→client 方向被截断：got=%q want=%q（EncodeHeaders 只写 vs[0]，见 internal/meta/frame.go）",
			got, want)
	}
}

// TestQueryStringPreserved 请求行的 query string 必须逐字节转发。
func TestQueryStringPreserved(t *testing.T) {
	g := newTestRig(t)
	const uri = "/v1/messages?beta=true&alt=sse&x=%2Fslash"

	status, _, _ := g.post(uri, fakeDczBody(2<<10, "query"), map[string][]string{
		"Content-Type": {"application/json"},
	})
	if status != http.StatusOK {
		t.Fatalf("状态 = %d，想要 200", status)
	}
	hits := g.hitsSnapshot()
	if len(hits) != 1 {
		t.Fatalf("上游应收到 1 次请求，实际 %d 次", len(hits))
	}
	if hits[0].URI != uri {
		t.Errorf("query string 在 client→server 方向丢失：上游收到 %q，想要 %q（runner.go 只取 r.URL.Path）",
			hits[0].URI, uri)
	}
}

// 上游 4xx/5xx 必须在服务端逐请求日志里如实记录。
//
// 曾经的缺陷：serveOne 用 streamToClient 的返回值（本次推送完成度，恒为 200）
// 无条件覆盖 rec.Status，于是上游 400 在 server.jsonl 里记成 200 —— 与
// 「上游失败被渲染成空 200」同属一类「统计说谎」，排障时会把 4xx 当成成功。
func TestUpstreamErrorRecordedWithRealStatus(t *testing.T) {
	g := newTestRig(t)
	g.setReply(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream says no"}}`))
	})

	status, _, _ := g.post("/v1/messages", fakeDczBody(2<<10, "err-status"), nil)
	if status != http.StatusBadRequest {
		t.Fatalf("下游状态 = %d，想要 400", status)
	}

	rec := lastServerRecord(t, g.serverLog)
	if got := rec["status"]; got != float64(400) {
		t.Fatalf("服务端日志 status = %v，想要 400（上游失败被记成成功会让排障失去依据）", got)
	}
}

func lastServerRecord(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读服务端日志: %v", err)
	}
	lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
	if len(lines) == 0 {
		t.Fatal("服务端日志为空")
	}
	var rec map[string]any
	if err := json.Unmarshal(lines[len(lines)-1], &rec); err != nil {
		t.Fatalf("解析最后一行: %v\n%s", err, lines[len(lines)-1])
	}
	return rec
}
