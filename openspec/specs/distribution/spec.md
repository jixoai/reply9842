# distribution Specification

## Purpose

分发通道：npm 纯启动器（按需下载）、GitHub Release（全平台产物）、`go install`、
Makefile 本地交叉编译。四条通道的产物命名必须完全一致，否则「本地能验、CI 不同」。

## Requirements

### Requirement: 产物命名统一为 kind-os-arch

所有通道 SHALL 使用 `<kind>-<os>-<arch>[.exe]`，`kind ∈ cli|gui|server`，
`os ∈ linux|darwin|windows`，`arch ∈ amd64|arm64`。共 16 个产物（`gui/linux` 不存在）。
Makefile 的 `CLI_TARGETS` / `GUI_TARGETS` 与 `.github/workflows/release.yml` 的构建循环必须一致。

落地证据：`Makefile`（`CLI_TARGETS` 与 `GUI_TARGETS` 定义、`dist-cli` / `dist-gui`
目标）、`.github/workflows/release.yml` 的 Build all platforms 步骤（三 kind ×
三 os × 两 arch，跳过 gui/linux）、`npm/lib/run.js` 的 `artifactName`
（注释「与 `.github/workflows/release.yml` 的命名严格一致」）、
`npm/test/platform.test.js`（静态断言命名规则，`ok 40` 全过）。

#### Scenario: 本地 dist 与 Release 一致

- **WHEN** 本地 `make dist`
- **THEN** 产物名与 Release 页面 Assets 一一对应，可直接用来复现发布内容

#### Scenario: gui/linux 不存在

- **WHEN** 在 Linux 上请求 GUI 产物
- **THEN** 不构建、不下载，npm 侧给出人话提示（GUI 只提供 macOS 与 Windows）

### Requirement: npm 包是纯启动器，二进制按需下载

npm 包 SHALL NOT 内含任何二进制：包体只有 `bin/` + `lib/` + README/LICENSE（约 10 KB）。
首次运行按「平台 + 形态」从 GitHub Release 下载对应产物，缓存到
`~/.cache/reply9842/<version>/`（Windows 用 `LOCALAPPDATA`，其它用 `XDG_CACHE_HOME`），
按版本隔离。

落地证据：`npm/package.json`（`files` 只含 bin/lib/README/LICENSE，无 vendor）、
`npm/lib/run.js:78`（`cacheRoot`：Windows 走 `LOCALAPPDATA`，其它走 `XDG_CACHE_HOME`）、
`npm/lib/run.js:91`（`binaryPath` 含 version 段）、
`npm/lib/run.js:60`（`artifactName`）、
`npm/test/platform.test.js`（「包体不含 vendor」「缓存按版本隔离」断言）。

#### Scenario: 已缓存时零网络

- **WHEN** 同版本第二次运行
- **THEN** 直接执行缓存里的二进制，不发任何网络请求

#### Scenario: 换版本重新下载

- **WHEN** npm 包升级到新版本
- **THEN** 缓存路径含新版本号，下载新产物，不与旧版本串包

#### Scenario: 不支持的平台给人话

- **WHEN** 在不支持的平台（或 gui/linux）上运行
- **THEN** 打印具体原因与手动下载地址 / `go install` 命令，退出码 1

### Requirement: 下载走镜像前缀链并以直连兜底

下载 SHALL 按镜像前缀顺序尝试（`gh-proxy.com` → `ghfast.top` → `ghproxy.net` → 直连），
任一成功即用；全部失败时打印手动下载地址与 `go install` 命令。
镜像列表支持 `REPLY9842_MIRROR` 环境变量覆盖（逗号分隔，顺序即优先级，`direct` 或空值
表示直连）。

落地证据：`npm/lib/mirror.js:24`（`MIRROR_ENV = "REPLY9842_MIRROR"`，含「不复用
npm_config_* 因为 npm 会吃掉并做 config 解析」的理由）、
`npm/lib/mirror.js:18`（`DEFAULT_MIRRORS`）、
`npm/lib/download.js`（并发安全的下载器）、
`npm/test/mirror.test.js`（镜像解析规则测试）。

#### Scenario: 国内网络自动加速

- **WHEN** GitHub Release 直连超时
- **THEN** 自动换下一个镜像前缀重试，任一成功即继续

#### Scenario: 全部失败给出可操作信息

- **WHEN** 所有镜像与直连都失败
- **THEN** 打印手动下载 URL、应放到的缓存路径、以及 `go install` 命令

### Requirement: 下载必须原子落盘并校验完整性

下载器 SHALL：边收边写盘（不整段驻留内存）、失败重试时带
`Range: bytes=<已有长度>-` 续传、结束前核对字节数（content-length 或 content-range
的 total）、可选校验 `SHA256SUMS.txt`。绝不把半截文件 rename 成正式产物。

依据：2026-11-13 事故——`scp` 被 `timeout 50` 掐断留下 2.5MB/9.9MB 的半截 ELF，ELF
header 完好使 `file` 报 normal，部署后 `status=11/SEGV`。二进制下载是同一个风险面。

落地证据：`npm/lib/download.js:1`（四条约束的完整理由）、
`npm/lib/download.js:387`（导出 `buildRangeHeader` / `classifyResponse` /
`downloadArtifact` / `fetchChecksums` 等）、
`npm/lib/run.js:235`（SHA256SUMS 是 best-effort，取不到只靠字节数校验）、
`npm/test/platform.test.js`（「下载是原子落盘（.part -> rename）」断言）。

#### Scenario: 传输中断可续传

- **WHEN** 下载到一半连接断开
- **THEN** 重试从已有字节处续传，`.part` 文件跨进程存活

#### Scenario: 半截文件绝不启用

- **WHEN** 收完的字节数与 Content-Length 不符
- **THEN** 报错并保留 `.part`，不 rename 成正式产物

