package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeRelease 是一个可配置的假 release 服务器：
//
//	/<prefix>/<repo>/releases/download/<tag>/<artifact>   → 产物字节
//	/<prefix>/<repo>/releases/download/<tag>/SHA256SUMS.txt → 校验和
//
// 前缀为空表示直连（就是正常的 URL 路径）。
type fakeRelease struct {
	artifact string
	content  []byte
	sums     string // 若为空，按 content 自动生成

	// supportRange=false 时忽略 Range 头，永远回 200 全量。
	// 这是真实世界里 CDN/对象存储的常见行为，必须覆盖。
	supportRange bool

	// truncate 是「服务端只发这么多字节就 EOF」的注入点，用来验证大小校验。
	// 0 表示不截断。
	truncate int

	// omitLength=true 时不发 Content-Length（chunked 传输）。
	omitLength bool

	// requests 记录收到的请求，供断言 Range 头。
	mu       sync.Mutex
	requests []*http.Request
	ranges   []string
}

func (f *fakeRelease) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.Clone(context.Background()))
		f.ranges = append(f.ranges, r.Header.Get("Range"))
		f.mu.Unlock()

		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/"+ChecksumsAsset):
			body := f.sums
			if body == "" {
				body = fmt.Sprintf("%x  %s\n", sha256.Sum256(f.content), f.artifact)
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			_, _ = w.Write([]byte(body))
			return
		case strings.HasSuffix(path, "/"+f.artifact):
			f.serveArtifact(w, r)
			return
		}
		http.NotFound(w, r)
	})
}

func (f *fakeRelease) serveArtifact(w http.ResponseWriter, r *http.Request) {
	body := f.content
	start := 0

	if f.supportRange {
		if rg := r.Header.Get("Range"); strings.HasPrefix(rg, "bytes=") {
			spec := strings.TrimPrefix(rg, "bytes=")
			spec = strings.TrimSuffix(spec, "-")
			if n, err := strconv.Atoi(spec); err == nil {
				if n >= len(body) {
					// 与真实服务端一致的 416 形态
					w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(body)))
					w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
					return
				}
				start = n
				w.Header().Set("Content-Range",
					fmt.Sprintf("bytes %d-%d/%d", start, len(body)-1, len(body)))
				w.Header().Set("Content-Length", strconv.Itoa(len(body)-start))
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write(body[start:])
				return
			}
		}
	}

	// 200 全量
	if !f.omitLength {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	}
	w.WriteHeader(http.StatusOK)
	if f.truncate > 0 && f.truncate < len(body) {
		// 截断：写出更少的字节然后直接结束（模拟代理/链路中断）。
		// 因为声明了完整 Content-Length，客户端会看到 unexpected EOF。
		_, _ = w.Write(body[:f.truncate])
		if hj, ok := w.(http.Hijacker); ok {
			// 硬断开连接，避免 net/http 帮忙补齐/报错。
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
			}
		}
		return
	}
	_, _ = w.Write(body)
}

func (f *fakeRelease) sawRange() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ranges...)
}

// newDownloadUpdater 构造一个把镜像置为「直连」的 Updater（前缀空串）。
func newDownloadUpdater(t *testing.T, srv *httptest.Server, dir string, mirrors []string) *Updater {
	t.Helper()
	opts := Options{
		Kind:           KindCLI,
		CurrentVersion: "v0.1.0",
		Dir:            dir,
		Mirrors:        mirrors,
	}
	u, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	// 让 DownloadURLFor 指向假服务器：repoBase 直接是 srv.URL，
	// 后面拼的 /jixoai/reply9842/releases/download/... 由 handler 用后缀匹配。
	u.opts.repoBase = srv.URL
	return u
}

func testInfo(artifact string, size int64) Info {
	return Info{Latest: "v0.2.1", Artifact: artifact, ArtifactSize: size}
}

