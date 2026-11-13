// Package autostart — 开机自启（登录项）的读写
//
// 正交意图（2 个，2026-11-13 kzf 需求：「托盘才是本体，开机自启」）：
//  1. 一个与平台无关的三函数接口：Open / Set / Available
//  2. 开关的真相是**操作系统状态**，不是我们自己的配置文件
//
// 为什么不能存进自己的 settings：用户可能在「系统设置 → 登录项」或
// Windows 的任务管理器里关掉它。若我们缓存一个 bool，三处就会互相说谎——
// 界面显示开着、系统里其实关了。所以每次 Open 都去读 OS 的真实状态。
//
// 各平台机制（详见各平台文件）：
//
//	darwin   ~/Library/LaunchAgents/<ID>.plist（自建，mygo 的 SMAppService 需要 bundle）
//	windows  HKCU\...\Run\<appID>（交给 mygo，它已处理 StartupApproved 等细节）
//	其它     errUnsupported（让 go build ./... 在 Linux 上也能过）
package autostart

import "errors"

const (
	// ID 是这个应用在系统里的登录项标识：macOS 的 LaunchAgent label，
	// Windows 注册表值名（mygo 用 App.Name()，与这里保持同一个字符串）。
	ID = "dev.jixoai.reply9842"

	// Marker 是「本次启动来自登录项」的命令行参数。
	//
	// 带它的启动不弹窗口、只驻留托盘——用户登录时不该被一个窗口糊脸。
	// Windows 侧 mygo 自己会传 --mygo-opened-at-login 并在 main 之前剥掉，
	// 用 mygo.App.WasOpenedAtLogin() 查询；这里是为 macOS 的 LaunchAgent
	// 准备的自有标记，main 里两者都认。
	Marker = "--autostart"
)

// ErrUnsupported 表示该平台没有实现开机自启（当前是 Linux）。
var ErrUnsupported = errors.New("autostart: 当前平台不支持开机自启")

// Available 报告当前平台是否实现了开机自启。
//
// 界面用它决定「设置」里那一行是显示开关还是显示「当前平台不支持」。
func Available() bool { return supported }

// Open 读取系统的真实状态。
//
// 返回的 error 只在「本该能读但读失败」时非 nil；「文件不存在」「路径过期」
// 「内容被手工改坏」一律读成 false（见 darwin.go 的读回校验）。
func Open() (bool, error) {
	if !supported {
		return false, ErrUnsupported
	}
	return open()
}

// Set 打开或关闭开机自启。
//
// 生效时机是**下次登录**，当前运行的实例不受影响：我们既不去 load 也不去
// unload LaunchAgent（load 会让 launchd 立刻再起一个进程）。
func Set(on bool) error {
	if !supported {
		return ErrUnsupported
	}
	return set(on)
}
