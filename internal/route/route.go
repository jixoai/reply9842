// Package route — 多线路（线路 = 一条到 relay-server 的出站路径）的建模、评分与择路。
//
// 正交意图（3 个）：
//  1. Route：线路的身份与出站参数（direct / proxy / relay 三类的唯一差别只是「怎么打到 server」）
//  2. Model：用真实请求样本拟合「固定开销 + 字节/带宽」两参数吞吐模型
//  3. Picker：按**预计完成时间**择路，失败自动降权，但永不彻底排除（保底）
//
// 为什么评分必须是「预计完成时间」而不是 RTT（owner 2026-11-24 原始需求）：
//
//	「你可以这样理解，通常需要数据传输完成，本地AI才会开始工作。所以我们需要看的是
//	 最终一个请求传输完成的时间总耗时，有的延迟低，但是线路带宽也不多，最终的时间消耗
//	 反而不如延迟高一点，但是能 200kb/s 的速度传输数据的线路。」
//
// 所以本包的核心量是 est(bytes) = a + bytes/bw：
//
//	a  ≈ 一次往返的固定开销（握手/队头/TTFB 部分），单位毫秒
//	bw ≈ 线路的有效带宽，单位 字节/毫秒（= KB/s）
//
// 两条线路谁赢，取决于**本次请求的字节数**落在哪个区间——这正是「小请求低延迟赢、
// 大请求高带宽赢」的数学表达，而不是拍脑袋的固定优先级。
package route

import (
	"errors"
	"fmt"
	"net"
	"strings"
)

// Kind 线路类型。
//
// 三者的差别只有一个：**怎么把连接打到 server**。
//
//	direct  绑（可选）源地址直连 server。绕开代理 TUN 劫持的常规方式。
//	proxy   先 CONNECT/SOCKS5 到代理，再在隧道里跑 TLS。只能承载 TCP。
//	relay   直连**另一台** server。复用 direct 的全部代码路径，只是 Addr 不同。
//
// relay 之所以单独留一个 Kind 而不是让用户「填个别的 server 就当 direct」：
// 语义不同会带来不同的 UI 提示与不同的基线共享风险（跨 server 的 delta 基线
// 不共享，切换时会有一次全量重发）。把它显式化，用户才不会误以为是同一条路。
type Kind string

const (
	KindDirect Kind = "direct"
	KindProxy  Kind = "proxy"
	KindRelay  Kind = "relay"
)

// Kinds 全部合法线路类型（UI 下拉与校验共用同一份，避免两处漂移）。
var Kinds = []Kind{KindDirect, KindProxy, KindRelay}

// KindLabels 中文标签（GUI / CLI 展示共用）
var KindLabels = map[Kind]string{
	KindDirect: "直连",
	KindProxy:  "代理",
	KindRelay:  "中继",
}

// 默认值（与 relayclient 的默认保持一致；这里独立定义是为了避免包依赖倒置）。
const (
	DefaultTransport = "auto"
	// DefaultServer 不带端口，与 relayclient.DefaultServer 形态一致。
	// 端口由 DefaultServerPort 补。
	DefaultServer     = "magpie.aiweb.xin"
	DefaultServerPort = "28443"
)