func TestDownloadBasic(t *testing.T) {
	content := []byte("#!/bin/sh\necho reply9842 v0.2.1\n")
	f := &fakeRelease{artifact: "cli-darwin-arm64", content: content, supportRange: true}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	dir := t.TempDir()
	u := newDownloadUpdater(t, srv, dir, []string{"direct"})

	var stages []Stage
	path, err := u.Download(context.Background(), testInfo(f.artifact, int64(len(content))), func(p Progress) {
		stages = append(stages, p.Stage)
	})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读下载文件: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("内容不符：%q", got)
	}
	// 落盘在按版本分的目录里
	if !strings.Contains(path, "0.2.1") {
		t.Fatalf("路径应含版本号目录: %s", path)
	}
	// .part 必须已被消费掉
	if _, err := os.Stat(path + ".part"); !os.IsNotExist(err) {
		t.Fatal(".part 应在 rename 后消失")
	}
	// 应报告 download 与 verify 两个阶段
	if !containsStage(stages, StageDownload) || !containsStage(stages, StageVerify) {
		t.Fatalf("进度阶段不全: %v", stages)
	}
	// 下载产物必须可执行
	st, _ := os.Stat(path)
	if st.Mode()&0o111 == 0 {
		t.Fatalf("产物应有执行位，mode=%s", st.Mode())
	}
}

func containsStage(ss []Stage, want Stage) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// 断点续传：预置 .part，断言发出 Range 头且内容正确拼接。
func TestDownloadResume(t *testing.T) {
	content := []byte(strings.Repeat("REPLY9842-PAYLOAD-", 200)) // 多块，确保分多次读
	f := &fakeRelease{artifact: "cli-linux-amd64", content: content, supportRange: true}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	dir := t.TempDir()
	u := newDownloadUpdater(t, srv, dir, []string{"direct"})

	// 预置一半的 .part（模拟上次被打断）
	half := len(content) / 2
	dest := filepath.Join(dir, "0.2.1", f.artifact)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest+".part", content[:half], 0o600); err != nil {
		t.Fatal(err)
	}

	path, err := u.Download(context.Background(), testInfo(f.artifact, int64(len(content))), nil)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}

	got, _ := os.ReadFile(path)
	if string(got) != string(content) {
		t.Fatalf("续传拼接错：长度 %d，期望 %d", len(got), len(content))
	}

	// 断言确实发了 Range（对产物文件的那次请求）
	ranges := f.sawRange()
	var sawArtifactRange bool
	for _, rg := range ranges {
		if rg == fmt.Sprintf("bytes=%d-", half) {
			sawArtifactRange = true
		}
	}
	if !sawArtifactRange {
		t.Fatalf("应发出 Range: bytes=%d-，实际收到 %v", half, ranges)
	}
}

// 服务端不支持 Range（200 全量）时必须从头写，不能把两份拼起来。
func TestDownloadRangeIgnored(t *testing.T) {
	content := []byte(strings.Repeat("ABCDEFGH", 100))
	f := &fakeRelease{artifact: "cli-linux-amd64", content: content, supportRange: false}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	dir := t.TempDir()
	u := newDownloadUpdater(t, srv, dir, []string{"direct"})

	// 预置一份「错的」.part（内容与真实不同），模拟上次下到一半但内容已过时。
	dest := filepath.Join(dir, "0.2.1", f.artifact)
	_ = os.MkdirAll(filepath.Dir(dest), 0o755)
	_ = os.WriteFile(dest+".part", []byte("STALE-PREFIX-CONTENT-THAT-MUST-BE-DISCARDED"), 0o600)

	path, err := u.Download(context.Background(), testInfo(f.artifact, int64(len(content))), nil)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(content) {
		t.Fatalf("200 回退应从头写，实际长度 %d（含旧前缀会超出 %d）", len(got), len(content))
	}
}

