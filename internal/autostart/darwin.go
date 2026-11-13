//go:build darwin

// macOS 的登录项：自建一个用户级 LaunchAgent。
//
// 为什么不用 mygo.App.SetOpenAtLogin：它在 macOS 13+ 走 SMAppService，要求
// 应用是个 .app bundle；从裸可执行文件调用会直接报「starting at login needs
// an app bundle」。macOS 12 的回退路径也不更好——它跑 `open -a <.app>`，仍然
// 绑 bundle。而本项目的分发产物就是**一个可执行文件**（bundle 是首次启动时
// 在用户磁盘上现拼的，不参与分发），所以登录项必须自己管。
//
// 机制是一个文件：
//
//	~/Library/LaunchAgents/dev.jixoai.reply9842.plist
//
// launchd 在登录时读它并启动 ProgramArguments。没有代码签名、没有特权 helper、
// 没有安装器。
package autostart

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const supported = true

// label 是 LaunchAgent 的 Label，也是 plist 的文件名主干。
//
// 与 bundle 的 CFBundleIdentifier 同一个字符串：用户在两处看到的是同一个标识，
// 排查时不会怀疑自己搞错了对象。
const label = ID

// plistPath 返回 LaunchAgent 的完整路径（尊重 $HOME，便于测试隔离）。
func plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("autostart: 无法确定用户主目录: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", label+".plist"), nil
}

// open 读回 plist 并校验它描述的确实是「当前这个可执行文件 + --autostart」。
//
// 校验的每一条都有理由，缺一条就会把「启动不生效」变成无声失败：
//   - 解析失败（手工改坏、写了一半）→ off
//   - ProgramArguments 不是恰好两项 → off
//   - 第一项不是当前可执行文件（二进制被移动/改名）→ off
//   - 第二项不是 --autostart → off（否则登录启动会弹窗口）
//
// 读成 off 不是「假装没有」，而是事实：launchd 那边确实不会按我们期望的方式
// 启动。用户重新打开开关就会用新路径重写。
func open() (bool, error) {
	path, err := plistPath()
	if err != nil {
		return false, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil // 没装过：off，不是错误
		}
		return false, fmt.Errorf("autostart: 读取 %s: %w", path, err)
	}
	doc, err := parsePlist(data)
	if err != nil {
		return false, nil // 解析不了就是坏的，读作 off
	}
	if got, _ := doc["Label"].(string); got != label {
		return false, nil
	}
	args, _ := doc["ProgramArguments"].([]string)
	if len(args) != 2 || args[1] != Marker {
		return false, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return false, err
	}
	if !samePath(args[0], exe) {
		return false, nil // 二进制搬走了：这个 plist 指向一个不再存在的程序
	}
	return true, nil
}

// set 写或删 plist。
//
// 不 load/unload：load 会让 launchd 立刻再起一个进程，用户会看到两个实例
// （虽然单实例锁会挡住，但那是一次无谓的启动抖动）。语义就是「下次登录生效」。
func set(on bool) error {
	path, err := plistPath()
	if err != nil {
		return err
	}
	if !on {
		// 关闭 = 删除文件。不存在也算成功（幂等）。
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("autostart: 删除 %s: %w", path, err)
		}
		return nil
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("autostart: 无法确定可执行文件路径: %w", err)
	}
	// 解析符号链接：用户可能通过 `ln -s` 的路径启动，而 launchd 在登录时
	// 按字面路径执行，符号链接指向的东西可能已经变了。
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}

	dir := filepath.Dir(path)
	// 0700：LaunchAgents 是用户私有目录，其他用户没有理由读它。
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("autostart: 创建 %s: %w", dir, err)
	}
	return writeFileAtomic(path, []byte(plistXML(exe)), 0o644)
}

