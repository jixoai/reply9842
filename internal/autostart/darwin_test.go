//go:build darwin

package autostart

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withTempHome 把 HOME 指向临时目录，让测试不碰用户真实的登录项。
//
// 这层隔离是硬要求：本包的 Set(true) 会真的往 ~/Library/LaunchAgents 写文件，
// 在开发机上「跑一次测试」不应该给开发者装上开机自启。
func withTempHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

// TestSetOpenRoundTrip 开→读回 true，关→读回 false，关不存在也是 false。
func TestSetOpenRoundTrip(t *testing.T) {
	home := withTempHome(t)

	if on, err := Open(); err != nil || on {
		t.Fatalf("初始 Open() = (%v, %v)，期望 (false, nil)", on, err)
	}
	if err := Set(true); err != nil {
		t.Fatalf("Set(true): %v", err)
	}
	path := filepath.Join(home, "Library", "LaunchAgents", label+".plist")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("plist 未生成: %v", err)
	}
	if on, err := Open(); err != nil || !on {
		t.Fatalf("开启后 Open() = (%v, %v)，期望 (true, nil)", on, err)
	}

	if err := Set(false); err != nil {
		t.Fatalf("Set(false): %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("关闭后 plist 仍在: stat err = %v", err)
	}
	if on, err := Open(); err != nil || on {
		t.Fatalf("关闭后 Open() = (%v, %v)，期望 (false, nil)", on, err)
	}
	// 幂等：再关一次不该报错
	if err := Set(false); err != nil {
		t.Fatalf("重复 Set(false) 报错: %v", err)
	}
}

// TestPlistContent 落盘的 plist 必须包含 launchd 需要的键，且参数恰好两项。
func TestPlistContent(t *testing.T) {
	home := withTempHome(t)
	if err := Set(true); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(home, "Library", "LaunchAgents", label+".plist"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	for _, want := range []string{
		"<key>Label</key><string>" + label + "</string>",
		"<key>RunAtLoad</key><true/>",
		"<key>LimitLoadToSessionType</key><string>Aqua</string>",
		"<key>ProcessType</key><string>Interactive</string>",
		"<string>" + Marker + "</string>",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("plist 缺少 %q\n实际内容：\n%s", want, s)
		}
	}
	// ProgramArguments 必须恰好两项（可执行文件 + 标记）。用解析器验，
	// 而不是数 <string> 出现次数——Label 与两个会话类型键也是 <string>。
	doc, err := parsePlist(data)
	if err != nil {
		t.Fatalf("生成的 plist 解析不了: %v", err)
	}
	args, _ := doc["ProgramArguments"].([]string)
	if len(args) != 2 {
		t.Errorf("ProgramArguments 有 %d 项，期望 2：%q", len(args), args)
	}
	if len(args) == 2 && args[1] != Marker {
		t.Errorf("ProgramArguments[1] = %q，期望 %q", args[1], Marker)
	}
}

// TestPermissions 目录 0700、plist 0644。
func TestPermissions(t *testing.T) {
	home := withTempHome(t)
	if err := Set(true); err != nil {
		t.Fatal(err)
	}
	dirInfo, err := os.Stat(filepath.Join(home, "Library", "LaunchAgents"))
	if err != nil {
		t.Fatal(err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("LaunchAgents 权限 = %o，期望 700", got)
	}
	fileInfo, err := os.Stat(filepath.Join(home, "Library", "LaunchAgents", label+".plist"))
	if err != nil {
		t.Fatal(err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o644 {
		t.Errorf("plist 权限 = %o，期望 644", got)
	}
}

// TestOpenRejectsCorrupt 读回校验：坏文件必须读成 off，而不是报错或误读成 on。
//
// 这几种坏法是现实中都会出现的：手工编辑、写入中断、二进制被移动、标记被删。
// 一律读 off 的理由是——launchd 确实不会按我们的期望启动，那就是 off。
func TestOpenRejectsCorrupt(t *testing.T) {
	cases := map[string]string{
		"不是 plist":    "hello",
		"被截断":         `<?xml version="1.0"?><plist version="1.0"><dict><key>Label</key><string>`,
		"Label 不对":    `<plist version="1.0"><dict><key>Label</key><string>other</string></dict></plist>`,
		"只有一项参数":      `<plist version="1.0"><dict><key>Label</key><string>` + label + `</string><key>ProgramArguments</key><array><string>/x</string></array></dict></plist>`,
		"标记不对":        `<plist version="1.0"><dict><key>Label</key><string>` + label + `</string><key>ProgramArguments</key><array><string>/x</string><string>--other</string></array></dict></plist>`,
		"路径指向不存在的二进制": `<plist version="1.0"><dict><key>Label</key><string>` + label + `</string><key>ProgramArguments</key><array><string>/nonexistent/reply9842</string><string>` + Marker + `</string></array></dict></plist>`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			home := withTempHome(t)
			dir := filepath.Join(home, "Library", "LaunchAgents")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, label+".plist"), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			on, err := Open()
			if err != nil {
				t.Fatalf("Open() 返回错误 %v，期望 (false, nil)", err)
			}
			if on {
				t.Fatalf("坏 plist 被读成 on")
			}
		})
	}
}

// TestXMLEscape 路径含 & < > 时必须转义，否则 launchd 在登录时解析失败。
func TestXMLEscape(t *testing.T) {
	got := xmlEscape(`/Users/a&b/<x>`)
	want := "/Users/a&amp;b/&lt;x&gt;"
	if got != want {
		t.Errorf("xmlEscape = %q，期望 %q", got, want)
	}
}

// TestAtomicallyReplaces 重复 Set(true) 用不同内容时必须整体替换，不留旧内容。
func TestAtomicallyReplaces(t *testing.T) {
	home := withTempHome(t)
	if err := Set(true); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "Library", "LaunchAgents", label+".plist")
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Set(true); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Errorf("重复开启改变了内容：\n第一次：\n%s\n第二次：\n%s", first, second)
	}
	// 目录里不该留下临时文件
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("留下了临时文件 %q", e.Name())
		}
	}
}
