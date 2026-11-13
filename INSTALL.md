# 安装

本文件是 README「安装」章节的独立版本（README 正由其他人修改，避免冲突）。

仓库提供两个可执行文件：

| 命令 | 形态 | 用途 |
|---|---|---|
| `relay-cli` | 命令行 | 本地中转 + 增量压缩隧道（无界面，适合服务器 / 脚本） |
| `relay-gui` | 桌面窗口 | 同一套转发逻辑的图形界面（状态、统计、一键复制配置） |

两者共用 `internal/relayclient` 的同一份转发实现：**逻辑只有一份**，不会出现
「GUI 能用、CLI 不能」这类裂缝。

---

## 一、go install（推荐，无需额外工具）

需要 **Go 1.27.3 或更高**（`mygo` 的 `go` 指令要求）。

```bash
# 命令行版
go install github.com/jixoai/rfc9842-reply/cmd/cli@latest

# 图形界面版
go install github.com/jixoai/rfc9842-reply/cmd/gui@latest
```

安装后二进制落在 `$(go env GOPATH)/bin`（通常是 `~/go/bin`），确认它在 `PATH` 里：

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
relay-cli version
relay-gui -version
```

### 为什么 GUI 也能 `go install`

`relay-gui` 用 mygo 的 **native UI**（Go 里写的界面，GPU 自绘），不嵌前端资源、
不读外部文件，所以编译出来就是自包含的单个可执行文件。这一点与 webview 方案
不同——后者需要 `//go:embed` 前端产物，虽然也能 `go install`，但多一层构建依赖。

### Windows：为什么 `go install` 出来的窗口不会带黑框

`go install` 无法传 `-ldflags "-H=windowsgui"`，编出来的 exe 是 console 子系统，
双击会先弹一个控制台窗口。`cmd/gui/console_windows.go` 在 `main()` 开头调用
`GetConsoleWindow` + `ShowWindow(SW_HIDE)` + `FreeConsole` 把那个窗口收起来，
行为与 `-H=windowsgui` 链接的产物一致。

Release 里的 Windows 产物另外用 `-H=windowsgui` 链接（`go build` 可以传），
两套机制同时存在，互为兜底。

---

## 二、下载预编译二进制

