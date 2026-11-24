// routes_flag.go — CLI 侧的线路参数解析
//
// 正交意图（2 个）：
//  1. `-route` 可重复（顺序即优先级），格式
//     `名称:类型:server[:代理或源地址[:证书域名]]`
//  2. `-routes-file` 读一份 JSON（与 settings.json 的 routes 段同构），
//     供复杂配置（多条代理、带认证的 URL）直接搬运
//
// 为什么不只提供 JSON：手敲 JSON 在命令行里是酷刑，而「加一条代理线路」
// 是最高频的操作。给一个紧凑的单行格式，日常够用；复杂情形退到 JSON。
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/jixoai/reply9842/internal/route"
)

// routeList 实现 flag.Value，使 -route 可以出现多次。
type routeList struct {
	routes []route.Route
}

// String 供 flag 包打印默认值
func (rl *routeList) String() string {
	if rl == nil || len(rl.routes) == 0 {
		return ""
	}
	var parts []string
	for _, r := range rl.routes {
		parts = append(parts, r.ID+":"+string(r.Kind)+":"+r.Server)
	}
	return strings.Join(parts, " ")
}

// Set 解析一次 -route 的取值。
//
// 格式：`名称:类型:server[:第四段[:第五段]]`
//
//	直连  港直连:direct:magpie.aiweb.xin:28443
//	代理  港代理:proxy:magpie.aiweb.xin:28443:http://127.0.0.1:7890
//	中继  备用:relay:47.117.152.3:28443:192.168.1.106:magpie.aiweb.xin
//	      └ 服务器 ─────────────┘ └ 源地址 ──┘ └ 证书域名 ─┘
//
// 各段语义按类型而定：
//
//	kind   第四段        第五段
//	proxy  代理 URL      无（URL 自身含冒号，吃下全部剩余）
//	direct 绑定的源地址  无
//	relay  绑定的源地址  证书域名（SNI）
//
// 中继指向 IP 时必须给第五段：证书 SAN 通常只有域名，用 IP 校验必然
// `x509: cannot validate certificate for <ip> ... no IP SANs`。
// （曾经这里直接拒收中继的第四段，理由写的是「中继不绑源地址」——错的：
// 本机到中继那一跳仍是本机外连，TUN 全局代理照样劫持它。）
//
// 解析策略：**先按前两段切**（名称、类型都必不含冒号），剩下的整体作为
// 「server 与可选第四段」再切。这样做是因为代理 URL 自身含冒号
// （http://host:port），用固定段数切会被 URL 里的冒号带偏。
func (rl *routeList) Set(v string) error {
	v = strings.TrimSpace(v)
	// 段 1：名称
	i := strings.Index(v, ":")
	if i <= 0 {
		return fmt.Errorf("线路格式应为 名称:类型:server[:代理/源地址]，实际 %q", v)
	}
	id := strings.TrimSpace(v[:i])
	rest := v[i+1:]

	// 段 2：类型
	i = strings.Index(rest, ":")
	if i <= 0 {
		return fmt.Errorf("线路格式应为 名称:类型:server[:代理/源地址]，实际 %q", v)
	}
	kind := route.Kind(strings.TrimSpace(rest[:i]))
	rest = rest[i+1:]
	if strings.TrimSpace(rest) == "" {
		return fmt.Errorf("线路 %q 缺少 server 地址", id)
	}

	r := route.Route{ID: id, Kind: kind}

	// 段 3：server，以及可能的第四段。
	//
	// 解析策略：**先用 net.SplitHostPort 试**，它能正确处理 IPv6 方括号
	// （`[2001:db8::1]:28443`）。之前那版按冒号逐段切的做法会把 IPv6 地址
	// 切碎（对抗审查 P3-5b），host 变成 "[2001"、第四段变成 "db8::1]:28443"。
	//
	// 分四种情形：
	//   host:port                          → 干净切出，无第四段
	//   host:port:extra                    → SplitHostPort 失败（多余冒号），
	//                                        手工在 port 之后切第四段
	//   host（无端口）                      → SplitHostPort 失败，整串当 server
	//   [v6]:port / [v6]:port:extra        → 同上前两种
	host, port, splitErr := net.SplitHostPort(rest)
	server := ""
	extra := ""
	switch {
	case splitErr == nil:
		// 标准 host:port。但若 port 之后还有内容（形如 h:28443:proxy），
		// SplitHostPort 会报错而不是成功——所以这里真的没有第四段。
		server = net.JoinHostPort(host, port)
	default:
		// 失败：可能是「host」或「host:port:extra」或 IPv6 无括号。
		// 用括号感知的切法找第一段（server），其余是第四段。
		if seg, tail, ok := cutServerFrom(rest); ok {
			server = seg
			extra = tail
		} else {
			server = rest // 整串当 server，交给 Normalize/Validate 判
		}
	}
	r.Server = server

	if extra != "" {
		switch r.Kind {
		case route.KindProxy:
			// 代理 URL 自身含冒号（http://host:port），吃下全部剩余，
			// 不再按第五段切。
			r.Proxy = extra
		case route.KindDirect:
			r.LocalAddr = extra
		case route.KindRelay:
			// 中继：第四段是源地址，第五段（可选）是证书域名。
			// 只切一次——源地址是 IP/主机名，不含冒号；第五段吃剩余全部。
			loc, sni, has := cutFirst(extra)
			r.LocalAddr = loc
			if has {
				r.ServerName = strings.TrimSpace(sni)
			}
			if r.LocalAddr != "" {
				r.LocalAddr = strings.TrimSpace(r.LocalAddr)
			}
		}
	}

	rl.routes = append(rl.routes, r)
	return nil
}

