package relayclient

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jixoai/reply9842/internal/stats"
)

// TestSettingsRoundTrip 偏好写盘 → 读回，值必须一模一样。
func TestSettingsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	want := Settings{
		RetainSec: int64(7 * 24 * time.Hour / time.Second),
		WindowSec: int64(6 * time.Hour / time.Second),
	}
	if err := SaveSettings(path, want); err != nil {
		t.Fatal(err)
	}
	got := LoadSettings(path)
	if got != want {
		t.Fatalf("读回 %+v，期望 %+v", got, want)
	}
}

// TestSettingsDefaultsWhenMissing 文件不存在时用默认值，不报错。
func TestSettingsDefaultsWhenMissing(t *testing.T) {
	got := LoadSettings(filepath.Join(t.TempDir(), "nope.json"))
	want := DefaultSettings()
	if got != want {
		t.Fatalf("缺文件时 %+v，期望默认 %+v", got, want)
	}
}

// TestSettingsSnapsToOptions 手工编辑出的非法值被吸附到最近的档位，
// 否则下拉框会显示一个不存在的选项。
func TestSettingsSnapsToOptions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	// 37 分钟 → 最近的保留档是 30 分钟；2 分钟 → 最近的显示档是 1 分钟
	os.WriteFile(path, []byte(`{"retain_sec":2220,"window_sec":120}`), 0o644)
	got := LoadSettings(path)
	if got.Retain() != 30*time.Minute {
		t.Errorf("retain=%s，期望吸附到 30 分钟", got.Retain())
	}
	if got.Window() != 1*time.Minute {
		t.Errorf("window=%s，期望吸附到 1 分钟", got.Window())
	}
}

// TestSettingsWindowClampedToRetain 显示区间不能超过保留时长（硬约束）。
func TestSettingsWindowClampedToRetain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	os.WriteFile(path, []byte(`{"retain_sec":3600,"window_sec":86400}`), 0o644)
	got := LoadSettings(path)
	if got.Window() > got.Retain() {
		t.Fatalf("window=%s 超过了 retain=%s", got.Window(), got.Retain())
	}
	if got.Window() != 1*time.Hour {
		t.Errorf("window=%s，期望被压到 1 小时（=保留时长）", got.Window())
	}
}

// TestSettingsCorruptFallsBack 损坏的 JSON 退回默认值（偏好读不出来不该拖垮启动）。
func TestSettingsCorruptFallsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	os.WriteFile(path, []byte(`{not json`), 0o644)
	if got := LoadSettings(path); got != DefaultSettings() {
		t.Errorf("损坏文件时 %+v，期望默认值", got)
	}
}

// TestOptionIndex 六档选项的下标查找与吸附。
func TestOptionIndex(t *testing.T) {
	if len(RetainOptions) != 6 {
		t.Fatalf("保留档位 %d 个，需求是 6 档", len(RetainOptions))
	}
	if len(WindowOptions) != 6 {
		t.Fatalf("显示档位 %d 个，需求是 6 档", len(WindowOptions))
	}
	// 每一档都必须能找回自己的下标（下拉框选中态依赖这个往返）
	for i, d := range RetainOptions {
		if got := OptionIndex(d, RetainOptions); got != i {
			t.Errorf("RetainOptions[%d]=%s 找回下标 %d", i, d, got)
		}
	}
	for i, d := range WindowOptions {
		if got := OptionIndex(d, WindowOptions); got != i {
			t.Errorf("WindowOptions[%d]=%s 找回下标 %d", i, d, got)
		}
	}
	// 需求里明确列出的六档顺序
	wantRetain := []time.Duration{
		30 * time.Minute, time.Hour, 6 * time.Hour,
		24 * time.Hour, 7 * 24 * time.Hour, 30 * 24 * time.Hour,
	}
	for i, d := range wantRetain {
		if RetainOptions[i] != d {
			t.Errorf("RetainOptions[%d]=%s，期望 %s", i, RetainOptions[i], d)
		}
	}
	wantWindow := []time.Duration{
		time.Minute, 5 * time.Minute, 30 * time.Minute,
		time.Hour, 6 * time.Hour, 24 * time.Hour,
	}
	for i, d := range wantWindow {
		if WindowOptions[i] != d {
			t.Errorf("WindowOptions[%d]=%s，期望 %s", i, WindowOptions[i], d)
		}
	}
}

// TestSnapshotSurvivesStop 统计器的生命周期与监听解耦：
// 停止监听后快照仍然是磁盘上的真实历史，不是零值。
func TestSnapshotSurvivesStop(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Listen: "127.0.0.1:0", Transport: "tcp",
		LogPath:   filepath.Join(dir, "client.jsonl"),
		StatsPath: filepath.Join(dir, "stats.json"),
	}
	r := New(cfg)
	// 直接走统计器写入（不发请求：这里测的是生命周期，不是转发）
	col, err := r.stats()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		col.Observe(&stats.Record{Status: 200, UpRaw: 100, UpWire: 10})
	}

	if err := r.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := r.Stop(nil); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	agg := r.Snapshot()
	if agg.Requests != 3 {
		t.Errorf("停止后 requests=%d，期望 3（统计不随停止清零）", agg.Requests)
	}
	r.Close()
}