// 已有 .part 恰好等于完整内容 → 416，应视为已完成，交给 sha 校验。
func TestDownloadResumeComplete(t *testing.T) {
	content := []byte("complete-payload")
	f := &fakeRelease{artifact: "cli-linux-amd64", content: content, supportRange: true}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	dir := t.TempDir()
	u := newDownloadUpdater(t, srv, dir, []string{"direct"})
	dest := filepath.Join(dir, "0.2.1", f.artifact)
	_ = os.MkdirAll(filepath.Dir(dest), 0o755)
	_ = os.WriteFile(dest+".part", content, 0o600)

	path, err := u.Download(context.Background(), testInfo(f.artifact, 0), nil)
	if err != nil {
		t.Fatalf("已下完的 .part 应被接受: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(content) {
		t.Fatalf("内容不符: %q", got)
	}
}

// 大小不符：服务端声明 100 字节但只发 10 字节 → 必须报 ErrSizeMismatch。
// 这正是那个 SEGV 事故的形态（截断的二进制被当成完整文件部署）。
func TestDownloadTruncated(t *testing.T) {
	content := []byte(strings.Repeat("X", 4096))
	f := &fakeRelease{
		artifact: "cli-linux-amd64", content: content,
		supportRange: false, truncate: 100,
	}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	dir := t.TempDir()
	u := newDownloadUpdater(t, srv, dir, []string{"direct"})

	_, err := u.Download(context.Background(), testInfo(f.artifact, int64(len(content))), nil)
	if err == nil {
		t.Fatal("截断的下载必须报错，不能静默接受")
	}
	if !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("应返回 ErrSizeMismatch，得到 %v", err)
	}
	// 目标文件绝不能存在（.part 保留供续传，但不能被 rename 成成品）
	dest := filepath.Join(dir, "0.2.1", f.artifact)
	if _, statErr := os.Stat(dest); statErr == nil {
		t.Fatal("截断后不应产生目标文件")
	}
}

// 镜像声明的 total 与 release 声明的 AssetSize 不符 → 当场拒绝。
// 防的是「镜像返回自己的 HTML 错误页但状态 200」。
func TestDownloadSizeMismatchAgainstRelease(t *testing.T) {
	html := []byte("<html><body>404 Not Found</body></html>")
	f := &fakeRelease{artifact: "cli-linux-amd64", content: html, supportRange: false}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	dir := t.TempDir()
	u := newDownloadUpdater(t, srv, dir, []string{"direct"})

	// release 声明产物是 9MB，镜像只给 39 字节
	_, err := u.Download(context.Background(), testInfo(f.artifact, 9<<20), nil)
	if err == nil {
		t.Fatal("大小与 release 声明不符必须报错")
	}
	if !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("应返回 ErrSizeMismatch，得到 %v", err)
	}
}

// sha256 不匹配 → 报 ErrChecksum，且不留目标文件、不留 .part。
func TestDownloadChecksumMismatch(t *testing.T) {
	content := []byte("real content")
	f := &fakeRelease{
		artifact: "cli-linux-amd64", content: content,
		supportRange: false,
		// 故意给一个错的校验和
		sums: strings.Repeat("a", 64) + "  cli-linux-amd64\n",
	}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	dir := t.TempDir()
	u := newDownloadUpdater(t, srv, dir, []string{"direct"})

	_, err := u.Download(context.Background(), testInfo(f.artifact, int64(len(content))), nil)
	if !errors.Is(err, ErrChecksum) {
		t.Fatalf("应返回 ErrChecksum，得到 %v", err)
	}
	dest := filepath.Join(dir, "0.2.1", f.artifact)
	if _, statErr := os.Stat(dest); statErr == nil {
		t.Fatal("校验失败绝不能产生目标文件")
	}
	if _, statErr := os.Stat(dest + ".part"); statErr == nil {
		t.Fatal("校验失败必须删掉 .part（否则下次续传把坏前缀当真）")
	}
}

