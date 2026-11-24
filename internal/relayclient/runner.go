// Package relayclient — 本地 HTTP 入口 + 隧道客户端的可复用运行体
//
// 正交意图（2 个，2026-11-13 kzf 需求：「把 cmd/relay-client 的主逻辑抽到
// internal/relayclient（一个可复用的 Runner，接受配置、启动 HTTP 监听、暴露统计），
// CLI 和 GUI 都调它」）：
//  1. 一个 Runner 完成「起监听 → 处理请求 → 停监听」的完整生命周期，可反复
//     Start/Stop（GUI 的启动/停止按钮直接按）。
//  2. 统计既落盘（CLI 的 status 子命令读文件），也支持进程内快照（GUI 每秒刷新），
//     两条读路径共用同一个 stats.Agg 结构。
//
// 为什么不把 handler 留在 cmd 里：GUI 与 CLI 必须跑同一份转发逻辑，任何一侧
// 单独演进都会让「GUI 能用、CLI 不能」这类裂缝出现。逻辑只有一份。
package relayclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/jixoai/reply9842/internal/basefind"
	"github.com/jixoai/reply9842/internal/clientstream"
	"github.com/jixoai/reply9842/internal/codec"
	"github.com/jixoai/reply9842/internal/idem"
	"github.com/jixoai/reply9842/internal/meta"
	"github.com/jixoai/reply9842/internal/route"
	"github.com/jixoai/reply9842/internal/stats"
	"github.com/jixoai/reply9842/internal/transport"
)

// 默认值集中在一处：CLI 的 flag 默认值、GUI 的输入框初值都从这里取，
// 避免「文档写的默认」与「代码跑的默认」漂移。
const (
	DefaultListen = "127.0.0.1:18899"
	// DefaultServer 用**域名**而不是 IP：证书是按这个域名签的，域名目标下
	// SNI 自动等于它，用户不需要再填第二栏；IP 变了也只改 DNS，客户端不用动。
	// （2026-11-14 DNS 已切到香港，域名可解析。）
	DefaultServer = "magpie.aiweb.xin"
	// DefaultServerName 是 TLS SNI。上游用 IP 连接时**必须**填证书上的域名——
	// 服务端证书的 SAN 通常只有 DNS 没有 IP，留空推导成 IP 会直接
	// x509: cannot validate certificate for <ip> because it doesn't contain any IP SANs。
	DefaultServerName = "magpie.aiweb.xin"
	DefaultTransport  = "auto"
	DefaultMinDelta   = 16 << 10
	DefaultHistoryMB  = 64
	DefaultCaptureN   = 30
	DefaultMaxResume  = 3
)

// DefaultTunnelPort 目标机器不写端口时用的端口（隧道监听口）。
const DefaultTunnelPort = "28443"

// NormalizeTarget 把用户填的「目标机器」补成 host:port。
//
// 为什么允许不写端口：用户脑子里这个字段就是「连哪台机器」，端口是产品
// 固定的 28443，不该让人记。写法上兼容三种：
//
//	magpie.aiweb.xin          → magpie.aiweb.xin:28443
//	magpie.aiweb.xin:8443     → 原样
//	https://magpie.aiweb.xin  → 去掉 scheme 再补端口
//
// IPv6 字面量必须带方括号与端口（[2001:db8::1]:28443），无括号裸写无法与
// host:port 区分，按「没写端口」处理会得到非法地址——由 dial 层报错即可。
func NormalizeTarget(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	for _, p := range []string{"https://", "http://", "tcp://", "quic://"} {
		s = strings.TrimPrefix(s, p)
	}
	s = strings.TrimSuffix(s, "/")
	if s == "" {
		return s
	}
	if strings.HasPrefix(s, "[") { // [v6]:port 或 [v6]
		if _, _, err := net.SplitHostPort(s); err == nil {
			return s
		}
		return s + ":" + DefaultTunnelPort
	}
	if strings.Count(s, ":") == 1 {
		return s // host:port
	}
	if strings.Contains(s, ":") {
		return s // 裸 IPv6：留给 dial 报错，别瞎改
	}
	return s + ":" + DefaultTunnelPort
}

// SNINeeded 目标机器是 IPv4/IPv6 字面量时，才需要单独填证书域名。
//
// 这就是「上游地址 + 服务器域名」两个字段可以合并的原因：目标是域名时，SNI
// 就是它自己，填第二遍纯属重复；只有目标是 IP（证书 SAN 一般只有 DNS）时，
// 才必须额外知道该用哪个名字去校验。见 transport.Dialer.serverName 的推导。
func SNINeeded(server string) bool {
	host, _, err := net.SplitHostPort(server)
	if err != nil {
		host = server
	}
	host = strings.Trim(host, "[]")
	return net.ParseIP(host) != nil
}

// Config 一次运行所需的全部配置（与 CLI flag 一一对应）
type Config struct {
	Listen     string        // 本地 HTTP 监听，如 127.0.0.1:18899
	Server     string        // relay-server 地址 host:port
	ServerName string        // TLS SNI，留空自动推导
	LocalAddr  string        // 绑定出站源地址（绕 TUN 代理劫持），留空让内核选
	Proxy      string        // 代理链路 http/https/socks5，留空直连
	Transport  string        // auto | quic | tcp
	Insecure   bool          // 跳过服务端证书校验（自签观察期）
	MinDelta   int           // 小于此字节数的请求体不做 delta
	HistoryMB  int64         // 基线历史总字节预算（MB）
	CaptureDir string        // 采样目录，留空不采
	CaptureN   int           // 最多采样多少条
	MaxResume  int           // 断线后最大续传重试次数（0=不续传）
	LogPath    string        // 逐请求日志
	StatsPath  string        // 汇总统计
	Retain     time.Duration // 日志保留时长（磁盘），同时是重启重建的扫描范围
	Window     time.Duration // 图表显示区间（横轴跨度）

	// Routes 多线路配置（有序，顺序即用户设定的优先级）。
	//
	// 空列表时由 Normalize 用上面那组单值字段合成一条线路——这保证了
	// 「老配置 / 老 CLI 调用」的行为与此前完全一致，是本变更向后兼容的
	// 全部机制。非空时，上面那组字段只作为默认值来源（ServerName /
	// Insecure / Transport 仍会应用到每条线路），不再直接决定出口。
	//
	// 用 route.Set 而不是 []route.Route：Set 上带着 Enabled()/Servers()/
	// Validate() 这些多路线路才需要的操作，声明成裸切片会让调用方到处散落
	// 「for 循环 + 手写过滤」的重复逻辑。
	Routes route.Set
}

// Normalize 补齐默认值与派生路径（幂等，可反复调用）
func (c *Config) Normalize() {
	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	if c.Server == "" {
		c.Server = DefaultServer
	}
	c.Server = NormalizeTarget(c.Server)
	// ServerName 留空时的兜底。注意：只在「上游是 IP 且证书是已知域名」这种
	// 内置默认组合下填；用户改了 Server 却留空 SNI 时，transport 层会按
	// Server 的 host 推导（对域名成立，对 IP 会失败——失败信息本身可读）。
	if c.ServerName == "" && c.Server == DefaultServer {
		c.ServerName = DefaultServerName
	}
	// 目标是域名时即便留了 ServerName 也用不上（transport 按 Addr 推导），
	// 这里不再额外清理：显式设置可能来自老配置，留着不影响连通性。
	if c.Transport == "" {
		c.Transport = DefaultTransport
	}
	if c.MinDelta == 0 {
		c.MinDelta = DefaultMinDelta
	}
	if c.HistoryMB == 0 {
		c.HistoryMB = DefaultHistoryMB
	}
	if c.CaptureN == 0 {
		c.CaptureN = DefaultCaptureN
	}
	if c.MaxResume == 0 {
		c.MaxResume = DefaultMaxResume
	}
	if c.LogPath == "" {
		c.LogPath = DefaultLogPath()
	}
	if c.StatsPath == "" {
		c.StatsPath = filepath.Join(filepath.Dir(c.LogPath), "client-stats.json")
	}
	if c.Retain <= 0 {
		c.Retain = DefaultRetain
	}
	if c.Window <= 0 {
		c.Window = DefaultWindow
	}
	if c.Window > c.Retain {
		c.Window = c.Retain
	}
	c.normalizeRoutes()
}