// TestSetStatsSettingsApplies 运行中改两个时间跨度也生效（不碰监听）。
func TestSetStatsSettingsApplies(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{
		Listen: "127.0.0.1:0", Transport: "tcp",
		LogPath:   filepath.Join(dir, "client.jsonl"),
		StatsPath: filepath.Join(dir, "stats.json"),
	})
	defer r.Close()

	if err := r.SetStatsSettings(7*24*time.Hour, 6*time.Hour); err != nil {
		t.Fatalf("SetStatsSettings: %v", err)
	}
	cfg := r.Config()
	if cfg.Retain != 7*24*time.Hour {
		t.Errorf("retain=%s，期望 7 天", cfg.Retain)
	}
	if cfg.Window != 6*time.Hour {
		t.Errorf("window=%s，期望 6 小时", cfg.Window)
	}
	if got := r.Snapshot(); got.WindowSec != int64(6*time.Hour/time.Second) {
		t.Errorf("快照 window_sec=%d，期望 %d", got.WindowSec, int64(6*time.Hour/time.Second))
	}
}

// TestSettingsUpdateFieldsRoundTrip 两个新增的更新偏好也要往返一致。
func TestSettingsUpdateFieldsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	want := Settings{
		RetainSec:    int64(DefaultRetain / time.Second),
		WindowSec:    int64(DefaultWindow / time.Second),
		AutoUpdate:   true,
		UpdateMirror: "https://gh-proxy.com/,https://ghfast.top/",
	}
	if err := SaveSettings(path, want); err != nil {
		t.Fatal(err)
	}
	if got := LoadSettings(path); got != want {
		t.Fatalf("读回 %+v，期望 %+v", got, want)
	}
}

// TestSettingsOldFileHasUpdateOff 老的 settings.json（没有更新字段）必须能正常
// 读取，且自动更新**默认关闭**。
//
// 这条守的是向后兼容：用户升级到带自动更新的版本时，磁盘上那份文件是旧的。
// 若默认值写成 true，升级那一刻就等于「用户同意了自我替换」——而他从未同意过。
func TestSettingsOldFileHasUpdateOff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	// 升级前的真实文件形态（只有两个时间跨度字段）
	os.WriteFile(path, []byte(`{"retain_sec":3600,"window_sec":1800}`), 0o644)

	got := LoadSettings(path)
	if got.AutoUpdate {
		t.Error("老配置文件读出的 auto_update 是 true——升级不该默认开启自我替换")
	}
	if got.UpdateMirror != "" {
		t.Errorf("老配置文件读出的 update_mirror = %q，期望空（未配置）", got.UpdateMirror)
	}
	if got.Retain() != time.Hour || got.Window() != 30*time.Minute {
		t.Errorf("老字段被影响：retain=%s window=%s", got.Retain(), got.Window())
	}
}

// TestSettingsMirrorTrimmed 镜像串两端空白被去掉（用户从网页复制常带空格），
// 但中间内容原样保留——合法性校验是 update 包的事，这里不做。
func TestSettingsMirrorTrimmed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	os.WriteFile(path, []byte(`{"update_mirror":"  https://a.example/ , https://b.example/  "}`), 0o644)
	got := LoadSettings(path)
	want := "https://a.example/ , https://b.example/"
	if got.UpdateMirror != want {
		t.Fatalf("update_mirror = %q，期望 %q", got.UpdateMirror, want)
	}
}

// TestSettingsKeepsConnectionFields 连接参数必须能存下来。
//
// 2026-11-14 实证：用户机器开着 TUN 全局代理，47.82.76.102 被路由到
// utun5/198.18.0.1，必须把「源地址」绑到物理网卡才能连上。这些值以前只在
// flag 里，GUI 一重启就回默认——用户看到的是「更新到 0.3 之后连不上了」。
func TestSettingsKeepsConnectionFields(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	s := DefaultSettings()
	s.Server = "47.82.76.102:28443"
	s.ServerName = "magpie.aiweb.xin"
	s.LocalAddr = "192.168.1.106"
	s.Transport = "tcp"
	s.Listen = "127.0.0.1:18899"
	if err := SaveSettings(p, s); err != nil {
		t.Fatal(err)
	}
	got := LoadSettings(p)
	if got.Server != s.Server || got.ServerName != s.ServerName ||
		got.LocalAddr != s.LocalAddr || got.Transport != s.Transport || got.Listen != s.Listen {
		t.Fatalf("连接参数没有往返：%+v", got)
	}

	var cfg Config
	cfg.Normalize()
	got.ApplyTo(&cfg)
	if cfg.LocalAddr != "192.168.1.106" || cfg.Transport != "tcp" {
		t.Fatalf("ApplyTo 没生效：%+v", cfg)
	}
}

// TestApplyToIgnoresEmpty 空字段不能把默认值盖成空串。
func TestApplyToIgnoresEmpty(t *testing.T) {
	var cfg Config
	cfg.Normalize()
	want := *cfg2ptr(cfg.Server)
	Settings{}.ApplyTo(&cfg)
	if cfg.Server != want {
		t.Fatalf("空 Settings 覆盖了默认服务器：%q → %q", want, cfg.Server)
	}
}

func cfg2ptr(s string) *string { return &s }
