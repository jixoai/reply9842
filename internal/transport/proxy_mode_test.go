package transport

// 代理模式的传输约束：
//   - 代理只承载 TCP（HTTP CONNECT / SOCKS5 都是 TCP 隧道），QUIC 是 UDP，
//     走不了代理；
//   - 因此「代理 + 显式 QUIC」必须立刻报错，不能白等一个握手超时；
//   - auto 模式即使带着「上次 QUIC 失败过」的粘性偏好，也永不在代理链路上
//     回头再试 QUIC（早期实现里 prefer 分支会覆盖代理的 [TCP] 顺序）。

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestProxyModeRejectsExplicitQUIC(t *testing.T) {
	d := &Dialer{
		Addr:  "127.0.0.1:9", // 不期望被拨号
		Mode:  KindQUIC,
		Proxy: "http://127.0.0.1:9",
	}
	start := time.Now()
	_, err := d.Dial(context.Background())
	if err == nil {
		t.Fatal("代理 + 显式 QUIC 应当报错，实际返回 nil")
	}
	if !strings.Contains(err.Error(), "代理模式不支持 QUIC") {
		t.Fatalf("错误应明确说明代理不支持 QUIC，实际: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
		t.Fatalf("应当立即失败，实际耗时 %v（疑似仍在拨 QUIC）", elapsed)
	}
}

func TestProxyAutoNeverFallsBackToQUIC(t *testing.T) {
	d := &Dialer{
		Addr:  "127.0.0.1:9",
		Mode:  KindAuto,
		Proxy: "http://127.0.0.1:1", // 无监听：CONNECT 会立即失败
	}
	// 模拟历史：之前 QUIC 失败过，prefer 已被置为 TCP。
	d.prefer.Store(KindTCP)

	start := time.Now()
	_, err := d.Dial(context.Background())
	if err == nil {
		t.Fatal("代理不可达时应当报错，实际返回 nil")
	}
	if strings.Contains(strings.ToLower(err.Error()), "quic") {
		t.Fatalf("代理模式下不得再尝试 QUIC，实际错误: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 700*time.Millisecond {
		t.Fatalf("代理不可达应当快速失败，实际耗时 %v（疑似又等了 QUIC 探测超时）", elapsed)
	}
}