### Requirement: 启动器透传信号与退出码

启动器 SHALL 把 `SIGINT`/`SIGTERM`/`SIGHUP` 透传给子进程，并透传退出码（被信号杀掉时
按 128+signal 惯例）。Ctrl-C 行为必须与原生二进制一致。

落地证据：`npm/lib/run.js` 的 `main`（信号转发与退出码透传）、
`npm/test/platform.test.js`（启动器语义断言）。

#### Scenario: Ctrl-C 与原生一致

- **WHEN** 用户对 `npx reply9842` 按 Ctrl-C
- **THEN** 子进程收到同一信号并退出，启动器等它退出后以相同码退出

### Requirement: GitHub Release 附带校验和

打 tag 触发的发布 SHALL 上传全部 16 个产物 + `SHA256SUMS.txt`。校验和生成必须同时
支持 `sha256sum`（Linux）与 `shasum -a 256`（macOS），且排除 `SHA256SUMS.txt` 自身
（workflow 重跑时该文件已存在，不排除会自我包含）。

落地证据：`.github/workflows/release.yml` 的 Checksums 步骤（含 `shopt -s extglob`
排除自身、双平台分支）、Publish GitHub Release 步骤（`fail_on_unmatched_files: true`）。

#### Scenario: 自托管 macOS runner 能生成校验和

- **WHEN** release job 落在 macOS 的 self-hosted runner
- **THEN** 用 `shasum -a 256` 生成，不因缺 `sha256sum` 而失败

#### Scenario: workflow 重跑不自我包含

- **WHEN** 同一 tag 的 workflow 被重跑，`SHA256SUMS.txt` 已存在
- **THEN** 该文件不被算进校验和清单

### Requirement: 镜像可由环境变量配置

镜像前缀链 SHALL 可用 `REPLY9842_MIRROR` 覆盖，支持逗号分隔的回退链。
解析规则必须与 Go 侧 `internal/update/mirror.go` 完全一致（三处共用：npm 引导器 /
update 包 / GUI 设置页）：逐项 trim、空项跳过、`direct` 大小写不敏感、无有效项即直连
且**不回退**默认列表、前缀末尾自动补 `/`、非法 URL 报错带原值、保序去重。

不使用 `npm_config_*` 前缀：npm 会吃掉自己的变量并做一层 config 解析，语义会变形。

落地证据：`npm/lib/mirror.js`、`npm/test/mirror.test.js`（12 例）、
`internal/update/mirror.go`、`internal/update/mirror_test.go`。

#### Scenario: 只给了 without 尾斜杠的镜像

- **WHEN** `REPLY9842_MIRROR=https://m.example.com`
- **THEN** 自动补成 `https://m.example.com/` 再拼接，不产出 `https://m.example.comhttps://github.com/...`

#### Scenario: 显式直连

- **WHEN** `REPLY9842_MIRROR=`, `REPLY9842_MIRROR=direct` 或 `REPLY9842_MIRROR=DIRECT`
- **THEN** 结果只有直连，不回退内置镜像列表

#### Scenario: 拼错的 URL

- **WHEN** 某个 item 不是合法 http(s) URL 或缺主机名
- **THEN** 报错并带出原始值，不静默忽略（否则用户会看到「全部镜像失败」却不知为何）

### Requirement: 下载必须真正断点续传

下载器 SHALL 流式写盘并支持 `Range` 续传，而不是把整个文件收进内存后一次性落盘。
响应必须分类处理：`append`（206 且起点等于已有长度）、`restart`（body 从 0 开始）、
`refetch`（起点对不上 → 删 `.part` 重发）、`complete`（416 且已有长度正好等于总长）。

落地证据：`npm/lib/download.js`（`classifyResponse`）、`npm/test/download.test.js`（27 例）。

#### Scenario: 中断后续传

- **WHEN** 下载到 3 MB 时连接断了，重跑
- **THEN** 带 `Range: bytes=3000000-` 续上，只花全量约 41% 的时间，最终 sha256 与
  `SHA256SUMS.txt` 一致

#### Scenario: 416 的空 body 不能当数据读

- **WHEN** `.part` 比服务端文件还长，服务端回 416
- **THEN** 删除 `.part` 后重发无 `Range` 请求；把 416 的 body 当续传流读完会报出
  「实际 0 字节，服务端声明 N 字节」——错误信息离真因极远（实测踩过）

#### Scenario: 内存不随文件线性增长

- **WHEN** 下载 15.3 MB 的 GUI 产物
- **THEN** 峰值内存增量约 2 MB（旧实现 Buffer.concat 约 9 MB）

### Requirement: 镜像内容不一致时换镜像重试

sha256 校验失败 SHALL 删除 `.part` 并在**下一个镜像**重试；全部镜像都失败才整体报错。
每个镜像各给一次「免罚重来」的机会，避免首个镜像的内容错误立刻把尝试次数用光。

落地证据：`npm/lib/download.js`、`internal/update/download.go`（`verifyChecksum` 逐镜像）。

#### Scenario: 首个镜像返回错误内容

- **WHEN** 镜像缓存了别的版本或返回 HTML 错误页
- **THEN** 校验失败 → 换下一个镜像 → 成功，用户无感

## 已知边界

- **产物未做代码签名 / 公证**：macOS 首次运行会被 Gatekeeper 拦下，文档给
  `xattr -d com.apple.quarantine` 的手工解法。见 changes/`add-release-signing`。
- **npm 包从未真实发布成功过**：v0.1.0/v0.2.1 的 npm 发布经历两次失败
  （`NODE_AUTH_TOKEN` 覆盖 OIDC、缺 `environment: npm-publish`），最终修复已入库但
  仅有一次成功发布的记录待补。
