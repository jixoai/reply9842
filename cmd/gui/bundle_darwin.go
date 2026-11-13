//go:build darwin

// macOS 自建 app bundle：让裸 Mach-O 拥有 Dock 身份。
//
// 问题：直接跑 `./relay-gui` 的进程没有 bundle，LaunchServices 无从得知它是
// 什么应用——Dock 显示通用黑色 exec 方块，进程名是二进制文件名，Cmd-Tab 里
// 也是一块空白。图标画得再好也显示不出来，因为显示图标的是 Dock，而 Dock 读的
// 是 bundle 的 Info.plist + Contents/Resources/AppIcon.icns。
//
// 解法（与 mygo 的 macos-bundle 参考实现一致）：首次 GUI 启动时在
// ~/Applications/reply9842.app 现场拼一个最小 bundle，把图标、Info.plist
// 和当前二进制的副本写进去，然后 re-exec 副本。产物仍是「一个可执行文件」——
// bundle 是运行时在用户磁盘上生成的，不参与分发，不需要签名或公证。
package main

import (
	_ "embed"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// appIcon 是打包时 go:embed 进来的 icns；不由磁盘路径读取，因为分发产物
// 只有一个二进制，资源必须内置。
//
//go:embed assets/AppIcon.icns
var appIcon []byte

const (
	bundleID = "dev.jixoai.reply9842"
	// execName 是 bundle 内的可执行文件名（= CFBundleExecutable）。
	// 用固定名字而不是 os.Args[0]，避免用户把二进制改成任意名字时 bundle 跟着变形。
	execName = "reply9842"
	// envBundled 标记「本进程已经是 bundle 副本」，防止无限 re-exec。
	envBundled = "RFC9842_BUNDLED"
)

// relaunchIntoBundleIfNeeded 在需要时启动 bundle 副本并返回 true，调用方应立即退出。
//
// 为什么不是 `open -a`：open 走 LaunchServices，会丢掉环境变量与工作目录
// （HOME=$TMP 之类的测试隔离会失效），父进程也拿不到子进程的控制权。
// 直接 exec bundle 里的副本可以原样保留 env、cwd 和 stdio。
func relaunchIntoBundleIfNeeded(version string) bool {
	if os.Getenv(envBundled) == "1" {
		return false
	}
	target, needed, err := ensureAppBundle(version)
	if err != nil {
		// 失败不阻塞启动：没有 bundle 只是图标和名字变成通用样式，
		// 功能一切正常。身份是装饰性的，不该成为启动依赖。
		log.Printf("app bundle: %v", err)
		return false
	}
	if !needed {
		return false
	}
	cmd := exec.Command(target, os.Args[1:]...)
	cmd.Env = append(os.Environ(), envBundled+"=1")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Start(); err != nil {
		log.Printf("app bundle: relaunch: %v", err)
		return false
	}
	return true
}

// ensureAppBundle 写入/刷新 bundle，返回其可执行文件路径。
// needed=false 表示当前进程已经在 .app 内运行，不需要再次 relaunch。
func ensureAppBundle(version string) (target string, needed bool, err error) {
	exe, err := os.Executable()
	if err != nil {
		return "", false, err
	}
	if real, evalErr := filepath.EvalSymlinks(exe); evalErr == nil {
		exe = real
	}
	// 已经跑在某个 .app 里（不管是本程序建的还是用户自己打包的）就别再套一层。
	if strings.Contains(exe, ".app/Contents/MacOS/") {
		refreshOwnPlist(exe, version)
		return exe, false, nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", false, fmt.Errorf("无法确定用户主目录: %w", err)
	}

	bundle := filepath.Join(home, "Applications", execName+".app")
	contents := filepath.Join(bundle, "Contents")
	macosDir := filepath.Join(contents, "MacOS")
	resDir := filepath.Join(contents, "Resources")
	if err := os.MkdirAll(macosDir, 0o755); err != nil {
		return "", false, err
	}
	if err := os.MkdirAll(resDir, 0o755); err != nil {
		return "", false, err
	}

	target = filepath.Join(macosDir, execName)
	if err := copyIfChanged(exe, target); err != nil {
		return "", false, err
	}
	if err := writeIfChanged(filepath.Join(resDir, "AppIcon.icns"), appIcon, 0o644); err != nil {
		return "", false, err
	}
	plist := []byte(infoPlist(shortVersion(version)))
	if err := writeIfChanged(filepath.Join(contents, "Info.plist"), plist, 0o644); err != nil {
		return "", false, err
	}
	return target, true, nil
}

// refreshOwnPlist 在「已经跑在 bundle 里」时刷新 Info.plist 的版本号。
//
// 为什么需要：自动更新替换的是 bundle 内的可执行文件，而 Info.plist 里的
// CFBundleShortVersionString 是构建时写死的。不刷新的后果是 Finder 的「简介」、
// 系统报告、以及 Dock 的关于窗口长期显示更新前的版本号——用户会以为更新没生效
// （而进程的 --version 明明是新的）。
//
// 只在**本程序自己的 bundle 布局**上动手（可执行文件名匹配 execName），
// 不碰用户自己打包的 .app：那些的 Info.plist 可能有签名或额外字段。
//
// 失败静默：版本号显示是装饰性的，不该影响启动。
func refreshOwnPlist(exe, version string) {
	if filepath.Base(exe) != execName {
		return
	}
	contents := filepath.Dir(filepath.Dir(exe))
	if filepath.Base(filepath.Dir(exe)) != "MacOS" {
		return
	}
	_ = writeIfChanged(filepath.Join(contents, "Info.plist"),
		[]byte(infoPlist(shortVersion(version))), 0o644)
}

// copyIfChanged 只在新源更新或大小不同时替换 bundle 里的副本。
//
// 为什么不能每次都覆盖：正在运行的进程映像被替换会导致后续页错误崩溃；
// 而且每次启动重写几个 MB 纯属浪费。
//
// # 更新器写过的副本优先（防回滚）
//
// 自动更新替换的就是 bundle 副本本身（见 update.SelfPath）。此时若用户下次
// 双击的是**下载目录里那个旧版裸二进制**，朴素比较会得出「源比副本更新」
// （其实只是大小不同 / mtime 更晚）→ 把新副本覆盖回旧版 → 一次更新白做。
//
// 所以更新器在 Contents/ 写一个 .reply9842-updated 标记（cmd/gui/update.go 的
// markBundleUpdated）。有标记时：
//
//	标记比源文件更新 → 跳过覆盖（副本是更新的那一份）
//	源文件比标记更新 → 照常覆盖（用户编译/下载了更新的版本，尊重它）
//
// 用 mtime 比较而不是「有标记就永不覆盖」：否则开发者的 `go build` 再也
// 送不进 bundle，本地迭代会被一个陈旧的标记卡住。
func copyIfChanged(src, dst string) error {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}
	if dstInfo, statErr := os.Stat(dst); statErr == nil {
		stampMod := time.Time{}
		if stamp, serr := os.Stat(filepath.Join(filepath.Dir(filepath.Dir(dst)), bundleUpdateStamp)); serr == nil {
			stampMod = stamp.ModTime()
		}
		if updatedBundleBeats(srcInfo.ModTime(), stampMod) ||
			(stampMod.IsZero() && dstInfo.Size() == srcInfo.Size() && !srcInfo.ModTime().After(dstInfo.ModTime())) {
			return nil
		}
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".rfc9842-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// 给副本打上新 mtime：下一次比较依赖它判断「源是否更新」。
	now := time.Now()
	_ = os.Chtimes(tmp.Name(), now, now)
	return os.Rename(tmp.Name(), dst)
}

// writeIfChanged 只在字节变化时写入，保证重复启动完全不动磁盘。
// temp + chmod + rename 是为了不留下写了一半的文件。
func writeIfChanged(path string, data []byte, mode os.FileMode) error {
	if old, err := os.ReadFile(path); err == nil && string(old) == string(data) {
		return nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".rfc9842-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// shortVersion 把构建版本压成 Apple 接受的纯数字形式。
//
// Apple 只认数字与点（v0.5.0-dirty 会被拒绝）。版本注入可能来自
// `git describe`，形如 v0.5.0-3-gabcdef，故只取开头连续的数字与点；
// 拿不到任何数字时退回 0.0.0（launchd/LaunchServices 不接受空值）。
func shortVersion(v string) string {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	var b strings.Builder
	for _, r := range v {
		if (r >= '0' && r <= '9') || r == '.' {
			b.WriteRune(r)
			continue
		}
		break
	}
	if out := strings.Trim(b.String(), "."); out != "" {
		return out
	}
	return "0.0.0"
}

// infoPlist 生成最小可用的 Info.plist。
//
// 各字段的用途：
//   - CFBundleIdentifier         LaunchServices 的唯一身份；Dock 靠它把图标和
//     偏好设置关联起来，重复的 id 会让两个 app 互相抢图标
//   - CFBundleIconFile           不含扩展名的图标文件名，对应 Resources/AppIcon.icns
//   - CFBundlePackageType APPL   告诉系统这是应用而不是框架/插件
//   - LSMinimumSystemVersion     12.0（Monterey）：mygo 的窗口后端最低要求
//   - NSHighResolutionCapable    不开这个，Retina 上整个界面会被放大成 2 倍模糊
func infoPlist(version string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleIdentifier</key><string>` + bundleID + `</string>
  <key>CFBundleName</key><string>reply9842</string>
  <key>CFBundleDisplayName</key><string>reply9842</string>
  <key>CFBundleExecutable</key><string>` + execName + `</string>
  <key>CFBundleIconFile</key><string>AppIcon</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleInfoDictionaryVersion</key><string>6.0</string>
  <key>CFBundleShortVersionString</key><string>` + version + `</string>
  <key>CFBundleVersion</key><string>` + version + `</string>
  <key>LSMinimumSystemVersion</key><string>12.0</string>
  <key>NSHighResolutionCapable</key><true/>
</dict>
</plist>
`
}