// normalizeRoutes 保证 Routes 至少有一条，且每条都已规范化。
//
// 三条规则（对应三种「用户可能怎么给」）：
//
//  1. Routes 为空 → 用单值字段合成一条线路。这是**向后兼容的全部机制**：
//     老 CLI（只有 -server/-proxy/-local-addr）、老 settings.json（只有
//     network_mode/proxy_url）都走这条路，行为与今天一字不差。
//  2. Routes 非空但某条缺 Server/Transport → 用单值字段补（而不是硬编码默认），
//     这样用户只写「一条代理线路」时不必重复填 server。
//  3. 逐条 Normalize（补名、补端口、纠正 proxy+quic）。
//
// 幂等：可反复调用（SetConfig / New 都会触发）。
func (c *Config) normalizeRoutes() {
	if len(c.Routes) == 0 {
		r := route.Route{
			ID:        "默认线路",
			Server:    c.Server,
			LocalAddr: c.LocalAddr,
			Transport: c.Transport,
		}
		if c.Proxy != "" {
			r.Kind = route.KindProxy
			r.Proxy = c.Proxy
		} else {
			r.Kind = route.KindDirect
		}
		c.Routes = []route.Route{r.Normalize()}
		return
	}

	// 复制一份再改，避免调用方的切片被就地污染（Config 是值语义传递的）
	rs := make([]route.Route, len(c.Routes))
	copy(rs, c.Routes)
	for i := range rs {
		if rs[i].Server == "" {
			rs[i].Server = c.Server
		}
		// Transport 只在用户没显式给时继承全局（显式给了就必须尊重——
		// 那正是「这条线路走 QUIC、那条走 TCP」的能力所在）
		if rs[i].Transport == "" {
			rs[i].Transport = c.Transport
		}
		// 源地址同样继承（缺它是真实踩到的可用性缺陷）：
		// 用户用 -routes-file 描述多条线路时，`-local-addr` 这个**全局**
		// 开关（绕开代理 TUN 劫持）对每条 direct 线路都该生效。早期实现
		// 只在「Routes 为空、由单值字段合成」的路径里带上它，于是
		// `-routes-file + -local-addr` 组合会连不上（实测 502/EOF）。
		//
		// 只在 direct 线路且用户没逐条指定时补——relay 线路不该绑源地址
		// （它的语义就是「不依赖本机网络」，Validate 也会拒绝）。
		if rs[i].LocalAddr == "" && c.LocalAddr != "" && rs[i].Kind == route.KindDirect {
			rs[i].LocalAddr = c.LocalAddr
		}
		// 代理地址同理：全局 -proxy 对「未指定代理的 proxy 线路」生效。
		// 反过来 direct 线路绝不能继承 proxy（那会静默走代理，Validate 会拒）。
		if rs[i].Proxy == "" && c.Proxy != "" && rs[i].Kind == route.KindProxy {
			rs[i].Proxy = c.Proxy
		}
	}
	c.Routes = route.Set(rs).Normalize()
}

// DefaultDir 运行时数据目录（日志与统计同放这里）
func DefaultDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".reply9842")
}

// DefaultLogPath 当日日志路径（按日期滚动，便于 jq 直读）
func DefaultLogPath() string {
	return filepath.Join(DefaultDir(), "client-"+time.Now().Format("2006-01-02")+".jsonl")
}

// DefaultStatsPath 汇总统计路径
func DefaultStatsPath() string { return filepath.Join(DefaultDir(), "client-stats.json") }

// Runner 可反复启动/停止的客户端运行体
type Runner struct {
	mu sync.Mutex
	c  Config

	srv    *http.Server
	ln     net.Listener
	col    *stats.Collector
	dialer *transport.Dialer // 单线路时代的兼容字段：指向 Routes[0] 的 Dialer
	finder *basefind.Finder

	// pools 多线路连接池集合（每条线路独立池）。
	// 只有一条线路时它就是那一条的池，行为与旧的单个 pool 完全等价。
	pools *transport.PoolSet
	// picker 择路器（按预计完成时间 + 失败降权）。
	picker *route.Picker

	running   bool
	startedAt time.Time
	lastErr   error

	// statsErr 统计器打开失败的原因。统计是观测功能，它起不来不该拖住转发，
	// 但也不能静默——GUI 会把它显示在提示行。
	statsErr error

	// Logger 运行日志出口；为 nil 时用 log.Default()（CLI 走标准输出，
	// GUI 没有终端，因此 GUI 会把它设成一个环状缓冲，供界面显示）。
	logger *log.Logger
	// OnEvent 每次启动/停止/出错时回调（GUI 用来触发重绘）。可为 nil。
	OnEvent func()
}

// New 创建 Runner（不启动）
func New(c Config) *Runner {
	c.Normalize()
	return &Runner{c: c, logger: log.Default()}
}

// SetLogger 替换日志出口（GUI 用内存环形缓冲，CLI 用标准输出）
func (r *Runner) SetLogger(l *log.Logger) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logger = l
}

// Config 当前生效配置（已 Normalize）
func (r *Runner) Config() Config {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.c
}

// SetConfig 停止状态下更新配置。运行中调用无效（返回错误），
// 避免「改了参数但旧监听还在跑」的错觉。
func (r *Runner) SetConfig(c Config) error {
	c.Normalize()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running {
		return errors.New("运行中不能改配置，请先停止")
	}
	// 路径或时间跨度变了，旧统计器就指向了错的文件/错的窗口，必须重建。
	// 重建 = 关掉旧的、置 nil，下次 Stats() 时按新配置重新流式读盘。
	if c.LogPath != r.c.LogPath || c.StatsPath != r.c.StatsPath ||
		c.Retain != r.c.Retain || c.Window != r.c.Window {
		if r.col != nil {
			r.col.Close()
			r.col = nil
		}
	}
	r.c = c
	return nil
}

// Running 是否正在监听
func (r *Runner) Running() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running
}

// Addr 实际监听地址（启动后才有意义，例如端口填 0 时返回真实端口）
func (r *Runner) Addr() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ln == nil {
		return r.c.Listen
	}
	return r.ln.Addr().String()
}

// Err 最近一次启动失败原因（成功后清空）
func (r *Runner) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastErr
}

// StartedAt 本次启动时刻（未运行为零值）
func (r *Runner) StartedAt() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.startedAt
}

