// probe-proxy —— 透明 HTTP 代理，用来测量「到 OpenDesign 的真实首帧延迟」。
//
// 目的：ZCode → 本代理 → OpenDesign。代理不改任何内容，只记录每个请求的
// 各阶段耗时（DNS / TCP / TLS / 上游首字节 / 收到首块 body / 总时长），
// 让我们看清「慢在哪一段」。
//
// 为什么必须透明（不缓冲、不聚合）：流式响应的关键指标是「首帧何时到达
// 客户端」。任何一次 io.ReadAll 或批量 flush 都会把首帧人为推迟，测出来的
// 数字就没有意义了。所以这里用 io.Copy 边读边写，每读一块立刻 flush。
//
// 用法：
//   go run ./tools/probe-proxy -listen 127.0.0.1:19010 \
//       -upstream https://amr-link.open-design.ai
//
// 然后客户端把 base URL 指向 http://127.0.0.1:19010/v1
package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

var (
	listen   = flag.String("listen", "127.0.0.1:19010", "本地监听地址")
	upstream = flag.String("upstream", "https://amr-link.open-design.ai", "上游 base URL")
	verbose  = flag.Bool("v", true, "打印每个请求的详细时间线")
	seqNo    atomic.Int64
)

func main() {
	flag.Parse()

	u, err := url.Parse(*upstream)
	if err != nil {
		log.Fatalf("upstream 解析失败: %v", err)
	}

	// 传输层：保留 TLS 会话缓存，这样重复请求能看清「有没有复用连接」。
	// 不设 DisableKeepAlives——我们要观察真实的连接复用行为。
	tr := &http.Transport{
		TLSClientConfig:     &tls.Config{ServerName: u.Hostname()},
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
	}

	h := &handler{up: u, client: &http.Client{Transport: tr}}
	srv := &http.Server{
		Addr:              *listen,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
		// 不设 WriteTimeout：SSE 长连接需要
	}

	fmt.Printf("probe-proxy 监听 http://%s\n", *listen)
	fmt.Printf("  上游      %s\n", *upstream)
	fmt.Printf("  用法      把 base URL 指向 http://%s\n", *listen)
	fmt.Println("  提示      按 Ctrl-C 停止；每个请求会打印时间线")
	os.Stdout.Sync()

	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

type handler struct {
	up     *url.URL
	client *http.Client
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := seqNo.Add(1)
	t0 := time.Now()

	// 上游 URL = 上游 host + 原路径（保留 /v1/... 原样）
	target := *h.up
	target.Path = singleJoin(h.up.Path, r.URL.Path)
	target.RawQuery = r.URL.RawQuery

	// httptrace 记录连接建立各阶段
	var dnsDone, connDone, tlsDone time.Time
	trace := &httptrace.ClientTrace{
		DNSDone: func(httptrace.DNSDoneInfo) { dnsDone = time.Now() },
		ConnectDone: func(_, _ string, _ error) {
			if connDone.IsZero() {
				connDone = time.Now()
			}
		},
		TLSHandshakeDone: func(_ tls.ConnectionState, _ error) {
			if tlsDone.IsZero() {
				tlsDone = time.Now()
			}
		},
		GotConn: func(info httptrace.GotConnInfo) {
			if info.Reused {
				fmt.Printf("[#%d] 复用连接\n", id)
			}
		},
	}

	body := r.Body
	defer body.Close()

	req, err := http.NewRequestWithContext(
		httptrace.WithClientTrace(r.Context(), trace),
		r.Method, target.String(), body)
	if err != nil {
		http.Error(w, "build upstream request: "+err.Error(), 502)
		return
	}
	copyHeaders(req.Header, r.Header)
	// Host 用原目标（部分网关按 Host 路由）
	req.Host = h.up.Host

	resp, err := h.client.Do(req)
	if err != nil {
		fmt.Printf("[#%d] 上游失败 %.3fs: %v\n", id, time.Since(t0).Seconds(), err)
		http.Error(w, "upstream: "+err.Error(), 502)
		return
	}
	defer resp.Body.Close()

	tHeaders := time.Now()

	// 回写状态与响应头；X-Accel-Buffering: no 阻止任何中间层缓冲
	copyHeaders(w.Header(), resp.Header)
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(resp.StatusCode)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	// 流式转发：每读到一块立刻写给客户端并 flush。
	// 这是整个工具的核心——任何「先收集再发」都会污染首帧测量。
	firstByte := time.Time{}
	var total int64
	buf := make([]byte, 32*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if firstByte.IsZero() {
				firstByte = time.Now()
			}
			if _, werr := w.Write(buf[:n]); werr != nil {
				break
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			total += int64(n)
		}
		if rerr != nil {
			if rerr != io.EOF {
				fmt.Printf("[#%d] 读取中断: %v\n", id, rerr)
			}
			break
		}
	}

	tEnd := time.Now()
	if *verbose {
		ms := func(t time.Time) string {
			if t.IsZero() {
				return "  -  "
			}
			return fmt.Sprintf("%5.0f", t.Sub(t0).Seconds()*1000)
		}
		fmt.Printf("[#%d] %s %s  status=%d  bytes=%d\n", id, r.Method, r.URL.Path, resp.StatusCode, total)
		fmt.Printf("      DNS=%sms  TCP=%sms  TLS=%sms  首字节=%sms  首块body=%sms  总=%sms\n",
			ms(dnsDone), ms(connDone), ms(tlsDone), ms(tHeaders), ms(firstByte), ms(tEnd))
	}
}

func singleJoin(a, b string) string {
	a = strings.TrimSuffix(a, "/")
	if b == "" {
		return a
	}
	if !strings.HasPrefix(b, "/") {
		b = "/" + b
	}
	return a + b
}

func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		// 逐跳头不该转发
		switch strings.ToLower(k) {
		case "connection", "keep-alive", "proxy-authenticate",
			"proxy-authorization", "te", "trailers",
			"transfer-encoding", "upgrade", "host":
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

var _ = net.Dial
