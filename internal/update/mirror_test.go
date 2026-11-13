package update

import (
	"os"
	"reflect"
	"testing"
)

// 镜像解析的表驱动测试。
//
// 这张表就是与 npm/lib/mirror.js 的**对照基准**：npm 侧的 parseMirrorList
// 对同一批输入必须给出相同结果。改任一侧时先跑这张表。
func TestParseMirrorList(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
		err  bool
	}{
		// ---- 空与直连 ----
		{name: "空串=直连", in: "", want: []string{""}},
		{name: "纯空白=直连", in: "   \t\n ", want: []string{""}},
		{name: "direct=直连", in: "direct", want: []string{""}},
		{name: "DIRECT 大小写不敏感", in: "DIRECT", want: []string{""}},
		{name: "DiReCt 混合大小写", in: "  DiReCt  ", want: []string{""}},
		{name: "只有逗号=直连（无有效项）", in: ",", want: []string{""}},
		{name: "逗号与空白=直连", in: " , , ", want: []string{""}},

		// ---- 单个前缀 ----
		{name: "单个带斜杠原样", in: "https://gh-proxy.com/", want: []string{"https://gh-proxy.com/"}},
		{name: "单个无斜杠自动补", in: "https://gh-proxy.com", want: []string{"https://gh-proxy.com/"}},
		{name: "http 也接受", in: "http://m.local:8080", want: []string{"http://m.local:8080/"}},
		{name: "带路径前缀", in: "https://m.example.com/gh", want: []string{"https://m.example.com/gh/"}},
		{name: "带路径前缀已有斜杠", in: "https://m.example.com/gh/", want: []string{"https://m.example.com/gh/"}},

		// ---- 多项 ----
		{
			name: "两项按顺序",
			in:   "https://a.com,https://b.com",
			want: []string{"https://a.com/", "https://b.com/"},
		},
		{
			name: "空项跳过（a,,b 不加直连）",
			in:   "https://a.com,,https://b.com",
			want: []string{"https://a.com/", "https://b.com/"},
		},
		{
			name: "结尾逗号不产生直连",
			in:   "https://a.com,",
			want: []string{"https://a.com/"},
		},
		{
			name: "显式 direct 才产生直连",
			in:   "https://a.com,direct",
			want: []string{"https://a.com/", ""},
		},
		{
			name: "direct 在中间",
			in:   "https://a.com,direct,https://b.com",
			want: []string{"https://a.com/", "", "https://b.com/"},
		},
		{
			name: "各项两侧空白被 trim",
			in:   "  https://a.com  ,\thttps://b.com\n",
			want: []string{"https://a.com/", "https://b.com/"},
		},

		// ---- 去重 ----
		{
			name: "完全重复去重，保留首次顺序",
			in:   "https://b.com,https://a.com,https://b.com",
			want: []string{"https://b.com/", "https://a.com/"},
		},
		{
			name: "补斜杠后等价的项也去重",
			in:   "https://a.com,https://a.com/",
			want: []string{"https://a.com/"},
		},
		{
			name: "多个 direct 去重成一个",
			in:   "direct,DIRECT,direct",
			want: []string{""},
		},

		// ---- 非法输入 ----
		{name: "非 URL 报错", in: "not-a-url", err: true},
		{name: "协议不对报错", in: "ftp://x.com", err: true},
		{name: "缺主机名报错", in: "https://", err: true},
		// WHATWG URL 与 Go url.Parse 的接受集合不一致：`https:///x` 在 Go 里
		// 解析成功但 Host 为空、Path 为 /x。前缀若放行，拼接出的 URL 是
		// `https:///xhttps://github.com/...`，下载时才失败且错误离现场极远。
		{name: "三斜杠无主机名报错", in: "https:///x", err: true},
		{name: "多斜杠无主机名报错", in: "https:////example.com", err: true},
		{name: "一项坏则整批报错", in: "https://a.com,bad", err: true},
		{name: "file 协议报错", in: "file:///tmp/x", err: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseMirrorList(tc.in)
			if tc.err {
				if err == nil {
					t.Fatalf("期望报错，得到 %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("意外错误: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("输入 %q\n得到 %v\n期望 %v", tc.in, got, tc.want)
			}
		})
	}
}