// specsFor 把 Config 的线路列表翻译成 transport 侧的建连参数。
//
// 单独抽出来的理由：这段翻译有三处容易错且不必碰网络的分支
// （SNI 继承、中继换主机、代理纠正），必须能脱离 Start 单独测试——
//   - 单线路时产物必须与旧代码逐字段等价（回归护栏）
//   - 中继指向不同主机时不能沿用原 SNI
//
// 返回空列表时给出错误（而不是让调用方自己判）：没有出口的客户端
// 启动成功毫无意义，失败必须发生在 bind 之后、running 置位之前。
func specsFor(c Config) ([]transport.DialerSpec, error) {
	specs := make([]transport.DialerSpec, 0, len(c.Routes))
	for _, rt := range c.Routes {
		if !rt.IsEnabled() {
			continue
		}
		mode := transport.Kind(strings.ToLower(rt.Transport))
		switch mode {
		case transport.KindQUIC, transport.KindTCP, transport.KindAuto:
		default:
			return nil, fmt.Errorf("线路 %q 的传输方式 %q 非法（可选 auto|quic|tcp）", rt.ID, rt.Transport)
		}

		// SNI 优先级（实测抓出的缺口：中继复用同一张证书时必然 x509 失败）：
		//
		//	1. 线路自己的 server_name（**最高优先**）——中继指向另一台 IP
		//	   但复用同一张证书（SAN 只有域名）时，必须靠它才能通过校验
		//	2. 同 server 的线路继承全局 ServerName
		//	3. 异主机的中继不继承（那是给另一台机器签的名字），留空让
		//	   transport 按 Addr 推导（域名目标下成立，IP 目标则需第 1 项）
		sni := rt.ServerName
		if sni == "" {
			if rt.Kind == route.KindRelay && rt.Server != c.Server {
				sni = ""
			} else {
				sni = c.ServerName
			}
		}

		specs = append(specs, transport.DialerSpec{
			ID:         rt.ID,
			Addr:       rt.Server,
			ServerName: sni,
			LocalAddr:  rt.LocalAddr,
			Proxy:      rt.Proxy,
			Mode:       mode,
			Insecure:   c.Insecure,
		})
	}
	if len(specs) == 0 {
		return nil, errors.New("没有任何启用的线路（至少需要一条）")
	}
	return specs, nil
}

// Start 启动监听。先 bind 再返回，因此返回 nil 意味着端口已经真实可用。
//
// 锁的纪律（踩过一次）：本函数只在「读/写共享状态」的短片段里持锁，
// 中途的 bind、建目录、写日志一律不持锁。早期版本用 defer Unlock 罩住整个
// 函数，而 logf 也要拿 mu——结果是 Start 自死锁：端口已经 bind 成功（所以
// `lsof` 能看到在听），但 Start 永远不返回，界面就一直显示「未连接」。
func (r *Runner) Start() error {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return errors.New("已在运行")
	}
	c := r.c
	logger := r.logger
	r.mu.Unlock()

	col, err := r.stats()
	if err != nil {
		r.mu.Lock()
		r.lastErr = fmt.Errorf("stats: %w", err)
		r.mu.Unlock()
		return fmt.Errorf("stats: %w", err)
	}

	mode := transport.Kind(strings.ToLower(c.Transport))
	switch mode {
	case transport.KindQUIC, transport.KindTCP, transport.KindAuto:
	default:
		err := fmt.Errorf("未知传输方式 %q（可选 auto|quic|tcp）", c.Transport)
		r.mu.Lock()
		r.lastErr = err
		r.mu.Unlock()
		return err
	}

	// 先 bind 再置 running：bind 失败时 Runner 保持「未运行」，GUI 的
	// 状态栏能直接显示原因
	ln, err := net.Listen("tcp", c.Listen)
	if err != nil {
		err = fmt.Errorf("listen %s: %w", c.Listen, err)
		r.mu.Lock()
		r.lastErr = err
		r.mu.Unlock()
		return err
	}

	// ---- 多线路装配 ----
	//
	// 每条线路一个独立 Dialer（各自持有 prefer 粘性状态）+ 一个独立连接池。
	// 见 internal/transport/pools.go 顶部「为什么按线路独立池」。
	//
	// 单线路时这套结构与旧的「单个 dialer + 单个 pool」等价：
	// 只有一个 map 条目、Picker 永远选它——行为逐字段一致。
	specs, err := specsFor(c)
	if err != nil {
		ln.Close()
		r.mu.Lock()
		r.lastErr = err
		r.mu.Unlock()
		return err
	}

	pools := transport.BuildPools(specs, 0)
	picker := route.NewPicker(c.Routes.Enabled())

	finder := basefind.New(c.HistoryMB)
	h := &handler{
		cfg: c, pools: pools, picker: picker, finder: finder, col: col, logger: logger,
		// 续传路径需要一个"当前线路"的 Dialer（走池之外的直连）。
		// 见 P2-4 修复：续传实际应回到本请求的线路，这里的 dialer 只作兜底。
		dialer: pools.MustFirstDialer(),
	}
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
		// 不设 WriteTimeout：SSE 长连接需要
	}

	r.mu.Lock()
	r.col, r.ln, r.finder, r.srv = col, ln, finder, srv
	r.dialer = h.dialer
	r.pools, r.picker = pools, picker
	r.running = true
	r.startedAt = time.Now()
	r.lastErr = nil
	r.mu.Unlock()

	if logger != nil {
		logger.Printf("relay-client 启动")
		logger.Printf("  本地 HTTP   http://%s", ln.Addr())
		if len(specs) == 1 {
			logger.Printf("  隧道上游    %s (%s)", specs[0].Addr, specs[0].Mode)
		} else {
			logger.Printf("  隧道线路    %d 条（按预计完成时间择路）", len(specs))
			for _, sp := range specs {
				line := fmt.Sprintf("    - %s → %s (%s", sp.ID, sp.Addr, sp.Mode)
				if sp.Proxy != "" {
					line += "，经代理"
				}
				if sp.LocalAddr != "" {
					line += "，绑 " + sp.LocalAddr
				}
				logger.Printf("%s)", line)
			}
			if len(c.Routes.Servers()) > 1 {
				logger.Printf("  注意        存在跨 server 线路（%v）：delta 基线不共享，跨 server 切换会偶发一次全量重发",
					c.Routes.Servers())
			}
		}
		logger.Printf("  delta 门槛  >=%d 字节（块复用率>=50%%）", c.MinDelta)
		logger.Printf("  基线历史    %d MB", c.HistoryMB)
		logger.Printf("  日志        %s", c.LogPath)
	}

	go r.superviseServe(h, srv, ln, c.Listen, logger)

	r.notify()
	return nil
}

// Stop 停止监听并落盘统计。未运行时是 no-op（GUI 上「停止」按钮可以随便按）。
//
// 统计器**不在这里关闭**：它还持有磁盘上的历史聚合，停止后 GUI 仍要显示
// 「今天一共省了多少」。关掉它等于把界面上唯一的长期数字清零。
func (r *Runner) Stop(ctx context.Context) error {
	r.mu.Lock()
	if !r.running {
		r.mu.Unlock()
		return nil
	}
	srv := r.srv
	r.running = false
	r.srv, r.ln = nil, nil
	r.mu.Unlock()

	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := srv.Shutdown(ctx)
	// Shutdown 之后仍有在途请求，Close 兜底
	_ = srv.Close()

	// 关闭连接池：每条线路的空闲连接都要释放。
	// 不在这里关的后果是「Stop 之后进程还握着 N 条到香港的 TCP/QUIC 连接」，
	// 用户在服务端看到一批不消失的连接，且下次 Start 会再建一批。
	r.mu.Lock()
	pools := r.pools
	r.pools, r.picker = nil, nil
	r.mu.Unlock()
	if pools != nil {
		pools.Close()
	}

	r.logf("已停止监听")
	r.notify()
	return err
}

// stats 取（或惰性创建）统计器。
//
// 为什么统计器的生命周期比一次 Start/Stop 长（2026-11-13 改造的核心）：
// 统计的事实来源是磁盘日志，与「监听是否在跑」无关。启动时就流式读一遍
// 历史，停止后继续保留，GUI 才能答「今天到底省了多少」——这正是用户要求
// 的「重启不丢」的另一半：重启不丢、停止也不丢。
func (r *Runner) stats() (*stats.Collector, error) {
	r.mu.Lock()
	if r.col != nil {
		col := r.col
		r.mu.Unlock()
		return col, nil
	}
	c := r.c
	r.mu.Unlock()

	col, err := stats.Open(stats.Options{
		LogPath:   c.LogPath,
		StatsPath: c.StatsPath,
		Retain:    c.Retain,
		Window:    c.Window,
	})
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		r.statsErr = err
		return nil, err
	}
	r.statsErr = nil
	// 竞态：两个 goroutine 同时进来时保留先到的那个，后到的关掉自己
	if r.col != nil {
		col.Close()
		return r.col, nil
	}
	r.col = col
	return col, nil
}

