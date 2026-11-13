package update

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Options 是调用方对一次更新会话的全部配置。
//
// 刻意把「策略」留空：是否自动更新、多久检查一次、要不要弹窗，都由调用方
// 决定。本包只提供「事实」（有没有新版）与「动作」（下载/替换/重启）。
type Options struct {
	// Kind 是要更新的产品形态。必填——cli / gui / server 的产物名不同，
	// 猜错就会下载到另一个可执行文件并覆盖自己（那是不可恢复的变砖）。
	Kind Kind

	// CurrentVersion 是当前版本（cmd/*/main.go 的 `var version`）。
	// 空串等价 "dev"，会被当作开发构建处理。
	CurrentVersion string

	// Mirrors 是显式配置的镜像前缀列表，**优先级最高**。
	//
	// nil（零值）= 未配置 → 退回环境变量 → 再退回 DefaultMirrors。
	// 非 nil（含空切片）= 用户明确要求，逐项按 mirror.go 的规则规范化校验，
	// 不再看环境变量。GUI 设置页保存的值从这里进来。
	Mirrors []string

	// Token 是 GitHub API token，用于把限额从 60 次/小时提到 5000 次/小时。
	// 留空时回退环境变量 GITHUB_TOKEN（CI / 开发者本机的常见位置）。
	Token string

	// Dir 是下载文件的缓存目录。留空时用
	// <UserCacheDir>/reply9842/updates/<LatestVersion>/。
	//
	// 按版本分目录的理由与 npm 侧一致：换版本不串包，回滚时旧文件还在。
	Dir string

	// Progress 是进度回调，可为 nil。Download 的 progressFn 参数优先。
	Progress ProgressFunc

	// HTTPClient 覆盖默认客户端（测试注入、企业代理、自定义 CA）。
	// 留空时用 DefaultHTTPClient()。
	HTTPClient *http.Client

	// KeepBackups 是保留的旧二进制备份数量，默认 2。
	//
	// 留 2 而不是 1：一次更新把「能跑的上一个版本」和「再上一个」都留着，
	// 第二次更新才发现问题（比如某个上游接口不兼容）时还能退回两档。
	// 0 表示用默认值；负数表示不保留备份（不推荐，但允许调用方选择）。
	KeepBackups int

	// Timeout 是检查版本的总超时，默认 15s。下载不受它约束
	// （由 ctx 控制，大文件本来就要几分钟）。
	Timeout time.Duration

	// APIBase 覆盖 GitHub API 根地址（仅测试用）。
	apiBase string
	// RepoBase 覆盖 https://github.com 前缀（仅测试用）。
	repoBase string
}

// DefaultTimeout 是 Check 的默认超时。
//
// 15s 是权衡：GitHub API 在国内经代理常见 2-5s，15s 足够；再长会让「手动
// 检查更新」的按钮看起来像没反应。
const DefaultTimeout = 15 * time.Second

// Updater 承载一次更新会话的配置。零值不可用，必须经 New 构造。
//
// 为什么不把 opts 直接散成函数参数：一个 GUI 会话里 Check / Download / Apply
// 用的是同一套配置，散开会导致调用方重复传 6 个参数、且在两次调用之间偷偷
// 换掉镜像（用户刚改设置）——那种不一致极难排查。
type Updater struct {
	opts Options

	// mirrors 是本次会话解析好的镜像列表，构造时解析一次。
	//
	// 一旦解析失败就记在这里，Check/Download 直接返回该错误：与其在下载
	// 中途才发现配置有问题，不如第一次调用就明确报出来。
	mirrorErr error
	mirrors   []string
}