// 校验和文件里没有目标产物 → 也是 ErrChecksum（不能"跳过校验"）。
func TestDownloadChecksumMissingEntry(t *testing.T) {
	content := []byte("x")
	f := &fakeRelease{
		artifact: "cli-linux-amd64", content: content, supportRange: false,
		sums: strings.Repeat("b", 64) + "  some-other-file\n",
	}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	dir := t.TempDir()
	u := newDownloadUpdater(t, srv, dir, []string{"direct"})
	_, err := u.Download(context.Background(), testInfo(f.artifact, int64(len(content))), nil)
	if !errors.Is(err, ErrChecksum) {
		t.Fatalf("缺少条目应报 ErrChecksum（强制校验），得到 %v", err)
	}
}

// 镜像回退：第一个镜像 500，第二个成功。
func TestDownloadMirrorFallback(t *testing.T) {
	content := []byte("payload-from-second-mirror")
	f := &fakeRelease{artifact: "cli-linux-amd64", content: content, supportRange: true}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	dir := t.TempDir()
	// 第一个镜像是「必然失败的地址」：用 srv.URL 的路径变体去撞 404 不如
	// 直接用一个指向同一服务器的坏前缀——让 handler 404。
	badMirror := srv.URL + "/broken"
	u := newDownloadUpdater(t, srv, dir, []string{badMirror, "direct"})

	var mirrors []string
	path, err := u.Download(context.Background(), testInfo(f.artifact, int64(len(content))), func(p Progress) {
		if p.Mirror != "" && (len(mirrors) == 0 || mirrors[len(mirrors)-1] != p.Mirror) {
			mirrors = append(mirrors, p.Mirror)
		}
	})
	if err != nil {
		t.Fatalf("第二个镜像应成功: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(content) {
		t.Fatalf("内容不符: %q", got)
	}
	if len(mirrors) < 1 {
		t.Fatalf("应报告实际使用的镜像标签，得到 %v", mirrors)
	}
}

// 全部镜像失败 → 返回带最后一个错误的聚合错误。
func TestDownloadAllMirrorsFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer srv.Close()

	u := newDownloadUpdater(t, srv, t.TempDir(), []string{"direct", srv.URL + "/x"})
	_, err := u.Download(context.Background(), testInfo("cli-linux-amd64", 0), nil)
	if err == nil {
		t.Fatal("全部失败应报错")
	}
	if !strings.Contains(err.Error(), "全部镜像下载失败") {
		t.Fatalf("错误信息应说明全部镜像失败: %v", err)
	}
}

// 已存在完整且校验通过的文件 → 直接复用，不再发下载请求。
func TestDownloadReuseVerified(t *testing.T) {
	content := []byte("already-downloaded-and-valid")
	f := &fakeRelease{artifact: "cli-linux-amd64", content: content, supportRange: true}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	dir := t.TempDir()
	u := newDownloadUpdater(t, srv, dir, []string{"direct"})
	dest := filepath.Join(dir, "0.2.1", f.artifact)
	_ = os.MkdirAll(filepath.Dir(dest), 0o755)
	_ = os.WriteFile(dest, content, 0o755)
	_ = chmodExecutable(dest)

	path, err := u.Download(context.Background(), testInfo(f.artifact, int64(len(content))), nil)
	if err != nil {
		t.Fatalf("复用应成功: %v", err)
	}
	if path != dest {
		t.Fatalf("应返回已有路径 %s，得到 %s", dest, path)
	}
	// 只应有校验和请求，没有产物下载请求
	f.mu.Lock()
	n := len(f.requests)
	f.mu.Unlock()
	if n != 1 {
		t.Fatalf("复用时只该请求 SHA256SUMS.txt，实际请求 %d 次", n)
	}
}