// StatsErr 统计器打不开的原因（nil 表示正常）。
func (r *Runner) StatsErr() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.statsErr
}

// Snapshot 进程内统计快照。
//
// 未运行时**不再返回零值**：统计从磁盘重建，停止状态下也有真实历史。
// 只有「一份日志都没有」的新用户看到全 0。
func (r *Runner) Snapshot() stats.Agg {
	col, err := r.stats()
	if err != nil || col == nil {
		return stats.Agg{}
	}
	return col.Snapshot()
}

// Routes 返回各线路的实时质量快照（供 GUI / status 展示）。
//
// 未运行时返回空：线路评分是运行时观测，进程重启后没有历史（这是有意的——
// 用上一次运行时的评分来指导现在选路，比没有更危险）。
func (r *Runner) Routes() []route.Snapshot {
	r.mu.Lock()
	p := r.picker
	r.mu.Unlock()
	if p == nil {
		return nil
	}
	return p.Snapshots()
}

// SetStatsSettings 应用两个时间跨度偏好（保留时长 / 显示区间）。
//
// 运行中也能改：这两项不碰监听、不碰在途请求，只影响读盘范围与分桶粒度，
// 所以不必要求用户先停止。改完立刻重算，界面下一帧就是新窗口的数据。
func (r *Runner) SetStatsSettings(retain, window time.Duration) error {
	r.mu.Lock()
	r.c.Retain, r.c.Window = retain, window
	r.c.Normalize()
	col := r.col
	r.mu.Unlock()

	if col == nil {
		// 还没打开过：留到第一次 Snapshot/Start 时按新配置建
		_, err := r.stats()
		return err
	}
	col.SetRange(retain, window)
	r.notify()
	return nil
}

// Close 释放统计器（CLI 退出、单实例让位时调用）。
func (r *Runner) Close() error {
	r.mu.Lock()
	col := r.col
	r.col = nil
	r.mu.Unlock()
	if col != nil {
		return col.Close()
	}
	return nil
}

func (r *Runner) logf(format string, args ...any) {
	r.mu.Lock()
	l := r.logger
	r.mu.Unlock()
	if l != nil {
		l.Printf(format, args...)
	}
}

