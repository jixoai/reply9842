package update

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// safeBuffer 是并发安全的 bytes.Buffer（子进程输出从另一个 goroutine 写入）。
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Restart 必须真的启动一个新进程，并继承 stdio（这里用 buffer 捕获）。
func TestRestartSpawnsProcess(t *testing.T) {
	skipIfNoExec(t)
	dir := t.TempDir()
	exe := filepath.Join(dir, "child")
	// 子进程打印参数后退出，便于断言"参数被重放"
	body := "#!/bin/sh\necho \"ARGS:$@\"\n"
	if err := os.WriteFile(exe, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	out := &safeBuffer{}
	err := Restart(exe, []string{"-listen", "127.0.0.1:1234"}, RestartOptions{
		Stdout:      out,
		Stderr:      out,
		WaitForExit: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Restart: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "ARGS:-listen 127.0.0.1:1234") {
		t.Fatalf("参数未被重放，输出: %q", got)
	}
}

// BeforeSpawn 必须在 spawn 之前调用（GUI 的释放单实例锁就在这里）。
// 若顺序错了，GUI 的重启会变成一个"把窗口叫到前面"的假重启。
func TestRestartBeforeSpawnOrdering(t *testing.T) {
	skipIfNoExec(t)
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	exe := filepath.Join(dir, "child")
	// 子进程会检查标记文件是否存在；存在才输出 OK。这就在子进程侧
	// 证明了 BeforeSpawn 已经执行过。
	body := "#!/bin/sh\nif [ -f " + marker + " ]; then echo OK; else echo MISSING; fi\n"
	if err := os.WriteFile(exe, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	out := &safeBuffer{}
	err := Restart(exe, nil, RestartOptions{
		Stdout: out, Stderr: out,
		WaitForExit: 5 * time.Second,
		BeforeSpawn: func() error {
			return os.WriteFile(marker, []byte("released"), 0o644)
		},
	})
	if err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if !strings.Contains(out.String(), "OK") {
		t.Fatalf("BeforeSpawn 应在 spawn 前执行，子进程输出: %q", out.String())
	}
}

// BeforeSpawn 返回错误 → 不 spawn，且错误可辨认。
func TestRestartBeforeSpawnFailure(t *testing.T) {
	skipIfNoExec(t)
	dir := t.TempDir()
	exe := filepath.Join(dir, "child")
	spawned := filepath.Join(dir, "spawned")
	body := "#!/bin/sh\ntouch " + spawned + "\n"
	if err := os.WriteFile(exe, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	boom := errors.New("锁没释放")
	err := Restart(exe, nil, RestartOptions{
		BeforeSpawn: func() error { return boom },
	})
	if !errors.Is(err, boom) {
		t.Fatalf("应返回 BeforeSpawn 的错误，得到 %v", err)
	}
	// 给可能的子进程一点时间落盘（如果错误地 spawn 了）
	time.Sleep(100 * time.Millisecond)
	if _, statErr := os.Stat(spawned); statErr == nil {
		t.Fatal("BeforeSpawn 失败时不该启动子进程")
	}
}

// 目标不可执行 → ErrNotExecutable，且不在 BeforeSpawn 之前动手。
func TestRestartNotExecutable(t *testing.T) {
	skipIfNoExec(t)
	dir := t.TempDir()
	exe := filepath.Join(dir, "noexec")
	if err := os.WriteFile(exe, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	called := false
	err := Restart(exe, nil, RestartOptions{
		BeforeSpawn: func() error { called = true; return nil },
	})
	if !errors.Is(err, ErrNotExecutable) {
		t.Fatalf("应返回 ErrNotExecutable，得到 %v", err)
	}
	if called {
		t.Fatal("目标不可执行时不该释放锁（BeforeSpawn 不应被调用）")
	}
}

func TestRestartMissingTarget(t *testing.T) {
	err := Restart(filepath.Join(t.TempDir(), "nope"), nil, RestartOptions{})
	if err == nil {
		t.Fatal("目标不存在应报错")
	}
}

func TestRestartDirectory(t *testing.T) {
	dir := t.TempDir()
	err := Restart(dir, nil, RestartOptions{})
	if err == nil {
		t.Fatal("目标是目录应报错")
	}
}

// 不等待时应立即返回（GUI 的重启必须这样：旧进程要马上退出释放锁）。
func TestRestartReturnsImmediately(t *testing.T) {
	skipIfNoExec(t)
	dir := t.TempDir()
	exe := filepath.Join(dir, "sleeper")
	// 睡 30 秒：如果 Restart 在等它，这个测试会超时失败。
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	out := &safeBuffer{}
	err := Restart(exe, nil, RestartOptions{Stdout: out, Stderr: out, Detach: true})
	if err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("Restart 应立即返回，耗时 %v", d)
	}
	// 注意：这里留下的 sleep 30 子进程会自然退出（最长 30s）。为了不在
	// 测试机上留孤儿，显式杀掉它。
	killOrphanChildren(t)
}

// killOrphanChildren 回收本测试进程遗留的子进程。
//
// 项目铁律（AGENTS.md）：子代理/测试留下的孤儿进程曾造成两小时排查。
// sleep 30 的子进程若不管，会活到自然结束；这里按父 pid 显式清理。
func killOrphanChildren(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	// pkill -P <ppid> 只杀当前测试进程的直接子进程，不会误伤用户自己的进程。
	cmd := execCommand("pkill", "-P", strconv.Itoa(os.Getpid()))
	_ = cmd.Run() // 没有子进程时返回 1，属正常
}

// execCommand 是 exec.Command 的薄包装（本文件只在 Unix 分支用它）。
func execCommand(name string, args ...string) *exec.Cmd {
	return exec.Command(name, args...)
}

// RestartArgsFrom 默认重放全部参数。
func TestRestartArgsFrom(t *testing.T) {
	orig := os.Args
	defer func() { os.Args = orig }()
	os.Args = []string{"reply9842-gui", "-listen", "127.0.0.1:1", "--autostart"}

	got := RestartArgsFrom()
	want := []string{"-listen", "127.0.0.1:1", "--autostart"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("得到 %v，期望 %v", got, want)
	}

	// 排除一次性标记
	got = RestartArgsFrom("--autostart")
	if strings.Join(got, "|") != "-listen|127.0.0.1:1" {
		t.Fatalf("排除后得到 %v", got)
	}

	// `-listen value` 形态：值必须一并丢掉，否则新进程会多一个位置参数。
	got = RestartArgsFrom("-listen")
	if strings.Join(got, "|") != "--autostart" {
		t.Fatalf("带值 flag 排除失败: %v", got)
	}

	// `--flag=value` 形态
	os.Args = []string{"reply9842-gui", "--retain=30m", "--light"}
	got = RestartArgsFrom("--retain")
	if strings.Join(got, "|") != "--light" {
		t.Fatalf("=value 形态排除失败: %v", got)
	}

	// 排除最后一项且没有值：不该越界
	os.Args = []string{"reply9842-gui", "-listen", "x:1", "-light"}
	got = RestartArgsFrom("-light")
	if strings.Join(got, "|") != "-listen|x:1" {
		t.Fatalf("排除末尾 flag 失败: %v", got)
	}
}

func TestExecutableName(t *testing.T) {
	if got := ExecutableName("/usr/local/bin/reply9842"); got != "reply9842" {
		t.Fatalf("得到 %q", got)
	}
	if got := ExecutableName(""); got != "" {
		t.Fatalf("空路径应返回空，得到 %q", got)
	}
}

// ---- 端到端 Run：检查 → 下载 → 校验 → 替换（不重启）----

func TestRunEndToEnd(t *testing.T) {
	skipIfNoExec(t)
	content := []byte("#!/bin/sh\necho \"reply9842 v0.2.1\"\n")
	f := &fakeRelease{artifact: currentArtifactOrSkip(t), content: content, supportRange: true}
	srv := newFullReleaseServer(t, f)

	workDir := t.TempDir()
	target := filepath.Join(workDir, "reply9842")
	if err := os.WriteFile(target, []byte("#!/bin/sh\necho \"reply9842 v0.1.0\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	u := testUpdater(t, Options{
		Kind:           KindCLI,
		CurrentVersion: "v0.1.0",
		Dir:            filepath.Join(workDir, "cache"),
		Mirrors:        []string{"direct"},
	})
	u.opts.repoBase = srv.URL
	u.opts.apiBase = srv.URL

	info, err := u.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !info.HasUpdate {
		t.Fatal("前置：应有更新")
	}

	res, err := u.Run(context.Background(), info, UpdateOptions{Target: target})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Target != target {
		t.Fatalf("Target 不符: %s", res.Target)
	}
	if res.Backup == "" {
		t.Fatal("应保留旧版本备份")
	}
	if res.Restarted {
		t.Fatal("Relaunch=false 时不该重启")
	}
	if got := readFile(t, target); !strings.Contains(got, "v0.2.1") {
		t.Fatalf("目标未更新: %q", got)
	}
	if got := readFile(t, res.Backup); !strings.Contains(got, "v0.1.0") {
		t.Fatalf("备份内容不对: %q", got)
	}
}

// Run 在 Relaunch=true 时应 spawn 新进程。
func TestRunRelaunch(t *testing.T) {
	skipIfNoExec(t)
	artifact := currentArtifactOrSkip(t)
	// 新版本会写一个 marker 文件，据此判断"新进程真的起来了"
	workDir := t.TempDir()
	marker := filepath.Join(workDir, "relaunched")
	content := []byte("#!/bin/sh\necho \"reply9842 v0.2.1\"\ntouch " + marker + "\n")
	f := &fakeRelease{artifact: artifact, content: content, supportRange: true}
	srv := newFullReleaseServer(t, f)

	target := filepath.Join(workDir, "reply9842")
	if err := os.WriteFile(target, []byte("#!/bin/sh\necho \"reply9842 v0.1.0\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	u := testUpdater(t, Options{
		Kind: KindCLI, CurrentVersion: "v0.1.0",
		Dir: filepath.Join(workDir, "cache"), Mirrors: []string{"direct"},
	})
	u.opts.repoBase, u.opts.apiBase = srv.URL, srv.URL

	info, err := u.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	res, err := u.Run(context.Background(), info, UpdateOptions{
		Target:   target,
		Relaunch: true,
		Args:     []string{},
		Restart:  RestartOptions{Stdout: &safeBuffer{}, Stderr: &safeBuffer{}, Detach: true},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Restarted {
		t.Fatal("Relaunch=true 应 spawn 新进程")
	}
	// 等新进程写 marker
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("新进程未运行（marker 未出现）")
}

// Run 前置校验：Info.Latest 为空要立刻报错，而不是下载一个空 URL。
func TestRunRequiresLatest(t *testing.T) {
	u := testUpdater(t, Options{})
	if _, err := u.Run(context.Background(), Info{}, UpdateOptions{}); err == nil {
		t.Fatal("Info.Latest 为空应报错")
	}
}

// 下载失败时目标必须原样（更新不能"半途弄坏正在运行的二进制"）。
func TestRunDownloadFailureKeepsTarget(t *testing.T) {
	workDir := t.TempDir()
	target := filepath.Join(workDir, "reply9842")
	original := "#!/bin/sh\necho reply9842 v0.1.0\n"
	if err := os.WriteFile(target, []byte(original), 0o755); err != nil {
		t.Fatal(err)
	}

	// 服务器对一切返回 500
	srv := newFailingServer(t)
	u := testUpdater(t, Options{
		Kind: KindCLI, CurrentVersion: "v0.1.0",
		Dir: filepath.Join(workDir, "cache"), Mirrors: []string{"direct"},
	})
	u.opts.repoBase = srv

	info := Info{Latest: "v0.2.1", Artifact: "cli-linux-amd64", Current: "v0.1.0"}
	if _, err := u.Run(context.Background(), info, UpdateOptions{Target: target}); err == nil {
		t.Fatal("下载失败应报错")
	}
	if got := readFile(t, target); got != original {
		t.Fatalf("下载失败时目标被改动了: %q", got)
	}
}

// 分类错误必须能被 errors.Is 识别，调用方据此给不同文案。
func TestSentinelErrors(t *testing.T) {
	// 这里只断言错误变量本身可用（IS 语义由上面的用例覆盖）。
	for _, err := range []error{
		ErrChecksum, ErrSizeMismatch, ErrSelfCheck, ErrPermission,
		ErrDeferred, ErrNotExecutable, ErrRateLimited, ErrNoRelease,
	} {
		if err == nil || err.Error() == "" {
			t.Fatalf("哨兵错误 %v 不应为空", err)
		}
	}
}

// ---- 测试用的服务器/平台辅助 ----

// newFullReleaseServer 起一个同时提供 GitHub API 与 release 下载的假服务器。
//
// 走一个服务器而不是两个：Run 的 Check 与 Download 都会打 repoBase/apiBase，
// 用同一个 httptest 实例能让 opts 的两个覆盖字段指向同一处，少一层拼装。
func newFullReleaseServer(t *testing.T, f *fakeRelease) *httptest.Server {
	t.Helper()
	rel := releaseJSON(t, "v0.2.1", "v0.2.1", "notes", map[string]int64{
		f.artifact: int64(len(f.content)),
	})
	relHandler := f.handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/"+Repo+"/releases/latest" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(rel))
			return
		}
		relHandler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newFailingServer 返回一个对所有下载请求都 500 的地址。
func newFailingServer(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// currentArtifactOrSkip 拿到当前平台的 CLI 产物名。
//
// 用 CLI 而不是 GUI：测试机可能是 linux（GUI 无产物），CLI 全平台都有。
func currentArtifactOrSkip(t *testing.T) string {
	t.Helper()
	a, err := CurrentArtifact(KindCLI)
	if err != nil {
		t.Skipf("当前平台无 CLI 产物: %v", err)
	}
	return a
}
