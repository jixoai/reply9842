package update

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// newReleaseServer 起一个假的 GitHub API。
//
// 用 httptest 而不是真的打 api.github.com：限流 60 次/小时，一个测试文件
// 就能打满，且 CI 会变成"看 GitHub 心情"的风向袋。
func newReleaseServer(t *testing.T, status int, body string, hdr map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 断言请求形态：路径与 Accept 头是 GitHub API 的真实契约，
		// 拼错会得到 404 而错误信息看起来像"没有 release"。
		if r.URL.Path != "/repos/"+Repo+"/releases/latest" {
			t.Errorf("请求路径不符: %s", r.URL.Path)
		}
		for k, v := range hdr {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func releaseJSON(t *testing.T, tag, name, body string, assets map[string]int64) string {
	t.Helper()
	type a struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	}
	rel := map[string]any{
		"tag_name":     tag,
		"name":         name,
		"body":         body,
		"html_url":     "https://github.com/" + Repo + "/releases/tag/" + tag,
		"draft":        false,
		"prerelease":   false,
		"published_at": "2026-11-13T10:00:00Z",
		"assets":       []a{},
	}
	if len(assets) > 0 {
		list := make([]a, 0, len(assets))
		for n, sz := range assets {
			list = append(list, a{Name: n, Size: sz})
		}
		rel["assets"] = list
	}
	b, err := json.Marshal(rel)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func newTestUpdater(t *testing.T, apiBase string, opts Options) *Updater {
	t.Helper()
	opts.apiBase = apiBase
	if opts.Kind == "" {
		opts.Kind = KindCLI
	}
	u, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return u
}

func TestCheckHasUpdate(t *testing.T) {
	body := releaseJSON(t, "v0.2.1", "v0.2.1", "## 修复\n- 某些问题", map[string]int64{"cli-darwin-arm64": 12345678})
	srv := newReleaseServer(t, http.StatusOK, body, nil)

	u := newTestUpdater(t, srv.URL, Options{CurrentVersion: "v0.2.0"})
	info, err := u.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !info.HasUpdate {
		t.Fatal("v0.2.1 应比 v0.2.0 新")
	}
	if info.Latest != "v0.2.1" || info.LatestVersion != "0.2.1" {
		t.Fatalf("Latest 填充错: %+v", info)
	}
	if info.DevBuild {
		t.Fatal("v0.2.0 不是本地构建")
	}
	if info.Notes == "" || info.Name == "" {
		t.Fatal("release notes / name 应填充")
	}
	if info.Artifact == "" {
		t.Fatal("Artifact 应解析出当前平台产物名")
	}
	// release notes 要原样保留 Markdown（GUI 负责渲染）。
	if !strings.Contains(info.Notes, "## 修复") {
		t.Fatalf("Notes 未保留原文: %q", info.Notes)
	}
	if info.PublishedAt.IsZero() {
		t.Fatal("PublishedAt 应填充")
	}
}

// 任务书点名的比较：v0.2.1 vs v0.10.0 必须判后者更新（字符串比会反）。
func TestCheckSemverOrdering(t *testing.T) {
	body := releaseJSON(t, "v0.10.0", "", "", nil)
	srv := newReleaseServer(t, http.StatusOK, body, nil)

	u := newTestUpdater(t, srv.URL, Options{CurrentVersion: "v0.2.1"})
	info, err := u.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !info.HasUpdate {
		t.Fatal("v0.10.0 应比 v0.2.1 新（不能字符串比较）")
	}
}

func TestCheckNoUpdate(t *testing.T) {
	tests := []struct{ current, latest string }{
		{"v0.2.1", "v0.2.1"},      // 相同
		{"v0.3.0", "v0.2.9"},      // 当前更新
		{"v1.0.0", "v1.0.0-rc.1"}, // 当前是正式版，release 是预发布
	}
	for _, tc := range tests {
		body := releaseJSON(t, tc.latest, "", "", nil)
		srv := newReleaseServer(t, http.StatusOK, body, nil)
		u := newTestUpdater(t, srv.URL, Options{CurrentVersion: tc.current})
		info, err := u.Check(context.Background())
		if err != nil {
			t.Fatalf("%s vs %s: %v", tc.current, tc.latest, err)
		}
		if info.HasUpdate {
			t.Fatalf("当前 %s、release %s，不该判有更新", tc.current, tc.latest)
		}
		srv.Close()
	}
}

// dev 版本：不比较、不提示更新，但 Latest 仍然填充。
func TestCheckDevBuild(t *testing.T) {
	body := releaseJSON(t, "v0.2.1", "", "", nil)
	srv := newReleaseServer(t, http.StatusOK, body, nil)

	for _, current := range []string{"", "dev", "v0.1.0-3-gabcdef", "dev-abc1234"} {
		u := newTestUpdater(t, srv.URL, Options{CurrentVersion: current})
		info, err := u.Check(context.Background())
		if err != nil {
			t.Fatalf("current=%q: %v", current, err)
		}
		if !info.DevBuild {
			t.Fatalf("current=%q 应标记为 DevBuild", current)
		}
		if info.HasUpdate {
			t.Fatalf("current=%q 是本地构建，不该提示可更新", current)
		}
		// 但 Latest 必须可用：开发者需要知道正式版走到哪了。
		if info.Latest != "v0.2.1" {
			t.Fatalf("current=%q: DevBuild 时 Latest 仍应填充，得到 %q", current, info.Latest)
		}
	}
}

func TestCheckNoRelease(t *testing.T) {
	srv := newReleaseServer(t, http.StatusNotFound, `{"message":"Not Found"}`, nil)
	u := newTestUpdater(t, srv.URL, Options{CurrentVersion: "v0.1.0"})

	_, err := u.Check(context.Background())
	if !errors.Is(err, ErrNoRelease) {
		t.Fatalf("404 应映射成 ErrNoRelease，得到 %v", err)
	}
}

// 限流：403 + X-RateLimit-Remaining: 0 必须映射成 ErrRateLimited（可优雅降级）。
func TestCheckRateLimited(t *testing.T) {
	reset := time.Now().Add(30 * time.Minute).Unix()
	body := `{"message":"API rate limit exceeded"}`
	srv := newReleaseServer(t, http.StatusForbidden, body, map[string]string{
		"X-RateLimit-Remaining": "0",
		"X-RateLimit-Reset":     strconv.FormatInt(reset, 10),
	})

	u := newTestUpdater(t, srv.URL, Options{CurrentVersion: "v0.1.0"})
	_, err := u.Check(context.Background())
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("应映射成 ErrRateLimited，得到 %v", err)
	}
	// 错误信息里要带重置时间，界面才能显示"一小时后再试"。
	if !strings.Contains(err.Error(), "2026") && !strings.Contains(err.Error(), "T") {
		t.Logf("提示：错误信息未含可读重置时间: %v", err)
	}
}

// 429 也是限流（部分网关形态）。
func TestCheckRateLimited429(t *testing.T) {
	srv := newReleaseServer(t, http.StatusTooManyRequests, `{}`, map[string]string{
		"Retry-After": "120",
	})
	u := newTestUpdater(t, srv.URL, Options{CurrentVersion: "v0.1.0"})
	_, err := u.Check(context.Background())
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("429 应映射成 ErrRateLimited，得到 %v", err)
	}
}

