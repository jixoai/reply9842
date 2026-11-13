// 版本检查：查 GitHub 最新 release，与当前版本比较。
//
// 为什么走 GitHub API 而不是「构造 releases/latest/download 的 URL 然后看
// 重定向」：后者拿不到 tag_name（无法判断是否有新版）、拿不到发布时间与
// release notes（GUI 要显示）、也无法区分「没有 release」与「网络不通」。
//
// 限流是硬约束：未认证 60 次/小时/IP。GUI 若每次打开设置页都查一次，很容易
// 在多用户共享出口 IP 的环境下打满，此后**所有** GitHub API 调用都会 403，
// 包括用户自己的操作。所以：
//   - 检查结果由调用方缓存（本包不缓存，保持无状态）
//   - 支持 GITHUB_TOKEN 提升到 5000 次/小时
//   - 403/429 转成 ErrRateLimited，调用方据此显示「稍后再试」而不是崩溃
package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Repo 是发布仓库。写成常量而不是配置项：本包只服务这一个项目，
// 留一个可配置的 owner/repo 只会制造「配错了下载到别人的二进制」的风险。
const Repo = "jixoai/reply9842"

// LatestReleaseAPI 是查询最新 release 的端点。
//
// 走 /releases/latest 而不是 /releases：后者返回数组，还要自己按时间/版本
// 排序；而 latest 端点由 GitHub 维护「最新正式版」语义（跳过 draft 与
// prerelease），正是我们要的。
const LatestReleaseAPI = "https://api.github.com/repos/" + Repo + "/releases/latest"

// 关键错误，调用方据此分支（例如限流时提示「一小时后再试」而非「网络错误」）。
var (
	// ErrRateLimited 表示 GitHub API 限流（未认证 60/小时）。
	ErrRateLimited = errors.New("update: GitHub API 已限流")
	// ErrNoRelease 表示仓库还没有任何正式 release（全新仓库的常态）。
	ErrNoRelease = errors.New("update: 仓库还没有正式 release")
	// ErrNotReleaseBuild 表示当前版本不是可比较的正式版本（如 dev 构建）。
	ErrNotReleaseBuild = errors.New("update: 当前版本不是正式发布版本，无法比较")
)

// Info 是一次版本检查的完整结果。GUI 直接把它渲染成「关于」区块。
type Info struct {
	// Current 是调用方传进来的当前版本（原样保存，便于界面显示）。
	Current string
	// Latest 是最新 release 的 tag（形如 v0.2.1，保持 GitHub 原样含 v）。
	Latest string
	// LatestVersion 是解析后的版本（不带 v），供界面显示。
	LatestVersion string
	// HasUpdate 表示 Latest 比 Current 新。
	HasUpdate bool
	// DevBuild 表示当前是无法比较的开发构建（version == "dev" 或非 semver）。
	//
	// 这种情况下 HasUpdate 恒为 false，但 Latest 仍然有效——GUI 可以显示
	// 「你在跑开发版，最新正式版是 v0.2.1」而不提供「立即更新」按钮。
	DevBuild bool
	// ReleaseURL 是 GitHub 上的 release 页面（人工兜底入口）。
	ReleaseURL string
	// PublishedAt 是 release 发布时间（GitHub 可能返回零值）。
	PublishedAt time.Time
	// Notes 是 release body（Markdown 原文）。
	Notes string
	// Name 是 release 标题。
	Name string
	// Artifact 是当前平台对应的产物名（已按 Kind 与 runtime 解析）。
	Artifact string
	// ArtifactSize 是 release 声明的产物字节数；0 表示未知（老 release 或
	// 镜像没给出资产列表）。known 时用它做下载大小的第三方交叉校验——
	// 镜像返回错误页 HTML 也能当场识破，而不是等到 sha256 才发现。
	ArtifactSize int64
}

