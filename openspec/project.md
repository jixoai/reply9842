# Project: reply9842

**基于内容寻址基版本的增量压缩隧道，把 AI Agent 的上行带宽砍掉 99.8%。**

Agent 每次请求都要重传 1.3–2.2 MB 的完整上下文。本项目让相邻请求之间只传差异：
客户端在历史请求里挑一个「块哈希命中率」最高的当字典，用 zstd delta 压缩新请求，
服务端按内容哈希取字典还原。隧道两端都是有状态的压缩器——**上一轮的请求体就是
这一轮的字典**。

> 本文件是 openspec 的项目上下文。能力规格在 `openspec/specs/`，
> 未落地需求的变更提案在 `openspec/changes/`。

## 技术约束

- **Go 1.27.3**（`go.mod`）：`go install` 路径要求的最低版本。纯 Go、`CGO_ENABLED=0`，
  因此任意机器都能交叉编译全部平台。
- **两条独立构建线**：CLI 全平台（linux/darwin/windows × amd64/arm64），
  GUI 只有 darwin/windows（mygo native UI 无 Linux 后端）。server 全平台。
  产物命名统一 `<kind>-<os>-<arch>[.exe]`，kind ∈ `cli|gui|server`。
- **GUI 是 native UI（mygo v0.9.0）**，不是 webview：不嵌前端资源、不读外部文件，
  编译出来就是自包含单文件，因此也能 `go install`。
- **npm 包是纯启动器**（约 10KB，无二进制），首次运行按「平台 + 形态」从 GitHub
  Release 下载并缓存到 `~/.cache/reply9842/<version>/`。
- **CI 是单 job 内并发**（`xargs -P`，并发度按核数探测，上限 8），不是 matrix：
  本机 8 核实测 16 个产物串行 65 秒，matrix 会退化成 16 次 checkout + setup-go。

## 不可违反的设计立场

以下每条都有实测依据，属于「改了就坏」的硬约束：

1. **三道防静默损坏不可关闭**：zstd frame CRC（`internal/codec/dcz.go:87` 的
   `WithEncoderCRC(true)`）、
   dcz 头 BLAKE3 哈希、`ResultHash` 解码后校验。任一失败必须返回错误帧让客户端重发
   全量——没有关闭开关。
2. **一致性只靠内容哈希**：`store` 是纯 `hash → body` KV，无目录、无版本号、无会话 ID。
   查得到就用，查不到就 `DeltaMiss`。这是 RFC 9842 §2.1.3 的明文要求（服务端 ID 不得
   用于保证字典内容，必须校验哈希）。
3. **基线判定用块哈希命中率，不用公共前缀**：真实请求从第 45 字节就不同（`max_tokens`
   数值变），公共前缀永远只有 45 字节，实测区分度 **0%**；4 KB 块哈希命中率区分度约
   90 个百分点。
4. **不做块级去重（CDC）**：292 KB 请求实测 FastCDC 要 17 KB，整文档 zstd delta 只要
   1,297 B，差 13 倍。LLM 会话是尾部追加为主，LZ77 变长 match 天然赢。
5. **pump 与 client writer 完全解耦**：上游请求用 `context.WithoutCancel` 派生，
   客户端断开不取消上游。这是「请求能续上」的全部前提。
6. **续传以「已写给下游的字节」为准**，不是「已从隧道收到的字节」——只有前者代表
   下游真正消费到的内容；缓冲区里没刷出去的部分断线后必须重放。
7. **QUIC 回退实现在应用层**：HTTP/3 的自动回退是 HTTP 协议族能力（Alt-Svc），
   不是 QUIC 传输层自带。quic-go 连不上就是连不上，所以回退逻辑在 `DialAuto`。
8. **幂等键必须含 callerKeyId**：否则不同用户发相同内容会互相命中缓存，造成跨用户
   响应泄露。

## 目录结构

```
cmd/
├── cli/              本地入口 + 隧道客户端（serve/status/version 三子命令）
├── gui/              mygo native UI + 托盘 + 开机自启
└── server/           隧道服务端 + 解码校验 + 上游转发 + 双区域 spool

internal/
├── meta/             隧道帧格式（magic + 版本 + 类型 + 长度前缀）
├── codec/            dcz 编解码、自适应 level、静默损坏防护
├── downlink/         下行响应流式压缩（上行 dcz 的对偶）
├── store/            内容寻址 LRU（服务端字典缓存，零协商）
├── basefind/         客户端基线选择（4 KB 块哈希命中率）
├── transport/        QUIC + TCP 双栈 + auto 回退 + 源地址绑定
├── relayclient/      客户端运行时（配置归一化、Runner、设置持久化）
├── spool/            磁盘双区域流式缓冲（tmp/idem + 字节预算淘汰）
├── idem/             幂等键（callerKeyId + 请求指纹）
├── clientstream/     帧 → HTTP 响应 + 续传游标
├── autostart/        开机自启（launchd plist / HKCU Run）
├── glass/            GUI 玻璃材质调色板与 Surface
├── e2e/              断线续传与下行压缩的端到端测试（需 E2E=1）
└── stats/            逐请求与汇总统计（JSONL 即事实来源，重启不清零）

npm/                  纯 Node 启动器（bin/ + lib/），按需下载二进制
site/                 官网（纯 HTML/CSS，GitHub Pages）
scripts/deploy.sh     二进制部署（sha256 强校验）
```

## 验证方式

- `go test ./...` 全绿（当前 11 个有测试的包；`internal/e2e` 默认 skip，需 `E2E=1`
  且本地 relay-server 在 19443 运行）。
- `go vet ./...`、`gofmt` 干净。
- `make dist` 能交叉编译出全平台产物（命名与 CI 一致）。
- `cd npm && node --test test/*.test.js` 全绿。
- `openspec validate --all` 全绿。

## 相关文档

- `README.md`：产品介绍、实测数据、三个关键设计决策、已知限制。
- `INSTALL.md`：安装（go install / 预编译 / 源码构建）、图标工艺、首次运行。
- `DEPLOY.md`：服务端三种部署方式（Docker / systemd / npm）、反代配置。