// 缓存里的文件已损坏（内容被改）→ 不能直接复用，必须重新下载。
func TestDownloadReuseRejectsCorrupt(t *testing.T) {
	content := []byte("correct-content")
	f := &fakeRelease{artifact: "cli-linux-amd64", content: content, supportRange: true}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	dir := t.TempDir()
	u := newDownloadUpdater(t, srv, dir, []string{"direct"})
	dest := filepath.Join(dir, "0.2.1", f.artifact)
	_ = os.MkdirAll(filepath.Dir(dest), 0o755)
	_ = os.WriteFile(dest, []byte("corrupted-content"), 0o755)

	path, err := u.Download(context.Background(), testInfo(f.artifact, int64(len(content))), nil)
	if err != nil {
		t.Fatalf("应重新下载成功: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(content) {
		t.Fatalf("应覆盖成正确内容，得到 %q", got)
	}
}

// ctx 取消：必须返回 context.Canceled，且不继续尝试其它镜像。
func TestDownloadContextCanceled(t *testing.T) {
	block := make(chan struct{})
	started := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ChecksumsAsset) {
			_, _ = w.Write([]byte(strings.Repeat("0", 64) + "  cli-linux-amd64\n"))
			return
		}
		// 通知测试「产物请求已到达」，然后挂住，等测试取消 ctx。
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(block)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	u := newDownloadUpdater(t, srv, t.TempDir(), []string{"direct"})

	go func() {
		<-started
		cancel()
	}()

	_, err := u.Download(ctx, testInfo("cli-linux-amd64", 100), nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("应返回 context.Canceled，得到 %v", err)
	}
}

func TestParseContentRange(t *testing.T) {
	tests := []struct {
		in         string
		start, tot int64
		wantErr    bool
	}{
		{"bytes 100-199/200", 100, 200, false},
		{"bytes 0-0/1", 0, 1, false},
		{"bytes */1234", -1, 1234, false},
		{"bytes 0-99/*", 0, -1, false},
		{"", 0, -1, true},
		{"items 0-99/100", 0, -1, true},
		{"bytes 100-199", 0, -1, true},
		{"bytes abc-199/200", 0, -1, true},
		{"bytes 0-199/xyz", 0, -1, true},
	}
	for _, tc := range tests {
		start, tot, err := parseContentRange(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("%q 应报错，得到 start=%d total=%d", tc.in, start, tot)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%q: %v", tc.in, err)
		}
		if start != tc.start || tot != tc.tot {
			t.Fatalf("%q 得到 (%d,%d)，期望 (%d,%d)", tc.in, start, tot, tc.start, tc.tot)
		}
	}
}

func TestParseChecksums(t *testing.T) {
	// 两种格式 + 空行 + 注释 + 无法识别的行
	text := `
# 这是注释
` + strings.Repeat("a", 64) + `  cli-darwin-arm64
` + strings.Repeat("B", 64) + ` *gui-windows-amd64.exe
` + strings.Repeat("c", 64) + ` server-linux-amd64
garbage line without hash
`
	sums := ParseChecksums(text)
	if len(sums) != 3 {
		t.Fatalf("应解析出 3 项，得到 %v", sums)
	}
	if sums["cli-darwin-arm64"] != strings.Repeat("a", 64) {
		t.Fatalf("文本模式解析错: %v", sums)
	}
	// 二进制模式（* 前缀）也要识别，且 hex 统一小写
	if sums["gui-windows-amd64.exe"] != strings.Repeat("b", 64) {
		t.Fatalf("二进制模式应识别并小写化: %v", sums)
	}
	if sums["server-linux-amd64"] != strings.Repeat("c", 64) {
		t.Fatalf("最后一行解析错: %v", sums)
	}
}

func TestFileSHA256(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	content := []byte("hello")
	_ = os.WriteFile(p, content, 0o644)
	got, err := fileSHA256(p)
	if err != nil {
		t.Fatal(err)
	}
	want := hex.EncodeToString(func() []byte { s := sha256.Sum256(content); return s[:] }())
	if got != want {
		t.Fatalf("得到 %s，期望 %s", got, want)
	}
}