// New 构造 Updater。Kind 非法或镜像配置非法时返回错误。
//
// 镜像配置在这里就解析（而不是等到 Download），因为设置页保存的值可能是
// 用户手写的坏 URL——在「保存」那一刻就报错，比点「立即更新」后下载失败好。
func New(opts Options) (*Updater, error) {
	if opts.Kind == "" {
		return nil, fmt.Errorf("update: Options.Kind 必填（cli|gui|server）")
	}
	if _, err := ParseKind(string(opts.Kind)); err != nil {
		return nil, err
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.KeepBackups == 0 {
		opts.KeepBackups = 2
	}

	u := &Updater{opts: opts}
	u.mirrors, u.mirrorErr = ResolveMirrors(opts.Mirrors)
	return u, nil
}

// Options 返回会话配置的副本（调用方偶尔需要读 Kind / CurrentVersion）。
func (u *Updater) Options() Options { return u.opts }

// Mirrors 返回本次会话生效的镜像列表（已规范化）。
//
// 即使解析出错也返回已解析出的部分（通常为空），配合 error 使用。
func (u *Updater) Mirrors() ([]string, error) {
	return u.mirrors, u.mirrorErr
}

// Kind 返回产品形态。
func (u *Updater) Kind() Kind { return u.opts.Kind }

// token 优先用显式配置，其次环境变量。
func (u *Updater) token() string {
	if t := strings.TrimSpace(u.opts.Token); t != "" {
		return t
	}
	return strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
}

// apiBase 返回 GitHub API 根地址（测试可覆盖）。
func (u *Updater) apiBase() string {
	if u.opts.apiBase != "" {
		return strings.TrimRight(u.opts.apiBase, "/")
	}
	return "https://api.github.com"
}

// repoBase 返回 release 下载的根地址（测试可覆盖）。
func (u *Updater) repoBase() string {
	if u.opts.repoBase != "" {
		return strings.TrimRight(u.opts.repoBase, "/")
	}
	return "https://github.com"
}

// client 返回检查版本用的 HTTP 客户端。
//
// 刻意给 Check 一个带超时的客户端：GitHub API 无响应时，用户点「检查更新」
// 不该无限转圈。下载走另一个无总超时的客户端（见 download.go），因为大文件
// 的耗时不可预测，只该受 ctx 约束。
func (u *Updater) client() *http.Client {
	if u.opts.HTTPClient != nil {
		return u.opts.HTTPClient
	}
	return &http.Client{Timeout: u.opts.Timeout}
}

// downloadClient 返回下载用的客户端。
//
// 不设 Timeout：8-17MB 的产物在慢速链路上可能超过任何合理总时长，硬超时
// 会把「正在下载」变成「每次都失败」。进度回调是判断「卡住了没有」的地方，
// 不由总超时负责。ResponseHeaderTimeout 仍保留，防的是「连接建立后服务端
// 永不响应」这种挂死。
func (u *Updater) downloadClient() *http.Client {
	if u.opts.HTTPClient != nil {
		return u.opts.HTTPClient
	}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ResponseHeaderTimeout: 30 * time.Second,
			IdleConnTimeout:       90 * time.Second,
			ForceAttemptHTTP2:     true,
		},
		// 不跟随无限重定向：镜像偶尔会 302 到自己的 CDN，跟 10 次足够了。
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("update: 重定向次数过多")
			}
			return nil
		},
	}
}

// downloadURLFor 构造某个 release 产物的直连 URL（不含镜像前缀）。
func (u *Updater) downloadURLFor(tag, artifact string) string {
	return u.repoBase() + "/" + Repo + "/releases/download/" + normalizeTag(tag) + "/" + artifact
}

// normalizeTag 让 v0.2.1 与 0.2.1 都变成 v0.2.1。
//
// release 的 tag 约定带 v，但 Check 返回的是 GitHub 原样值；调用方也可能
// 只存了「0.2.1」这样的版本号。两种都容忍，避免拼出 .../download/0.2.1/ 这种 404。
func normalizeTag(tag string) string {
	tag = strings.TrimSpace(tag)
	if tag == "" || strings.HasPrefix(tag, "v") {
		return tag
	}
	return "v" + tag
}

// 编译期断言：确保 filepath 在本文件被使用（downloadDir 在 download.go 用，
// 这里保留 join 的小工具是为了让 Dir 的默认值逻辑集中在一处）。
func (u *Updater) cacheDir() (string, error) {
	if u.opts.Dir != "" {
		return u.opts.Dir, nil
	}
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		// UserCacheDir 在极端环境（无 HOME）会失败。退回临时目录仍然比失败好：
		// 下载到 /tmp 也能完成更新，只是重启后要重下。
		base = os.TempDir()
	}
	return filepath.Join(base, "reply9842", "updates"), nil
}
