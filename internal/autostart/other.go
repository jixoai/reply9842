//go:build !darwin && !windows

// 其它平台（Linux、BSD…）：没有实现。
//
// 存在的意义是让 `go build ./...` 与 `go vet ./...` 在 Linux 上也能过——
// 本项目的 GUI 只发布 darwin/windows，但 CLI 全平台都发，仓库必须在所有平台
// 上可编译。调用方通过 Available() 判断，不需要感知平台。
package autostart

import "errors"

const supported = false

func open() (bool, error) { return false, errors.ErrUnsupported }

func set(bool) error { return errors.ErrUnsupported }
