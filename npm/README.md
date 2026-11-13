# reply9842

**RFC 9842 增量压缩隧道：把 AI Agent 的上行带宽砍掉 99.8%。**

一个 npm 包，内含全平台（macOS / Linux / Windows）三种可执行文件的**按需下载启动器**
（首次运行时只拉当前平台那一个二进制）：

| 命令 | 用途 |
|---|---|
| `reply9842` | 本地转发客户端（把 Agent 请求压缩后经隧道发到远端） |
| `reply9842-gui` | 图形界面（macOS / Windows） |
| `reply9842-server` | 隧道服务端（部署在远端） |

## 为什么需要它

自建 AI Agent 每次调用都要把完整上下文（system prompt + tools schema + 多轮历史）重新上传。
实测某生产 Agent：日请求 2.2 万次、日上行 20–44 GB、单请求中位 1.4 MB。

**相邻请求之间有 90% 以上的字节完全相同**——上一轮已经传过了。

本项目在客户端与服务端之间建立一条内容寻址的增量通道：只传差异，实测 **1.4 MB → 约 1 KB**。

## 安装

### npm

```bash
# 直接用，不安装
npx reply9842 --help

# 或全局安装
npm i -g reply9842
```

首次运行会从 GitHub Release 下载当前平台的二进制（8–17MB）并缓存到本地，之后不再重复下载。
中断（Ctrl-C、断网、超时）后再次运行会从 `.part` 断点续传，并在落盘前校验字节数与
`SHA256SUMS.txt`——不会出现「半截二进制跑出 SEGV」的情况。

#### 下载镜像（加速前缀）

国内直连 GitHub Release 常常超时。用 `REPLY9842_MIRROR` 指定加速前缀：

```bash
# 单个镜像
REPLY9842_MIRROR=https://gh-proxy.com/ npx reply9842 --help

# 多条回退链（从左到右依次尝试，最后 direct 表示直连兜底）
REPLY9842_MIRROR="https://my.mirror/,https://gh-proxy.com/,direct" npx reply9842

# 强制直连（绕开默认镜像）
REPLY9842_MIRROR=direct npx reply9842
```

规则：

- 逗号分隔，顺序即优先级；每项两侧空白忽略，空项跳过
- 只写前缀，程序在后面拼完整 URL（`<前缀>https://github.com/...`）；末尾 `/` 可省略，会自动补
- `direct` 表示直连；整个变量留空同样视为直连
- 不设置该变量时用内置顺序：`gh-proxy.com` → `ghfast.top` → `ghproxy.net` → 直连
- 不合法的前缀（非 http/https）会直接报错，不会被静默忽略
- 实际生效的顺序会打印在 `--help` 里

### go install（需要 Go 1.27.3+）

```bash
go install github.com/jixoai/reply9842/cmd/cli@latest
go install github.com/jixoai/reply9842/cmd/gui@latest
go install github.com/jixoai/reply9842/cmd/server@latest
```

## 快速开始

### 客户端

```bash
reply9842 -server <服务端地址>:28443 -server-name magpie.aiweb.xin
```

然后把要经过隧道的客户端的 base URL 指向本地入口：

```
http://127.0.0.1:18899
```

具体怎么配取决于你的客户端——设置它的 base URL / 上游地址即可。
reply9842 不绑定任何特定服务商或协议实现，它只是一个 HTTP 正向转发。

### 服务端（远端部署）

```bash
reply9842-server \
  -listen 0.0.0.0:28443 \
  -upstream http://127.0.0.1:3431 \
  -cert /path/to/fullchain.pem \
  -key /path/to/privkey.pem
```

### 图形界面

```bash
reply9842-gui
```

显示连接状态、请求数、压缩比、累计节省流量、p50 延迟，一键复制配置命令。

## 平台支持

| 平台 | CLI | GUI | Server |
|---|---|---|---|
| macOS arm64 / amd64 | ✅ | ✅ | ✅ |
| Linux amd64 / arm64 | ✅ | ❌ | ✅ |
| Windows amd64 | ✅ | ✅ | ✅ |

Linux 没有 GUI 产物：mygo native UI 的 Linux 后端依赖 GTK3，暂不提供。

## 包体与下载

包体本身只有几 KB：不含二进制，只带启动器与下载逻辑。首次运行按「当前平台 + 形态」从
GitHub Release 拉取对应产物（8–17MB），缓存到平台缓存目录并按包版本隔离：

- 用户只为自己的平台付一次下载，而不是装一次拿到 13 个二进制
- 缓存命中零网络开销；换版本不会串包
- 下载中断可续传，落盘前校验大小与 SHA256
- 如果平台没有预编译产物（例如 Linux 上的 GUI），会给出明确提示与源码构建指引


## 原理

```
Agent ──► reply9842 (本地)
            │
            ├─ 在历史请求里找「块哈希命中率」最高的当基线
            ├─ zstd delta：把基线当字典压缩新请求
            ├─ 40 字节 dcz 头（magic + BLAKE3 哈希）+ zstd 流
            └─ QUIC / TCP 隧道
                        │
                        ▼
                  reply9842-server
                        │
                        ├─ 按哈希取字典，解 delta
                        ├─ 三道防静默损坏校验
                        └─ 转发到上游
```

详细设计与实测数据见 [GitHub 仓库](https://github.com/jixoai/reply9842)。

## 许可证

MIT
