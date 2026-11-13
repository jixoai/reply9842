# reply9842

**RFC 9842 增量压缩隧道：把 AI Agent 的上行带宽砍掉 99.8%。**

一个 npm 包，内含全平台（macOS / Linux / Windows）的三种可执行文件：

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

### go install（需要 Go 1.27.3+）

```bash
go install github.com/jixoai/rfc9842-reply/cmd/cli@latest
go install github.com/jixoai/rfc9842-reply/cmd/gui@latest
go install github.com/jixoai/rfc9842-reply/cmd/server@latest
```

## 快速开始

### 客户端

```bash
reply9842 -server <服务端地址>:28443 -server-name magpie.aiweb.xin
```

然后把 Agent 的上游指过来：

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:18899
# 或
export OPENAI_BASE_URL=http://127.0.0.1:18899/v1
```

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

## 这个包为什么这么大

单包内含 **13 个平台二进制**（约 150 MB）。这是刻意的取舍：

- **一次安装，零解析**：不需要在安装时探测平台、下载对应产物、校验
- **零失败路径**：没有「你的平台没有预编译产物」这种错误
- **离线可用**：装完就能跑，不依赖网络

如果包体成为问题，未来会拆成 `optionalDependencies` 按平台分发。

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

详细设计与实测数据见 [GitHub 仓库](https://github.com/jixoai/rfc9842-reply)。

## 许可证

MIT