见 [Releases](https://github.com/jixoai/rfc9842-reply/releases) 页面的 Assets。

| 文件 | 平台 |
|---|---|
| `relay-cli-linux-amd64` | Linux x86-64 |
| `relay-cli-linux-arm64` | Linux ARM64 |
| `relay-cli-darwin-amd64` | macOS Intel |
| `relay-cli-darwin-arm64` | macOS Apple Silicon |
| `relay-cli-windows-amd64.exe` | Windows x86-64 |
| `relay-gui-darwin-arm64` | macOS Apple Silicon |
| `relay-gui-darwin-amd64` | macOS Intel |
| `relay-gui-windows-amd64.exe` | Windows x86-64 |

校验下载完整性：

```bash
shasum -a 256 -c SHA256SUMS.txt
```

### macOS 首次运行

未签名的二进制会被 Gatekeeper 拦下。右键 → 打开，或：

```bash
xattr -d com.apple.quarantine ./relay-gui-darwin-arm64
chmod +x ./relay-gui-darwin-arm64
./relay-gui-darwin-arm64
```

`relay-cli` 同理。

首次启动 GUI 时，程序会在 `~/Applications/rfc9842-reply.app` 现场拼一个最小
app bundle（当前二进制的副本 + `Info.plist` + 内嵌图标），然后 re-exec 副本。
这一步只为了 Dock 身份——裸 Mach-O 没有 bundle，Dock 里会是通用黑色 exec 方块、
进程名是文件名。完成后 Dock、Cmd-Tab 与应用菜单都显示 `rfc9842-reply` 和正确图标。

要点：

- 产物仍是**一个可执行文件**；bundle 是运行时在你自己磁盘上生成的，不参与分发。
- 重复启动不会重写任何文件（内容没变就跳过）。
- 卸载：退出应用后删掉 `~/Applications/rfc9842-reply.app` 即可。
- 想跳过：设 `RFC9842_BUNDLED=1` 直接跑裸二进制（图标会退化成通用样式）。

Windows 侧的图标与版本信息编在 PE 资源段里，双击即用，无需额外步骤。

---

## 三、从源码构建

```bash
git clone https://github.com/jixoai/rfc9842-reply
cd rfc9842-reply

make build          # 当前平台，产物到 bin/
make dist           # 全平台，产物到 dist/（命名与 Releases 一致）
```

直接 `go build` 也可以：

```bash
CGO_ENABLED=0 go build -ldflags "-s -w" -o relay-cli ./cmd/cli
CGO_ENABLED=0 go build -ldflags "-s -w" -o relay-gui ./cmd/gui
```

全平台交叉编译不需要 CGO（mygo 与项目依赖都是纯 Go）：

```bash
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o relay-gui-darwin-arm64 ./cmd/gui
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build \
  -ldflags "-s -w -H=windowsgui" -o relay-gui-windows-amd64.exe ./cmd/gui
```

### 图标

图标唯一事实来源是 `cmd/gui/assets/icon.svg`（手写的复古粗衬线 9842 排版）。
派生产物都已入库，日常构建不需要重新生成：

| 文件 | 用途 |
|---|---|
| `assets/AppIcon.icns` | macOS bundle 图标，`go:embed` 进二进制 |
| `assets/app.ico` | Windows 图标，喂给 `go-winres` |
| `assets/tray.svg` → `assets/tray.png` | 菜单栏/通知区域的 template 图标（纯黑透明，无底板） |
| `rsrc_windows_amd64.syso` | Windows PE 资源（图标 + 版本信息 + 清单），`go build` 自动链接 |
| `assets/preview/` | 各尺寸预览，供人工核查 16px 可辨认性 |

改了 SVG 才需要重跑：

```bash
make icons    # SVG → icns + ico + 托盘 + 预览图（需要 rsvg-convert，macOS 用 iconutil）
make winres   # ico → rsrc_windows_amd64.syso（需要 go-winres）
```

`make dist` 会在带 tag 的构建里自动重跑 `make winres`，把版本号写进 exe 属性。
macOS 侧不需要额外步骤：icns 已 `go:embed`，版本号由 `-ldflags -X main.version` 注入。

**设计定位：粗壮复古的机加工铭牌**（不是「锋利硬朗」）。这个选择是有据的：
Alfa Slab One 的字母带 slab 衬线，但数字 9/8 是纯圆几何形（直线边占比 30~40%），
4/2 带平底衬线（48~79%），字形本身气质不统一。手改 9/8 的路径能拉齐硬朗度，
但 16px 下字怀会消失，所以锋利感改由底板承担：近实色底板、直角压边框、
硬边压入阴影。详细理由写在 `icon.svg` 头部注释里，改图标前请先读。

**版式是 2×2 等分格**：格距 384，每个数字的墨迹包围盒归一到 300×300 方盒后
居中放在格心。栏距/行距恒为 84px、墨迹块 684×684 严格正方。代价是每字
4~15% 的横向缩放（比例数字天生宽窄不一，不归一化就只能得到 78/95/85/77
四个不等的栏距和 0.908 的宽高比）。

注意：SVG 是**满幅**的（铺满 1024 画布，不做圆角、不留透明边）。macOS 14 起系统
会自己把满幅图标裁成 squircle 并补边缘高光；如果提交自带透明留边的预圆角图，
系统会当成旧式图标，在外面套一块浅灰底板。

Windows 不裁切任何东西，所以 `.ico` 的圆角由 `make_ico.py` 在栅格化时单独加
（约 8% 圆角 + 1px 淡亮边），源 SVG 保持满幅不变。托盘图标（`tray.svg`）
是另一份事实来源：template 图标只认 alpha，必须去掉底板与油墨。

---

## 四、第一次运行

### 命令行

```bash
# 启动转发（Agent 把 base_url 指到 http://127.0.0.1:18899）
relay-cli -listen 127.0.0.1:18899 -server 47.82.76.102:28443

# 或等价地写明子命令
relay-cli serve -listen 127.0.0.1:18899 -server 47.82.76.102:28443

# 另开一个终端看统计
relay-cli status
relay-cli status -json | jq .up_ratio

# 版本
relay-cli version
```

不带子命令时按 `serve` 处理，所以旧写法的脚本无需改动。

### 图形界面

```bash
relay-gui                  # 打开窗口，点「启动」
relay-gui -autostart       # 打开窗口并立即开始转发
relay-gui -light           # 亮色玻璃（默认深色）
relay-gui -listen 127.0.0.1:19000 -server 47.82.76.102:28443   # 覆盖初值
```

窗口里能做的事：

- **连接状态**：大字「已连接 / 未连接」，绿/红
- **关键统计**：请求数、上行压缩比、累计节省流量、p50 延迟（每秒刷新）
- **启动 / 停止**：随时切换，不影响窗口
- **复制配置**：一键把 `export ANTHROPIC_BASE_URL=http://127.0.0.1:18899` 放进剪贴板
- **复制配置**：一键把 `export ANTHROPIC_BASE_URL=http://127.0.0.1:18899` 放进剪贴板

---

## 五、让 Agent 走 relay

本项目**不提供自动接管**。原因：改 Agent 配置文件要适配每个 Agent 的格式（易碎），
改 hosts 需要提权且会让整站 TLS 校验失败（还要装根证书做 MITM）。两者都是
「省一步手工」换「全局副作用或格式风险」，不成比例。

手工配置很简单，按你的 Agent 任选其一：

```bash
# Anthropic SDK / Claude Code
export ANTHROPIC_BASE_URL=http://127.0.0.1:18899

# OpenAI 兼容
export OPENAI_BASE_URL=http://127.0.0.1:18899/v1
```

或者写进 Agent 自己的配置文件（具体字段看该 Agent 的文档）。

---

## 六、目录与文件

运行时数据都在 `~/.rfc9842-reply/`：

```
~/.rfc9842-reply/
├── client-<date>.jsonl      逐请求追加日志（jq 直读）
└── client-stats.json        汇总快照（每次请求后覆盖写）
```

`relay-cli status` 读的就是 `client-stats.json`（跨进程唯一可用通道）；
GUI 读的是进程内快照（`stats.Collector.Snapshot()`）。

---

## 七、故障排查

**`bind: address already in use`**
已有另一个 relay 实例占着端口。`lsof -nP -iTCP:18899 -sTCP:LISTEN` 找出它并结束，
或换端口：`relay-gui -listen 127.0.0.1:19000`。

**`dial: ... i/o timeout`**
上游地址不可达。检查 `-server` 与网络；自签证书观察期需要加 `-insecure`。

**`relay-cli status` 报「还没有统计数据」**
说明这个路径下还没有进程写过统计。先确认 `-stats` 指向同一文件（默认都在
`~/.rfc9842-reply/`）。

**GUI 在 Linux 上没有产物**
这是有意的：native UI 的 Linux 后端依赖 GTK3，而本项目的 GUI 只面向 macOS 与
Windows。Linux 上用 `relay-cli`。
