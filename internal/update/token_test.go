package update

import (
	"context"
	"errors"
	"testing"
)

func TestTokenPrecedence(t *testing.T) {
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")

	old := ghAuthToken
	t.Cleanup(func() { ghAuthToken = old })

	ghCalls := 0
	ghAuthToken = func(context.Context) (string, error) {
		ghCalls++
		return "gh-token", nil
	}

	explicit := &Updater{opts: Options{Token: " explicit "}}
	if got := explicit.token(); got != "explicit" {
		t.Fatalf("显式 Token 优先，实际 %q", got)
	}

	t.Setenv("GH_TOKEN", "gh-env")
	if got := (&Updater{}).token(); got != "gh-env" {
		t.Fatalf("GH_TOKEN 应优先于 gh CLI，实际 %q", got)
	}

	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "git-env")
	if got := (&Updater{}).token(); got != "git-env" {
		t.Fatalf("GITHUB_TOKEN 应优先于 gh CLI，实际 %q", got)
	}

	t.Setenv("GITHUB_TOKEN", "")
	if got := (&Updater{}).token(); got != "gh-token" {
		t.Fatalf("无环境变量时应回退 gh CLI，实际 %q", got)
	}
	if ghCalls == 0 {
		t.Fatal("未调用 gh CLI 兜底")
	}
}

func TestLocalGHTokenFailureIsEmpty(t *testing.T) {
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	old := ghAuthToken
	t.Cleanup(func() { ghAuthToken = old })
	ghAuthToken = func(context.Context) (string, error) {
		return "", errors.New("not logged in")
	}
	if got := (&Updater{}).token(); got != "" {
		t.Fatalf("gh 未登录时应返回空串，实际 %q", got)
	}
}
