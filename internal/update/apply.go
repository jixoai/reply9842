// 安全替换与重启。
//
// 这是整个更新链路里唯一会动「用户此刻正在运行的东西」的一步，也是唯一
// 会把应用变砖的一步。设计立场：**宁可更新失败，也不能留下不可启动的状态**。
// 每一道防线都能独立中止，且中止时现有二进制完好无损。
//
// 顺序（每一步失败都停下，不动目标）：
//
//  1. 新文件 sha256 由 Download 保证（这里再核一次大小非零，防被外部清空）
//  2. 跑 `<新文件> --version` 自检：能启动、退出码 0、输出含版本号
//  3. 新文件放在**目标同目录**的 `<target>.new`（跨文件系统 rename 会失败）
//  4. 旧文件 rename 成 `<target>.old-<旧版本>`（保留回滚能力）
//  5. `<target>.new` rename 成 `<target>`（同目录、同文件系统，原子）
//  6. 清理超出 KeepBackups 的旧备份（只留最近 N 个）
//
// 若第 5 步失败，把第 4 步的备份 rename 回去——否则目标消失、应用起不来。
package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// ErrSelfCheck 表示新二进制的自检失败（无法执行 / 退出码非 0 / 无版本输出）。
//
// 这是拦住「坏产物」的最后一道，也是最有用的一道：架构不符（在 arm 上跑
// x86 产物）、动态库缺失、半截 ELF（那个 SEGV 事故）都会在这里暴露，
// 而不是等用户重启后才发现应用打不开。
var ErrSelfCheck = errors.New("update: 新二进制自检失败，已放弃替换")

// ErrPermission 表示没有写权限（系统级安装、只读卷、SIP 保护路径）。
//
// 单独定义是**刻意不提权**：用 sudo 替用户改 /usr/local/bin 会留下 root
// 所有的文件，用户下次用普通权限更新会失败，且卸载要 sudo。让用户自己
// 决定用包管理器还是手工替换，比替他冒险好。
var ErrPermission = errors.New("update: 没有写入权限")

// ApplyOptions 是 Apply 的细粒度选项。零值即默认行为。
type ApplyOptions struct {
	// SkipSelfCheck 跳过「跑新二进制 --version」的自检。**仅供测试**：
	// 生产的每一次 Apply 都必须自检（理由见 ErrSelfCheck）。
	SkipSelfCheck bool
	// SelfCheckArgs 覆盖自检用的参数，默认 ["--version"]。
	//
	// cli 支持 `version` 子命令（无 - 前缀），gui 支持 `--version`，server
	// **两者都没有**（只有 flag 解析，无版本输出）。见 selfCheckArgs 的说明。
	SelfCheckArgs []string
	// BackupTag 是备份文件名里的版本标记，默认取目标文件的 mtime 时间戳。
	// 调用方知道旧版本号时传进来更可读（<target>.old-v0.1.0）。
	BackupTag string
}

