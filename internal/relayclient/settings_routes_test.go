package relayclient

// settings_routes_test.go — 旧 settings.json 的线路迁移。
//
// 要钉住的语义（openspec/specs/transport 的「旧配置必须无损迁移」）：
// 用户升级后，**原有设置不能丢**，且迁移必须幂等。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/jixoai/reply9842/internal/route"
)

// 生产环境的真实旧配置：direct + 域名 server + 无 proxy
func TestMigrateProductionDirectConfig(t *testing.T) {
	raw := `{
  "retain_sec": 86400, "window_sec": 1800, "auto_update": false,
  "update_mirror": "",
  "network_mode": "direct",
  "server": "magpie.aiweb.xin",
  "listen": "127.0.0.1:18899",
  "transport": "auto",
  "running": true
}`
	var s Settings
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("解析: %v", err)
	}

	rs := s.MigrateRoutes()
	if len(rs) != 1 {
		t.Fatalf("应迁移出 1 条线路，实际 %d", len(rs))
	}
	r := rs[0]
	if r.Kind != route.KindDirect {
		t.Fatalf("kind 应为 direct，实际 %q", r.Kind)
	}
	if r.Server != "magpie.aiweb.xin:28443" {
		t.Fatalf("server 应为 magpie.aiweb.xin:28443，实际 %q", r.Server)
	}
	if r.Transport != "auto" {
		t.Fatalf("transport 应为 auto，实际 %q", r.Transport)
	}
	// 关键：其他偏好一个都不能丢
	if s.RetainSec != 86400 || s.WindowSec != 1800 || !s.Running || s.Listen != "127.0.0.1:18899" {
		t.Fatalf("迁移过程丢失了其他偏好：%+v", s)
	}
}

// 旧配置里配了代理：必须保留，不能退化成直连
func TestMigrateLegacyProxyConfig(t *testing.T) {
	s := Settings{
		NetworkMode: NetworkProxy,
		ProxyURL:    "http://user:pass@127.0.0.1:7890",
		Server:      "magpie.aiweb.xin",
		Transport:   "tcp",
	}
	rs := s.MigrateRoutes()
	if len(rs) != 1 {
		t.Fatalf("应 1 条，实际 %d", len(rs))
	}
	r := rs[0]
	if r.Kind != route.KindProxy {
		t.Fatalf("kind 应为 proxy，实际 %q", r.Kind)
	}
	if r.Proxy != "http://user:pass@127.0.0.1:7890" {
		t.Fatalf("代理 URL 被改写：%q", r.Proxy)
	}
}

// network_mode=proxy 但 URL 为空：退化为直连（而不是造一条永远失败的线路）
func TestMigrateProxyWithoutURLDegradesToDirect(t *testing.T) {
	s := Settings{NetworkMode: NetworkProxy, ProxyURL: "", Server: "h"}
	r := s.MigrateRoutes()[0]
	if r.Kind != route.KindDirect {
		t.Fatalf("空代理应退化为直连，实际 %q", r.Kind)
	}
	if err := r.Validate(); err != nil {
		t.Fatalf("退化出的线路应合法：%v", err)
	}
}

// network_mode=system：直连但不绑源地址（语义区别不能丢）
func TestMigrateSystemModeHasNoLocalAddr(t *testing.T) {
	s := Settings{NetworkMode: NetworkSystem, LocalAddr: "192.168.1.5", Server: "h"}
	r := s.MigrateRoutes()[0]
	if r.Kind != route.KindDirect {
		t.Fatalf("system 应映射为 direct，实际 %q", r.Kind)
	}
	// system 的语义是「交给系统路由」，所以即便旧配置里有 localAddr 也不该绑。
	// （旧配置里 localAddr 有值意味着原本是 direct，NetworkModeOr 已经处理了
	//   这个迁移；显式写了 system 就说明用户要系统路由。）
	if r.LocalAddr == "192.168.1.5" {
		t.Fatal("system 模式不应沿用源地址")
	}
}

// Routes 非空时 MigrateRoutes 不得覆盖
func TestMigrateRespectsExistingRoutes(t *testing.T) {
	s := Settings{
		NetworkMode: NetworkDirect,
		Server:      "old:28443",
		Routes: route.Set{
			{ID: "新的", Kind: route.KindRelay, Server: "new:28443"},
		},
	}
	rs := s.MigrateRoutes()
	if len(rs) != 1 || rs[0].ID != "新的" || rs[0].Server != "new:28443" {
		t.Fatalf("已有 Routes 被覆盖：%+v", rs)
	}
}

// 迁移必须幂等：连写两轮磁盘，读回一致
func TestMigrateIdempotentAcrossSaveLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	s := Settings{
		NetworkMode: NetworkDirect,
		Server:      "magpie.aiweb.xin",
		Transport:   "auto",
		RetainSec:   86400,
		WindowSec:   1800,
	}
	// 第一轮：迁移 → 存
	s.Routes = s.MigrateRoutes()
	if err := SaveSettings(path, s); err != nil {
		t.Fatal(err)
	}
	// 第二轮：读回 → 再迁移 → 再存
	a := LoadSettings(path)
	if len(a.Routes) != 1 {
		t.Fatalf("读回后线路数 %d，期望 1", len(a.Routes))
	}
	a2 := a
	a2.Routes = a2.MigrateRoutes()
	if err := SaveSettings(path, a2); err != nil {
		t.Fatal(err)
	}
	b := LoadSettings(path)

	if !a.Equal(b) {
		t.Fatalf("两轮读写后漂移：\n  a=%+v\n  b=%+v", a, b)
	}
}

// 旧字段必须**保留**在文件里（旧版本回退时仍能读到可用配置）
func TestLegacyFieldsPreservedOnDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	s := Settings{
		NetworkMode: NetworkDirect,
		Server:      "magpie.aiweb.xin",
		ProxyURL:    "http://p:7890",
		Transport:   "auto",
	}
	s.Routes = s.MigrateRoutes()
	if err := SaveSettings(path, s); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"network_mode", "server", "proxy_url", "transport"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("旧字段 %q 在落盘文件中消失了（旧版本回退会丢配置）", k)
		}
	}
	if _, ok := m["routes"]; !ok {
		t.Fatal("新字段 routes 未落盘")
	}
}

// ApplyTo 必须把线路带到 Config
func TestApplyToCarriesRoutes(t *testing.T) {
	s := Settings{
		Routes: route.Set{
			{ID: "A", Kind: route.KindDirect, Server: "a:28443"},
			{ID: "B", Kind: route.KindProxy, Server: "a:28443", Proxy: "socks5://p:1080"},
		},
	}
	var cfg Config
	s.ApplyTo(&cfg)
	if len(cfg.Routes) != 2 {
		t.Fatalf("ApplyTo 后线路数 %d，期望 2", len(cfg.Routes))
	}
	cfg.Normalize()
	if len(cfg.Routes) != 2 {
		t.Fatalf("Normalize 后线路数被改：%d", len(cfg.Routes))
	}
}

// ApplyTo 在 Routes 为空时不得动 cfg.Routes（交给 Normalize 合成）
func TestApplyToEmptyRoutesLeavesConfig(t *testing.T) {
	s := Settings{Server: "h"}
	cfg := Config{Server: "h", Routes: route.Set{{ID: "x", Kind: route.KindDirect, Server: "h:28443"}}}
	s.ApplyTo(&cfg)
	if len(cfg.Routes) != 1 || cfg.Routes[0].ID != "x" {
		t.Fatalf("空 Routes 的 settings 不应清空 cfg.Routes：%+v", cfg.Routes)
	}
}
