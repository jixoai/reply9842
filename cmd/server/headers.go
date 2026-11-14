package main

// headers.go —— 可重复的 -header 参数。
//
// 为什么把「Host 覆盖」泛化成自定义请求头（owner 2026-11-14）：
// 这个服务端本质上就是「一次代理请求」——目标 + 请求头就够描述它了。
// 单独一个 -uphost 只能改 Host，碰到要带 X-Api-Version、Authorization
// 之类固定头的上游就没办法；而 Host 只是众多请求头里的一个特例。
//
// 语法与 curl 一致：`-header 'Name: value'`，可重复；同名后者覆盖前者。
// 保留 -uphost 作为兼容别名（它的值先设，随后被 -header 'Host: ...' 覆盖）。

import (
	"fmt"
	"net/textproto"
	"strings"
)

type headerList []string

func (h *headerList) String() string { return strings.Join(*h, ", ") }

func (h *headerList) Set(v string) error {
	if !strings.Contains(v, ":") {
		return fmt.Errorf("header 需要 'Name: value' 形式，收到 %q", v)
	}
	name := strings.TrimSpace(v[:strings.Index(v, ":")])
	if name == "" {
		return fmt.Errorf("header 缺少名字：%q", v)
	}
	*h = append(*h, v)
	return nil
}

// headerMap 解析成 map（同名后者覆盖前者），供转发时应用。
func (c *cfg) headerMap() map[string]string {
	if len(c.headers) == 0 {
		return nil
	}
	out := make(map[string]string, len(c.headers))
	for _, raw := range c.headers {
		i := strings.Index(raw, ":")
		if i <= 0 {
			continue
		}
		name := strings.TrimSpace(raw[:i])
		val := strings.TrimSpace(raw[i+1:])
		if name == "" {
			continue
		}
		// 规范化键名：Go 的 Header.Set 本来就会 canonical 化，但 map 里留两种
		// 大小写会让遍历顺序决定谁覆盖谁（`Host` 与 `host` 各占一个键）。
		out[textproto.CanonicalMIMEHeaderKey(name)] = val
	}
	return out
}
