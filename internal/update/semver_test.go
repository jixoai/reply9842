package update

import (
	"strings"
	"testing"
)

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		// ---- 基础序 ----
		{"v0.2.1", "v0.2.1", 0},
		{"v0.2.1", "v0.2.2", -1},
		{"v0.2.2", "v0.2.1", 1},
		{"v1.0.0", "v0.9.9", 1},

		// ---- 任务书点名的有效性：不能字符串比较 ----
		{"v0.2.1", "v0.10.0", -1}, // 字符串比较会得出 "0.2" > "0.10"
		{"v0.10.0", "v0.2.1", 1},
		{"v1.9.0", "v1.10.0", -1},
		{"v2.0.0", "v10.0.0", -1},

		// ---- v 前缀可省略 ----
		{"0.2.1", "v0.2.1", 0},
		{"V1.2.3", "v1.2.3", 0},

		// ---- 预发布 < 正式 ----
		{"v1.0.0-rc.1", "v1.0.0", -1},
		{"v1.0.0", "v1.0.0-rc.1", 1},
		{"v1.0.0-alpha", "v1.0.0-beta", -1},
		{"v1.0.0-beta", "v1.0.0-rc", -1},

		// ---- 预发布里数字按数值比 ----
		{"v1.0.0-rc.9", "v1.0.0-rc.10", -1},
		{"v1.0.0-rc.10", "v1.0.0-rc.9", 1},

		// ---- 数字段 < 字母段 ----
		{"v1.0.0-1", "v1.0.0-alpha", -1},
		{"v1.0.0-alpha", "v1.0.0-1", 1},

		// ---- 前缀相同：段数少的更小 ----
		{"v1.0.0-alpha", "v1.0.0-alpha.1", -1},
		{"v1.0.0-alpha.1", "v1.0.0-alpha", 1},
		{"v1.0.0-alpha.1", "v1.0.0-alpha.1", 0},

		// ---- build 元数据不参与比较 ----
		{"v1.0.0+build1", "v1.0.0+build2", 0},
		{"v1.0.0+build1", "v1.0.0", 0},

		// ---- 多层预发布 ----
		{"v1.0.0-rc.1.2", "v1.0.0-rc.1.10", -1},
	}

	for _, tc := range tests {
		got, err := CompareVersions(tc.a, tc.b)
		if err != nil {
			t.Fatalf("CompareVersions(%q,%q) 意外错误: %v", tc.a, tc.b, err)
		}
		if got != tc.want {
			t.Fatalf("CompareVersions(%q,%q)=%d，期望 %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestParseVersionRejects(t *testing.T) {
	// 这些形态必须报错，不能被"猜"成某个版本的零值：
	// 猜错的后果是"检查更新说有新版"，然后下载 404。
	bad := []string{
		"",
		"dev",
		"v0.1",          // 缺 patch
		"v1.2.3.4",      // 多段
		"1.2.3-",        // 悬空连字符
		"v1.2.3-rc..1",  // 空标识
		"vv1.2.3",       // 双 v
		"v01.2.3",       // 前导零
		"v1.2.3-",       // 空预发布
		"release-1.2.3", // 前缀杂质
		"v1.2.3-rc 1",   // 空格
		"dev-abc1234",   // git describe 的 workflow_dispatch 形态
	}
	for _, s := range bad {
		if _, err := ParseVersion(s); err == nil {
			t.Fatalf("ParseVersion(%q) 应当报错", s)
		}
	}
}

// 关键回归：git describe 的产物**能**被 semver 解析，但必须被识别成本地构建。
// 若不识别，开发者每次启动都会被提示"有新版本"，点了就把工作区构建覆盖掉。
func TestIsDevBuild(t *testing.T) {
	dev := []string{
		"", "dev", "DEV", " dev ",
		"dev-abc1234",
		"v0.1.0-3-gabcdef",
		"v0.1.0-3-gabcdef-dirty",
		"v0.5.0-dirty",
		"not-a-version",
	}
	for _, s := range dev {
		if !IsDevBuild(s) {
			t.Fatalf("IsDevBuild(%q) 应为 true", s)
		}
	}
	notDev := []string{
		"v0.2.1", "0.2.1",
		"v0.10.0",
		"v1.0.0-rc.1", // 真实预发布，不是本地构建
		"v1.0.0+build.5",
	}
	for _, s := range notDev {
		if IsDevBuild(s) {
			t.Fatalf("IsDevBuild(%q) 应为 false", s)
		}
	}
	// 这条断言锁住"为什么需要 IsDevBuild"：
	if !IsSemver("v0.1.0-3-gabcdef") {
		t.Skip("前提变化：git describe 形态不再能被 semver 解析，IsDevBuild 的说明可简化")
	}
}

func TestParseVersionFields(t *testing.T) {
	v, err := ParseVersion("v0.10.2-rc.3+build.7")
	if err != nil {
		t.Fatal(err)
	}
	if v.Major != 0 || v.Minor != 10 || v.Patch != 2 {
		t.Fatalf("数值解析错: %+v", v)
	}
	if len(v.Pre) != 2 || v.Pre[0] != "rc" || v.Pre[1] != "3" {
		t.Fatalf("预发布解析错: %v", v.Pre)
	}
	if v.Build != "build.7" {
		t.Fatalf("build 解析错: %q", v.Build)
	}
	if v.Original != "v0.10.2-rc.3+build.7" {
		t.Fatalf("Original 应保留原值: %q", v.Original)
	}
}

func TestIsSemver(t *testing.T) {
	if !IsSemver("v1.2.3") {
		t.Fatal("v1.2.3 应可解析")
	}
	for _, s := range []string{"dev", "", "0.1", "dev-abc123"} {
		if IsSemver(s) {
			t.Fatalf("%q 不该被当成 semver", s)
		}
	}
}

// 排序性质：Compare 与字符串排序在同长度数字下不一致，这正是不能字符串比较的理由。
func TestSemverNotStringComparison(t *testing.T) {
	// 若实现退化成字符串比较，v0.10.0 < v0.9.0（'1'<'9'），断言会失败。
	if c, _ := CompareVersions("v0.9.0", "v0.10.0"); c != -1 {
		t.Fatalf("v0.9.0 应小于 v0.10.0，得到 %d", c)
	}
	// 字符串比较的结论相反（'9' > '1'），这正说明必须走 semver 比较。
	if strings.Compare("v0.9.0", "v0.10.0") <= 0 {
		t.Fatal("前提失效：字符串比较不再与 semver 相反，这条测试失去意义")
	}
}
