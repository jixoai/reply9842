# brand-assets Specification

## Purpose

品牌资产：9842 图标（源 SVG 是唯一事实来源，派生产物入库）、托盘 template 图标、
Windows PE 资源、以及产品改名（rfc9842-reply → reply9842）后的资产同步。

## Requirements

### Requirement: icon.svg 是唯一事实来源

`cmd/gui/assets/icon.svg` SHALL 是图标的唯一事实来源；`.icns` / `.ico` / 托盘 PNG /
各尺寸预览都是从它派生的产物。日常构建不需要重新生成（派生产物已入库），改了 SVG 才
需要重跑生成脚本。

落地证据：`cmd/gui/make_icons.sh`（SVG → iconset → icns + ico + tray + preview
全流程与依赖说明）、`INSTALL.md` 的「图标」章节（同一规则的文档形态）、
`Makefile` 的 `icons` / `winres` 目标。

#### Scenario: 纯代码改动不碰图标

- **WHEN** 只改动 Go 源码并 `make build`
- **THEN** 不触发 rsvg-convert / iconutil / go-winres，构建不因缺这些工具而失败

#### Scenario: 改 SVG 后必须重跑

- **WHEN** 修改了 `icon.svg`
- **THEN** 必须跑 `make icons`（和 Windows 侧的 `make winres`），否则二进制里还是旧图标

### Requirement: 版式为 2×2 等分格 + 逐字墨迹归一化

图标 SHALL 为 2×2 等分格布局：画布 1024，格距 384，四格心固定在 (320|704, 320|704)，
每个数字的墨迹包围盒归一到 300×300 后放在格心，最终墨迹块 684×684 严格正方、四条栏距
恒为 84px、四边留白 170px。

落地证据：`cmd/gui/assets/icon.svg` 头部注释（版式、为什么逐字归一化而非统一字号、
代价是每字 4%~15% 横向缩放）、`INSTALL.md` 的「版式是 2×2 等分格」段落。

#### Scenario: 不被「修正」成 edge-align

- **WHEN** 后人看到右列 8 / 2 外缘各差约 ±7px
- **THEN** 不改：那是比例数字的光学居中，改成边缘对齐会让右列看起来被削过
  （SVG 注释已明确标注「后人不要把它当 bug 改成 edge-align」）

#### Scenario: 16px 下仍可辨认

- **WHEN** 缩到 16px
- **THEN** 四个数字象限分离度约 92%，能分辨出「9842」

### Requirement: 源 SVG 满幅、圆角由平台各自处理

源 SVG SHALL 铺满 1024 画布、不做圆角、不留透明边。macOS 14 起系统会自己裁 squircle
并补边缘高光；若提交带透明留边的预圆角图，系统会当成旧式图标并在外面套浅灰底板。
Windows 不裁切，所以 `.ico` 的圆角由 `cmd/gui/make_ico.py` 在栅格化时单独加（约 8% 圆角 +
1px 淡亮边）。

落地证据：`INSTALL.md` 的「注意：SVG 是满幅的」段落、
`cmd/gui/make_ico.py`（栅格化时加圆角与亮边）。

#### Scenario: macOS 上不被套灰底

- **WHEN** 在 macOS 14+ 上查看 Dock 图标
- **THEN** 图标是系统裁出的 squircle，外面没有浅灰底板

#### Scenario: Windows 上有圆角

- **WHEN** 在 Windows 资源管理器里查看 exe 图标
- **THEN** 图标是 8% 圆角 + 1px 亮边（由 `cmd/gui/make_ico.py` 在栅格化时加）

### Requirement: 托盘图标是独立的 template 事实来源

托盘 SHALL 用独立的 `tray.svg`（template 图标只认 alpha，必须去掉底板与油墨），
栅格化为 48px PNG 后 `go:embed` 进二进制。48 是唯一一档能覆盖菜单栏 18pt Retina 36px
与 Windows DPI 16/24/32 全部需求的尺寸。

