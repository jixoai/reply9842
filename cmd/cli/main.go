// relay-cli — 本地 HTTP 入口 + 隧道客户端（命令行版）
//
// 职责（2026-11-13 kzf 需求：CLI 与 GUI 双可执行文件，逻辑同源）：
//  1. 解析子命令与 flag，构造 relayclient.Config
//  2. 交给 internal/relayclient.Runner 跑（与 GUI 完全同一份转发逻辑）
//  3. 阻塞等信号，收到 SIGINT/SIGTERM 优雅停止
//
// 为什么用「子命令 + 无子命令即 serve」而非 cobra：
// 只有三个子命令，且 `relay-cli -listen ...` 这种旧写法必须继续可用（文档、
// 老脚本都这么写）。用一层 os.Args 嗅探就够了，引入依赖得不偿失。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jixoai/rfc9842-reply/internal/relayclient"
	"github.com/jixoai/rfc9842-reply/internal/stats"
)

// version 由 -ldflags "-X main.version=..." 注入（Makefile / release workflow）
var version = "dev"

func main() {
	// 子命令嗅探：第一个非 flag 参数是已知子命令才消费掉，否则按 serve 处理
	//
	// 注意 `relay-cli -listen x:1` 这种「没有子命令」的旧写法：args[1] 以 '-'
	// 开头，不是子命令名，直接走 serve。
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "serve", "status", "version", "help":
			cmd = args[0]
			args = args[1:]
		default:
			fmt.Fprintf(os.Stderr, "未知子命令 %q\n\n", args[0])
			usage()
			os.Exit(2)
		}
	}

	switch cmd {
	case "serve":
		runServe(args)
	case "status":
		runStatus(args)
	case "version":
		fmt.Printf("relay-cli %s\n", version)
	case "help":
		usage()
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `relay-cli — rfc9842-reply 客户端（本地中转 + 增量压缩隧道）

用法:
  relay-cli [flags]             启动转发（等价于 relay-cli serve）
  relay-cli serve [flags]       启动转发
  relay-cli status [flags]      读取 stats.json 显示当前状态
  relay-cli version             版本信息

serve 参数:
`)
	fs := newServeFlags(&relayclient.Config{})
	fs.SetOutput(os.Stderr)
	fs.PrintDefaults()
}

// newServeFlags 注册 serve 的全部 flag（默认值取自 relayclient 的常量）
func newServeFlags(c *relayclient.Config) *flag.FlagSet {
	c.Normalize()
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	fs.StringVar(&c.Listen, "listen", c.Listen, "本地 HTTP 监听")
	fs.StringVar(&c.Server, "server", c.Server, "relay-server 地址 host:port")
	fs.StringVar(&c.ServerName, "server-name", "", "TLS SNI（留空自动推导）")
	fs.StringVar(&c.Transport, "transport", c.Transport, "传输: auto|quic|tcp")
	fs.BoolVar(&c.Insecure, "insecure", false, "跳过服务端证书校验（自签观察期）")
	fs.IntVar(&c.MinDelta, "min-delta", c.MinDelta, "小于此字节数的请求体不做 delta")
	fs.Int64Var(&c.HistoryMB, "history-mb", c.HistoryMB, "基线历史总字节预算（MB）")
	fs.StringVar(&c.CaptureDir, "capture-dir", "", "采样目录：把请求体原样落盘供离线分析（留空不采）")
	fs.IntVar(&c.CaptureN, "capture-n", c.CaptureN, "最多采样多少条")
	fs.IntVar(&c.MaxResume, "max-resume", c.MaxResume, "断线后最大续传重试次数（0=不续传）")
	fs.StringVar(&c.LogPath, "log", "", "逐请求日志（默认 ~/.rfc9842-reply/client-<date>.jsonl）")
	fs.StringVar(&c.StatsPath, "stats", "", "汇总统计（默认同目录 client-stats.json）")
	return fs
}

func runServe(args []string) {
	c := relayclient.Config{}
	fs := newServeFlags(&c)
	fs.Parse(args) // ExitOnError：-h 会自己退出

	// 日志带时间戳前缀，与旧版输出保持一致
	log.SetFlags(log.LstdFlags)

	r := relayclient.New(c)
	if err := r.Start(); err != nil {
		log.Fatalf("%v", err)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Printf("收到退出信号，正在关闭")

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := r.Stop(ctx); err != nil {
		log.Printf("停止时出错: %v", err)
	}
}

// runStatus 读落盘的 stats.json 并按人眼友好的格式打印。
//
// 为什么不连进程内 stats：CLI 的 status 是「看另一个进程在干什么」，
// 唯一能跨进程读的通道就是文件。GUI 才是进程内快照。
func runStatus(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	statsPath := fs.String("stats", relayclient.DefaultStatsPath(), "汇总统计文件路径")
	jsonOut := fs.Bool("json", false, "原样输出 JSON（供脚本消费）")
	fs.Parse(args)

	if *jsonOut {
		b, err := os.ReadFile(*statsPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "读取 %s 失败: %v\n", *statsPath, err)
			os.Exit(1)
		}
		os.Stdout.Write(b)
		if len(b) > 0 && b[len(b)-1] != '\n' {
			fmt.Println()
		}
		return
	}

	a, err := stats.ReadAgg(*statsPath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Printf("还没有统计数据：%s 不存在\n", *statsPath)
			fmt.Println("（先用 relay-cli serve 跑起来，处理过请求后就会生成）")
			return
		}
		fmt.Fprintf(os.Stderr, "解析 %s 失败: %v\n", *statsPath, err)
		os.Exit(1)
	}

	saved := a.UpRaw - a.UpWire
	fmt.Printf("统计文件  %s\n", *statsPath)
	fmt.Printf("观测窗口  %s → %s\n", a.StartedAt, a.UpdatedAt)
	fmt.Println(strings.Repeat("-", 46))
	fmt.Printf("请求数    %d（错误 %d，基线命中 %d）\n", a.Requests, a.Errors, a.BaseHits)
	fmt.Printf("上行      %s → %s（%.3f%%，省 %s）\n",
		human(a.UpRaw), human(a.UpWire), ratio(a.UpRatio)*100, human(saved))
	fmt.Printf("下行      %s → %s\n", human(a.DownRaw), human(a.DownWire))
	fmt.Printf("延迟      p50 %d ms / p90 %d ms（TTFB p50 %d ms）\n", a.P50Ms, a.P90Ms, a.P50TTFB)
	if len(a.Encodings) > 0 {
		fmt.Printf("编码      %s\n", fmtMap(a.Encodings))
	}
	if len(a.Transport) > 0 {
		fmt.Printf("传输      %s\n", fmtMap(a.Transport))
	}
}

func fmtMap(m map[string]int64) string {
	// 固定几个常见键的顺序，避免每次输出顺序抖动
	order := []string{"dcz(L19)", "dcz(L12)", "dcz(L9)", "zstd", "identity", "resend-full", "quic", "tcp"}
	seen := map[string]bool{}
	var parts []string
	for _, k := range order {
		if v, ok := m[k]; ok {
			parts = append(parts, fmt.Sprintf("%s×%d", k, v))
			seen[k] = true
		}
	}
	for k, v := range m {
		if !seen[k] {
			parts = append(parts, fmt.Sprintf("%s×%d", k, v))
		}
	}
	return strings.Join(parts, "  ")
}

func ratio(r float64) float64 {
	if r <= 0 {
		return 0
	}
	return r
}

// human 把字节数转成人眼可读（与 GUI 共用一套口径，避免两处显示不一致）
func human(n int64) string {
	if n < 0 {
		return "-" + human(-n)
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