// Route 一条线路。
//
// 字段的可见性刻意偏「扁平」：用户要在 GUI 里直接编辑它们，嵌套结构会让
// 「哪些字段对哪种 kind 生效」变得难讲。校验集中在 Validate 一处。
type Route struct {
	// ID 用户可见的名字，同时是日志里的 route 标识。
	// 必须非空且在同一份配置里唯一——否则日志无法回答「当时走的是哪条」。
	ID string `json:"id"`

	Kind Kind `json:"kind"`

	// ServerName 该线路的 TLS SNI（证书上签的域名）。
	//
	// 为什么需要它（实测抓出的缺口）：中继线路常指向**另一台 IP**，但复用
	// 同一张证书（证书 SAN 只有域名）。此时若不显式给 SNI，transport 会按
	// Addr 的 host 推导 → 用 IP 去校验 → 必然
	// `x509: cannot validate certificate for <ip> ... no IP SANs`。
	//
	// 留空时由 specsFor 按既有规则推导（同主机继承全局、异主机按 Addr）。
	ServerName string `json:"server_name,omitempty"`

	// Server 该线路连到哪台 relay-server。
	//
	// 允许省略端口（"magpie.aiweb.xin"）：Normalize 会补 DefaultServerPort。
	// 这与 relayclient 的默认值形态一致，避免「配置文件里必须写端口」
	// 这种与既有约定不符的额外负担。
	Server string `json:"server,omitempty"`

	// Proxy 仅 kind=proxy 生效：http/https/socks5 URL。
	Proxy string `json:"proxy,omitempty"`

	// LocalAddr 绑定的出站源地址（如 "192.168.1.106"），direct 与 relay 都适用。
	//
	// relay 为什么也要：中继只是让服务端再转发一跳，**本机到中继这一跳仍是
	// 本机发起的外连**，TUN 全局代理照样会劫持它。实测不绑源地址就是清一色
	// `tcp tls: EOF`。（曾经这里写着「relay 不提供此字段是有意的，它的意义
	// 就是不依赖本机网络」——那个判断是错的。）
	//
	// proxy 不适用：CONNECT 由代理进程发起，绑本机网卡会和代理的路由冲突。
	LocalAddr string `json:"local_addr,omitempty"`

	// Transport auto | quic | tcp。空串按 DefaultTransport。
	Transport string `json:"transport,omitempty"`

	// Enabled 启停开关。false 时该线路不参与择路，但配置保留。
	Enabled *bool `json:"enabled,omitempty"`
}

// IsEnabled 默认启用（Enabled 为 nil 时视为 true）。
//
// 用指针而不是 bool 的理由：老配置里没有这个字段，反序列化得零值 false，
// 那会把所有线路误判为「已停用」。指针能区分「没配过」与「配过且关闭」。
func (r Route) IsEnabled() bool { return r.Enabled == nil || *r.Enabled }

// SetEnabled 返回一个设定了启停状态的副本（值语义，避免调用方踩共享指针）。
func (r Route) SetEnabled(on bool) Route {
	enabled := on
	r.Enabled = &enabled
	return r
}

// Normalize 补齐默认值并去空白。幂等。
func (r Route) Normalize() Route {
	r.ID = strings.TrimSpace(r.ID)
	r.Server = strings.TrimSpace(r.Server)
	r.ServerName = strings.TrimSpace(r.ServerName)
	r.Proxy = strings.TrimSpace(r.Proxy)
	r.LocalAddr = strings.TrimSpace(r.LocalAddr)
	r.Transport = strings.ToLower(strings.TrimSpace(r.Transport))

	if r.Kind == "" {
		r.Kind = KindDirect
	}
	// 空 server 不被静默填默认值（规格审查 F10）。
	//
	// 早期实现无条件补 DefaultServer，于是 GUI 里「新增一条中继线路」
	// （刻意留空 server 以提示「请填另一台服务器」）在保存时被补成与其它
	// 线路相同的 server——中继变成「指向同一台机器的线路」，而「基线不共享」
	// 提示也随之消失。用户以为加了中继，实际什么都没变。
	//
	// 现在只对**直连/代理**补默认（它们本来就该指向同一台主服务器）；
	// relay 的空 server 保留为空，由 Validate 报「缺少 server 地址」。
	if r.Server == "" && r.Kind != KindRelay {
		r.Server = DefaultServer
	}
	// 只给主机名时补默认端口。判据：能 SplitHostPort 成功就不动；
	// 失败且「不含冒号」才认为是裸主机名（含冒号的非法值留给 Validate 报，
	// 不能在这里瞎修——"h:badport" 该报错，不该被补成 "h:badport:28443"）。
	//
	// 空串必须单独挡住：它也不含冒号，JoinHostPort("") 会产出 ":28443"
	// （空主机名），于是一条「本来就缺 server 的 relay 线路」会被伪装成
	// 「有 server」，Validate 再也报不出来（规格审查 F10 的连带缺陷）。
	if r.Server != "" {
		if _, _, err := net.SplitHostPort(r.Server); err != nil && !strings.Contains(r.Server, ":") {
			r.Server = net.JoinHostPort(r.Server, DefaultServerPort)
		}
	}
	if r.Transport == "" {
		r.Transport = DefaultTransport
	}
	// 代理只承载 TCP：代理线路上的 quic 是必然失败的组合。
	//
	// **不在这里静默改正**（规格审查 F2）：spec 承诺「建连前即报出可读错误」，
	// 而 Normalize 静默改掉会让那句错误在生产路径上永不可达——用户填了 quic
	// 却不知道它被改过，排查时看到的是「我明明配了 quic」。改由 Validate
	// 明确报错，用户在保存/启动时就看到原因。
	return r
}

