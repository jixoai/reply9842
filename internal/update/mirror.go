// 镜像（加速前缀）解析。
//
// 契约与 npm/lib/mirror.js **逐条等价**，三处共用（npm 下载器 / 本包 / GUI 设置页）。
// 改任一侧都必须同步其余两处；mirror_test.go 的表驱动用例就是对照测试的锚点。
//
// 规则（与 npm 侧 parseMirrorList / resolveMirrors 一一对应）：
//
//	优先级  显式 Options.Mirrors > 环境变量 REPLY9842_MIRROR > DefaultMirrors
//	格式    英文逗号分隔的前缀列表，顺序即回退顺序
//	特殊值  direct（大小写不敏感）= 直连；空串（整串 trim 后为空）= 直连
//	空项    跳过（`a,,b` → [a, b]），直连必须显式写 direct
//	无有效项 → 单个直连 [""]，不回退默认列表（尊重用户显式设置）
//	补斜杠  前缀末尾无 "/" 时补一个（拼接契约是 <prefix><full url> 直接相连）
//	合法性  非 direct 的项必须是 http/https URL，否则**报错**（带原始值）
//	去重    保持首次出现顺序
//	不改写  不做 URL 编码/重写，前缀原样参与拼接
package update

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

// DefaultMirrors 是未配置镜像时的默认回退链，顺序即优先级，末位空串 = 直连。
//
// 导出它有两个用途：GUI 设置页把默认值展示给用户（「恢复默认」按钮的语义），
// npm 侧与 Go 侧用同一份基准做一致性对照。
//
// 顺序来源：2026-11 实测，gh-proxy 系列在国内可通，官方直连留作兜底。
var DefaultMirrors = []string{
	"https://gh-proxy.com/",
	"https://ghfast.top/",
	"https://ghproxy.net/",
	"", // 空串 = 直连
}

// MirrorEnv 是覆盖镜像列表的环境变量名。
//
// 不用 npm_config_*：npm 会先吃掉自己前缀的变量并做一层 config 解析，用户从
// .npmrc 传下来的值语义会变形。REPLY9842_ 前缀无歧义。
const MirrorEnv = "REPLY9842_MIRROR"

// DirectMirror 是直连前缀（空串）。之所以用常量而不是零值字面量，是为了让
// 「这里是直连」在代码里可 grep、可读。
const DirectMirror = ""

// ResolveMirrors 解析最终生效的镜像列表。
//
// explicit 非 nil 时原样采用（仅做规范化与校验，不走环境变量）——这是 GUI
// 设置页保存的值，用户在那里改过就应该盖过一切。explicit 为 nil 时才看环境变量；
// 环境变量**未设置**返回默认列表，环境变量**设置为空**返回直连。
//
// 注意 nil 与空切片的区别：nil = 「调用方没配置」，empty slice = 「调用方明确
// 要求空列表」。后者没有意义（没有任何前缀可试），按「配置了但无有效项」处理
// 成直连，与 npm 侧行为一致。
func ResolveMirrors(explicit []string) ([]string, error) {
	if explicit != nil {
		return normalizeMirrors(explicit, "配置")
	}

	raw, ok := os.LookupEnv(MirrorEnv)
	if !ok {
		return append([]string(nil), DefaultMirrors...), nil
	}
	return ParseMirrorList(raw)
}

// ParseMirrorList 解析一行镜像配置文本（环境变量或设置页输入的原始文本）。
//
// 空文本（含纯空白）→ 直连；切分后无任何有效项 → 直连。两条都不回退默认列表，
// 因为「显式设置了这个变量」本身就是意图表达。
func ParseMirrorList(raw string) ([]string, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return []string{DirectMirror}, nil
	}
	return normalizeMirrors(strings.Split(text, ","), "环境变量 "+MirrorEnv)
}

