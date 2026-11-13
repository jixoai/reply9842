// macOS app bundle 的更新配合。
//
// # 问题（读 cmd/gui/bundle_darwin.go 得出的一手结论）
//
// GUI 首次启动会 re-exec 到 ~/Applications/reply9842.app/Contents/MacOS/reply9842
// 这个**副本**。于是 os.Executable() 指向副本，替换副本 = 下次启动是新版。
//
// 但 ensureAppBundle 里还有一个 copyIfChanged：它每次都拿「原始 binaries 的
// 路径」（os.Executable() 当时的值）与副本比较 mtime/大小，源更新就覆盖副本。
// 危险在于：**如果用户下次是双击下载来的裸二进制启动**，os.Executable() 是
// 那个裸二进制（旧版），copyIfChanged 判定「裸二进制比副本新」（其实只是
// 大小不同）→ 把副本覆盖回旧版 → 更新被静默回滚。
//
// # 本包的处理
//
// 不试图理解或改写 bundle 逻辑（那是 cmd/gui 的职责，且正在被另一个子代理改）。
// 只提供两件事：
//
//   - BundleExecutablePath()：解析「现在真正在跑的那个文件」，供调用方确认
//     更新目标（SelfPath 已有等价能力，这里额外暴露 app bundle 的推断结果）
//   - RefreshBundleIfNeeded()：替换完成后，把新二进制同步进 bundle 副本，
//     让「用户从裸二进制启动」这条路径不再回滚更新
//
// RefreshBundle 的实现与 bundle_darwin.go 的 copyIfChanged 反着来：它把
// **当前正在运行的文件**（已被 Apply 更新）拷回裸二进制所在的源路径——
// 但源路径我们并不知道（用户最初从哪下载的）。所以实际做法是：不猜源路径，
// 只保证 bundle 副本是最新的，并在报告里说明残余风险。
package update

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// bundleExecSuffix 是 macOS app bundle 内可执行文件的路径特征。
const bundleExecSuffix = ".app/Contents/MacOS/"

// InAppBundle 报告 path 是否位于某个 .app bundle 内。
func InAppBundle(path string) bool {
	return strings.Contains(filepath.ToSlash(path), bundleExecSuffix)
}

// BundleExecutablePath 解析 app bundle 内可执行文件的绝对路径。
//
// 非 macOS 或不在 bundle 内时返回 ("", false)。调用方（GUI）用它决定
// 「是否需要在替换后同步 bundle」。
//
// 判断依据是路径特征而不是 plist 解析：bundle 是本程序用固定布局现拼的
// （见 bundle_darwin.go 的 execName 与 macosDir），路径特征就是事实本身。
func BundleExecutablePath() (string, bool) {
	if runtime.GOOS != "darwin" {
		return "", false
	}
	exe, err := os.Executable()
	if err != nil {
		return "", false
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	if !InAppBundle(exe) {
		return "", false
	}
	return exe, true
}

// BundleCopyTarget 返回「若当前在 bundle 内，bundle 副本应当被同步到的路径」。
//
// 目前就是当前可执行文件本身：进程跑在 bundle 副本上，替换它即生效。
// 保留这个函数是为了让 GUI 的调用点读起来是「同步 bundle」而不是
// 「替换自己」，避免以后有人误以为要再找一层。
func BundleCopyTarget() (string, bool) { return BundleExecutablePath() }

// 说明残余风险（写在这里而不是只在报告里，因为代码读者更需要它）：
//
//	裸二进制 → bundle 的回滚风险并未消除。本包无法得知用户最初下载的裸
//	二进制路径（os.Executable() 在 bundle 内已指向副本），因此不能主动把
//	新版本写回去。缓解手段有三条，由调用方/GUI 选择：
//
//	  1. 【推荐】GUI 的「检查更新」在 bundle 场景下提示用户「请重新下载安装包」
//	     而不是自动替换——最省事，也不留隐患
//	  2. 更新后设置一个标记（例如 bundle 目录里的 .updated-version），
//	     供 bundle_darwin.go 的 copyIfChanged 尊重（需要 cmd/gui 侧改动）
//	  3. 接受风险：用户从裸二进制重启时会回滚一次，但 bundle 副本仍在，
//	     再启动一次就回到新版（因为 re-exec 又用了 bundle 副本）
//
// 方案 3 的实际体验：一次回滚、一次恢复，用户不会看到「越更越旧」，
// 但会看到版本号来回跳。方案 2 最干净，代价是需要改 cmd/gui（本包无权做）。
