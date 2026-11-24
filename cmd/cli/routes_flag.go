// routes_flag.go — CLI 侧的线路参数解析
//
// 正交意图（2 个）：
//  1. `-route` 可重复（顺序即优先级），格式 `名称:类型:server[:代理或源地址]`
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
// 格式：`名称:类型:server[:第四段]`
//
//	直连  港直连:direct:magpie.aiweb.xin:28443
//	代理  港代理:proxy:magpie.aiweb.xin:28443:http://127.0.0.1:7890
//	中继  备用:relay:118.26.39.57:28443
//
// 第四段的语义按类型而定：proxy 是代理 URL，direct 是绑定的源地址。
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
			r.Proxy = extra
		case route.KindDirect:
			r.LocalAddr = extra
		case route.KindRelay:
			// relay 的第四段无意义（它没有代理，也不绑源地址）。
			// 不静默忽略：直接报错，免得用户以为它生效了。
			return fmt.Errorf("线路 %q 是中继类型，不支持第四段参数 %q（中继只用 server）", id, extra)
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