func TestPercent(t *testing.T) {
	tests := []struct {
		got, total int64
		want       int
	}{
		{0, 100, 0},
		{50, 100, 50},
		{100, 100, 100},
		{0, 0, -1}, // 未知总长
		{5, 0, -1},
		{150, 100, 100}, // 防御：不显示 150%
	}
	for _, tc := range tests {
		if got := percent(tc.got, tc.total); got != tc.want {
			t.Fatalf("percent(%d,%d)=%d，期望 %d", tc.got, tc.total, got, tc.want)
		}
	}
}

// ---- 续传基线不可用时的 refetch 语义（防"用户永远卡在坏 .part 上"）----

// .part 长度超过服务端文件（旧版本残留 / 别的产物）→ 416 → 删掉重下成功。
//
// 这是修复过的缺陷：早先版本在这里直接 return error，于是每次重试都读到
// 同一份超长 .part、每次 416、每次失败，用户永远无法恢复（他并不知道要
// 去缓存目录手工删一个隐藏文件）。
func TestDownloadStaleOversizedPartRefetches(t *testing.T) {
	content := []byte("the-real-content-here")
	f := &fakeRelease{artifact: "cli-linux-amd64", content: content, supportRange: true}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	dir := t.TempDir()
	u := newDownloadUpdater(t, srv, dir, []string{"direct"})

	// .part 比真实文件长：416 的典型来源
	dest := filepath.Join(dir, "0.2.1", f.artifact)
	_ = os.MkdirAll(filepath.Dir(dest), 0o755)
	stale := make([]byte, len(content)+500)
	for i := range stale {
		stale[i] = 'X'
	}
	_ = os.WriteFile(dest+".part", stale, 0o600)

	path, err := u.Download(context.Background(), testInfo(f.artifact, int64(len(content))), nil)
	if err != nil {
		t.Fatalf("坏 .part 应被自动清掉重下，得到错误: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != string(content) {
		t.Fatalf("重下内容不对: %q", got)
	}
	// 断言真的重发了一次不带 Range 的请求
	ranges := f.sawRange()
	var sawNoRangeAfterRange bool
	for i, rg := range ranges {
		if strings.HasPrefix(rg, "bytes=") && i+1 < len(ranges) && ranges[i+1] == "" {
			sawNoRangeAfterRange = true
		}
	}
	if !sawNoRangeAfterRange {
		t.Fatalf("应在 Range 失败后重发无 Range 请求，实际 Range 序列: %v", ranges)
	}
}

// 服务端返回的 206 起点与本地 .part 不符 → 不能追加（会拼出内容错误的文件）
// → 同样走 refetch。
func TestDownloadPartialStartMismatchRefetches(t *testing.T) {
	content := []byte(strings.Repeat("PAYLOAD-", 64))
	// 自定义 handler：对 Range 请求返回一个**起点不对**的 206。
	var mu sync.Mutex
	var ranges []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ChecksumsAsset) {
			_, _ = w.Write([]byte(fmt.Sprintf("%x  cli-linux-amd64\n", sha256.Sum256(content))))
			return
		}
		rg := r.Header.Get("Range")
		mu.Lock()
		ranges = append(ranges, rg)
		mu.Unlock()

		if rg == "" {
			w.Header().Set("Content-Length", strconv.Itoa(len(content)))
			_, _ = w.Write(content)
			return
		}
		// 故意给一个与请求起点不符的区间（从 0 开始）
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(content)-1, len(content)))
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(content)
	}))
	defer srv.Close()

	dir := t.TempDir()
	u := newDownloadUpdater(t, srv, dir, []string{"direct"})

	dest := filepath.Join(dir, "0.2.1", "cli-linux-amd64")
	_ = os.MkdirAll(filepath.Dir(dest), 0o755)
	_ = os.WriteFile(dest+".part", content[:100], 0o600)

	path, err := u.Download(context.Background(), testInfo("cli-linux-amd64", int64(len(content))), nil)
	if err != nil {
		t.Fatalf("起点不符应重下而不是失败: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(content) {
		t.Fatalf("重下内容不对（长度 %d，期望 %d）", len(got), len(content))
	}
	mu.Lock()
	defer mu.Unlock()
	// 应先带 Range 撞一次，再不带 Range 成功
	if len(ranges) < 2 || ranges[0] == "" || ranges[len(ranges)-1] != "" {
		t.Fatalf("请求序列不符（应先 Range 后无 Range）: %v", ranges)
	}
}