func (r *Runner) notify() {
	r.mu.Lock()
	fn := r.OnEvent
	r.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// ---- 转发 handler ----

type handler struct {
	cfg      Config
	dialer   *transport.Dialer // 续传路径用的「当前线路」Dialer（不走池）
	pools    *transport.PoolSet
	picker   *route.Picker
	finder   *basefind.Finder
	col      *stats.Collector
	logger   *log.Logger
	capMu    sync.Mutex
	capCount int
}

// lease 是一次「借出的连接」的完整记录。
//
// 为什么把 routeID 与线路对象一起带出来：归还样本时需要知道**这条连接属于
// 哪条线路**（成功样本要记到该线路的模型上，失败样本也要降到该线路的权重）。
// 旧代码只有一个池，所以连接天然属于唯一线路；多线路后这个归属必须显式携带，
// 否则样本会记错线路，评分完全失真。
type lease struct {
	conn    *transport.Conn
	routeID string
	cand    *route.Candidate
	reason  string
}

// getConn 为一次「预计要搬 approxBytes 字节」的请求借一条连接。
//
// 流程（对应 specs/transport 的「按预计完成时间择路」+「降权不熔断」）：
//
//  1. picker 按 est 选出候选线路
//  2. 从该线路的池取连接
//  3. 取不到 → 记一次失败（降权该线路），换下一条线路重试
//     （最多试遍全部线路；全部失败才返回错误——这是「保底」的落地）
//
// 第 3 步的循环是本变更「线路坏了不影响可用性」的核心：没有它，一条线路
// 的 dial 失败会让请求直接 502，即使另一条线路完全健康。
func (h *handler) getConn(ctx context.Context, approxBytes int64) (lease, error) {
	if h.pools == nil {
		// 极端情形（未 Start 就调用）：给出可读错误而不是 panic
		return lease{}, errors.New("relayclient: 连接池未就绪")
	}

	var lastErr error
	// tried 记录本次请求**已经试过**的线路。
	//
	// 为什么必须有它（真实 e2e 抓出的缺陷）：
	// 一条从未成功过的线路样本数为 0，会走「主动探测」通道被优先选中；
	// 它失败后样本**仍然是 0**（失败不进拟合），于是下一轮 Select 又会
	// 优先选它——同一次请求里反复打同一条死线路，直到试满次数。
	// 把「已试过的线路」交给 SelectExcluding，把「一次请求内每条线路
	// 最多试一次」变成硬约束。
	tried := make(map[string]bool, len(h.picker.All()))
	maxTries := len(h.picker.All())
	if maxTries == 0 {
		maxTries = 1
	}

	for attempt := 0; attempt < maxTries; attempt++ {
		choice, ok := h.picker.SelectExcluding(approxBytes, tried)
		if !ok {
			if lastErr != nil {
				return lease{}, lastErr
			}
			return lease{}, errors.New("relayclient: 没有启用任何线路")
		}

		cand := h.picker.Candidate(choice.Route.ID)
		conn, err := h.pools.Get(ctx, choice.Route.ID)
		if err == nil {
			return lease{conn: conn, routeID: choice.Route.ID, cand: cand, reason: choice.Reason}, nil
		}

		// 该线路建连失败：降权并尝试下一条。
		// 若这次是被「主动探测」选中的（Known=false 且线路无样本），
		// 额外记一次探测失败以驱动指数退避——否则黑洞型线路会每 30 秒
		// 吃掉一个用户请求并让它多等满一次 dial 超时（对抗审查 P2-2）。
		tried[choice.Route.ID] = true
		if cand != nil {
			cand.Model.ObserveFailure()
			if !choice.Known {
				if n, _, _, _ := cand.Model.Stats(); n == 0 {
					h.picker.NoteProbeFailure(choice.Route.ID)
				}
			}
		}
		h.logf("线路 %q 取连接失败（%v），按降权换下一条", choice.Route.ID, err)
		// 合并而不是覆盖（对抗审查 P3-3b）：多线路全败时只留最后一条错误，
		// 排障的人看不到其余线路为什么也失败——而「全都不通」恰恰是最需要
		// 全部证据的场景。
		lastErr = errors.Join(lastErr, fmt.Errorf("线路 %q: %w", choice.Route.ID, err))
	}
	if lastErr == nil {
		lastErr = errors.New("relayclient: 所有线路均取连接失败")
	}
	return lease{}, lastErr
}

// release 归还连接并回写样本。
//
// reusable 表示「连接可复用」（响应完整读完，或 DeltaMiss 这种协议层
// 但连接本身干净的情况）。
func (h *handler) release(l lease, bytes int64, wire time.Duration, reusable bool) {
	h.releaseWith(l, bytes, wire, reusable, true)
}

// releaseWith 是 release 的完整形态：countSample 控制「是否把这次计入成功样本」。
//
// 存在的理由只有一个场景：FrameDeltaMiss。那次请求在同一条线路上**没有完成**
// （要重发全量），所以不该当成一次成功样本（否则会给出「这条线只要几百字节」
// 的错误印象）；但它也绝不是线路失败（协议层面的基线缺失而已）。
// 用「不计样本、不记失败」表达这个中间态。
func (h *handler) releaseWith(l lease, bytes int64, wire time.Duration, reusable, countSample bool) {
	if l.cand != nil {
		switch {
		case !reusable:
			l.cand.Model.ObserveFailure()
		case countSample:
			l.cand.Model.Observe(bytes, wire)
		default:
			// 中间态：不记成功也不记失败。连接仍按 reusable 处理。
		}
	}
	if l.conn == nil {
		return
	}
	if reusable {
		h.pools.Put(l.routeID, l.conn)
	} else {
		h.pools.Drop(l.conn)
	}
}

// estimateWireBytes 预估这次请求在隧道上**实际要发多少字节**，供择路使用。
//
// 为什么不直接返回 len(body)：delta 命中时两者差 4 个数量级
// （实测 1.5 MB → 129 B），用明文体积择路会让系统永远只挑"带宽最大"的线路，
// 而该请求其实只受 RTT 影响。这是真实 e2e 抓出来的缺陷。
const deltaHitFloor = 4 << 10

func estimateWireBytes(finder *basefind.Finder, body []byte, minDelta int) int64 {
	n := int64(len(body))
	if n <= 0 {
		return 0
	}
	if int(n) < minDelta {
		return n // 小请求：编码后与明文同阶（或更小），不会误判
	}

	// 命中基线时**不能**一律估「几百字节」。
	//
	// 对抗审查指出的缺陷：复用率的分母是**基线长度**，所以「64 KB 旧基线
	// + 1.4 MB 新增内容」的 1.5 MB 请求命中率算出 100%，但字典只覆盖了
	// 64 KB，剩下 1.4 MB 仍要按普通 zstd 压（实际 wire 约 200–400 KB）。
	// 若按「命中 = 4 KB」来估，系统会把这种请求误判成小请求，交给窄带线路——
	// 方向恰好与 owner 关心的「传完总耗时」相反。
	//
	// 正确的量是「没被基线覆盖的那部分」：未覆盖字节数按 zstd 经验比 0.2 估，
	// 再与下限取大（命中时协议开销确实很小）。
	_, base, reused := finder.PickWithReuse(body)
	if base != nil {
		remaining := n - int64(reused)
		if remaining < 0 {
			remaining = 0
		}
		est := remaining / 5
		if est < deltaHitFloor {
			est = deltaHitFloor
		}
		return est
	}
	// 无基线：zstd 经验比 0.2（实测 dcz(L19) 在真实 Agent 请求上压到 0.2 左右），
	// 下限 1 KB 避免极小值把择路推给"看起来零成本"的线路。
	// 注：对不可压内容（二进制/已压缩负载）这会低估 5 倍——本产品的负载是
	// LLM JSON 文本，几乎恒可压，接受这个偏差（对抗审查已确认该取舍）。
	est := n / 5
	if est < 1024 {
		est = 1024
	}
	return est
}

// multiServer 当前配置是否包含指向**不同 server**的多条线路。
//
// 用途：区分「delta-miss 是跨 server 的必然结果」与「是单 server 的 store 淘汰」。
// 前者应丢本地基线（那台机器确实没有），后者不能丢（丢了会让下次请求
// 必然 miss，陷入永久振荡——实测单 server 复现）。
func (h *handler) multiServer() bool {
	return len(h.cfg.Routes.Servers()) > 1
}

func (h *handler) logf(format string, args ...any) {
	if h.logger != nil {
		h.logger.Printf(format, args...)
	}
}

// capture 把请求体原样落盘（按序号命名），用于离线重放分析。
// 只采前 captureN 条，避免无限增长。
func (h *handler) capture(body []byte) {
	h.capMu.Lock()
	n := h.capCount
	if n >= h.cfg.CaptureN {
		h.capMu.Unlock()
		return
	}
	h.capCount++
	h.capMu.Unlock()

	os.MkdirAll(h.cfg.CaptureDir, 0o755)
	path := filepath.Join(h.cfg.CaptureDir, fmt.Sprintf("req-%04d.bin", n))
	if err := os.WriteFile(path, body, 0o600); err != nil {
		h.logf("capture write err: %v", err)
		return
	}
	h.logf("captured #%d %d bytes -> %s", n, len(body), path)
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rec := &stats.Record{Method: r.Method, Path: r.URL.RequestURI()}

	// ---- 读请求体 ----
	body, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		http.Error(w, "read body: "+err.Error(), 400)
		rec.Status, rec.Err = 400, err.Error()
		h.col.Observe(rec)
		return
	}
	rec.UpRaw = int64(len(body))

	// 采样（仅在开启时）：原样落盘，供离线分析二进制前缀特征
	if h.cfg.CaptureDir != "" {
		h.capture(body)
	}

	// ---- 先取连接：上行编码按**这条连接**的 ALPN 协商结果选 ----
	//
	// 为什么取连接要排在编码之前：dcz2 只有双方都支持时才能用（见
	// transport.ALPNv2），而「对面能不能解 dcz2」写在连接协商结果里。池里可能
	// 混着 v1/v2 两种连接（服务端滚动升级期间），所以判据必须是本次要用的
	// 那条连接，而不是「客户端自己支持 dcz2」。选错的下场不是降级：老服务端
	// 收到 EncDcz2 只会报未知编码，请求整个失败。
	//
	// 择路的体积口径：**预估的隧道传输量**，不是明文 body 长度。
	// 见 estimateWireBytes 的注释（delta 命中时两者差 4 个数量级）。
	estWire := estimateWireBytes(h.finder, body, h.cfg.MinDelta)

	l, err := h.getConn(r.Context(), estWire)
	if err != nil {
		http.Error(w, "dial: "+err.Error(), 502)
		rec.Status, rec.Err = 502, err.Error()
		rec.MsTotal = time.Since(start).Milliseconds()
		h.col.Observe(rec)
		return
	}
	conn := l.conn
	rec.Route = l.routeID
	rec.RouteReason = l.reason
	// 复用连接先探活。服务端重启后池里的连接已经死了，但**写进去不会立刻报错**：
	// 数据进了内核缓冲，接着读就等到 QUIC 的 MaxIdleTimeout（实测 120 秒）才失败。
	// 用户看到的是「本地客户端卡死/闪退」，而这本来一个 ping 就能判定。
	// 只在闲置较久（probeIdleGap）时才探，刚用完的连接必活，不必付这次 RTT。
	if conn.Reused() && conn.IdleFor() > probeIdleGap {
		if perr := pingConn(conn); perr != nil {
			h.logf("线路 %q 池里的连接已失效（%v），换新连接", l.routeID, perr)
			h.release(l, 0, 0, false)
			// 换连接时重新择路：刚失效的那条线路会被降权，可能换到另一条
			if l, err = h.getConn(r.Context(), estWire); err != nil {
				http.Error(w, "dial: "+err.Error(), 502)
				rec.Status, rec.Err = 502, err.Error()
				rec.MsTotal = time.Since(start).Milliseconds()
				h.col.Observe(rec)
				return
			}
			conn = l.conn
			rec.Route = l.routeID
			rec.RouteReason = l.reason
		}
	}

	// ---- 选择编码 ----
	enc := meta.EncIdentity
	encName := "identity"
	var payload []byte
	var baseHash codec.Hash
	// baseBody 记录本次 delta 实际使用的基线：重发到「另一个协议版本」
	// 的连接时要按新连接重编（dcz2 ↔ 私有 dcz），不能原样重发旧帧。
	var baseBody []byte
	// dcz2 = 这条连接协商到了 ALPNv2（双方都支持 RFC 9842 §5 的 dcz2）。
	// v1 连接上继续用私有 dcz —— 线上老服务端只认它。
	dcz2 := conn.SupportsDcz2()

	encStart := time.Now()
	switch {
	case len(body) < h.cfg.MinDelta:
		// 小请求：直接 zstd，不折腾 delta
		if len(body) > 1024 {
			z, err := codec.Compress(body, 19, nil)
			if err == nil && len(z) < len(body) {
				payload, enc = z, meta.EncZstd
				encName = "zstd"
			}
		}
		if payload == nil {
			payload = body
		}
	default:
		// 大请求：尝试 delta
		bh, bb := h.finder.Pick(body)
		if !bh.IsZero() {
			lvl := codec.PickLevel(len(body))
			var (
				z    []byte
				derr error
			)
			if dcz2 {
				// RFC 9842 §5：40 字节头里放 SHA-256(基线)，zstd 帧不写字典 ID
				z, derr = codec.EncodeDcz2(body, lvl, bb)
			} else {
				z, derr = codec.EncodeDcz(body, bh, lvl, bb)
			}
			if derr == nil {
				payload, baseHash, baseBody, enc = z, bh, bb, meta.EncDcz
				encName = fmt.Sprintf("dcz(L%d)", lvl)
				if dcz2 {
					enc, encName = meta.EncDcz2, fmt.Sprintf("dcz2(L%d)", lvl)
				}
				rec.BaseHit = true
			}
		}
		if payload == nil {
			// 无合适基线 → 普通 zstd
			lvl := codec.PickLevel(len(body))
			if z, err := codec.Compress(body, lvl, nil); err == nil && len(z) < len(body) {
				payload, enc = z, meta.EncZstd
				encName = fmt.Sprintf("zstd(L%d)", lvl)
			} else {
				payload = body
			}
		}
	}
	rec.MsEncode = time.Since(encStart).Milliseconds()
	rec.Encoding = encName
	rec.UpWire = int64(len(payload))

	// 记住这份 body 供下次当基线
	h.finder.Remember(body)

	// ---- 幂等键（同内容同键，用于断线续传与去重）----
	callerKey := idem.CallerKeyID(r.Header)
	idemKey := idem.Key(callerKey, r, body)

	// ---- 构造帧 ----
	headers := meta.EncodeHeaders(r.Header)
	// Path 带 query（RequestURI）：上游据此还原 URL，幂等键也据此区分
	// 不同 query 的同一路径。旧实现只带 r.URL.Path，`?beta=true` 到上游
	// 就没了（2026-11-16 由透传测试钉住后修复）。
	reqHead := &meta.RequestHead{
		Method: r.Method, Path: r.URL.RequestURI(),
		BaseHash: convertHash(baseHash), ResultHash: convertHash(codec.Sum(body)),
		Encoding: enc, BodyLen: int64(len(payload)),
		IdemKey:    convertHash(idemKeyToCodec(idemKey)),
		ResumeFrom: 0,
		// 声明能解压压缩的下行响应。服务端据此决定是否压缩；
		// 不发这一位的老客户端会被服务端当作「不支持」而退回明文。
		AcceptDownZstd: true,
	}
	reqPayload, err := meta.EncodeRequest(reqHead, headers, payload)
	if err != nil {
		// 一个字节都没写出去，连接是干净的：回池，别浪费一次握手。
		// 样本记成功但零字节——Model.Observe 会因 bytes<=0 忽略它，
		// 所以这里给 0 不会污染拟合，同时连接仍被复用。
		h.release(l, 0, 0, true)
		http.Error(w, "encode: "+err.Error(), 500)
		rec.Status, rec.Err = 500, err.Error()
		h.col.Observe(rec)
		return
	}
	frame, err := meta.WrapFrame(meta.FrameRequest, reqPayload)
	if err != nil {
		h.release(l, 0, 0, true)
		http.Error(w, "wrap frame: "+err.Error(), 500)
		rec.Status, rec.Err = 500, err.Error()
		h.col.Observe(rec)
		return
	}

	// ---- 流式传输 + 断线续传 ----
	wireStart := time.Now()
	stream := &clientstream.Session{IdemKey: idemKey}

	if err := transport.WriteFrame(conn, frame); err != nil {
		h.pools.Drop(conn)
		// 池里的连接可能在「探活之后、写之前」的窗口里死掉（这个窗口在
		// 按连接 ALPN 选编码之后变宽了一个编码耗时，见 dcz2 协商）：换一条
		// 新连接重发一次。服务端按幂等键去重，重发不会让上游被请求两次。
		//
		// 关键：dcz2 与私有 dcz **绑定连接版本**，新连接的 ALPN 未必与旧
		// 连接相同（-dcz2=false 回滚窗口、同地址混版本后端）。原样重发会把
		// dcz2 发给只认私有 dcz 的服务端，得到 502 unknown encoding —— 而
		// 这正是重发本该救回的那次请求（2026-11-17 复核实证 5/5 必现）。
		// 所以版本不一致时必须按新连接重编帧。
		retried := false
		if conn.Reused() {
			// 这条线路刚失败 → 记一次失败（降权），并重新择路换连接
			if l.cand != nil {
				l.cand.Model.ObserveFailure()
			}
			// 重新择路的体积口径用编码后的实际 payload 长度，与首次一致
			// （用明文会让前后两次择路用两把尺子，降权判断跟着失真）。
			if l2, e := h.getConn(r.Context(), int64(len(payload))); e == nil {
				c2 := l2.conn
				// 重试连接也要探活（对抗审查 P3-3a）：服务端重启后池里可能
				// 全是死连接，不 ping 一遍就只能再失败一次 502——而一次探活
				// 本来就能把它救回来。窗口虽窄，代价也小（只有重试路径付）。
				//
				// 探活失败时放弃重试（而不是 goto——那会让这条本该简单的
				// 重试路径变成跳转迷宫）。
				probeOK := true
				if c2.Reused() && c2.IdleFor() > probeIdleGap {
					if perr := pingConn(c2); perr != nil {
						h.logf("重试连接也已失效（%v），放弃重试", perr)
						h.release(l2, 0, 0, false)
						probeOK = false
					}
				}
				if !probeOK {
					// 跳过重试：走下面的 !retried 分支给出 502
				} else {
					f2 := frame
					// 唯一**不兼容**的方向是 dcz2 → v1 连接（v1 服务端只认
					// 私有 dcz，会回 unknown encoding）；反向（dcz → v2 连接）
					// 兼容，v2 服务端两种都解，不必重编。
					if enc == meta.EncDcz2 && !c2.SupportsDcz2() && baseBody != nil {
						lvl := codec.PickLevel(len(body))
						if z2, derr := codec.EncodeDcz(body, baseHash, lvl, baseBody); derr == nil {
							reqHead.Encoding = meta.EncDcz
							reqHead.BodyLen = int64(len(z2))
							if pl, e1 := meta.EncodeRequest(reqHead, headers, z2); e1 == nil {
								if f3, e2 := meta.WrapFrame(meta.FrameRequest, pl); e2 == nil {
									f2, payload = f3, z2
									rec.Encoding = fmt.Sprintf("dcz(L%d)", lvl)
									rec.UpWire = int64(len(z2))
								}
							}
						}
					}
					if e2 := transport.WriteFrame(c2, f2); e2 == nil {
						conn, retried = c2, true
						l = l2 // 后续归还/样本都归到新线路
						rec.Route = l2.routeID
						rec.RouteReason = l2.reason
						// 计时基线重置：前面的失败尝试发生在**别的线路上**，
						// 把它的耗时算进新线路的样本会凭空抬高该线路的 est。
						// （请求总耗时 rec.MsTotal 仍从 start 起算，不受影响。）
						wireStart = time.Now()
					} else {
						if l2.cand != nil {
							l2.cand.Model.ObserveFailure()
						}
						h.pools.Drop(c2)
					}
				} // if l2 != nil
			}
		}
		if !retried {
			http.Error(w, "send: "+err.Error(), 502)
			rec.Status, rec.Err = 502, err.Error()
			rec.MsTotal = time.Since(start).Milliseconds()
			h.col.Observe(rec)
			return
		}
		h.logf("连接写入失败（%v），已换线路 %q 重发", err, l.routeID)
	}
	clientstream.Drain(conn, w, stream, start)
	// 归还策略：只有「响应完整读完、且连接没出错」才进池。
	// Drain 中途出错时连接里可能还有未读帧，复用它会让下一个请求读到
	// 上一个请求的残留数据（串台），所以一律丢弃。
	// 归还策略（含 FrameDeltaMiss 的特判）：
	//
	// DeltaMiss 是「基线不在对端 server」的**协议事件**，线路本身完全健康
	// （服务端 main.go 明确注释：miss 之后连接状态是干净的、可复用）。
	// 若把它当作线路失败，跨 server 切换（relay 混搭）每次都会给新线路记
	// 一次假失败：降权 1.5×、污染成功率、还要白付一次握手。
	//
	// 所以：miss 时不记失败、也不记成功样本（这次没传成完整请求），
	// 但连接可以进池（服务端语义支持）。
	deltaMiss := stream.ResumeMiss
	reusable := (stream.Completed || deltaMiss) && stream.Err == nil
	h.releaseWith(l, int64(len(payload))+stream.WireBytes, time.Since(wireStart),
		reusable, !deltaMiss)

	// DeltaMiss / 断线 → 按需要重发
	if stream.ResumeMiss {
		// 服务端明确说「没有这份基线」→ 从本地 finder 里丢掉它。
		//
		// 真实 e2e（跨 server 线路）抓出的缺陷：不丢掉的话，下一次请求还会
		// 选中同一份基线、再吃一次 miss、再全量重发——形成「每次都 miss、
		// 每次都全量」的稳定劣化。基线是 server 侧状态，跨 server 必然不一致；
		// 丢掉让客户端下一轮重新挑（大概率挑到对端也有的那份）。
		//
		// 但只有当「这条基线**不是**刚由本线路的 server 确认过的」才丢。
		// 判据：本次请求走的是哪台 server —— 若与基线记录时的 server 不同，
		// 那丢掉是对的（那台机器没有它）；若是同一台，说明是 store 淘汰，
		// 丢掉会让下一次请求必然 miss（实测单 server 会因此陷入
		// 「zstd → resend-full → zstd → …」的永久振荡）。
		//
		// 现状实现：只在本连接的 server 与基线来源不同时丢。client 侧没有
		// 「基线来自哪台 server」的记录，所以改用更保守的判据——只在
		// **存在多条线路且各自指向不同 server** 时才丢弃。
		if !baseHash.IsZero() && h.multiServer() {
			if h.finder.Forget(baseHash) {
				h.logf("服务端无此基线，已从本地遗忘（跨 server 常态）")
			}
		}
		// 服务端无该 key（store 淘汰或首发失败）→ 全量重发一次
		z, cerr := codec.Compress(body, codec.PickLevel(len(body)), nil)
		if cerr != nil || len(z) >= len(body) {
			z = body
			reqHead.Encoding = meta.EncIdentity
		} else {
			reqHead.Encoding = meta.EncZstd
		}
		reqHead.BaseHash = meta.Hash{}
		reqHead.BodyLen = int64(len(z))
		pl2, e1 := meta.EncodeRequest(reqHead, headers, z)
		if e1 == nil {
			f2, e2 := meta.WrapFrame(meta.FrameRequest, pl2)
			if e2 == nil {
				// 重发时重新择路：这次的体积是 z（全量），与首次的 payload
				// 不同，正好让评分更准。失败的那条线路刚被降权，可能换到别的。
				if l2, e3 := h.getConn(r.Context(), int64(len(z))); e3 == nil {
					if e4 := transport.WriteFrame(l2.conn, f2); e4 != nil {
						if l2.cand != nil {
							l2.cand.Model.ObserveFailure()
						}
						h.pools.Drop(l2.conn)
					} else {
						clientstream.Drain(l2.conn, w, stream, start)
						ok := stream.Completed && stream.Err == nil
						h.release(l2, int64(len(z))+stream.WireBytes,
							time.Since(wireStart), ok)
					}
				}
			}
		}
		rec.Encoding = "resend-full"
	}

	// 连接中断但上游未完成 → 尝试续传。
	// 关键：若下游（HTTP 客户端）已断开，续传毫无意义——数据没人接收。
	if isDownstreamGone(stream.Err) {
		h.logf("下游已断开，停止续传 key=%x（条目保留在服务端幂等区）", idemKey[:4])
	}
	// 只有「可恢复的中断」才续传：已完成 / 服务端判定失败 / 下游已走 都不续传
	for att := 0; att < h.cfg.MaxResume && !stream.Completed && !stream.Fatal &&
		stream.Err != nil && !stream.ResumeMiss && !isDownstreamGone(stream.Err); att++ {
		h.logf("断线续传 #%d：key=%x from=%d event=%q err=%v",
			att+1, idemKey[:4], stream.Bytes, stream.EndEventID, stream.Err)
		time.Sleep(time.Duration(att+1) * 300 * time.Millisecond)

		rf, e := clientstream.BuildResumeFrame(idemKey, stream.Bytes, stream.EndEventID)
		if e != nil {
			break
		}
		// 续传重连不走连接池：服务端对 FrameResume 明确不复用连接
		//（见 cmd/server/main.go 里的注释），所以这里也不必把连接放回池。
		//
		// 关键（对抗审查 P2-4）：必须回到**本请求实际使用的线路**。
		// 幂等区（spool）按 server 进程隔离，若续传发往另一台 server，
		// 幂等键必然 miss → 用户看到误导性的「连续两次未命中幂等键」，
		// 真实原因是「续传发错了机器」。线路自己 dial 失败时才回退到
		// 默认 dialer（保底，不让续传因线路问题彻底失效）。
		dialer := h.dialer
		if d, ok := h.pools.Dialer(l.routeID); ok {
			dialer = d
		}
		c3, e := dialer.Dial(r.Context())
		if e != nil {
			continue
		}
		stream.Err = nil
		if e := transport.WriteFrame(c3, rf); e != nil {
			c3.Close()
			stream.Err = e
			continue
		}
		clientstream.Drain(c3, w, stream, start)
		c3.Close()
		if stream.ResumeMiss || stream.Completed || stream.Fatal {
			stream.Err = nil
			break
		}
	}

	// 收尾兜底：一个字节都没写给下游时，必须补一个真实的 5xx。
	//
	// 两个来源会走到这里：
	//  1. 服务端在响应头产生前就失败（上游不可达/超时），发来 FrameError；
	//  2. 幂等键连续两次未命中（delta-miss → 全量重发 → 再 miss）。
	// 原实现直接返回，net/http 会替我们补一个「200 + 空 body」，调用方只能
	// 看到「空返回」——2026-11-16 上游端口配成 down 槽时，全部请求都变成
	// 这个现象，连错误原因都传不出去。
	finalizeNoHead(w, stream)

	rec.MsWire = time.Since(wireStart).Milliseconds()
	// transport 取值必须来自**本次实际使用的连接**，不能用全局 dialer 的
	// LastKind：多线路下 LastKind 是「最近一次任意线路成功的传输方式」，
	// 与这条请求无关（A 走 QUIC、B 走 TCP 时会串）。
	rec.Transport = string(conn.Kind())
	// 握手耗时：只有本次请求真的新建了连接才有值。复用时恒为 0，
	// 这正是「池在起作用」的度量——若这里经常非零，说明池没命中，
	// 每个请求都在付实测约 2 秒的握手钱。
	if !conn.Reused() {
		rec.MsHandshake = conn.Handshake().Milliseconds()
	}
	rec.Status = stream.Status
	rec.DownRaw = stream.Bytes
	// down_wire 是隧道实际承载量（含帧头/续片头），不是 body 长度。
	// 流式场景下每 32KiB 续片都带 17 字节帧头，两者必然不等。
	rec.DownWire = stream.WireBytes
	if rec.DownWire == 0 && stream.Bytes > 0 {
		rec.DownWire = stream.Bytes // 兜底：极早期版本路径
	}
	rec.MsTotal = time.Since(start).Milliseconds()
	// TTFB：首个 body 字节抵达下游的时刻（流式路径原先恒为 0）
	rec.MsUpstrm = stream.TTFB.Milliseconds()
	if stream.Err != nil && !stream.Completed {
		rec.Err = stream.Err.Error()
	}
	if rec.Status == 0 {
		rec.Status = 200
	}

	if d := float64(rec.MsWire) / 1000; d > 0 {
		rec.UpBps = float64(rec.UpWire) / d
	}
	if secs := float64(rec.MsTotal) / 1000; secs > 0 {
		rec.DownBps = float64(stream.Bytes) / secs
	}
	h.col.Observe(rec)
}

