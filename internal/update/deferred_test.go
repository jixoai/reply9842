package update

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// DeferApply + ApplyPendingIfAny 的完整闭环：这是 Windows「退出后替换」的
// 降级路径，必须在任意平台上都能测（否则只能靠一台 Windows 机器验证）。
func TestDeferredRoundTrip(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "reply9842")
	oldContent := []byte("OLD-BINARY-v0.1.0")
	if err := os.WriteFile(target, append(append([]byte{}, oldContent...), '\n'), 0o755); err != nil {
		t.Fatal(err)
	}

	newContent := []byte("NEW-BINARY-v0.2.1")
	src := filepath.Join(dir, "downloaded")
	if err := os.WriteFile(src, newContent, 0o755); err != nil {
		t.Fatal(err)
	}

	// 阶段一：推迟替换
	newPath, err := DeferApply(src, target)
	if err != nil {
		t.Fatalf("DeferApply: %v", err)
	}
	// 注意：target 经 resolveTarget 解析成绝对路径（macOS 上 /var → /private/var），
	// 所以用 Base 比较而不是整串相等。
	if filepath.Base(newPath) != "reply9842.new" || filepath.Dir(newPath) != filepath.Dir(realPath(t, target)) {
		t.Fatalf(".new 应紧邻目标，得到 %s", newPath)
	}
	// 此刻目标必须**仍是旧内容**——推迟的含义是"现在不动手"。
	if got := readFile(t, target); got != string(oldContent)+"\n" {
		t.Fatalf("DeferApply 不该动目标，得到 %q", got)
	}
	if !PendingUpdateAvailable(target) {
		t.Fatal("应报告有待应用的更新")
	}

	// 阶段二：下次启动完成替换
	applied, err := ApplyPendingIfAny(target)
	if err != nil {
		t.Fatalf("ApplyPendingIfAny: %v", err)
	}
	if !applied {
		t.Fatal("应完成替换")
	}
	if got := readFile(t, target); got != string(newContent) {
		t.Fatalf("目标未替换成新内容: %q", got)
	}
	// 标记必须清掉，否则每次启动都白试一次
	if PendingUpdateAvailable(target) {
		t.Fatal("替换后应清掉待应用标记")
	}
	if _, err := os.Stat(target + ".new"); !os.IsNotExist(err) {
		t.Fatal(".new 应已被消费")
	}
	// 旧版本应作为备份留下
	matches, _ := filepath.Glob(target + ".old-*")
	if len(matches) != 1 {
		t.Fatalf("应留 1 个备份，实际 %v", matches)
	}
	if got := readFile(t, matches[0]); got != string(oldContent)+"\n" {
		t.Fatalf("备份内容不对: %q", got)
	}
	// 替换后可执行
	st, _ := os.Stat(target)
	if st.Mode()&0o111 == 0 {
		t.Fatal("替换后应可执行")
	}
}

// 没有待应用的更新时是 no-op（每次启动都会调用它）。
func TestApplyPendingNoop(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "reply9842")
	_ = os.WriteFile(target, []byte("current"), 0o755)

	applied, err := ApplyPendingIfAny(target)
	if err != nil {
		t.Fatalf("无待应用更新不该报错: %v", err)
	}
	if applied {
		t.Fatal("不该报告 applied")
	}
	if got := readFile(t, target); got != "current" {
		t.Fatalf("目标被改动了: %q", got)
	}
}

// 标记在但 .new 没了（用户手工删了 / 上次清理过）→ 清掉悬空标记，不报错。
func TestApplyPendingStaleMarker(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "reply9842")
	_ = os.WriteFile(target, []byte("current"), 0o755)
	_ = os.WriteFile(target+pendingMarkerSuffix, []byte(target+"\n"), 0o644)

	applied, err := ApplyPendingIfAny(target)
	if err != nil {
		t.Fatalf("悬空标记不该报错: %v", err)
	}
	if applied {
		t.Fatal("不该报告 applied")
	}
	if _, err := os.Stat(target + pendingMarkerSuffix); !os.IsNotExist(err) {
		t.Fatal("悬空标记应被清掉")
	}
}

// .new 是空文件 → 绝不能应用（否则目标变空 = 变砖），清掉并报错。
func TestApplyPendingRejectsEmptyNew(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "reply9842")
	_ = os.WriteFile(target, []byte("current"), 0o755)
	_ = os.WriteFile(target+".new", nil, 0o755)
	_ = os.WriteFile(target+pendingMarkerSuffix, []byte(target+"\n"), 0o644)

	applied, err := ApplyPendingIfAny(target)
	if err == nil {
		t.Fatal("空 .new 应报错")
	}
	if applied {
		t.Fatal("不该应用空文件")
	}
	if got := readFile(t, target); got != "current" {
		t.Fatalf("目标被改动了: %q", got)
	}
	if _, err := os.Stat(target + ".new"); !os.IsNotExist(err) {
		t.Fatal("空 .new 应被丢弃")
	}
}

// DeferApply 应能覆盖上一次的 .new（重复失败后重试）。
func TestDeferApplyOverwrites(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "reply9842")
	_ = os.WriteFile(target, []byte("old"), 0o755)

	first := filepath.Join(dir, "first")
	_ = os.WriteFile(first, []byte("FIRST"), 0o755)
	if _, err := DeferApply(first, target); err != nil {
		t.Fatal(err)
	}

	second := filepath.Join(dir, "second")
	_ = os.WriteFile(second, []byte("SECOND"), 0o755)
	if _, err := DeferApply(second, target); err != nil {
		t.Fatalf("第二次 DeferApply: %v", err)
	}

	if got := readFile(t, target+".new"); got != "SECOND" {
		t.Fatalf(".new 应被覆盖成最新内容，得到 %q", got)
	}
}

// isLockedError 的误判面：不该把所有错误都当成"被占用"。
func TestIsLockedError(t *testing.T) {
	locked := []error{
		errors.New("rename: The process cannot access the file because it is being used by another process."),
		errors.New("open: Access is denied."),
		errors.New("permission denied"),
		errors.New("operation not permitted"),
		errors.New("另一个程序正在使用此文件，进程无法访问。"),
	}
	for _, err := range locked {
		if !isLockedError(err) {
			t.Fatalf("%v 应判为被占用", err)
		}
	}
	notLocked := []error{
		nil,
		errors.New("no space left on device"),
		errors.New("cross-device link"),
		errors.New("file name too long"),
	}
	for _, err := range notLocked {
		if isLockedError(err) {
			t.Fatalf("%v 不该判为被占用", err)
		}
	}
}

// 进度标签用于日志/界面：直连要显示成可读文案而不是空。
func TestProgressMirrorLabel(t *testing.T) {
	if got := MirrorLabel(""); !strings.Contains(got, "直连") {
		t.Fatalf("直连标签应可读，得到 %q", got)
	}
	if got := MirrorLabel("https://gh-proxy.com/"); got != "gh-proxy.com" {
		t.Fatalf("得到 %q", got)
	}
}

// realPath 返回解析过符号链接的绝对路径（用于与 resolveTarget 的结果比较）。
func realPath(t *testing.T, p string) string {
	t.Helper()
	got, err := resolveTarget(p)
	if err != nil {
		t.Fatal(err)
	}
	return got
}