// Apply 用 downloadedPath 替换 targetPath。
//
// targetPath 通常是 SelfPath() 的结果（不是 os.Args[0]：后者可能是 PATH 里
// 一个符号链接，替换符号链接本身不会影响真实文件）。
//
// 返回值为被替换掉的旧版本备份路径（已不存在于原位时为空串）。调用方可以
// 把它显示给用户（「已保留 v0.1.0，可手工回滚」）。
func (u *Updater) Apply(downloadedPath, targetPath string, aopts ApplyOptions) (string, error) {
	if downloadedPath == "" || targetPath == "" {
		return "", fmt.Errorf("update: Apply 需要下载路径与目标路径")
	}

	srcInfo, err := os.Stat(downloadedPath)
	if err != nil {
		return "", fmt.Errorf("update: 新文件不可读: %w", err)
	}
	// 空文件是最危险的形态：它有合法大小 0，rename 后目标就彻底废了。
	if srcInfo.Size() == 0 {
		return "", fmt.Errorf("%w：下载文件大小为 0", ErrSelfCheck)
	}

	target, err := resolveTarget(targetPath)
	if err != nil {
		return "", err
	}
	// 目标目录必须可写。提前查一次是为了给出「需要管理员权限」这种可行动
	// 的错误，而不是等 rename 抛出 EACCES 让人猜。
	if err := ensureWritableDir(filepath.Dir(target)); err != nil {
		return "", err
	}

	// 防线 3：自检。**必须在动目标之前**——自检要在 .new 上跑，而不是先
	// 替换再验证（那时目标已经被毁了）。
	newPath := target + ".new"
	if err := copyFile(downloadedPath, newPath, 0o755); err != nil {
		_ = os.Remove(newPath)
		return "", err
	}
	// rename 成功前任何提前返回都要清掉 .new，避免下次 Apply 撞上一个
	// 大小正确但内容过期的残留文件。
	keepNew := false
	defer func() {
		if !keepNew {
			_ = os.Remove(newPath)
		}
	}()

	if !aopts.SkipSelfCheck {
		u.report(Progress{Stage: StageVerify})
		if err := selfCheck(newPath, u.opts.Kind, aopts.SelfCheckArgs); err != nil {
			return "", err
		}
	}

	u.report(Progress{Stage: StageApply})

	// 防线 5：先给旧文件改名（不删）。Windows 上正在运行的 exe 不能删除但
	// 能改名；Unix 上改名不影响运行中的进程（inode 保持）。
	backup := ""
	if _, statErr := os.Stat(target); statErr == nil {
		backup = backupPath(target, aopts.BackupTag)
		// 同名备份已存在（同一秒内跑了两次 Apply）→ 加后缀，绝不覆盖：
		// 覆盖掉的那份可能正是用户唯一能回滚的版本。
		for i := 1; ; i++ {
			if _, e := os.Stat(backup); os.IsNotExist(e) {
				break
			}
			backup = fmt.Sprintf("%s.%d", backupPath(target, aopts.BackupTag), i)
		}
		if err := os.Rename(target, backup); err != nil {
			// Windows 上杀软/EDR 可能仍持有目标句柄，rename 被拒。此时**不能**
			// 把这次更新判死：新文件已经校验+自检通过，只是差一个「退出后替换」
			// 的时机。降级为待应用（.new + 标记），下次启动完成替换。
			// 返回 ErrDeferred，让调用方提示「重启后生效」而不是「更新失败」。
			if isLockedError(err) {
				// .new 已经在位（copyFile + 自检都过了），只需补一个标记。
				keepNew = true
				if merr := writePendingMarker(target); merr != nil {
					return "", fmt.Errorf("%w；且写入待应用标记失败: %v", permissionAware(err, "备份旧版本"), merr)
				}
				return "", fmt.Errorf("%w：目标被占用（%s）", ErrDeferred, ExecutableName(target))
			}
			return "", permissionAware(err, fmt.Sprintf("备份旧版本 %s", target))
		}
	}

	// 防线 4：同目录 rename（同一个文件系统 ⇒ 原子）。
	if err := os.Rename(newPath, target); err != nil {
		// 回滚：目标已经不在原位了，不还回去等于删除了应用。
		if backup != "" {
			if rbErr := os.Rename(backup, target); rbErr != nil {
				return "", fmt.Errorf("update: 替换失败且回滚失败（原文件在 %s）: %v / %v", backup, err, rbErr)
			}
			return "", permissionAware(err, fmt.Sprintf("写入 %s（已回滚）", target))
		}
		return "", permissionAware(err, fmt.Sprintf("写入 %s", target))
	}
	keepNew = true

	// 防御性收尾：确保目标可执行（copyFile 已 chmod，但某些文件系统会丢位）。
	if err := chmodExecutable(target); err != nil {
		return backup, err
	}

	// 清掉超出保留数的旧备份。失败不影响本次更新成功——只是少清理一点垃圾。
	pruneBackups(target, u.opts.KeepBackups)
	return backup, nil
}

// Replace 是 Apply 的便捷别名，供只关心「换掉即可」的调用方使用。
//
// 保留两个名字是因为任务书里两处提及（Apply / Replace）；它们行为完全相同，
// 避免调用方为了「用哪个名字」纠结。
func (u *Updater) Replace(downloadedPath, targetPath string) error {
	_, err := u.Apply(downloadedPath, targetPath, ApplyOptions{})
	return err
}

// SelfPath 返回「应该被替换的那个可执行文件」的绝对路径。
//
// # 为什么不直接用 os.Executable()
//
// os.Executable() 返回**当前进程实际运行的那个文件**，这通常正是要替换的目标。
// 但在 macOS 上 GUI 会 re-exec 进 bundle 副本（cmd/gui/bundle_darwin.go），
// 于是 os.Executable() 指向 ~/Applications/reply9842.app/Contents/MacOS/reply9842。
//
// 判断依据（一手实测 + 读 bundle_darwin.go）：
//   - 该副本是 ensureAppBundle 从裸二进制 copyIfChanged 过去的
//   - 所以「运行中的进程」就是 bundle 副本本身，替换它 = 下次启动是新版
//   - 但**下次从裸二进制启动时**，copyIfChanged 会按 mtime/size 判断
//     「源比副本新」并把旧版本重新拷过去，等于把更新回滚掉
//
// 因此本函数仍然返回 os.Executable()（替换当前运行的二进制是唯一安全的
// 语义：我们不能替用户改 bundle 之外的裸二进制，那个路径我们并不掌握）。
// 要让更新在 bundle 场景下真正生效，调用方应在 Apply+Restart 之后由
// bundle 副本自己承担「源」的角色——即重启时**直接 exec bundle 副本**，
// 而不是再走一次裸二进制的 re-exec 流程（envBundled=1 会阻止无限 re-exec，
// 但新进程是 bundle 副本的替换版，所以它本身就是最新版）。
//
// 一句话结论：os.Executable() 就是正确答案；风险不在路径选择，而在
// 「用户从裸二进制重新启动」时要重新分发一次（见 doc.go 的 macOS 段）。
func SelfPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("update: 无法确定当前可执行文件: %w", err)
	}
	// EvalSymlinks：用户可能把二进制软链到 PATH 里的某个目录。替换软链本身
	// 没有意义（真实文件没变），必须解析到真实文件。
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	abs, err := filepath.Abs(exe)
	if err != nil {
		return "", fmt.Errorf("update: 解析可执行文件绝对路径: %w", err)
	}
	return abs, nil
}