// finalizeNoHead 在「一个响应头都没收到就结束」时给下游补一个真实 5xx。
//
// 返回 true 表示它写了响应。两种失败会走到这里：服务端在头产生前就发来
// FrameError（上游不可达、超时），或者幂等键连续两次未命中。若什么都不写，
// net/http 会在 Handler 返回时自动补 200 + 空 body —— 调用方只能看到
// 「空返回」，真实原因全部丢失。
func finalizeNoHead(w http.ResponseWriter, s *clientstream.Session) bool {
	if s.HeadDone || s.Completed {
		return false
	}
	msg := "隧道没有返回响应头（上游不可达或幂等键未命中）"
	switch {
	case s.Err != nil:
		msg = s.Err.Error()
	case s.ResumeMiss:
		msg = "服务端连续两次未命中幂等键（全量重发也未成功），未收到响应头"
	}
	http.Error(w, msg, http.StatusBadGateway)
	s.Status = http.StatusBadGateway
	if s.Err == nil {
		s.Err = errors.New(msg)
	}
	return true
}

// isDownstreamGone 判断错误是否来自「写下游失败」（客户端已断开）。
// 这类错误不应触发续传：下游没人接收，重连只会白等上游。
// 条目会由服务端在 Detach 后保留在幂等区，供客户端**重新发起请求**时复用。
func isDownstreamGone(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, pat := range []string{
		"broken pipe", "connection reset by peer", "write: ",
		"use of closed network connection",
	} {
		if strings.Contains(msg, pat) {
			return true
		}
	}
	return false
}