// normalizeMirrors 是两条入口的公共实现：逐项 trim / 跳过空项 / 识别 direct /
// 补斜杠 / 校验 / 去重。source 只用于错误信息，让用户知道坏值是从哪来的。
//
// 刻意让**两条入口共用这一个函数**：显式切片与 CSV 文本如果各走一套规则，
// 「GUI 保存的值」与「环境变量里的值」就会在边角上分歧（例如 `a,,b` 在一侧
// 得到 [a,b]、另一侧得到 [a,direct,b]），而这种分歧只会在用户同时用两种
// 配置方式时才暴露，属于最难查的一类 bug。
//
// 代价（需要调用方知道）：显式切片里的空串元素**也会被跳过**，所以
// 「先直连再镜像」要写成 []string{"direct", "https://m/"}，而不是
// []string{"", "https://m/"}。这与 CSV 里必须显式写 direct 是同一条规则。
func normalizeMirrors(items []string, source string) ([]string, error) {
	out := make([]string, 0, len(items))
	seen := make(map[string]struct{}, len(items))

	for _, item := range items {
		token := strings.TrimSpace(item)
		if token == "" {
			continue // 容忍 `a,,b` 与结尾逗号
		}

		prefix := token
		if !isDirectToken(token) {
			prefix = NormalizePrefix(token)
			if err := validatePrefix(prefix, item, source); err != nil {
				return nil, err
			}
		} else {
			prefix = DirectMirror
		}

		if _, dup := seen[prefix]; dup {
			continue
		}
		seen[prefix] = struct{}{}
		out = append(out, prefix)
	}

	if len(out) == 0 {
		return []string{DirectMirror}, nil
	}
	return out, nil
}

// isDirectToken 判断一个已 trim 的 token 是否表示直连。
//
// 大小写不敏感：用户在设置页手写 "Direct" 不该被判成非法 URL。
func isDirectToken(token string) bool {
	return token == DirectMirror || strings.EqualFold(token, "direct")
}

// NormalizePrefix 给前缀补上末尾斜杠。
//
// 镜像代理的契约是 `<prefix><完整 url>` 纯字符串拼接，少一个斜杠会拼出
// `https://my.mirrorhttps://github.com/...` 这种不可解析的 URL。
// 已带斜杠的原样返回，不重复补。
func NormalizePrefix(prefix string) string {
	if prefix == DirectMirror || strings.HasSuffix(prefix, "/") {
		return prefix
	}
	return prefix + "/"
}

// validatePrefix 校验前缀是带主机名的 http(s) URL。
//
// 校验放在补斜杠**之后**：`https://m.example.com` 补成 `https://m.example.com/`
// 才是真正参与拼接的形态，用最终形态做校验才等价于 npm 侧。
//
// 三条必须分开拦（Go 的 url.Parse 比 WHATWG URL 宽松，不拦就会放行坏值）：
//
//	解析失败         "ht tp://x" 这类
//	协议不对         ftp:// file:// javascript: 等
//	主机名为空       `https:///x` —— url.Parse 会**成功**并给出 Host==""、
//	                 Path="/x"；拼接后得到 `https:///xhttps://github.com/...`，
//	                 下载时才失败，错误信息离现场极远。npm 侧同样补了这条。
func validatePrefix(prefix, raw, source string) error {
	u, err := url.Parse(prefix)
	if err != nil {
		return fmt.Errorf("update: %s 里的镜像前缀不是合法 URL：%q: %w", source, strings.TrimSpace(raw), err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("update: %s 只接受 http(s) 前缀，收到：%q", source, strings.TrimSpace(raw))
	}
	// Host 同时覆盖 `https://`（Host 空）与 `https:///x`（Host 空、Path 有值）。
	// 不额外校验 u.Opaque：`http:foo` 的 Scheme 是 http 但无 Host，同样被这条拦住。
	if u.Host == "" {
		return fmt.Errorf("update: %s 里的镜像前缀缺少主机名：%q", source, strings.TrimSpace(raw))
	}
	return nil
}

// MirrorLabel 把前缀转成人看的标签，供进度回调与日志使用。
func MirrorLabel(prefix string) string {
	if prefix == DirectMirror {
		return "github.com（直连）"
	}
	if u, err := url.Parse(prefix); err == nil && u.Host != "" {
		return u.Host
	}
	return prefix
}