// resolveTarget 把调用方给的路径规范成绝对路径并解析符号链接。
//
// 目标可以是符号链接（/usr/local/bin/reply9842 → /opt/...）：rename 覆盖
// 软链会创建一个普通文件而留下真实文件没动，行为极难理解。解析后替换真实文件。
func resolveTarget(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("update: 解析目标路径: %w", err)
	}
	// 目标可能还不存在（首次安装）；EvalSymlinks 会失败，那就用 abs。
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real, nil
	}
	return abs, nil
}

// ensureWritableDir 检查目录可写。
//
// 用「创建再删除一个探针文件」而不是检查权限位：ACL、SIP、只读挂载、
// Windows 上的继承权限都会让权限位说谎。实测比 stat 可靠。
func ensureWritableDir(dir string) error {
	if st, err := os.Stat(dir); err != nil {
		return fmt.Errorf("update: 目标目录不可访问 %s: %w", dir, err)
	} else if !st.IsDir() {
		return fmt.Errorf("update: 目标父路径不是目录: %s", dir)
	}
	probe, err := os.CreateTemp(dir, ".reply9842-writecheck-*")
	if err != nil {
		return fmt.Errorf("%w：目录 %s 不可写（系统级安装常需管理员权限，请手工处理）: %v",
			ErrPermission, dir, err)
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return nil
}

// selfCheck 跑 `newPath <args>` 并确认它是个能正常启动的本产品二进制。
//
// 判定三条（任一不满足即放弃）：
//
//  1. 能启动（exec 不报错）——拦住架构不符、动态库缺失、无执行位
//  2. 退出码 0——拦住「启动即崩」（半截 ELF 的 SEGV 会在这里暴露）
//  3. 输出里含 expectedWord（默认 "reply9842"）——拦住「下载到了别的产品」
//
// 为什么必须有第 3 条：只验退出码的话，一个被替换成 /bin/true 的产物也能过。
// 而真正的失败模式是「镜像返回了另一个可执行文件」，它退出码也是 0。
func selfCheck(newPath string, kind Kind, args []string) error {
	if len(args) == 0 {
		args = selfCheckArgs(kind)
	}
	// 超时兜底：坏产物可能启动后挂住（等 stdin、重试网络）。10s 足够任何
	// 正常 `--version` 输出完，又不至于让 GUI 的「立即更新」卡太久。
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, newPath, args...)
	// 显式给一个空环境而不是继承：`--version` 不该因调用方的 GODEBUG /
	// 代理变量而挂住；同时清掉可能让新进程尝试连网的变量。
	cmd.Env = []string{"HOME=" + os.Getenv("HOME"), "PATH=" + os.Getenv("PATH")}
	// 合并 stderr：有些构建把版本打到 stderr（log.Printf）。我们只关心
	// 「有没有输出版本号」，两个流都算。
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("%w：%s %s 超时未退出", ErrSelfCheck, filepath.Base(newPath), strings.Join(args, " "))
	}
	if err != nil {
		return fmt.Errorf("%w：%s %s 执行失败（%v）；输出：%s",
			ErrSelfCheck, filepath.Base(newPath), strings.Join(args, " "), err, clip(string(out), 400))
	}
	// 只要输出里有本产品的名字就算过了。不比对具体版本号：新版本的输出格式
	// 可能微调（relay-gui → reply9842-gui 这类改名），那不该阻塞更新。
	if !strings.Contains(string(out), "reply9842") {
		return fmt.Errorf("%w：%s 的输出里没有产品标识；输出：%s",
			ErrSelfCheck, filepath.Base(newPath), clip(string(out), 400))
	}
	return nil
}