落地证据：`cmd/gui/assets/tray.svg`（独立事实来源）、
`cmd/gui/tray.go:46`（`go:embed assets/tray.png` 与「为什么是 48px」的完整理由）、
`cmd/gui/make_icons.sh:66`（`rsvg-convert -w 48 -h 48 tray.svg`）。

#### Scenario: 深浅色菜单栏都清晰

- **WHEN** 系统切到深色或浅色菜单栏
- **THEN** template 图标由系统按当前外观着色，两态都清晰（因为只有 alpha 通道）

### Requirement: Windows PE 资源承载图标与版本

`rsrc_windows_amd64.syso` SHALL 入库（`go build` 自动链接同目录 `.syso`），承载
RT_GROUP_ICON 与版本信息。含 tag 的构建必须重新生成一次，否则 exe 属性里永远停在入库
那一刻的版本；非 Windows 构建不受影响。

落地证据：`cmd/gui/make_winres.sh`（为什么需要 .syso 的完整说明与版本号来源优先级）、
`Makefile` 的 `dist-gui`（`VERSION_NUM != dev` 时强制重跑 make_winres）、
`.github/workflows/release.yml`（Self-check 断言 `gui-windows-amd64.exe` 是 GUI 子系统）。

#### Scenario: 发版 exe 里版本号正确

- **WHEN** 用 `v0.2.1` tag 构建
- **THEN** exe 属性里的版本是 `0.2.1`，不是入库那一刻的旧版本

#### Scenario: go install 也有图标

- **WHEN** 用户 `go install .../cmd/gui@latest`
- **THEN** 链接了同目录的 `.syso`，exe 有图标与版本信息

### Requirement: 改名必须全链路同步

项目改名（`jixoai/rfc9842-reply` → `jixoai/reply9842`）SHALL 一次性同步全部标识：
Go module path、数据目录（`~/.reply9842`、server 侧 `/var/lib/reply9842`）、
macOS bundle（`dev.jixoai.reply9842` / `reply9842.app`）、`mygo.App.SetName("reply9842")`
（单实例锁按此键）、隧道 ALPN（`reply9842/1`）。不做兼容、不保留旧路径。

落地证据：`git log` 的 fa872d5（改名裁决与同步清单）、`go.mod`（module path）、
`internal/transport/tunnel.go:39`（`ALPN = "reply9842/1"`）、
`cmd/gui/bundle_darwin.go:35`（`bundleID`）、`cmd/gui/main.go:250`
（`SetName("reply9842")`）、`internal/autostart/autostart.go:33`（`ID`）。

#### Scenario: 旧客户端连不上新服务端

- **WHEN** 改名前的客户端连改名后的服务端
- **THEN** ALPN 不匹配，握手失败——这是刻意的不兼容

#### Scenario: 单实例锁按新名字生效

- **WHEN** 改名后启动两个 GUI 实例
- **THEN** 第二个被单实例锁拦下（锁键是 `reply9842`）

## 已知边界（改名未完成的部分，见 changes/`finish-rebrand-to-reply9842`）

改名提交只覆盖了 Go 代码与 npm 包，以下位置**仍写 `rfc9842-reply`**，属于改名未落地：

| 位置 | 现状 |
|---|---|
| `site/index.html` `<title>` / `og:title` / `og:url` / 品牌名 / 四处 GitHub 链接 | 全是 `rfc9842-reply` |
| `site/CNAME` | `rfc9842-reply.jixoai.com` |
| `site/style.css` 头注释 | `rfc9842-reply` |
| `cmd/gui/assets/icon.svg` 与 `tray.svg` 的 `<title>` | `rfc9842-reply` |
| `cmd/gui/make_winres.sh` 的 `--product-name` / `--file-description` | `rfc9842-reply 客户端` |
| `README.md` 的官网地址 | 同时出现 `reply9842.jixoai.com` 与正文里的 `rfc9842.jixoai.com` |
| `INSTALL.md` 与 `cmd/gui/bundle_darwin.go` 的环境变量 `RFC9842_BUNDLED` | 保留旧前缀（可接受，但未在文档里说明为何不改） |
