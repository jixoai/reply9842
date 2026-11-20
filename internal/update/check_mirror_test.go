package update

// 版本检查的限流兜底：
//   - 直连 api.github.com 被限流（403 + X-RateLimit-Remaining: 0）时，
//     用镜像前缀再试一次 API 路径；
//   - 镜像请求绝不带 Authorization（token 不能交给第三方）；
//   - 直连 404 是权威结论，不应再去问镜像。

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

const mirrorReleaseJSON = `{
  "tag_name": "v9.9.9",
  "name": "mirror release",
  "body": "from mirror",
  "html_url": "https://example.invalid/releases/v9.9.9",
  "published_at": "2026-01-02T03:04:05Z",
  "assets": []
}`

func TestCheckFallsBackToAPIMirror(t *testing.T) {
	directAuth := ""
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		directAuth = r.Header.Get("Authorization")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", "4102444800")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"API rate limit exceeded"}`)
	}))
	defer direct.Close()

	mirrorAuth := ""
	mirrorHits := 0
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mirrorHits++
		mirrorAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, mirrorReleaseJSON)
	}))
	defer mirror.Close()

	u, err := New(Options{
		Kind:           KindGUI,
		CurrentVersion: "v0.1.0",
		Token:          "secret",
		Mirrors:        []string{mirror.URL + "/"},
		apiBase:        direct.URL,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	info, err := u.Check(t.Context())
	if err != nil {
		t.Fatalf("Check 应通过镜像成功，实际: %v", err)
	}
	if info.Latest != "v9.9.9" || !info.HasUpdate {
		t.Fatalf("info = %+v，期望 v9.9.9 / HasUpdate", info)
	}
	if mirrorHits != 1 {
		t.Fatalf("镜像命中 %d 次，期望 1", mirrorHits)
	}
	if directAuth != "Bearer secret" {
		t.Fatalf("直连应携带 token，实际 Authorization=%q", directAuth)
	}
	if mirrorAuth != "" {
		t.Fatalf("镜像请求不得携带 Authorization，实际 %q", mirrorAuth)
	}
}

func TestCheckDirect404DoesNotTryMirror(t *testing.T) {
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	}))
	defer direct.Close()

	mirrorHits := 0
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mirrorHits++
		_, _ = io.WriteString(w, mirrorReleaseJSON)
	}))
	defer mirror.Close()

	u, err := New(Options{
		Kind:           KindGUI,
		CurrentVersion: "v0.1.0",
		Mirrors:        []string{mirror.URL + "/"},
		apiBase:        direct.URL,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = u.Check(t.Context())
	if !errors.Is(err, ErrNoRelease) {
		t.Fatalf("直连 404 应返回 ErrNoRelease，实际: %v", err)
	}
	if mirrorHits != 0 {
		t.Fatalf("直连 404 后不应再请求镜像，实际命中 %d 次", mirrorHits)
	}
}