// selfCheckArgs 返回各形态的自检参数。
//
// 依据各 cmd/*/main.go 的实际实现（2026-11-13 核对）：
//
//	cli    支持子命令 `version`（不带 -），也支持 flag 解析 → 用 ["version"]
//	gui    flag.Bool("version") → 用 ["--version"]
//	server 只有 flag.Parse，**没有版本输出** → 用 ["--help"]：
//	       flag 包对 -h/--help 返回 ErrHelp，进程 exit 0 并打印用法到 stderr，
//	       用法里含 "reply9842-server"，能同时满足「退出码 0」与「含产品标识」。
//
// server 之所以不强行要求版本输出：给它加 --version 是 cmd/ 的改动，本包
// 无权做（另一个子代理正在改 cmd/gui，且 cmd/ 属于交付面的另一块）。
// --help 的自检强度足够——它证明的正是「这个二进制能在本机跑起来」。
func selfCheckArgs(kind Kind) []string {
	switch kind {
	case KindGUI:
		return []string{"--version"}
	case KindServer:
		return []string{"--help"}
	default:
		return []string{"version"}
	}
}

// backupPath 生成旧二进制备份路径：<target>.old-<tag>。
//
// tag 的优先级：调用方给的 BackupTag > 目标文件 mtime（可读的时间戳）。
// 用 mtime 而不是「运行 --version 读旧版本号」：旧二进制可能已经不可执行
// （那正是我们要更新的原因），为了取个名字而冒跑坏文件的风险不值得。
func backupPath(target, tag string) string {
	if tag == "" {
		if st, err := os.Stat(target); err == nil {
			tag = st.ModTime().Format("20060102-150405")
		} else {
			tag = "unknown"
		}
	}
	// 文件名里不能有路径分隔符或冒号（Windows）：版本号通常安全，但仍防御。
	tag = sanitizeTag(tag)
	return target + ".old-" + tag
}

func sanitizeTag(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.' || r == '-' || r == '_' || r == '+':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "unknown"
	}
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

// pruneBackups 只保留最近 keep 个 <target>.old-* 备份。
//
// keep < 0 表示不清理（调用方明确要全部保留）。keep == 0 是「不保留」，
// 但 New 已把 0 换成默认 2，所以这里只会看到负数或正数。
//
// 排序按 mtime 而不是文件名：文件名里有版本号与时间戳两种格式，字典序
// 会把 v0.10.0 排在 v0.9.0 前面（这正是任务书里点名的那类错误）。
func pruneBackups(target string, keep int) {
	if keep < 0 {
		return
	}
	dir := filepath.Dir(target)
	prefix := filepath.Base(target) + ".old-"
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type cand struct {
		path string
		mod  time.Time
	}
	var cands []cand
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		cands = append(cands, cand{filepath.Join(dir, e.Name()), info.ModTime()})
	}
	if len(cands) <= keep {
		return
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].mod.After(cands[j].mod) })
	for _, c := range cands[keep:] {
		_ = os.Remove(c.path)
	}
}

// copyFile 把 src 复制到 dst（同目录，保证后续 rename 在同一文件系统）。
//
// 为什么不用 io.Copy + Create 而要先 CreateTemp：目标是「可预测的名字」
// （.new），中途崩溃会留下一个半截的 .new；下次 Apply 会覆盖它，但若用户
// 手工看到 .new 会困惑。用固定名是为了让失败后的人工排查有迹可循
// （「有 .new 说明上次替换没完成」）。写入本身用 O_TRUNC 保证不留旧字节。
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("update: 打开下载文件: %w", err)
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return permissionAware(err, "创建 "+dst)
	}
	if _, err := out.ReadFrom(in); err != nil {
		out.Close()
		return fmt.Errorf("update: 复制到 %s: %w", dst, err)
	}
	// chmod 显式调用：Windows 与某些 umask 下 O_CREATE 的模式会被改写。
	if err := out.Chmod(mode); err != nil && runtime.GOOS != "windows" {
		out.Close()
		return fmt.Errorf("update: chmod %s: %w", dst, err)
	}
	// fsync：rename 之前必须确保内容落盘，否则断电后目标可能是个空文件。
	if err := out.Sync(); err != nil {
		out.Close()
		return fmt.Errorf("update: fsync %s: %w", dst, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("update: 关闭 %s: %w", dst, err)
	}
	return nil
}

// permissionAware 把 EACCES/EPERM 翻译成 ErrPermission，便于调用方统一提示。
func permissionAware(err error, what string) error {
	if errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("%w：%s（系统级路径需管理员权限，请勿由本程序提权）", ErrPermission, what)
	}
	return fmt.Errorf("update: %s: %w", what, err)
}

// clip 截断过长输出，避免把整个 --help 塞进错误信息。
func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
