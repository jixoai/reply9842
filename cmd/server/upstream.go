package main

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// upstreamSet 上游 base URL 列表 + 拨号期故障转移。
//
// 为什么是列表：magpie 是蓝绿部署（a 槽 3425 / b 槽 3426，rolling-update.sh
// 翻转两槽的 down 标记）。写死单个端口的话，每次轮转之后 relay 就整体失联，
// 而现象只是「客户端拿到空返回」——2026-11-16 生产实测就是被配到了 b 槽的
// UI 端口 3431（standby，down），每个请求都 connection refused。
//
// 故障转移只发生在**拨号**阶段：dial 失败说明请求一个字节都没发出去，换下
// 一个候选完全安全；请求已经发出去之后的失败不重试（POST 不可重放）。
type upstreamSet struct {
	targets []*url.URL
	base    string // 首个候选的 scheme://host，用来拼请求 URL
}

func parseUpstreams(spec string) (*upstreamSet, error) {
	var targets []*url.URL
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		u, err := url.Parse(part)
		if err != nil {
			return nil, fmt.Errorf("上游地址非法 %q: %w", part, err)
		}
		if u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("上游地址非法 %q（要形如 http://127.0.0.1:3425）", part)
		}
		targets = append(targets, u)
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("上游地址为空")
	}
	return &upstreamSet{
		targets: targets,
		base:    targets[0].Scheme + "://" + targets[0].Host,
	}, nil
}

// URL 把请求路径拼到首选上游上。真正的拨号地址由 dial 决定，可能与这里
// 的 host 不同（首选槽挂了的时候）。
func (u *upstreamSet) URL(path string) string {
	return strings.TrimSuffix(u.base, "/") + path
}

// dial 依次尝试候选上游，返回第一个连通的连接。
//
// Transport 传进来的 addr 被忽略：它只是「首选上游」的 host:port，而候选
// 列表才是权威。这样 http.Transport 的连接池、keep-alive、TLS 逻辑都不用改，
// 只在真正建连时做一次短路重试。
func (u *upstreamSet) dial(ctx context.Context, network, _ string) (net.Conn, error) {
	var lastErr error
	for _, t := range u.targets {
		d := net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
		conn, err := d.DialContext(ctx, network, t.Host)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("没有可用的上游")
	}
	return nil, lastErr
}
