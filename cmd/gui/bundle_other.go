//go:build !darwin

// 非 macOS 平台的空实现。
//
// 存在的意义只是让 main 里那句 relaunchIntoBundleIfNeeded 无条件成立：
// Windows 的快捷方式图标由 PE 资源（rsrc_windows_amd64.syso）提供，Linux
// 没有统一的 bundle 概念，两者都不需要运行时包装。签名与 darwin 版保持一致。
package main

// relaunchIntoBundleIfNeeded 在非 darwin 平台永远返回 false（不 relaunch）。
func relaunchIntoBundleIfNeeded(string) bool { return false }

// shortVersion 在非 darwin 平台不会被调用；导出同签名是为了让潜在的共享
// 代码（如 --version 输出）不因平台而异。
func shortVersion(v string) string { return v }