// 三种优先级的锁：显式 > 环境变量 > 默认。
func TestResolveMirrorsPrecedence(t *testing.T) {
	defaults := append([]string(nil), DefaultMirrors...)

	t.Run("都没设=默认列表", func(t *testing.T) {
		t.Setenv(MirrorEnv, "")
		os.Unsetenv(MirrorEnv) // t.Setenv 只能设值，清掉以模拟"未设置"
		got, err := ResolveMirrors(nil)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, defaults) {
			t.Fatalf("得到 %v，期望默认 %v", got, defaults)
		}
	})

	t.Run("环境变量覆盖默认", func(t *testing.T) {
		t.Setenv(MirrorEnv, "https://only.example.com")
		got, err := ResolveMirrors(nil)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"https://only.example.com/"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("得到 %v，期望 %v", got, want)
		}
	})

	t.Run("显式配置压过环境变量", func(t *testing.T) {
		t.Setenv(MirrorEnv, "https://from-env.example.com")
		got, err := ResolveMirrors([]string{"https://from-options.example.com"})
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"https://from-options.example.com/"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("得到 %v，期望 %v", got, want)
		}
	})

	t.Run("环境变量设为空=直连而非默认", func(t *testing.T) {
		// 关键区别：设置为空 vs 未设置。前者是用户显式要直连。
		t.Setenv(MirrorEnv, "")
		got, err := ResolveMirrors(nil)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, []string{""}) {
			t.Fatalf("得到 %v，期望 [\"\"]（直连）", got)
		}
	})

	t.Run("环境变量纯空白=直连", func(t *testing.T) {
		t.Setenv(MirrorEnv, "   ")
		got, err := ResolveMirrors(nil)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, []string{""}) {
			t.Fatalf("得到 %v，期望直连", got)
		}
	})

	t.Run("显式空切片=直连（非 nil 时尊重调用方）", func(t *testing.T) {
		t.Setenv(MirrorEnv, "https://from-env.example.com")
		got, err := ResolveMirrors([]string{})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, []string{""}) {
			t.Fatalf("得到 %v，期望直连（无有效项）", got)
		}
	})

	t.Run("返回的默认列表是副本", func(t *testing.T) {
		os.Unsetenv(MirrorEnv)
		got, _ := ResolveMirrors(nil)
		got[0] = "https://mutated.example.com/"
		if DefaultMirrors[0] == "https://mutated.example.com/" {
			t.Fatal("默认列表被调用方改写污染了")
		}
	})
}

// 显式切片的边角：与 CSV 共用同一套规则（含「空串元素被跳过」这条）。
func TestResolveMirrorsExplicitSliceRules(t *testing.T) {
	t.Setenv(MirrorEnv, "https://ignored.example.com")

	tests := []struct {
		name string
		in   []string
		want []string
	}{
		// 这是最容易踩的一条：显式切片里写 "" 想表达直连，但它会被当空项跳过。
		// 要直连必须写 "direct"。测试锁住这个（反直觉但必要的）语义，
		// 免得以后有人"顺手修一下"造成两侧分歧。
		{"空串元素被跳过", []string{"", "https://a.com"}, []string{"https://a.com/"}},
		{"direct 字面量才表示直连", []string{"direct", "https://a.com"}, []string{"", "https://a.com/"}},
		{"全空元素=直连", []string{"", "  "}, []string{""}},
		{"nil 之外的空切片=直连", []string{}, []string{""}},
		{"trim 与补斜杠", []string{" https://a.com "}, []string{"https://a.com/"}},
		{"去重", []string{"https://a.com/", "https://a.com"}, []string{"https://a.com/"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveMirrors(tc.in)
			if err != nil {
				t.Fatalf("意外错误: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("输入 %v，得到 %v，期望 %v", tc.in, got, tc.want)
			}
		})
	}

	// 非法项要报错（GUI 保存时就要拦住，而不是等下载失败）
	if _, err := ResolveMirrors([]string{"https://a.com", "bad"}); err == nil {
		t.Fatal("显式切片里的坏值应报错")
	}
}

// 默认列表与 npm 侧的 DEFAULT_MIRRORS 必须逐项相同。
func TestDefaultMirrorsContract(t *testing.T) {
	want := []string{
		"https://gh-proxy.com/",
		"https://ghfast.top/",
		"https://ghproxy.net/",
		"", // 直连兜底
	}
	if !reflect.DeepEqual(DefaultMirrors, want) {
		t.Fatalf("默认镜像列表与 npm 侧契约不一致：%v", DefaultMirrors)
	}
	// 末位必须是直连，否则「全部镜像失败」时没有兜底。
	if last := DefaultMirrors[len(DefaultMirrors)-1]; last != "" {
		t.Fatalf("末位必须是直连兜底，实际是 %q", last)
	}
}

func TestMirrorLabel(t *testing.T) {
	tests := map[string]string{
		"":                          "github.com（直连）",
		"https://gh-proxy.com/":     "gh-proxy.com",
		"https://m.example.com/gh/": "m.example.com",
		"http://127.0.0.1:1234/":    "127.0.0.1:1234",
	}
	for in, want := range tests {
		if got := MirrorLabel(in); got != want {
			t.Fatalf("MirrorLabel(%q)=%q，期望 %q", in, got, want)
		}
	}
}

func TestNormalizePrefix(t *testing.T) {
	tests := map[string]string{
		"":                  "",
		"https://a.com":     "https://a.com/",
		"https://a.com/":    "https://a.com/",
		"https://a.com/gh":  "https://a.com/gh/",
		"https://a.com/gh/": "https://a.com/gh/",
	}
	for in, want := range tests {
		if got := NormalizePrefix(in); got != want {
			t.Fatalf("NormalizePrefix(%q)=%q，期望 %q", in, got, want)
		}
	}
}