// sha256 失败后必须换下一个镜像重试，而不是整体失败。
//
// 缺陷背景：镜像可能返回错误内容（被劫持 / HTML 错误页 / 缓存了别的版本），
// 换一个镜像正好能救；早先的实现校验失败就整体 return，等于白白浪费回退链。
func TestDownloadChecksumFailureRetriesNextMirror(t *testing.T) {
	content := []byte("good-content-from-mirror-2")
	correctSum := fmt.Sprintf("%x", sha256.Sum256(content))

	// 镜像 1（坏）：内容与校验和都不对
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ChecksumsAsset) {
			_, _ = w.Write([]byte(strings.Repeat("f", 64) + "  cli-linux-amd64\n"))
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		_, _ = w.Write(content)
	}))
	defer bad.Close()

	// 镜像 2（好）：内容与校验和都正确
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ChecksumsAsset) {
			_, _ = w.Write([]byte(correctSum + "  cli-linux-amd64\n"))
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		_, _ = w.Write(content)
	}))
	defer good.Close()

	dir := t.TempDir()
	// 第一个镜像用 "direct" 字面量（空串元素会被 CSV 规则跳过）。
	u := newDownloadUpdater(t, bad, dir, []string{"direct", good.URL + "/"})

	path, err := u.Download(context.Background(), testInfo("cli-linux-amd64", int64(len(content))), nil)
	if err != nil {
		t.Fatalf("校验失败后应换镜像成功，得到: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != string(content) {
		t.Fatalf("内容不对: %q", got)
	}
}

// 所有镜像都校验失败 → 返回 ErrChecksum（可分支的哨兵），且不留任何文件。
func TestDownloadAllChecksumFail(t *testing.T) {
	content := []byte("content")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ChecksumsAsset) {
			_, _ = w.Write([]byte(strings.Repeat("0", 64) + "  cli-linux-amd64\n"))
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		_, _ = w.Write(content)
	}))
	defer srv.Close()

	dir := t.TempDir()
	u := newDownloadUpdater(t, srv, dir, []string{"direct", srv.URL + "/a"})

	_, err := u.Download(context.Background(), testInfo("cli-linux-amd64", int64(len(content))), nil)
	if !errors.Is(err, ErrChecksum) {
		t.Fatalf("应返回 ErrChecksum，得到 %v", err)
	}
	dest := filepath.Join(dir, "0.2.1", "cli-linux-amd64")
	for _, p := range []string{dest, dest + ".part"} {
		if _, statErr := os.Stat(p); statErr == nil {
			t.Fatalf("校验全失败时不该留下 %s", p)
		}
	}
}

