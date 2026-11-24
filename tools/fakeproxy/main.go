package main

// fakeproxy — 可控的 HTTP CONNECT 代理，用于多线路 e2e 演练。
//
// 用途：真实链路无法注入「带宽窄 / 丢包 / 拒绝连接」这些故障，而多线路
// 择路的全部价值恰恰体现在这些场景。这个代理给出可控的劣化面：
//
//	-bandwidth  限速（KB/s），0 = 不限
//	-delay      每次 CONNECT 的额外延迟
//	-reject     直接拒绝 CONNECT（模拟线路挂掉）
//
// 用法：
//   go run ./tools/fakeproxy -listen 127.0.0.1:18081 -bandwidth 50
import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	listen    = flag.String("listen", "127.0.0.1:18081", "监听地址")
	bandwidth = flag.Int("bandwidth", 0, "限速 KB/s（0=不限）")
	delay     = flag.Duration("delay", 0, "每次 CONNECT 的额外延迟")
	reject    = flag.Bool("reject", false, "直接拒绝 CONNECT（模拟线路不可用）")
	label     = flag.String("label", "proxy", "日志标签")
	// rewrite 把 CONNECT 目标重写成「host:port」（本机 fake-IP DNS 的绕行）。
	//
	// 为什么需要它：本机开着 Clash 的 fake-IP，magpie.aiweb.xin 解析到
	// 198.18.x.x（不可路由的假地址）。直连线路能绑物理网卡绕过（DNS 也自己解），
	// 但这个测试代理自己是用系统解析器拨号的，所以它也会拿到假 IP。
	// 显式给出真实 host:port 才能让「经代理的线路」真正可用。
	rewrite = flag.String("rewrite", "", "把 CONNECT 目标重写为该 host:port（留空不改）")
	// bind 给向上游拨号的目标绑定源地址（绕开本机 TUN 代理劫持）。
	//
	// 为什么需要：本机 Clash 的 TUN 把 0.0.0.0/1 + 128.0.0.0/1 全部导进 utun，
	// 连 relay 的 28443 也会被劫持并切断 TLS。relay 客户端自己绑物理网卡
	// 绕过了它，而这个测试代理用的是普通 Dial，所以也要绑才能复现同样的链路。
	bind   = flag.String("bind", "", "向上游拨号时绑定的源地址（如 192.168.1.106）")
	nConns atomic.Int64
)

func main() {
	flag.Parse()
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("[%s] CONNECT 代理监听 %s  bw=%dKB/s delay=%v reject=%v\n",
		*label, *listen, *bandwidth, *delay, *reject)
	os.Stdout.Sync()

	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go handle(c)
	}
}

func handle(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)

	// 读 CONNECT 请求行
	line, err := br.ReadString('\n')
	if err != nil {
		return
	}
	var method, target, proto string
	if _, err := fmt.Sscanf(line, "%s %s %s", &method, &target, &proto); err != nil || method != http.MethodConnect {
		return
	}
	// 吃掉剩余头
	for {
		h, err := br.ReadString('\n')
		if err != nil {
			return
		}
		if h == "\r\n" || h == "\n" {
			break
		}
	}

	n := nConns.Add(1)
	if *reject {
		fmt.Printf("[%s] #%d CONNECT %s → 拒绝\n", *label, n, target)
		os.Stdout.Sync()
		io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}
	if *delay > 0 {
		time.Sleep(*delay)
	}

	dialTarget := target
	if *rewrite != "" {
		dialTarget = *rewrite
	}
	d := &net.Dialer{Timeout: 10 * time.Second}
	if *bind != "" {
		if ip := net.ParseIP(*bind); ip != nil {
			d.LocalAddr = &net.TCPAddr{IP: ip}
		}
	}
	up, err := d.Dial("tcp", dialTarget)
	if err != nil {
		fmt.Printf("[%s] #%d CONNECT %s → 拨号 %s 失败 %v\n", *label, n, target, dialTarget, err)
		os.Stdout.Sync()
		io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}
	defer up.Close()

	if _, err := io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}
	fmt.Printf("[%s] #%d CONNECT %s → 已建立\n", *label, n, target)
	os.Stdout.Sync()

	if *bandwidth > 0 {
		pipeThrottled(c, br, up, *bandwidth*1024)
		return
	}
	pipe(c, br, up)
}

// pipe 双向直通。
//
// 关键细节（第一次写错过一次，症状是「回 200 但 TLS 立刻 EOF」）：
//
//  1. 客户端方向必须从 **bufio.Reader** 读。CONNECT 头读完时缓冲区里可能
//     已经躺着紧随其后的 TLS ClientHello（同一批 TCP 段），从原始 conn 读
//     会跳过它。
//  2. 必须处理**半关闭**：一方读完不要立刻关掉整条连接，否则另一方向
//     正在传的数据会被掐断（TLS 握手后紧接应用数据，就是这么丢的）。
//     用 CloseWrite 传播 EOF，两个方向都结束后才整体关闭。
func pipe(a net.Conn, clientBuf *bufio.Reader, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		copyStream(b, clientBuf)
		if cw, ok := b.(*net.TCPConn); ok {
			cw.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		copyStream(a, b)
		if cw, ok := a.(*net.TCPConn); ok {
			cw.CloseWrite()
		}
	}()
	wg.Wait()
}

// copyStream 显式读写循环（不用 io.Copy：bufio 的 WriteTo 快路径缓冲语义
// 与持续搬运有细微差别；透明转发要的是「读到即写」）。
func copyStream(dst io.Writer, src io.Reader) {
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// pipeThrottled 双向直通但限制总吞吐（用 token bucket 的简化版）。
//
// 实现：每读一块就按 (已读字节 / 速率) 睡够相应时间。简陋但对「模拟窄带宽」
// 完全够用，而且靠真实的时间流逝自然限速，不需要精确的调度器。
func pipeThrottled(a net.Conn, clientBuf *bufio.Reader, b net.Conn, bytesPerSec int) {
	var total atomic.Int64
	t0 := time.Now()

	throttle := func(n int) {
		total.Add(int64(n))
		should := time.Duration(float64(total.Load()) / float64(bytesPerSec) * float64(time.Second))
		if elapsed := time.Since(t0); should > elapsed {
			time.Sleep(should - elapsed)
		}
	}

	done := make(chan struct{}, 2)
	go func() {
		buf := make([]byte, 16*1024)
		for {
			n, err := b.Read(buf)
			if n > 0 {
				throttle(n)
				if _, werr := a.Write(buf[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		done <- struct{}{}
	}()
	go func() {
		buf := make([]byte, 16*1024)
		for {
			// 从 clientBuf（bufio）读：可能已有缓冲的 ClientHello
			n, err := clientBuf.Read(buf)
			if n > 0 {
				throttle(n)
				if _, werr := b.Write(buf[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		done <- struct{}{}
	}()
	<-done
}

var _ = strconv.Itoa
var _ = strings.TrimSpace