// release 是 GitHub API 的响应子集。只声明用到的字段：GitHub 的完整响应
// 有 40+ 字段，全量声明会让「字段拼错」变成静默的零值。
type release struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Body        string    `json:"body"`
	HTMLURL     string    `json:"html_url"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	Assets      []asset   `json:"assets"`
}

type asset struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// Check 查询最新 release 并与当前版本比较。
//
// current 由调用方传入（cmd/*/main.go 的 `var version`）。空串等价于 "dev"。
//
// # dev 版本的处理（own decision，理由）
//
// 开发构建一律**不比较、不提供自动更新**（HasUpdate=false，DevBuild=true）：
//
//   - "dev" 与 git describe 产物（v0.1.0-3-gabcdef-dirty）不是可排序的版本，
//     硬造一个「总是有新版」的结论会让开发者每次打开 GUI 都被提示更新，
//     而且更新会真的把本地构建覆盖成 release 产物——丢掉自己正在改的代码。
//   - 但 Latest 仍然填充：开发者需要知道「正式版走到哪了」，界面可以展示
//     而不给按钮。这样既保住信息，又不制造误操作。
func (u *Updater) Check(ctx context.Context) (Info, error) {
	u.report(Progress{Stage: StageCheck})

	artifact, err := CurrentArtifact(u.opts.Kind)
	if err != nil {
		return Info{}, err
	}

	current := strings.TrimSpace(u.opts.CurrentVersion)
	if current == "" {
		current = "dev"
	}
	info := Info{Current: current, Artifact: artifact}

	// 本地构建（dev / git describe）先标记，再照常填充 Latest：开发者需要
	// 知道"正式版走到哪了"，但不该被提示点击更新（理由见 IsDevBuild）。
	info.DevBuild = IsDevBuild(current)

	rel, err := u.fetchLatest(ctx)
	if err != nil {
		// 即使检查失败，也把「当前版本 + 产物名」带回去：GUI 可以显示
		// 「当前 v0.1.0，检查失败：已限流」，比整块空白有用。
		return info, err
	}

	info.Latest = rel.TagName
	info.Name = rel.Name
	info.Notes = rel.Body
	info.ReleaseURL = rel.HTMLURL
	info.PublishedAt = rel.PublishedAt
	for _, a := range rel.Assets {
		if a.Name == artifact {
			info.ArtifactSize = a.Size
			break
		}
	}

	latestVer, err := ParseVersion(rel.TagName)
	if err != nil {
		return info, fmt.Errorf("update: release tag 无法解析：%w", err)
	}
	info.LatestVersion = strings.TrimPrefix(rel.TagName, "v")

	currentVer, curErr := ParseVersion(current)
	if info.DevBuild || curErr != nil {
		// 本地构建不可比较。不算错误，只是不提供「检查更新」的结论。
		info.DevBuild = true
		return info, nil
	}

	info.HasUpdate = latestVer.Compare(currentVer) > 0
	return info, nil
}

// fetchLatest 拉取 release 元数据，并把 HTTP 层的失败翻译成可分支的错误。
func (u *Updater) fetchLatest(ctx context.Context) (release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.apiBase()+"/repos/"+Repo+"/releases/latest", nil)
	if err != nil {
		return release{}, fmt.Errorf("update: 构造请求: %w", err)
	}
	// Accept 固定 API 版本：GitHub 会在某天切换默认版本，显式声明才不会被
	// 上游的字段改名影响（本项目只用 tag_name/body，但原则不变）。
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "reply9842-update")
	if token := u.token(); token != "" {
		// 认证只影响限额（60 → 5000/小时），不改变响应结构。
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := u.client().Do(req)
	if err != nil {
		return release{}, fmt.Errorf("update: 检查更新失败: %w", err)
	}
	defer resp.Body.Close()

	// 404 有两种语义：仓库不存在（配置错了）或还没有 release（正常）。本包的
	// Repo 是常量，所以只能是后者——但错误信息仍要说明白，避免用户以为是网络问题。
	if resp.StatusCode == http.StatusNotFound {
		return release{}, ErrNoRelease
	}
	if limited, retry := isRateLimited(resp); limited {
		if !retry.IsZero() {
			return release{}, fmt.Errorf("%w（限额重置于 %s）", ErrRateLimited, retry.Format(time.RFC3339))
		}
		return release{}, ErrRateLimited
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return release{}, fmt.Errorf("update: GitHub API 返回 %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	// 限制读取量：正常响应 < 100KB。若上游被劫持返回超大 body，不该把内存吃满。
	var rel release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&rel); err != nil {
		return release{}, fmt.Errorf("update: 解析 release 响应: %w", err)
	}
	if rel.TagName == "" {
		return release{}, fmt.Errorf("update: release 响应缺少 tag_name")
	}
	// latest 端点理论上不会返回 draft/prerelease，但防御一下：
	// 若 GitHub 改了行为，我们宁可报错也不要提示用户升级到预发布版。
	if rel.Draft || rel.Prerelease {
		return release{}, ErrNoRelease
	}
	return rel, nil
}

// isRateLimited 识别限流响应（未认证超额时 GitHub 返回 403，部分网关返回 429）。
//
// 返回的重置时间来自 X-RateLimit-Reset（Unix 秒）。解析失败返回 0，
// 调用方只显示「已限流」——不要因为解析不出时间就丢掉限流这个判定。
func isRateLimited(resp *http.Response) (bool, time.Time) {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return false, time.Time{}
	}
	// 403 也可能是权限问题。只有 X-RateLimit-Remaining: 0 或 Retry-After
	// 存在时才确认是限流——否则会把「仓库被改名」误报成「限流」。
	if resp.Header.Get("Retry-After") != "" {
		return true, time.Time{}
	}
	if resp.Header.Get("X-RateLimit-Remaining") != "0" {
		return false, time.Time{}
	}
	sec, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64)
	if err != nil || sec <= 0 {
		return true, time.Time{}
	}
	return true, time.Unix(sec, 0)
}

// ReleaseURLFor 构造某个 tag 的 release 页面地址（人工下载兜底）。
func ReleaseURLFor(tag string) string {
	return "https://github.com/" + Repo + "/releases/tag/" + tag
}

// DownloadURLFor 构造某个产物的直连下载地址。
//
// 注意这是**不带镜像前缀**的原始 URL：镜像拼接由 download.go 统一处理，
// 保持「URL 构造」与「传输策略」分离，测试才能单独断言拼接规则。
func DownloadURLFor(tag, artifact string) string {
	if !strings.HasPrefix(tag, "v") {
		tag = "v" + tag
	}
	return "https://github.com/" + Repo + "/releases/download/" + tag + "/" + artifact
}