// idemKeyToCodec [32]byte → codec.Hash（类型别名转换）
func idemKeyToCodec(k [32]byte) codec.Hash {
	var out codec.Hash
	copy(out[:], k[:])
	return out
}

// ---- 哈希/编码类型转换（meta 与 codec 各自定义，避免包依赖倒置） ----

func convertHash(h codec.Hash) meta.Hash {
	var out meta.Hash
	copy(out[:], h[:])
	return out
}

// ── 连接探活 ──────────────────────────────────────────────────────────────

// probeIdleGap 是「值得先 ping 一下」的闲置时长。
//
// 取 5 秒而不是 0：刚归还的连接一定是活的，探活等于白付一个 RTT；而服务端
// 重启这种事故，间隔通常远大于 5 秒。
const probeIdleGap = 5 * time.Second

// probeTimeout 是探活的读超时。正常 RTT 到香港约 3ms，2.5 秒足够覆盖抖动，
// 又不会让用户等太久——探活失败意味着这条连接已经废了。
const probeTimeout = 2500 * time.Millisecond

// pingConn 在一条空闲连接上做一次 ping/pong。
//
// 服务端对 FramePing 原样回显（cmd/server/main.go 的 serveOne），所以
// 「设一个短读超时 → 发 ping → 读到任意一帧」就等于「链路还活着」。
// 读超时必须在这里设、并且用完清掉：正常请求路径不能有读超时，LLM 首帧
// 可能等 30–50 秒，固定超时会把合法请求误杀。
func pingConn(c *transport.Conn) error {
	deadline := time.Now().Add(probeTimeout)
	// 读**和**写都要设：服务端被 kill 之后，QUIC 流的 Write 会等不到 ACK
	// 一直挂着（实测 40 秒无返回），只设读超时根本走不到读那一步。
	if err := c.SetReadDeadline(deadline); err != nil {
		return err
	}
	if err := c.SetWriteDeadline(deadline); err != nil {
		return err
	}
	defer func() {
		_ = c.SetReadDeadline(time.Time{})
		_ = c.SetWriteDeadline(time.Time{})
	}()

	frame, err := meta.WrapFrame(meta.FramePing, nil)
	if err != nil {
		return err
	}
	if err := transport.WriteFrame(c, frame); err != nil {
		return err
	}
	_, err = c.Recv()
	return err
}