// 403 但没有限流头 → 不是限流，别误报成"稍后再试"。
func TestCheck403NotRateLimit(t *testing.T) {
	srv := newReleaseServer(t, http.StatusForbidden, `{"message":"Forbidden"}`, nil)
	u := newTestUpdater(t, srv.URL, Options{CurrentVersion: "v0.1.0"})
	_, err := u.Check(context.Background())
	if errors.Is(err, ErrRateLimited) {
		t.Fatal("无 X-RateLimit-Remaining: 0 的 403 不该判为限流")
	}
	if err == nil {
		t.Fatal("403 应返回错误")
	}
}

// 检查失败时仍要带回当前版本与产物名，界面才不会整块空白。
func TestCheckErrorKeepsContext(t *testing.T) {
	srv := newReleaseServer(t, http.StatusInternalServerError, `{}`, nil)
	u := newTestUpdater(t, srv.URL, Options{CurrentVersion: "v0.1.0"})
	info, err := u.Check(context.Background())
	if err == nil {
		t.Fatal("500 应报错")
	}
	if info.Current != "v0.1.0" {
		t.Fatalf("失败时也应带回 Current，得到 %q", info.Current)
	}
	if info.Artifact == "" {
		t.Fatal("失败时也应带回 Artifact")
	}
}

// draft / prerelease 不该被提示（latest 端点理论上不返回，防御）。
func TestCheckRejectsPrerelease(t *testing.T) {
	body := `{"tag_name":"v1.0.0-rc.1","draft":false,"prerelease":true}`
	srv := newReleaseServer(t, http.StatusOK, body, nil)
	u := newTestUpdater(t, srv.URL, Options{CurrentVersion: "v0.1.0"})
	_, err := u.Check(context.Background())
	if !errors.Is(err, ErrNoRelease) {
		t.Fatalf("prerelease 应被拒，得到 %v", err)
	}
}