// Validate 校验线路自身是否自洽。
//
// 刻意不检查「能不能连上」——那是运行时的事，配置阶段只判形态。
func (r Route) Validate() error {
	if r.ID == "" {
		return errors.New("route: 线路缺少名称")
	}
	switch r.Kind {
	case KindDirect, KindProxy, KindRelay:
	default:
		return fmt.Errorf("route: 线路 %q 的类型 %q 非法（可选 direct|proxy|relay）", r.ID, r.Kind)
	}
	if r.Server == "" {
		return fmt.Errorf("route: 线路 %q 缺少 server 地址", r.ID)
	}
	if _, _, err := net.SplitHostPort(r.Server); err != nil {
		return fmt.Errorf("route: 线路 %q 的 server %q 不是 host:port: %w", r.ID, r.Server, err)
	}
	// SNI 若显式给出，必须是域名而不是 IP——用 IP 当 SNI 等于没给
	// （证书校验按 SNI 匹配，IP 只会让 x509 校验必然失败）。
	if r.ServerName != "" && net.ParseIP(strings.Trim(r.ServerName, "[]")) != nil {
		return fmt.Errorf("route: 线路 %q 的 server_name %q 是 IP，应填证书上的域名", r.ID, r.ServerName)
	}
	if r.Kind == KindProxy {
		if r.Proxy == "" {
			return fmt.Errorf("route: 线路 %q 是代理类型，但没填代理地址", r.ID)
		}
		if !hasProxyScheme(r.Proxy) {
			return fmt.Errorf("route: 线路 %q 的代理地址 %q 缺少协议头（http:// / https:// / socks5://）", r.ID, r.Proxy)
		}
	}
	switch r.Transport {
	case "", "auto", "quic", "tcp":
	default:
		return fmt.Errorf("route: 线路 %q 的传输方式 %q 非法（可选 auto|quic|tcp）", r.ID, r.Transport)
	}
	// 代理只承载 TCP（CONNECT/SOCKS5 都是 TCP 隧道），QUIC 是 UDP。
	// transport 层在 Dial 时也会拦一次（双重防线：配置期就该报出来）。
	if r.Kind == KindProxy && r.Transport == "quic" {
		return fmt.Errorf("route: 线路 %q 是代理类型，不支持 QUIC（代理只承载 TCP）", r.ID)
	}

	// 字段与 kind 的交叉校验（规格审查 F9）：kind 不只是标签，它决定哪些字段生效。
	// 不校验的后果是「direct 线路填了 proxy URL 会静默走代理」——用户以为在
	// 直连，实际经了代理，排查时完全看不出。
	switch r.Kind {
	case KindDirect:
		if r.Proxy != "" {
			return fmt.Errorf("route: 线路 %q 是直连类型，但填了代理地址 %q（改类型为 proxy，或清空它）", r.ID, r.Proxy)
		}
	case KindRelay:
		// 中继不额外套代理（要「代理到中继」请直接用 proxy 类型把 server 指过去，
		// 不必叠两层语义）。但**源地址照填**：中继只是服务端再转发一跳，本机到
		// 中继这一跳仍是本机发起的外连，TUN 全局模式照样劫持——曾因这条错误
		// 限制，中继线路永远拿不到源地址绑定，实测全程 tls: EOF。
		if r.Proxy != "" {
			return fmt.Errorf("route: 线路 %q 是中继类型，不该填代理地址 %q（要经代理到中继请改用 proxy 类型）", r.ID, r.Proxy)
		}
	}
	return nil
}