// superviseServe 守住本地监听：Serve 意外返回时自动重开，而不是让「运行中」
// 变成一个没人服务的空壳。
//
// 为什么需要（2026-11-14 owner：「可以 let it crash，但关键是要有自恢复」）：
//   - 单个请求里的 panic 由 net/http 自己 recover，进程不受影响；
//   - 但 accept 循环出错（文件描述符耗尽、瞬时 EMFILE）会让 Serve 返回，
//     旧实现只是把 running 置 false —— 用户看到「已停止」，服务就此下线，
//     除非他自己点一次启动；
//   - 真出了未捕获 panic，进程会死，那由 launchd 的 KeepAlive 兜（见
//     internal/autostart：SuccessfulExit=false，崩溃才拉起）。
//
// 这里只负责「同一个进程内把监听重新拉起来」，退避 200ms 起、上限 5s，
// 避免端口被别的进程占着时把 CPU 打满。
func (r *Runner) superviseServe(h *handler, srv *http.Server, ln net.Listener, addr string, logger *log.Logger) {
	backoff := 200 * time.Millisecond
	for {
		err, panicked := serveSafely(srv, ln)
		if !r.isRunning() || err == http.ErrServerClosed {
			return
		}
		r.mu.Lock()
		r.lastErr = fmt.Errorf("serve: %w", err)
		r.mu.Unlock()
		if logger != nil {
			if panicked {
				logger.Printf("监听 goroutine panic: %v（%s 后重开）\n%s", err, backoff, debug.Stack())
			} else {
				logger.Printf("监听异常退出: %v（%s 后重开）", err, backoff)
			}
		}
		r.notify()

		time.Sleep(backoff)
		if !r.isRunning() {
			return
		}
		nln, e := net.Listen("tcp", addr)
		if e != nil {
			if logger != nil {
				logger.Printf("重开监听失败: %v", e)
			}
			if backoff < 5*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = 200 * time.Millisecond
		ln = nln
		srv = &http.Server{
			Handler:           h,
			ReadHeaderTimeout: 30 * time.Second,
		}
		r.mu.Lock()
		r.ln, r.srv = nln, srv
		r.mu.Unlock()
	}
}

// serveSafely 跑一轮 Serve，并把 panic 变成普通错误。
//
// 单独抽出来是因为「panic 也要重开」：恢复后直接 return 等于把服务停掉，
// 而 owner 的要求是自恢复——崩溃可以发生，但服务得自己回来。
func serveSafely(srv *http.Server, ln net.Listener) (err error, panicked bool) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
			panicked = true
		}
	}()
	return srv.Serve(ln), false
}

// isRunning 供 superviseServe 读运行态（持锁读，避免与 Stop 竞争）。
func (r *Runner) isRunning() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running
}
