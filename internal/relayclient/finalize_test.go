package relayclient

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jixoai/reply9842/internal/clientstream"
)

// 2026-11-16 生产事故：服务端在响应头产生前失败，客户端 handler 什么都没写，
// net/http 自动补了 200 + 空 body，调用方只看到「空返回」。
func TestFinalizeNoHeadWrites502(t *testing.T) {
	cases := []struct {
		name string
		sess *clientstream.Session
		want string
	}{
		{
			name: "frame error 透传原因",
			sess: &clientstream.Session{
				Err: errors.New(`server: Post "http://127.0.0.1:3431/v1/messages": connection refused`),
			},
			want: "connection refused",
		},
		{
			name: "幂等键连续未命中",
			sess: &clientstream.Session{ResumeMiss: true},
			want: "幂等键",
		},
		{
			name: "没有具体原因也用兜底文案",
			sess: &clientstream.Session{},
			want: "没有返回响应头",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			if !finalizeNoHead(rec, c.sess) {
				t.Fatal("应当接管这次失败")
			}
			if rec.Code != 502 {
				t.Fatalf("状态码 = %d，想要 502", rec.Code)
			}
			if body := rec.Body.String(); !strings.Contains(body, c.want) {
				t.Fatalf("响应体 %q 里没有 %q", body, c.want)
			}
			if c.sess.Status != 502 {
				t.Fatalf("session.Status = %d，想要 502（统计要如实记 502 而不是 200）", c.sess.Status)
			}
			if c.sess.Err == nil {
				t.Fatal("session.Err 应当被填上，否则日志里没有失败原因")
			}
		})
	}
}

// 头已经写给下游、或已经正常完成时，不能再补状态码：此时 200 已经在线上了。
func TestFinalizeNoHeadLeavesCommittedResponseAlone(t *testing.T) {
	for _, sess := range []*clientstream.Session{
		{HeadDone: true},
		{Completed: true, HeadDone: true},
	} {
		rec := httptest.NewRecorder()
		if finalizeNoHead(rec, sess) {
			t.Fatalf("已完成/已发头的响应不应被改写：%+v", sess)
		}
		if rec.Body.Len() != 0 {
			t.Fatalf("不应写入任何字节，实际 %q", rec.Body.String())
		}
	}
}