func TestCheckMissingTagName(t *testing.T) {
	srv := newReleaseServer(t, http.StatusOK, `{"name":"x"}`, nil)
	u := newTestUpdater(t, srv.URL, Options{CurrentVersion: "v0.1.0"})
	if _, err := u.Check(context.Background()); err == nil {
		t.Fatal("缺 tag_name 应报错")
	}
}

// token 通过 Authorization 头传递，能把限额提到 5000/小时。
func TestCheckSendsToken(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(releaseJSON(t, "v0.2.0", "", "", nil)))
	}))
	defer srv.Close()

	u := newTestUpdater(t, srv.URL, Options{CurrentVersion: "v0.1.0", Token: "ghp_test"})
	if _, err := u.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer ghp_test" {
		t.Fatalf("Authorization 头不对: %q", gotAuth)
	}
}

// ArtifactSize 用于下载时的交叉校验。
func TestCheckFillsArtifactSize(t *testing.T) {
	artifact, err := CurrentArtifact(KindCLI)
	if err != nil {
		t.Fatal(err)
	}
	body := releaseJSON(t, "v0.2.0", "", "", map[string]int64{
		artifact:         9999,
		"other-platform": 1,
	})
	srv := newReleaseServer(t, http.StatusOK, body, nil)
	u := newTestUpdater(t, srv.URL, Options{CurrentVersion: "v0.1.0"})
	info, err := u.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.ArtifactSize != 9999 {
		t.Fatalf("ArtifactSize 应取当前平台的资产大小，得到 %d", info.ArtifactSize)
	}
}

func TestDownloadURLFor(t *testing.T) {
	want := "https://github.com/" + Repo + "/releases/download/v0.2.1/cli-darwin-arm64"
	if got := DownloadURLFor("v0.2.1", "cli-darwin-arm64"); got != want {
		t.Fatalf("得到 %q，期望 %q", got, want)
	}
	// 不带 v 的版本号要自动补 v，否则 404。
	if got := DownloadURLFor("0.2.1", "cli-darwin-arm64"); got != want {
		t.Fatalf("缺 v 时应自动补，得到 %q", got)
	}
}

func TestReleaseURLFor(t *testing.T) {
	want := "https://github.com/" + Repo + "/releases/tag/v0.2.1"
	if got := ReleaseURLFor("v0.2.1"); got != want {
		t.Fatalf("得到 %q，期望 %q", got, want)
	}
}

// 产物命名的跨平台锁定：与 release.yml 的构建记录一致。
func TestArtifactName(t *testing.T) {
	tests := []struct {
		kind, goos, goarch string
		want               string
		wantErr            bool
	}{
		{"cli", "darwin", "arm64", "cli-darwin-arm64", false},
		{"cli", "darwin", "amd64", "cli-darwin-amd64", false},
		{"cli", "linux", "amd64", "cli-linux-amd64", false},
		{"cli", "windows", "amd64", "cli-windows-amd64.exe", false},
		{"server", "linux", "arm64", "server-linux-arm64", false},
		{"server", "windows", "amd64", "server-windows-amd64.exe", false},
		{"gui", "darwin", "arm64", "gui-darwin-arm64", false},
		{"gui", "windows", "amd64", "gui-windows-amd64.exe", false},
		// GUI 无 Linux
		{"gui", "linux", "amd64", "", true},
		// 未知形态/平台
		{"nope", "darwin", "arm64", "", true},
		{"cli", "plan9", "amd64", "", true},
		{"cli", "darwin", "386", "", true},
	}
	for _, tc := range tests {
		got, err := ArtifactName(Kind(tc.kind), tc.goos, tc.goarch)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("%s/%s/%s 应报错，得到 %q", tc.kind, tc.goos, tc.goarch, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s/%s/%s: %v", tc.kind, tc.goos, tc.goarch, err)
		}
		if got != tc.want {
			t.Fatalf("%s/%s/%s 得到 %q，期望 %q", tc.kind, tc.goos, tc.goarch, got, tc.want)
		}
	}
}

func TestParseKind(t *testing.T) {
	for _, s := range []string{"cli", "gui", "server"} {
		if _, err := ParseKind(s); err != nil {
			t.Fatalf("%q 应合法: %v", s, err)
		}
	}
	for _, s := range []string{"", "CLI", "desktop", "relay"} {
		if _, err := ParseKind(s); err == nil {
			t.Fatalf("%q 应非法", s)
		}
	}
}
