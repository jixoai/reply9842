package relayclient

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"testing"
	"time"
)

// TestStartStop 启动→查状态→停止的完整往返必须不阻塞，且 Start 返回后
// 端口已经真实可用（GUI 的启动按钮依赖这一点）。
//
// 这个测试专门盯一类 bug：Start 内部持有 mu 时又调用了会加锁的辅助函数，
// 结果是 Start 永久阻塞、而监听端口已经 bind 成功——现象是「端口在听、
// 界面永远显示未连接」，极难从界面上看出来。
func TestStartStop(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{
		// 端口填 0 让内核分配，避免与真实实例冲突
		Listen:    "127.0.0.1:0",
		Server:    "127.0.0.1:1", // 不会被连上：本测试不发请求
		Transport: "tcp",
		LogPath:   dir + "/client.jsonl",
		StatsPath: dir + "/stats.json",
	})
	r.SetLogger(log.New(os.Stderr, "[test] ", 0))

	done := make(chan error, 1)

	if err := r.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if !r.Running() {
		t.Errorf("Start 返回后 Running() 应为 true")
	}
	if addr := r.Addr(); addr == "" || addr == "127.0.0.1:0" {
		t.Errorf("Addr() 应给出内核分配的真实端口，得到 %q", addr)
	}
	if r.StartedAt().IsZero() {
		t.Errorf("StartedAt 不应是零值")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() { done <- r.Stop(ctx) }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Stop: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop 超时（可能的锁重入死锁）")
	}

	if r.Running() {
		t.Errorf("Stop 之后 Running() 应为 false")
	}
}

// TestStartReportsBindError 端口被占用时 Start 要立刻返回错误，
// 而不是假装成功（GUI 会把它显示在状态栏）。
func TestStartReportsBindError(t *testing.T) {
	dir := t.TempDir()
	// Listen 必须显式给 0：留空会被 Normalize 填成默认端口，测试就依赖真实端口了
	base := Config{Listen: "127.0.0.1:0", LogPath: dir + "/a.jsonl", StatsPath: dir + "/a-stats.json", Transport: "tcp"}

	first := New(base)
	first.SetLogger(log.New(os.Stderr, "[1] ", 0))
	if err := first.Start(); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	defer first.Stop(context.Background())

	// 第二个实例指向同一个监听地址
	second := New(Config{
		Listen: first.Addr(), Transport: "tcp",
		LogPath: dir + "/b.jsonl", StatsPath: dir + "/b-stats.json",
	})
	second.SetLogger(log.New(os.Stderr, "[2] ", 0))
	err := second.Start()
	if err == nil {
		second.Stop(context.Background())
		t.Fatalf("端口已被占用时 Start 应返回错误")
	}
	if second.Running() {
		t.Errorf("Start 失败后 Running() 应为 false")
	}
	if second.Err() == nil {
		t.Errorf("Start 失败后 Err() 应保存原因")
	}
}

// TestSnapshotNotRunning 未运行时快照是全零，不是上次残留
func TestSnapshotNotRunning(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{Listen: "127.0.0.1:0", Transport: "tcp",
		LogPath: dir + "/c.jsonl", StatsPath: dir + "/c-stats.json"})

	if agg := r.Snapshot(); agg.Requests != 0 || agg.UpRaw != 0 {
		t.Errorf("未运行时快照应为零值，得到 %+v", agg)
	}
}

// TestSetConfigWhileRunning 运行中改配置必须被拒绝，避免「改了参数但旧监听还在跑」
func TestSetConfigWhileRunning(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{Listen: "127.0.0.1:0", Transport: "tcp",
		LogPath: dir + "/d.jsonl", StatsPath: dir + "/d-stats.json"})
	r.SetLogger(log.New(os.Stderr, "[cfg] ", 0))
	if err := r.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer r.Stop(context.Background())

	if err := r.SetConfig(Config{Listen: "127.0.0.1:1"}); err == nil {
		t.Errorf("运行中 SetConfig 应返回错误")
	}
}

// panicListener 在 Accept 时 panic，模拟「监听循环里冒出未捕获 panic」。
type panicListener struct{}

func (panicListener) Accept() (net.Conn, error) { panic("boom: accept") }
func (panicListener) Close() error              { return nil }
func (panicListener) Addr() net.Addr            { return &net.TCPAddr{} }

// TestServeSafelyTurnsPanicIntoError 监听 goroutine 的 panic 必须变成普通错误。
//
// 契约（owner 2026-11-14：「可以 let it crash，但关键是要有自恢复」）：
// serveSafely 恢复 panic 并返回 panicked=true，supervisor 据此走「退避 + 重开
// 监听」，服务在同一个进程里自己回来，而不是把 running 置 false 就下线。
func TestServeSafelyTurnsPanicIntoError(t *testing.T) {
	srv := &http.Server{Handler: http.NewServeMux()}
	err, panicked := serveSafely(srv, panicListener{})
	if !panicked {
		t.Fatalf("panic 没有被转成错误（panicked=false, err=%v）", err)
	}
	if err == nil {
		t.Fatal("panicked 时 err 不能是 nil")
	}
}