// 校验和不能只信「提供字节的那个镜像」：任何一份镜像的校验和与本地字节
// 相符就算过。
//
// 缺陷背景（本包实测踩到）：早先的实现固定从 mirrors[0] 取 SHA256SUMS，
// 只要那份是坏的，好镜像的字节就会被判死，且「换镜像重试」时又拿同一份
// 坏校验和再判一次——回退链完全失效。
//
// 本用例的关键构造：**提供字节的镜像自身的校验和是错的**（它的 txt 缓存
// 落后于二进制），而另一个镜像的校验和是对的。只有"逐份尝试"才能通过，
// 「只信 servedBy」会失败。
func TestDownloadChecksumCrossCheckedAcrossMirrors(t *testing.T) {
	content := []byte("correct-bytes")
	correctSum := fmt.Sprintf("%x", sha256.Sum256(content))

	// 镜像 A：能给出正确的校验和，但内容不可用（模拟只代理了 txt 的场景）
	mirrorA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ChecksumsAsset) {
			_, _ = w.Write([]byte(correctSum + "  cli-linux-amd64\n"))
			return
		}
		_, _ = w.Write([]byte("garbage-from-a"))
	}))
	defer mirrorA.Close()

	// 镜像 B：内容正确，但它自己的校验和是错的（txt 缓存落后）
	mirrorB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ChecksumsAsset) {
			_, _ = w.Write([]byte(strings.Repeat("f", 64) + "  cli-linux-amd64\n"))
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		_, _ = w.Write(content)
	}))
	defer mirrorB.Close()

	dir := t.TempDir()
	// 镜像 A 放在前面：它的内容是垃圾，会失败；接着 B 提供正确字节，
	// 但 B 的校验和是错的 —— 必须靠 A 的校验和通过。
	u := newDownloadUpdater(t, mirrorA, dir, []string{"direct", mirrorB.URL + "/"})
	// 两个镜像都参与：A 提供校验和，B 提供字节。
	u.mirrors = []string{mirrorA.URL + "/", mirrorB.URL + "/"}

	path, err := u.Download(context.Background(), testInfo("cli-linux-amd64", int64(len(content))), nil)
	if err != nil {
		t.Fatalf("应靠交叉核验成功，得到: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(content) {
		t.Fatalf("内容不对: %q", got)
	}
}

// 提供字节的镜像不提供 SHA256SUMS.txt（只代理二进制），另一镜像可以。
//
// 这是「逐份尝试校验和」的另一个真实收益：镜像对附件的支持并不总是一致
// （有的按后缀白名单代理）。
func TestDownloadChecksumFallbackWhenSourceLacksSums(t *testing.T) {
	content := []byte("bytes-from-binary-only-mirror")
	correctSum := fmt.Sprintf("%x", sha256.Sum256(content))

	binaryOnly := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ChecksumsAsset) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		_, _ = w.Write(content)
	}))
	defer binaryOnly.Close()

	sumsOnly := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ChecksumsAsset) {
			_, _ = w.Write([]byte(correctSum + "  cli-linux-amd64\n"))
			return
		}
		http.NotFound(w, r)
	}))
	defer sumsOnly.Close()

	dir := t.TempDir()
	u := newDownloadUpdater(t, binaryOnly, dir, []string{"direct"})
	u.mirrors = []string{binaryOnly.URL + "/", sumsOnly.URL + "/"}

	if _, err := u.Download(context.Background(), testInfo("cli-linux-amd64", int64(len(content))), nil); err != nil {
		t.Fatalf("应从另一个镜像取到校验和，得到: %v", err)
	}
}

func TestChecksumMirrorOrder(t *testing.T) {
	mirrors := []string{"https://a/", "https://b/", ""}
	// servedBy 排最前（先信它），其余保持原顺序
	got := checksumMirrorOrder(mirrors, "https://b/")
	want := []string{"https://b/", "https://a/", ""}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("得到 %v，期望 %v", got, want)
	}
	// 不知道来源（复用缓存文件）→ 原顺序
	got = checksumMirrorOrder(mirrors, "")
	if fmt.Sprint(got) != fmt.Sprint(mirrors) {
		t.Fatalf("得到 %v，期望原顺序 %v", got, mirrors)
	}
	// 单镜像：返回它自己
	got = checksumMirrorOrder([]string{"https://a/"}, "https://a/")
	if fmt.Sprint(got) != fmt.Sprint([]string{"https://a/"}) {
		t.Fatalf("单镜像应返回自身，得到 %v", got)
	}
	// servedBy 不在列表里也不能丢（复用缓存且列表无直连的情况）
	got = checksumMirrorOrder([]string{"https://a/"}, "https://z/")
	if fmt.Sprint(got) != fmt.Sprint([]string{"https://z/", "https://a/"}) {
		t.Fatalf("得到 %v", got)
	}
}
