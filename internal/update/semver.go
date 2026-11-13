// semver 解析与比较。
//
// 为什么要自己写而不是引库：本项目只用到「判断 A 是否比 B 新」这一个能力，
// 而 semver 的比较规则（预发布标识的数值 vs 字典序、段数不同时缺省补零、
// 有预发布 < 无预发布）是**唯一**需要精确的地方。引一个库只为 80 行逻辑不值当，
// 而且引入依赖意味着 release 构建还要多一份供应链信任。
//
// 与 npm 侧的差异：npm 用 semver 库做完整校验；这里只求「能比较就正确比较，
// 不能解析就当作非正式版本」。正则采用 semver.org 官方推荐的那一条。
package update

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// semverRe 是 semver.org 首页给出的官方推荐正则（已去掉 ^$ 锚点以便复用）。
// 它接受 "1.2.3"、"1.2.3-rc.1"、"1.2.3+build.5"、"1.2.3-rc.1+build.5"。
var semverRe = regexp.MustCompile(
	`^v?(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)` +
		`(?:-((?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)(?:\.(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*))?` +
		`(?:\+([0-9a-zA-Z-]+(?:\.[0-9a-zA-Z-]+)*))?$`,
)

// Version 是解析后的 semver。
type Version struct {
	Major, Minor, Patch int
	Pre                 []string // 预发布标识；nil 表示正式版
	Build               string   // 构建元数据，不参与比较
	Original            string   // 原始输入，便于错误信息与日志
}

// ParseVersion 解析版本字符串。
//
// 特意接受 `v` 前缀：GitHub 的 tag 与我们的 -ldflags 都带 v，调用方不必先剥。
// 大写 V 也接受（tag 偶尔这么写）；不接受 "1.2" 这类省略 patch 的写法——
// 本项目所有版本号都是三段的，放宽只会掩盖「版本号拼错」这类真问题。
func ParseVersion(s string) (Version, error) {
	raw := strings.TrimSpace(s)
	// 统一成小写 v 再交给正则：正则里写 [vV] 会让 capture group 的下标偏移，
	// 归一化更不容易出错。
	if len(raw) > 0 && raw[0] == 'V' {
		raw = "v" + raw[1:]
	}
	m := semverRe.FindStringSubmatch(raw)
	if m == nil {
		return Version{}, fmt.Errorf("update: 无法解析版本号 %q（要求 x.y.z[-pre][+build]）", raw)
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	patch, _ := strconv.Atoi(m[3])
	v := Version{Major: major, Minor: minor, Patch: patch, Build: m[5], Original: raw}
	if m[4] != "" {
		v.Pre = strings.Split(m[4], ".")
	}
	return v, nil
}

// IsSemver 报告一个版本字符串能否被解析。
//
// Check 用它区分「正式版本」与「dev / git describe 出来的构建」。
func IsSemver(s string) bool {
	_, err := ParseVersion(s)
	return err == nil
}

// gitDescribeRe 匹配 `git describe` 产物里的「距上个 tag 的提交数 + 短哈希」段，
// 例如 v0.1.0-3-gabcdef 里的 `-3-gabcdef`。
//
// 哈希长度放宽到 4+：git describe 默认 7 位，但 --abbrev=<n> 与部分平台的
// 默认值会给出 4-12 位。收窄到 7 会漏掉真实形态（本包测试就踩过：一段
// 6 位哈希的版本被当成了正式预发布）。
var gitDescribeRe = regexp.MustCompile(`-\d+-g[0-9a-f]{4,}`)

// IsDevBuild 判断一个版本字符串是「本地构建」而非发布版本。
//
// # 为什么不能只靠「semver 解析失败」判断
//
// 实测（2026-11-13 本包测试）：`v0.1.0-3-gabcdef`（Makefile 用 git describe
// 注入的形态）**能**通过 semver 正则——`3-gabcdef` 合法地构成一个预发布段。
// 若把它当正式版本比较，结论是「比 v0.1.0 更早的预发布」，于是开发者的
// 每次启动都不提示更新，反而在 release 是 v0.0.1 时提示「有新版」并且真的
// 把工作区构建覆盖成 release 产物——丢掉正在改的代码。
//
// 判定为本地构建的形态：
//
//	""                       调用方没传版本
//	dev                      Makefile 无 tag 时的兜底
//	dev-abc1234              release.yml 的 workflow_dispatch 版本
//	v0.1.0-3-gabcdef         git describe
//	v0.1.0-3-gabcdef-dirty   git describe + 脏工作区
//
// 不判为本地构建的：任何正常 semver，含 v1.0.0-rc.1（那是真实的预发布）。
func IsDevBuild(s string) bool {
	raw := strings.TrimSpace(s)
	if raw == "" || strings.EqualFold(raw, "dev") {
		return true
	}
	if strings.HasPrefix(raw, "dev-") || strings.HasSuffix(raw, "-dirty") {
		return true
	}
	if gitDescribeRe.MatchString(raw) {
		return true
	}
	return !IsSemver(raw)
}

// Compare 按 semver 规范比较两个版本：a<b 返回 -1，相等 0，a>b 返回 1。
func (v Version) Compare(o Version) int {
	if c := cmpInt(v.Major, o.Major); c != 0 {
		return c
	}
	if c := cmpInt(v.Minor, o.Minor); c != 0 {
		return c
	}
	if c := cmpInt(v.Patch, o.Patch); c != 0 {
		return c
	}
	return comparePre(v.Pre, o.Pre)
}

// comparePre 实现 semver §11 的预发布比较。
//
// 三条规则，漏一条就会得出反直觉的结论：
//   - 有预发布 < 无预发布（1.0.0-rc1 < 1.0.0）
//   - 两段都是纯数字 → 按数值比（rc.9 < rc.10，字符串比会反）
//   - 数字段 < 非数字段（1.0.0-1 < 1.0.0-alpha）
//   - 前缀相同时，段数少的更小（1.0.0-alpha < 1.0.0-alpha.1）
func comparePre(a, b []string) int {
	switch {
	case len(a) == 0 && len(b) == 0:
		return 0
	case len(a) == 0:
		return 1 // 正式版 > 预发布
	case len(b) == 0:
		return -1
	}

	for i := 0; i < len(a) && i < len(b); i++ {
		ai, aNum := parseNum(a[i])
		bi, bNum := parseNum(b[i])
		switch {
		case aNum && bNum:
			if c := cmpInt(ai, bi); c != 0 {
				return c
			}
		case aNum:
			return -1 // 数字段优先级低于字母段
		case bNum:
			return 1
		default:
			if c := strings.Compare(a[i], b[i]); c != 0 {
				return c
			}
		}
	}
	return cmpInt(len(a), len(b))
}

// parseNum 把纯数字段转成 int。带前导零的"数字"按 semver 不合法，
// 这里退化为非数字段交给字典序——反正 tag 不会这么写，不值得报错。
func parseNum(s string) (int, bool) {
	if s == "" || (len(s) > 1 && s[0] == '0') {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// CompareVersions 是比较两个版本字符串的便捷入口。
//
// 任一参数无法解析时返回 error，而不是"猜"一个结果：调用方据此判断
// "当前版本是不可比较的开发构建"，这必须由调用方显式决定怎么呈现。
func CompareVersions(a, b string) (int, error) {
	av, err := ParseVersion(a)
	if err != nil {
		return 0, err
	}
	bv, err := ParseVersion(b)
	if err != nil {
		return 0, err
	}
	return av.Compare(bv), nil
}