// cutServerFrom 从「host[:port][:extra]」里切出 server 段，其余作为第四段。
//
// 括号感知：IPv6 字面量 `[v6]` 内部的冒号不算分隔符。这是 P3-5b 的修复点——
// 旧实现按冒号无条件切，IPv6 地址会被切碎。
func cutServerFrom(s string) (server, extra string, ok bool) {
	// 找「server 段的结束位置」：括号闭合之后的第一个冒号，或无括号时的第一个冒号
	end := -1
	if strings.HasPrefix(s, "[") {
		if close := strings.Index(s, "]"); close > 0 {
			// 括号后可能有 :port，再看其后是否还有冒号
			rest := s[close+1:]
			if strings.HasPrefix(rest, ":") {
				// 找 port 之后的冒号
				if i := strings.Index(rest[1:], ":"); i >= 0 {
					end = close + 1 + 1 + i
				} else {
					// 只有 [v6]:port，没有第四段
					return s, "", false
				}
			} else {
				// 只有 [v6]，无端口
				return s, "", false
			}
		}
	} else {
		// 无括号：找出「host:port」的边界，其后若还有内容则是第四段。
		//
		// 判据：按冒号切开后，若第二段是**纯数字**（端口），则 server 到那里
		// 为止；第三段起是第四段。若第二段不是纯数字，则整串是 server。
		first := strings.Index(s, ":")
		if first < 0 {
			return s, "", false // 无端口
		}
		second := strings.Index(s[first+1:], ":")
		if second < 0 {
			return s, "", false // 只有 host:port，无第四段
		}
		port := s[first+1 : first+1+second]
		if !isAllDigits(port) {
			// 第二段不是端口 → 整串当 server（交给 Validate 报错）
			return s, "", false
		}
		end := first + 1 + second // 第二个冒号的位置
		return s[:end], s[end+1:], true
	}
	if end < 0 {
		return s, "", false
	}
	return s[:end], s[end+1:], true
}

// cutFirst 在第一个冒号处切成两段；无冒号时返回 hasTail=false。
// （用 strings.Cut 的语义，抽出来是为了让上面的解析读起来是线性的。）
func cutFirst(s string) (head, tail string, has bool) {
	if i := strings.Index(s, ":"); i >= 0 {
		return s[:i], s[i+1:], true
	}
	return s, "", false
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// loadRoutesFile 读一份 JSON 线路配置（数组形式，与 settings.json 的 routes 同构）。
func loadRoutesFile(path string) (route.Set, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rs route.Set
	if err := json.Unmarshal(b, &rs); err != nil {
		return nil, fmt.Errorf("解析 %s: %w", path, err)
	}
	return rs.Normalize(), nil
}
