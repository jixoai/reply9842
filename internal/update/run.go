// 高层编排：一次「检查 → 下载 → 校验 → 替换 → 重启」的完整更新。
//
// 存在的理由：GUI / CLI 都要这套顺序，而顺序本身有硬约束（见每一步的注释）。
// 把它留在调用方手里，等于让每个入口各自复制一遍顺序，迟早有一处漏掉
// 「先自检再替换」。
//
// 但它是**可选**的：调用方仍可只用 Check / Download / Apply / Restart 四步
// 自己拼，例如 GUI 想在下载完成后先弹「是否立即重启」再替换。
package update

import (
	"context"
	"fmt"
	"os"
	"time"
)

// UpdateOptions 是 Run 的配置。
type UpdateOptions struct {
	// Target 是要替换的可执行文件路径。空串 = SelfPath()。
	Target string
	// Args 是重启时传给新实例的参数。nil = RestartArgsFrom()（重放当前 argv）。
	Args []string
	// Relaunch 为假时只替换不重启（「下次启动生效」）。
	//
	// 这是 Windows 与「用户正在用」场景的安全出口：替换后立刻重启会把用户
	// 正在看的窗口关掉。GUI 通常在更新前就告知用户「将重启」，所以默认
	// 由调用方显式决定，本包不猜。
	Relaunch bool
	// Restart 是 Restart 的细粒度选项（Detach / BeforeSpawn 等）。
	Restart RestartOptions
	// Apply 是 Apply 的细粒度选项（BackupTag 等）。
	Apply ApplyOptions
	// SkipSelfCheck 透传给 Apply（仅测试）。
	SkipSelfCheck bool

	// WaitingOldVersion 是给备份命名的旧版本号（可读性用，可空）。
	WaitingOldVersion string
}

// Result 描述一次成功更新的结果。
type Result struct {
	// DownloadedPath 是缓存里的新产物（保留，便于「回滚到该版本」或重试）。
	DownloadedPath string
	// Target 是实际被替换的文件。
	Target string
	// Backup 是旧版本备份路径（空表示原先没有目标文件，属首次安装形态）。
	Backup string
	// Restarted 表示已经 spawn 新进程；调用方据此决定是否退出。
	Restarted bool
	// Info 是本次检查的元数据。
	Info Info
}

// Run 执行一次完整更新。
//
// 前置条件：info.HasUpdate 必须为真，或者 Info.Latest 至少可用。本函数不
// 重新检查——那是调用方的事（它已经拿到了 Info，可能是几分钟前缓存的）。
//
// 失败语义：任一步失败即返回，**目标文件保持原样**。下载失败不影响现有
// 二进制；自检失败只留下一个 .new（Apply 内部清理）；替换后的失败（chmod/
// 重启）已经完成更新，返回的 error 会带上「已替换」的事实，调用方可选择
// 提示「已更新，请手工重启」。
func (u *Updater) Run(ctx context.Context, info Info, uopts UpdateOptions) (Result, error) {
	res := Result{Info: info}
	if info.Latest == "" {
		return res, fmt.Errorf("update: Info.Latest 为空，需先调用 Check")
	}

	downloaded, err := u.Download(ctx, info, u.opts.Progress)
	if err != nil {
		return res, err
	}
	res.DownloadedPath = downloaded

	target := uopts.Target
	if target == "" {
		target, err = SelfPath()
		if err != nil {
			return res, err
		}
	}
	res.Target = target

	aopts := uopts.Apply
	if aopts.BackupTag == "" {
		aopts.BackupTag = uopts.WaitingOldVersion
	}
	if aopts.BackupTag == "" {
		aopts.BackupTag = info.Current
	}
	aopts.SkipSelfCheck = aopts.SkipSelfCheck || uopts.SkipSelfCheck

	backup, err := u.Apply(downloaded, target, aopts)
	res.Backup = backup
	if err != nil {
		return res, err
	}

	if !uopts.Relaunch {
		return res, nil
	}

	args := uopts.Args
	if args == nil {
		args = RestartArgsFrom()
	}
	u.report(Progress{Stage: StageRestart})
	if err := Restart(target, args, uopts.Restart); err != nil {
		// 替换已经成功，重启失败不该被报成「更新失败」——那是两件事。
		// 带上 res 让调用方能显示「已更新到 x，请手工重启」。
		return res, fmt.Errorf("update: 已替换但重启失败，请手工启动: %w", err)
	}
	res.Restarted = true
	return res, nil
}

// Rollback 把 backup 换回 target。
//
// 存在的理由：更新后新版本有问题（上游接口变更、平台兼容）时，用户需要一个
// 「退回上一个版本」的动作，而不是手动在 Finder 里改文件名。
//
// 刻意**不**校验 backup 的可执行性：回滚往往是在新版本起不来时进行的，
// 而旧版本我们刚刚还在运行它——它的可信度高于任何自动检查。失败就让
// 用户看到真实错误（文件不在了 / 无权限）。
func Rollback(target, backup string) error {
	if backup == "" {
		return fmt.Errorf("update: 没有可回滚的备份")
	}
	if _, err := os.Stat(backup); err != nil {
		return fmt.Errorf("update: 备份不存在 %s: %w", backup, err)
	}
	target, err := resolveTarget(target)
	if err != nil {
		return err
	}
	if err := ensureWritableDir(filepathDirOf(target)); err != nil {
		return err
	}
	// 目标先挪到 .rollback-<时间>，而不是直接覆盖：万一 rename 失败两次，
	// 至少当前文件还在。用时间戳避免与已有备份重名。
	aside := target + ".rollback-" + time.Now().Format("20060102-150405")
	if _, err := os.Stat(target); err == nil {
		if err := os.Rename(target, aside); err != nil {
			return permissionAware(err, "暂存当前版本")
		}
	}
	if err := os.Rename(backup, target); err != nil {
		// 把当前版本还回去。
		_ = os.Rename(aside, target)
		return permissionAware(err, "回滚到 "+ExecutableName(backup))
	}
	_ = chmodExecutable(target)
	return nil
}

// filepathDirOf 是 filepath.Dir 的薄包装，避免 rollback 里再 import filepath
// （这个文件的其余部分只用 os）。
func filepathDirOf(p string) string {
	for i := len(p) - 1; i > 0; i-- {
		if p[i] == '/' || p[i] == '\\' {
			return p[:i]
		}
	}
	return "."
}