// plistXML 生成 LaunchAgent 的内容。
//
// 各键的用途：
//   - Label                 launchd 的唯一标识，与文件名一致
//   - ProgramArguments      要执行的命令行；第一项是可执行文件
//   - RunAtLoad             登录时启动（这就是「开机自启」本身）
//   - KeepAlive={SuccessfulExit=false}  进程**崩溃**（非零退出）时由 launchd
//     自动拉起；用户主动退出（exit 0）不会被复活。2026-11-14 owner 要求
//     「可以 let it crash，但关键是要有自恢复」——这是最外层的自恢复，
//     隧道是否自动恢复运行由 settings.json 的 running 决定（见 gui 的
//     shouldStartOnLaunch）。
//   - ThrottleInterval=5   连续崩溃时至少间隔 5 秒，避免把 launchd 打成忙等
//   - LimitLoadToSessionType=Aqua  只在图形会话里启动，ssh/守护进程会话不启
//   - ProcessType=Interactive     告诉 launchd 这是个前台感受的应用，
//     调度上不按后台批处理对待
//
// 路径必须 XML 转义：含 `&` 或 `<` 的路径（用户目录里完全可能有）会让 launchd
// 在登录时解析失败，而失败发生在下一次登录，用户根本无从归因。
func plistXML(exe string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>` + xmlEscape(label) + `</string>
  <key>ProgramArguments</key>
  <array>
    <string>` + xmlEscape(exe) + `</string>
    <string>` + xmlEscape(Marker) + `</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key>
  <dict>
    <key>SuccessfulExit</key><false/>
  </dict>
  <key>ThrottleInterval</key><integer>5</integer>
  <key>LimitLoadToSessionType</key><string>Aqua</string>
  <key>ProcessType</key><string>Interactive</string>
</dict>
</plist>
`
}

// xmlEscape 转义 XML 文本节点里的三个敏感字符（属性值不涉及，故不处理引号）。
func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

// writeFileAtomic 同目录 temp + chmod + rename。
//
// 为什么不能直接 WriteFile：崩溃/断电会留下截断的 plist，launchd 下次登录
// 解析失败就等于登录项静默失效。rename 在同一文件系统上是原子的，读者要么
// 看到旧文件、要么看到完整的新文件。
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+label+"-*")
	if err != nil {
		return fmt.Errorf("autostart: 创建临时文件: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // rename 成功后这里是 no-op
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("autostart: 写入临时文件: %w", err)
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return fmt.Errorf("autostart: 设置权限: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("autostart: 关闭临时文件: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("autostart: 原子替换 %s: %w", path, err)
	}
	return nil
}

// samePath 比较两个路径是否指向同一个文件（符号链接已在写入侧解析过，
// 但读回时路径可能再次经过链接，所以两边都解析一遍）。
func samePath(a, b string) bool {
	if real, err := filepath.EvalSymlinks(a); err == nil {
		a = real
	}
	if real, err := filepath.EvalSymlinks(b); err == nil {
		b = real
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// parsePlist 解析 plist 里我们关心的那部分：顶层 dict 的 key→值。
//
// 为什么不引第三方 plist 库：只需要读自己写的这一种形状（string / array of
// string / bool）。一个 60 行的 token 遍历就够，而且**不会因为多支持了十种
// 类型而在坏输入上做出意外解释**——我们宁可读不懂就判 off。
func parsePlist(data []byte) (map[string]any, error) {
	dec := xml.NewDecoder(strings.NewReader(string(data)))
	out := map[string]any{}

	depth := 0        // dict 嵌套深度
	pendingKey := ""  // 刚读到的 key，等待它的值
	inArgs := false   // 正在读 ProgramArguments 的 array
	var args []string // 已读到的 ProgramArguments

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "dict":
				depth++
			case "array":
				// 只有紧跟在 ProgramArguments 后面的 array 才被收集
				inArgs = depth == 1 && pendingKey == "ProgramArguments"
				if inArgs {
					args = nil
				}
			case "string":
				var s string
				if err := dec.DecodeElement(&s, &t); err != nil {
					return nil, err
				}
				switch {
				case inArgs:
					args = append(args, s)
				case depth == 1 && pendingKey != "":
					out[pendingKey] = s
					pendingKey = ""
				}
			case "key":
				var k string
				if err := dec.DecodeElement(&k, &t); err != nil {
					return nil, err
				}
				if depth == 1 {
					pendingKey = k
				}
			case "true":
				if depth == 1 && pendingKey != "" {
					out[pendingKey] = true
					pendingKey = ""
				}
			case "false":
				if depth == 1 && pendingKey != "" {
					out[pendingKey] = false
					pendingKey = ""
				}
			case "integer", "real", "date", "data":
				// 我们从不写这些类型；读到它们说明文件不是我们生成的。
				// 跳过内容，让上层的严格校验判 off。
				var skip string
				if err := dec.DecodeElement(&skip, &t); err != nil {
					return nil, err
				}
				if depth == 1 {
					pendingKey = ""
				}
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "dict":
				depth--
			case "array":
				if inArgs {
					out["ProgramArguments"] = args
					inArgs = false
					pendingKey = ""
				}
			}
		}
	}
	return out, nil
}
