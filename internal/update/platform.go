// 产品形态与平台产物命名。
//
// 命名必须与 .github/workflows/release.yml 的构建循环、npm/lib/run.js 的
// artifactName 三方严格一致。任何一侧漂移的表现都是「检查更新说有新版，
// 下载 404」——这类错误在 GUI 里只显示为「下载失败」，极难定位。
package update

import (
	"fmt"
	"runtime"
)

// Kind 是要更新的产品形态。
type Kind string

const (
	KindCLI    Kind = "cli"
	KindGUI    Kind = "gui"
	KindServer Kind = "server"
)

// ParseKind 校验调用方传入的形态字符串。
func ParseKind(s string) (Kind, error) {
	switch Kind(s) {
	case KindCLI, KindGUI, KindServer:
		return Kind(s), nil
	}
	return "", fmt.Errorf("update: 未知产品形态 %q（可选 cli|gui|server）", s)
}

// ArtifactName 返回该形态在当前（或指定）平台上的产物文件名。
//
// 参数是显式的 goos/goarch 而不是直接读 runtime，是为了让测试能在任意宿主上
// 断言全部平台组合（含 Linux 上验证 windows 的 .exe 后缀）。
//
// GUI 无 Linux 产物（mygo native UI 无 Linux 后端），返回错误而不是拼一个
// 下载必然 404 的名字。
func ArtifactName(kind Kind, goos, goarch string) (string, error) {
	switch kind {
	case KindCLI, KindGUI, KindServer:
	default:
		return "", fmt.Errorf("update: 未知产品形态 %q", kind)
	}
	if kind == KindGUI && goos == "linux" {
		return "", fmt.Errorf("update: GUI 不提供 Linux 产物（mygo native UI 无 Linux 后端），请用 CLI 或从源码构建")
	}
	switch goos {
	case "darwin", "linux", "windows":
	default:
		return "", fmt.Errorf("update: 不支持的平台 %s", goos)
	}
	switch goarch {
	case "amd64", "arm64":
	default:
		return "", fmt.Errorf("update: 不支持的架构 %s", goarch)
	}
	ext := ""
	if goos == "windows" {
		ext = ".exe"
	}
	return string(kind) + "-" + goos + "-" + goarch + ext, nil
}

// CurrentArtifact 用运行时环境解析产物名。
//
// 运行 `go test` 的机器架构可能与用户机器不同——但真正部署时两者相同，
// 因为自更新跑的永远是「本机正在运行的这个二进制」。
func CurrentArtifact(kind Kind) (string, error) {
	return ArtifactName(kind, runtime.GOOS, runtime.GOARCH)
}

// ChecksumsAsset 是 release 里校验和文件的名字。
//
// 由 release.yml 的 `sha256sum` / `shasum -a 256` 生成，格式为
// `<64 hex>  <filename>`（sha256sum 对二进制文件也可能输出 `*<filename>`）。
const ChecksumsAsset = "SHA256SUMS.txt"
