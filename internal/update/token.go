package update

// 本机 gh CLI 的 token 读取。
//
// 为什么需要：GitHub REST API 的未认证配额是 60 次/小时/IP；同一出口后面
// 往往还有别人（公司 NAT、机场节点），一次检查就可能撞上 403。开发者机器
// 通常已经 `gh auth login`，它的 token 配额是 5000 次/小时。把这一步接上，
// 普通用户不用理解 token，开发者也不用为此配环境变量。
//
// 边界：
//   - 只在没有显式 token 与环境变量时尝试；
//   - 3 秒超时，gh 不存在/未登录都静默返回空串（检查更新走匿名或镜像）；
//   - token 不缓存、不落盘、不写日志、绝不发给第三方镜像。

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ghAuthToken 是 exec 的测试接缝（生产路径就是 gh auth token）。
var ghAuthToken = func(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	bin := ghBinary()
	if bin == "" {
		return "", fmt.Errorf("gh not found")
	}
	out, err := exec.CommandContext(ctx, bin, "auth", "token", "--hostname", "github.com").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// ghBinary 找 gh 可执行文件。
//
// 不能只靠 exec.LookPath：macOS 上 GUI 从 Finder/登录项启动时 PATH 往往只有
// /usr/bin:/bin:/usr/sbin:/sbin，Homebrew 的 /opt/homebrew/bin 不在里面，
// 而用户可能天天在终端里用 gh。这里补几个常见安装位置；不存在的直接跳过。
func ghBinary() string {
	if p, err := exec.LookPath("gh"); err == nil {
		return p
	}
	var candidates []string
	if runtime.GOOS == "windows" {
		if pf := os.Getenv("ProgramFiles"); pf != "" {
			candidates = append(candidates, filepath.Join(pf, "GitHub CLI", "gh.exe"))
		}
	} else {
		candidates = append(candidates, "/opt/homebrew/bin/gh", "/usr/local/bin/gh")
		if home, err := os.UserHomeDir(); err == nil {
			candidates = append(candidates, filepath.Join(home, ".local", "bin", "gh"))
		}
	}
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// localGHToken 返回本机 gh CLI 的登录 token；任何失败都返回空串。
func localGHToken() string {
	t, err := ghAuthToken(context.Background())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(t)
}
