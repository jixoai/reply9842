package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// format.go — 界面上的数字与文案口径（唯一一处，避免同一数字两处写法不同）
//
// 正交意图（2 个，2026-11-13 kzf 需求：「压缩比是 hero，数字不能跳动、方向不能看反」）：
//  1. 体积 / 时长 / 计数的显示口径
//  2. 把 relayclient 的原始错误翻成人话（原始串里前缀重复三层，直接显示会吓人）

// humanBytes 人类可读体积（与 CLI 的 status 输出同一口径）
func humanBytes(n int64) string {
	if n < 0 {
		return "-" + humanBytes(-n)
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// humanDuration 计时（02:14:08 / 14:08 / 8s），与设计稿的 uptime 同口径
func humanDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}

// displayVersion 版本号的显示形态，统一成一个前导 v。
//
// 注入源有两套口径：Makefile 用 `patsubst v%,%` 去掉了 v，GitHub release
// workflow 直接传 `git describe` 的输出（含 v）。所以这里不能假设有没有，
// 必须先归一——否则界面会显示成 "vv0.9.1"。
func displayVersion(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return "v?"
	}
	return "v" + strings.TrimPrefix(v, "v")
}

// groupDigits 千分位（12,847）。设计稿的请求数就是这么显示的，用户读位数更快。
func groupDigits(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// ratioFactor 压缩倍数：ratio 是 wire/raw（「压到多少」），倍数才是用户心里的
// 「压了多少」。0.05 显示成 20.0×，而不是让人误读成「只压了 5%」。
//
// 返回 (文本, 是否有效)。
func ratioFactor(ratio float64) (string, bool) {
	if ratio <= 0 || ratio > 1.0001 {
		return "", false
	}
	x := 1 / ratio
	switch {
	case x >= 100:
		return fmt.Sprintf("%.0f", x), true
	default:
		return fmt.Sprintf("%.1f", x), true
	}
}

// rawBytesPerKilo 每 1,000 字节明文实际发出的字节数（hero-note 的说明口径）
func rawBytesPerKilo(ratio float64) int {
	n := int(ratio*1000 + 0.5)
	if n < 1 {
		return 0
	}
	return n
}

// Error 前缀可能叠三层（`listen 127.0.0.1:19008: listen tcp 127.0.0.1:19008: bind: ...`）。
// 界面只该说一句人话，所以这里按已知形态做一次归一。
var (
	reBindInUse = regexp.MustCompile(`bind: address already in use`)
	// 端口取最后一个 host:port 里的 port：串里 127.0.0.1 的 "127" 不是端口，
	// 只认紧跟在 IP 之后的 `:<1-5 位数字>`。
	rePort = regexp.MustCompile(`(?:\d{1,3}(?:\.\d{1,3}){3}|\[[0-9a-fA-F:]+\]):(\d{1,5})`)
)

// humanError 把启动失败的原因翻成一句用户能行动的话。
//
// 只认shape，不猜业务：认不出来的原样返回（宁可糙，不可错）。
func humanError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()

	if reBindInUse.MatchString(msg) {
		// 取最后一个 host:port 的端口（错误串里地址出现两次，值相同）
		port := ""
		if all := rePort.FindAllStringSubmatch(msg, -1); len(all) > 0 {
			port = all[len(all)-1][1]
		}
		if port != "" {
			return "端口 " + port + " 已被占用（换个端口，或先关掉占用它的程序）"
		}
		return "端口已被占用（换个端口，或先关掉占用它的程序）"
	}
	if strings.Contains(msg, "permission denied") {
		return "没有权限监听该地址（1024 以下的端口需要管理员权限）"
	}
	if strings.Contains(msg, "unknown transport") || strings.Contains(msg, "未知传输方式") {
		return "传输方式不可用（可选 自动 / QUIC / TCP）"
	}
	// 已经是一句人话（没有三层嵌套的 "x: y: z"）时原样给出
	if strings.Count(msg, ": ") <= 1 {
		return msg
	}
	// 兜底：剥掉重复的 host:port 前缀，留最后一段最具体的原因
	if i := strings.LastIndex(msg, ": "); i >= 0 && i+2 < len(msg) {
		return msg[i+2:]
	}
	return msg
}