func hasProxyScheme(s string) bool {
	for _, p := range []string{"http://", "https://", "socks5://", "socks5h://"} {
		if strings.HasPrefix(strings.ToLower(s), p) {
			return true
		}
	}
	return false
}

// Set 一份线路配置（有序，顺序即用户设定的优先级参考）。
//
// 为什么用切片而不是 map：顺序有意义（GUI 里可上下移，同时也是「评分相同
// 时的裁决顺序」），而 map 的迭代顺序是随机的——用它会让同分时的择路行为
// 无法复现，排查时看到的现象会自相矛盾。
type Set []Route

// Normalize 逐条规范化，并补默认名（未命名线路按序号命名，避免校验失败卡住启动）。
func (s Set) Normalize() Set {
	out := make(Set, 0, len(s))
	for i, r := range s {
		r = r.Normalize()
		if r.ID == "" {
			r.ID = fmt.Sprintf("线路%d", i+1)
		}
		out = append(out, r)
	}
	return out
}

// Validate 校验整份配置：**只校验启用的线路**，外加 ID 唯一。
//
// 为什么跳过停用线路（规格审查 F3）：用户临时关掉一条配错的线路，应当能
// 让客户端照常起来——「任一线路的配置错误只影响它自己」是 spec 的明确要求。
// 早期实现校验全量，于是停用线路的错误也会拦死整份配置，用户只能二选一：
// 要么删掉那条线路，要么改对它（而它本来是被停用的）。
func (s Set) Validate() error {
	if len(s) == 0 {
		return nil // 空集合由上层决定语义（合成默认线路）
	}
	seen := make(map[string]struct{}, len(s))
	for _, r := range s {
		// ID 唯一性对**全部**线路校验（含停用的）：日志里名字冲突与启停无关。
		if r.ID != "" {
			if _, dup := seen[r.ID]; dup {
				return fmt.Errorf("route: 线路名称 %q 重复（名称是日志里的唯一标识）", r.ID)
			}
			seen[r.ID] = struct{}{}
		}
		if !r.IsEnabled() {
			continue // 停用线路的形态错误不该拦住其它线路
		}
		if err := r.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// Enabled 返回启用的子集（保持原顺序）。
func (s Set) Enabled() Set {
	out := make(Set, 0, len(s))
	for _, r := range s {
		if r.IsEnabled() {
			out = append(out, r)
		}
	}
	return out
}

// Find 按 ID 查找。
func (s Set) Find(id string) (Route, bool) {
	for _, r := range s {
		if r.ID == id {
			return r, true
		}
	}
	return Route{}, false
}

// Servers 返回本组线路涉及的 server 集合。
//
// 用途：判断「是否存在跨 server 线路」。跨 server 时 delta 基线不共享，
// 切换会触发一次全量重发（FrameDeltaMiss → 客户端重发），需要向用户可见。
func (s Set) Servers() []string {
	seen := map[string]struct{}{}
	var out []string
	for _, r := range s {
		srv := r.Normalize().Server
		if _, ok := seen[srv]; ok {
			continue
		}
		seen[srv] = struct{}{}
		out = append(out, srv)
	}
	return out
}
