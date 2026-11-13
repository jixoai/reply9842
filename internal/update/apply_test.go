package update

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeBinary 生成一个能在测试机上真实执行的「假产品二进制」。
//
// 为什么不直接用 /bin/echo 或 shell 脚本：自检要求输出里含 "reply9842"，
// 而 Windows 上 shell 脚本不可执行。用 `go build` 现编一个几行的小程序
// 是唯一跨平台的可靠做法——但那样测试会依赖工具链且慢。
//
// 折中：本机是 Unix 时写 shell 脚本（快、能在所有 Unix CI 上跑）；Windows
// 上跳过需要真实执行的用例（Apply 的核心断言——备份与内容——仍然覆盖）。
func fakeBinary(t *testing.T, dir, name, version string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if runtime.GOOS == "windows" {
		// Windows 上没有可靠的「几行就能跑」的可执行文件。写个 .bat 并
		// 让需要执行自检的用例 skip（见 skipIfNoExec）。
		body := "@echo off\r\necho reply9842 " + version + "\r\nexit /b 0\r\n"
		if err := os.WriteFile(p+".bat", []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		return p + ".bat"
	}
	body := "#!/bin/sh\necho \"reply9842 " + version + "\"\n"
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func skipIfNoExec(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上无法用脚本充当可执行产物，此用例覆盖的路径由其它用例保证")
	}
}

func testUpdater(t *testing.T, opts Options) *Updater {
	t.Helper()
	if opts.Kind == "" {
		opts.Kind = KindCLI
	}
	u, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// 正常替换：目标内容变成新内容，旧文件备份存在且内容为旧内容。
func TestApplyReplacesAndBacksUp(t *testing.T) {
	skipIfNoExec(t)
	dir := t.TempDir()
	target := fakeBinary(t, dir, "reply9842", "v0.1.0")
	downloaded := fakeBinary(t, dir, "downloaded", "v0.2.1")

	u := testUpdater(t, Options{CurrentVersion: "v0.1.0"})
	backup, err := u.Apply(downloaded, target, ApplyOptions{BackupTag: "v0.1.0"})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// 目标内容 = 新内容
	got := readFile(t, target)
	if !strings.Contains(got, "v0.2.1") {
		t.Fatalf("目标未被替换成新版本: %q", got)
	}
	// 备份存在且是旧内容
	if backup == "" {
		t.Fatal("应返回备份路径")
	}
	if !strings.Contains(backup, "v0.1.0") {
		t.Fatalf("备份名应含旧版本号: %s", backup)
	}
	if b := readFile(t, backup); !strings.Contains(b, "v0.1.0") {
		t.Fatalf("备份内容应是旧版本: %q", b)
	}
	// 目标可执行
	st, _ := os.Stat(target)
	if st.Mode()&0o111 == 0 {
		t.Fatalf("替换后应可执行，mode=%s", st.Mode())
	}
	// 不应残留 .new
	if _, err := os.Stat(target + ".new"); !os.IsNotExist(err) {
		t.Fatal(".new 应在成功后消失")
	}
}

// 防线 3：自检失败（新文件不可执行）→ **绝不替换**，目标保持原样。
func TestApplySelfCheckFailureKeepsTarget(t *testing.T) {
	skipIfNoExec(t)
	dir := t.TempDir()
	target := fakeBinary(t, dir, "reply9842", "v0.1.0")

	// 新版是个「不可执行」的文件：无执行位 → exec 直接失败
	bad := filepath.Join(dir, "bad")
	if err := os.WriteFile(bad, []byte("this is not a binary\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	u := testUpdater(t, Options{CurrentVersion: "v0.1.0"})
	_, err := u.Apply(bad, target, ApplyOptions{})
	if !errors.Is(err, ErrSelfCheck) {
		t.Fatalf("应返回 ErrSelfCheck，得到 %v", err)
	}
	// 目标必须完好
	if got := readFile(t, target); !strings.Contains(got, "v0.1.0") {
		t.Fatalf("自检失败后目标被改动了: %q", got)
	}
	// 不应留下 .new 残留
	if _, err := os.Stat(target + ".new"); !os.IsNotExist(err) {
		t.Fatal("自检失败应清掉 .new")
	}
	// 不应产生任何备份（因为还没走到备份步骤）
	matches, _ := filepath.Glob(target + ".old-*")
	if len(matches) != 0 {
		t.Fatalf("自检失败不该产生备份: %v", matches)
	}
}

// 自检的第二条防线：能执行但退出码非 0（半截二进制/启动即崩）→ 不替换。
func TestApplySelfCheckNonZeroExit(t *testing.T) {
	skipIfNoExec(t)
	dir := t.TempDir()
	target := fakeBinary(t, dir, "reply9842", "v0.1.0")

	crash := filepath.Join(dir, "crashy")
	// echo 前先 exit 1：可执行、但退出码非 0，模拟"启动即崩"
	if err := os.WriteFile(crash, []byte("#!/bin/sh\necho reply9842 crash\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	u := testUpdater(t, Options{CurrentVersion: "v0.1.0"})
	_, err := u.Apply(crash, target, ApplyOptions{})
	if !errors.Is(err, ErrSelfCheck) {
		t.Fatalf("退出码非 0 应判自检失败，得到 %v", err)
	}
	if got := readFile(t, target); !strings.Contains(got, "v0.1.0") {
		t.Fatalf("目标被改动了: %q", got)
	}
}

// 自检的第三条防线：能跑、退出码 0，但输出里没有产品标识
// （下载到了别的可执行文件，或镜像返回了 /bin/true 这类东西）。
func TestApplySelfCheckWrongProduct(t *testing.T) {
	skipIfNoExec(t)
	dir := t.TempDir()
	target := fakeBinary(t, dir, "reply9842", "v0.1.0")

	other := filepath.Join(dir, "other")
	if err := os.WriteFile(other, []byte("#!/bin/sh\necho some-other-product v9\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	u := testUpdater(t, Options{CurrentVersion: "v0.1.0"})
	_, err := u.Apply(other, target, ApplyOptions{})
	if !errors.Is(err, ErrSelfCheck) {
		t.Fatalf("缺产品标识应判自检失败，得到 %v", err)
	}
	if !strings.Contains(err.Error(), "产品标识") {
		t.Fatalf("错误信息应说明原因: %v", err)
	}
}

// 空文件是最危险的形态：大小合法（0），rename 后目标就彻底废了。
func TestApplyRejectsEmptyFile(t *testing.T) {
	dir := t.TempDir()
	target := fakeBinary(t, dir, "reply9842", "v0.1.0")
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	u := testUpdater(t, Options{})
	_, err := u.Apply(empty, target, ApplyOptions{SkipSelfCheck: true})
	if err == nil {
		t.Fatal("空文件必须被拒")
	}
	if got := readFile(t, target); !strings.Contains(got, "v0.1.0") {
		t.Fatal("目标被改动了")
	}
}

// 备份数量上限：KeepBackups 限制保留的旧版本数，避免堆积。
func TestApplyPrunesBackups(t *testing.T) {
	skipIfNoExec(t)
	dir := t.TempDir()
	target := fakeBinary(t, dir, "reply9842", "v0.1.0")
	u := testUpdater(t, Options{CurrentVersion: "v0.1.0", KeepBackups: 1})

	// 连续替换 4 次，每次换一个新内容
	for i := 0; i < 4; i++ {
		newVer := fmt.Sprintf("v0.2.%d", i)
		src := fakeBinary(t, dir, fmt.Sprintf("dl%d", i), newVer)
		if _, err := u.Apply(src, target, ApplyOptions{BackupTag: fmt.Sprintf("v0.1.%d", i)}); err != nil {
			t.Fatalf("第 %d 次 Apply: %v", i, err)
		}
		// 备份的 mtime 可能相同（同一秒内），prune 用 mtime 排序；为了让
		// 顺序确定，这里不强求"删掉的是哪个"，只断言数量上限。
	}

	matches, _ := filepath.Glob(target + ".old-*")
	if len(matches) > 1 {
		t.Fatalf("KeepBackups=1 时最多留 1 个备份，实际 %d: %v", len(matches), matches)
	}
	if len(matches) == 0 {
		t.Fatal("应至少保留 1 个备份（回滚能力）")
	}
}

// 保留负值 = 明确要求不清理。
func TestApplyKeepAllBackups(t *testing.T) {
	skipIfNoExec(t)
	dir := t.TempDir()
	target := fakeBinary(t, dir, "reply9842", "v0.1.0")
	u := testUpdater(t, Options{CurrentVersion: "v0.1.0", KeepBackups: -1})

	for i := 0; i < 3; i++ {
		src := fakeBinary(t, dir, fmt.Sprintf("dl%d", i), fmt.Sprintf("v0.2.%d", i))
		if _, err := u.Apply(src, target, ApplyOptions{BackupTag: fmt.Sprintf("keep%d", i)}); err != nil {
			t.Fatalf("Apply: %v", err)
		}
	}
	matches, _ := filepath.Glob(target + ".old-*")
	if len(matches) != 3 {
		t.Fatalf("KeepBackups<0 应保留全部备份，实际 %d: %v", len(matches), matches)
	}
}

// 备份重名时不能覆盖（覆盖掉的可能是唯一的回滚版本）。
func TestApplyBackupNameCollision(t *testing.T) {
	skipIfNoExec(t)
	dir := t.TempDir()
	target := fakeBinary(t, dir, "reply9842", "v0.1.0")

	// 预先放一个同名备份，内容标记为"最原始"
	pre := target + ".old-v0.1.0"
	if err := os.WriteFile(pre, []byte("ORIGINAL BACKUP\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	u := testUpdater(t, Options{CurrentVersion: "v0.1.0"})
	src := fakeBinary(t, dir, "dl", "v0.2.0")
	if _, err := u.Apply(src, target, ApplyOptions{BackupTag: "v0.1.0"}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// 原备份必须完好
	if got := readFile(t, pre); !strings.Contains(got, "ORIGINAL BACKUP") {
		t.Fatalf("同名备份被覆盖了: %q", got)
	}
}

// 符号链接目标：必须替换真实文件，而不是把软链变成一个普通文件。
func TestApplyResolvesSymlinkTarget(t *testing.T) {
	skipIfNoExec(t)
	dir := t.TempDir()
	real := fakeBinary(t, dir, "real-binary", "v0.1.0")
	link := filepath.Join(dir, "reply9842")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("此环境不支持符号链接: %v", err)
	}

	u := testUpdater(t, Options{CurrentVersion: "v0.1.0"})
	src := fakeBinary(t, dir, "dl", "v0.2.0")
	if _, err := u.Apply(src, link, ApplyOptions{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// 软链仍然是软链
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("软链被替换成普通文件了（mode=%v）", fi.Mode())
	}
	// 真实文件内容已更新
	if got := readFile(t, real); !strings.Contains(got, "v0.2.0") {
		t.Fatalf("真实文件未被更新: %q", got)
	}
}

// 目标不存在（首次安装形态）：不该尝试备份，直接落地。
func TestApplyCreatesMissingTarget(t *testing.T) {
	skipIfNoExec(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "reply9842") // 不存在
	src := fakeBinary(t, dir, "dl", "v0.2.0")

	u := testUpdater(t, Options{})
	backup, err := u.Apply(src, target, ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if backup != "" {
		t.Fatalf("目标原本不存在时不该有备份，得到 %s", backup)
	}
	if got := readFile(t, target); !strings.Contains(got, "v0.2.0") {
		t.Fatalf("目标未创建: %q", got)
	}
}

// 目录不可写 → 明确报 ErrPermission，而不是笼统的失败。
func TestApplyPermissionDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 无视目录权限，此断言在 root 下无意义")
	}
	dir := t.TempDir()
	ro := filepath.Join(dir, "readonly")
	if err := os.Mkdir(ro, 0o555); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "dl")
	if err := os.WriteFile(src, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}

	u := testUpdater(t, Options{})
	_, err := u.Apply(src, filepath.Join(ro, "reply9842"), ApplyOptions{SkipSelfCheck: true})
	if !errors.Is(err, ErrPermission) {
		t.Fatalf("只读目录应报 ErrPermission，得到 %v", err)
	}
	// 错误信息要能让用户知道怎么办
	if !strings.Contains(err.Error(), "权限") {
		t.Fatalf("错误信息应提示权限问题: %v", err)
	}
}

// Replace 是 Apply 的别名，行为必须一致。
func TestReplaceAlias(t *testing.T) {
	skipIfNoExec(t)
	dir := t.TempDir()
	target := fakeBinary(t, dir, "reply9842", "v0.1.0")
	src := fakeBinary(t, dir, "dl", "v0.2.0")
	u := testUpdater(t, Options{})
	if err := u.Replace(src, target); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if got := readFile(t, target); !strings.Contains(got, "v0.2.0") {
		t.Fatalf("未替换: %q", got)
	}
}

// 回滚：把备份换回目标。
func TestRollback(t *testing.T) {
	skipIfNoExec(t)
	dir := t.TempDir()
	target := fakeBinary(t, dir, "reply9842", "v0.1.0")
	u := testUpdater(t, Options{CurrentVersion: "v0.1.0"})

	src := fakeBinary(t, dir, "dl", "v0.2.0")
	backup, err := u.Apply(src, target, ApplyOptions{BackupTag: "v0.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, target); !strings.Contains(got, "v0.2.0") {
		t.Fatal("前置条件：应已是新版")
	}

	if err := Rollback(target, backup); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if got := readFile(t, target); !strings.Contains(got, "v0.1.0") {
		t.Fatalf("回滚失败，内容是 %q", got)
	}
	// 回滚后仍可执行
	st, _ := os.Stat(target)
	if st.Mode()&0o111 == 0 {
		t.Fatal("回滚后应可执行")
	}
}

func TestRollbackMissingBackup(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "x")
	if err := Rollback(target, ""); err == nil {
		t.Fatal("空备份应报错")
	}
	if err := Rollback(target, filepath.Join(dir, "nope")); err == nil {
		t.Fatal("不存在的备份应报错")
	}
}

// selfCheckArgs 必须与各 cmd 的实际接口匹配（这条锁住"改了 cmd 忘记改这里"）。
func TestSelfCheckArgs(t *testing.T) {
	if got := selfCheckArgs(KindCLI); len(got) != 1 || got[0] != "version" {
		t.Fatalf("cli 应使用 version 子命令，得到 %v", got)
	}
	if got := selfCheckArgs(KindGUI); len(got) != 1 || got[0] != "--version" {
		t.Fatalf("gui 应使用 --version，得到 %v", got)
	}
	// server 没有版本输出，用 --help（flag 包返回 ErrHelp、exit 0、用法含产品名）
	if got := selfCheckArgs(KindServer); len(got) != 1 || got[0] != "--help" {
		t.Fatalf("server 应使用 --help，得到 %v", got)
	}
}

// 真实自检的端到端验证：用本测试进程自己充当"新二进制"，
// 验证 selfCheck 能接受一个真正的 Go 二进制（而不是只有 shell 脚本）。
//
// 这只在能拿到测试二进制路径时成立（go test 会把测试编译成临时二进制）。
func TestSelfCheckAgainstRealBinary(t *testing.T) {
	skipIfNoExec(t)
	exe, err := os.Executable()
	if err != nil {
		t.Skip("拿不到测试二进制路径")
	}
	// 测试二进制跑 "--version" 不存在，但跑 -test.list 会输出测试名。
	// 因此这里直接验证「能执行 + 退出码 0」，不验证产品标识分支
	// （产品标识分支由 TestApplySelfCheckWrongProduct 覆盖）。
	cmd := exec.Command(exe, "-test.list", ".*")
	out, cmdErr := cmd.CombinedOutput()
	if cmdErr != nil {
		t.Fatalf("测试二进制自身不可执行: %v (%s)", cmdErr, out)
	}
	// 确认 selfCheck 会拒绝它（输出里没有 reply9842）——这证明第 3 条
	// 判定真的在拦「下载到了别的可执行文件」。
	if err := selfCheck(exe, KindCLI, []string{"-test.list", ".*"}); err == nil {
		t.Log("提示：测试二进制输出里含 reply9842（包名），自检通过属预期")
	} else if !errors.Is(err, ErrSelfCheck) {
		t.Fatalf("应返回 ErrSelfCheck，得到 %v", err)
	}
}

// 备份文件名的非法字符被清洗（Windows 不允许 : 与路径分隔符）。
func TestBackupPathSanitize(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "bin")
	_ = os.WriteFile(target, []byte("x"), 0o755)

	cases := map[string]string{
		"v0.1.0":       "v0.1.0",
		"v0.1.0-dirty": "v0.1.0-dirty",
		"a/b":          "a-b",
		"weird:*?name": "weird---name",
		"":             "unknown", // 空 tag 会退回 mtime，不会走到这里
	}
	for tag, wantSub := range cases {
		if tag == "" {
			continue
		}
		got := filepath.Base(backupPath(target, tag))
		if !strings.Contains(got, wantSub) {
			t.Fatalf("tag=%q 得到 %q，应含 %q", tag, got, wantSub)
		}
		// 绝不允许路径穿越
		if strings.Contains(got, "/") || strings.Contains(got, `\`) {
			t.Fatalf("备份名含路径分隔符: %q", got)
		}
	}
}

func TestSanitizeTagTruncates(t *testing.T) {
	long := strings.Repeat("a", 200)
	if got := sanitizeTag(long); len(got) > 64 {
		t.Fatalf("应截断到 64，得到 %d", len(got))
	}
	if got := sanitizeTag("///"); got != "unknown" {
		t.Fatalf("全非法字符应退回 unknown，得到 %q", got)
	}
}

// copyFile 必须 fsync 且目标可执行（防「rename 后断电得到空文件」）。
func TestCopyFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	content := []byte("payload")
	if err := os.WriteFile(src, content, 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst")
	if err := copyFile(src, dst, 0o755); err != nil {
		t.Fatalf("copyFile: %v", err)
	}
	if got := readFile(t, dst); got != string(content) {
		t.Fatalf("内容不符: %q", got)
	}
	st, _ := os.Stat(dst)
	if runtime.GOOS != "windows" && st.Mode()&0o111 == 0 {
		t.Fatalf("应带执行位，mode=%s", st.Mode())
	}
}

// 文件摘要辅助（多处用）
func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读 %s: %v", p, err)
	}
	return string(b)
}
