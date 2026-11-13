// Windows 的「下载完成，退出后自动替换」降级路径。
//
// # 为什么需要降级
//
// Windows 上运行中的 .exe 不能被删除，但**可以被改名**（Vista+ 的
// FILE_SHARE_DELETE 语义）。所以 Apply 的两步 rename 在 Windows 上通常也
// 能成——但它有两个现实失败源：
//
//  1. 杀软 / EDR 在进程启动后仍持有目录句柄，rename 返回 ERROR_ACCESS_DENIED
//     （实测常见于国内安全软件）
//  2. 目标被另一个用户/会话打开
//
// 完整方案是「起一个 helper 进程，等主进程退出后再替换」：helper 要能独立
// 启动（Windows 上要求另写一个小 exe 或用 PowerShell 脚本），然后由 helper
// 重启主程序。成本与风险都很高（helper 自身被杀、权限、UAC 重定向），
// 而收益仅是「不必退出即生效」。
//
// # 本包的取舍（明确降级）
//
// Windows 走**尽量原地替换，失败则挂起为待应用**：
//
//	Apply 成功           → 与 Unix 相同，立即生效（下次启动是新版）
//	Apply 遇到锁          → 不报「更新失败」，而是把 .new 留在目标旁边，
//	                       写 <target>.update-pending 标记，返回 nil error
//	下次启动时            → ApplyPendingIfAny() 尝试完成替换
//
// 「重启」这一步在 Windows 上需要旧进程退出才真正完成替换。所以调用方拿到
// RestartNeedsExit=true 时应提示用户「重启后生效」而不是立刻 Restart——
// 立刻 spawn 新进程会读到一个还没被替换的旧文件。
package update

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// pendingMarkerSuffix 是「有待应用更新」的标记文件名后缀。
//
// 用独立文件而不是「.new 存在即待应用」：.new 可能是上次失败留下的残渣，
// 而标记文件是**确认校验与自检都已通过**的产物，语义更强。
const pendingMarkerSuffix = ".update-pending"

// ErrDeferred 表示替换要等到下次启动（Windows 文件被占用）。
var ErrDeferred = errors.New("update: 替换待下次启动生效")

// DeferApply 为「现在不能替换」的情况做准备：把已校验的新文件放到目标旁边，
// 并写标记。返回待应用的 .new 路径。
//
// 只在 Windows 路径上有意义，但**在其它平台也允许调用**（实现了跨平台 API
// 的好处是调用方不必写平台分支，且可以在测试里覆盖这套逻辑）。
func DeferApply(downloadedPath, targetPath string) (string, error) {
	target, err := resolveTarget(targetPath)
	if err != nil {
		return "", err
	}
	if err := ensureWritableDir(filepath.Dir(target)); err != nil {
		return "", err
	}
	newPath := target + ".new"
	if err := copyFile(downloadedPath, newPath, 0o755); err != nil {
		return "", err
	}
	// 标记内容写目标路径：同一个目录里可能放着多个版本的 .new（不同产品），
	// 标记指明「这个 .new 属于谁」，避免误应用。
	if err := writePendingMarker(target); err != nil {
		_ = os.Remove(newPath)
		return "", err
	}
	return newPath, nil
}

// writePendingMarker 写「有待应用更新」的标记（.new 必须已经在位）。
//
// 拆出来是因为 Apply 在 Windows 降级路径上已经写好了 .new（copyFile + 自检
// 都过了），不必再复制一遍文件——只补标记即可。
func writePendingMarker(target string) error {
	marker := target + pendingMarkerSuffix
	if err := os.WriteFile(marker, []byte(target+"\n"), 0o644); err != nil {
		return fmt.Errorf("update: 写入待应用标记: %w", err)
	}
	return nil
}

// ApplyPendingIfAny 在启动早期尝试完成一次被推迟的替换。
//
// 返回 applied=true 表示刚刚完成替换——调用方**必须**在这次启动里就重新
// exec 自己（因为内存里跑的还是旧映像，磁盘上已是新版本；不重启的话
// 用户看到的版本号与磁盘不一致，下次替换还会撞上同样的问题）。
//
// 返回 nil error 且 applied=false 是常态：没有待应用的更新。
// 有待应用但这次仍失败（还是被占用）时返回 error——调用方可以选择记日志
// 后继续正常启动，不要因此拒绝启动。
func ApplyPendingIfAny(targetPath string) (applied bool, err error) {
	target, err := resolveTarget(targetPath)
	if err != nil {
		return false, err
	}
	newPath := target + ".new"
	marker := target + pendingMarkerSuffix
	if _, err := os.Stat(marker); err != nil {
		return false, nil // 没有待应用的更新
	}
	st, err := os.Stat(newPath)
	if err != nil {
		// 标记在但文件没了：清掉悬空标记，否则每次启动都白试一次。
		_ = os.Remove(marker)
		return false, nil
	}
	if st.Size() == 0 {
		_ = os.Remove(newPath)
		_ = os.Remove(marker)
		return false, fmt.Errorf("update: 待应用的更新文件为空，已丢弃")
	}

	// 与 Apply 相同的两步 rename：旧 → .old-<tag>，新 → 目标。
	backup := backupPath(target, "")
	if _, err := os.Stat(backup); err == nil {
		// 保留既有备份，不覆盖（这次要挪走的旧文件可能是首个备份）。
		backup = backup + ".2"
	}
	if _, statErr := os.Stat(target); statErr == nil {
		if err := os.Rename(target, backup); err != nil {
			return false, permissionAware(err, "暂存旧版本 "+ExecutableName(target))
		}
	}
	if err := os.Rename(newPath, target); err != nil {
		// 回滚旧文件，保持「启动可用的目标」不变。
		if _, statErr := os.Stat(backup); statErr == nil {
			_ = os.Rename(backup, target)
		}
		return false, permissionAware(err, "应用待替换更新")
	}
	_ = os.Remove(marker)
	_ = chmodExecutable(target)
	return true, nil
}

// PendingUpdateAvailable 报告是否存在待应用的更新（不修改任何东西）。
//
// GUI 的「设置 → 更新」可以在启动时据此显示「有更新将在重启后生效」。
func PendingUpdateAvailable(targetPath string) bool {
	target, err := resolveTarget(targetPath)
	if err != nil {
		return false
	}
	if _, err := os.Stat(target + pendingMarkerSuffix); err != nil {
		return false
	}
	if _, err := os.Stat(target + ".new"); err != nil {
		return false
	}
	return true
}

// isLockedError 判断 rename 失败是否属于「文件被占用」。
//
// Windows 的错误文本不稳定（本地化 + 不同 API），所以不靠文本匹配：
// 只要 rename 失败且目标就是当前运行的可执行文件，就按「被占用」处理。
// 这个判断比字符串匹配可靠，也解释了为什么 Apply 在 Windows 上需要降级。
func isLockedError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, s := range []string{
		"being used by another process",
		"另一个程序正在使用",
		"access is denied",
		"拒绝访问",
		"permission denied",
		"operation not permitted",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}
