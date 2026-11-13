# 部署服务端

服务端（`reply9842-server`）是隧道的远端终点：接收客户端的增量请求、还原、
转发给上游 AI 网关，并把上游响应压缩回传。

三种部署方式，按省事程度排序。

## 一、Docker（推荐）

### 1. 准备证书

客户端用 `-server-name` 校验服务端证书，所以证书必须是**客户端信任的域名证书**。

```bash
mkdir -p certs
cp /etc/letsencrypt/live/example.com/fullchain.pem certs/
cp /etc/letsencrypt/live/example.com/privkey.pem certs/
# 容器内以 UID 65532 运行：私钥必须让这个 UID 读得到。
# 只 chmod 600 不够——属主还是 root，容器会报
#   listen: load cert: open /certs/privkey.pem: permission denied
chown 65532:65532 certs/privkey.pem certs/fullchain.pem
chmod 600 certs/privkey.pem
```

> 没有证书？先用自签跑通链路：客户端加 `-insecure` 即可跳过校验。
> 生产别这么干——中间人可以解开全部流量。

### 2. 启动

```bash
docker compose -f docker-compose.server.yml up -d
```

默认端口：`8443`（隧道，TCP + UDP 都要）、`127.0.0.1:8080`（健康检查，不对外）。

环境变量可覆盖：

| 变量 | 默认 | 说明 |
|---|---|---|
| `TUNNEL_PORT` | 8443 | 宿主机隧道端口 |
| `HEALTH_PORT` | 8080 | 宿主机健康检查端口（只绑回环） |
| `UPSTREAM` | `http://127.0.0.1:3431` | 上游网关地址 |
| `UPHOST` | `magpie.aiweb.xin` | 转发时覆盖的 Host 头 |

### 3. 验证

```bash
# 健康检查
curl -s localhost:8080/healthz          # 期望 ok

# 容器健康状态
docker inspect reply9842-server --format '{{.State.Health.Status}}'
```

### 上游在同机怎么办

`UPSTREAM` 默认是 `127.0.0.1:3431`，但容器内的 `127.0.0.1` 是容器自己。
两种情况：

**上游也是容器**（推荐）：放进同一个 compose 网络，`UPSTREAM=http://magpie:3431`。

**上游是宿主机服务**：

```yaml
    extra_hosts:
      - "host.docker.internal:host-gateway"
```

然后 `UPSTREAM=http://host.docker.internal:3431`。

## 二、二进制 + systemd

从 [Releases](https://github.com/jixoai/reply9842/releases) 下载 `server-<os>-<arch>`，
或 `go install github.com/jixoai/reply9842/cmd/server@latest`。

```ini
# /etc/systemd/system/reply9842-server.service
[Unit]
Description=reply9842 server
After=network.target

[Service]
Type=simple
User=reply9842
WorkingDirectory=/opt/reply9842
ExecStart=/opt/reply9842/reply9842-server \
  -listen 0.0.0.0:8443 \
  -health-addr 127.0.0.1:8080 \
  -cert /opt/reply9842/certs/fullchain.pem \
  -key /opt/reply9842/certs/privkey.pem \
  -spool-dir /var/lib/reply9842/spool \
  -upstream http://127.0.0.1:3431
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl enable --now reply9842-server
```

## 三、npm

```bash
npx reply9842-server -listen 0.0.0.0:8443 -cert ... -key ...
```

npm 包只含启动器，首次运行自动下载对应平台的二进制（见 [npm 说明](./npm/README.md)）。

## 参数

| 参数 | 默认 | 说明 |
|---|---|---|
| `-listen` | `0.0.0.0:28443` | 隧道监听（QUIC 与 TCP/TLS 同端口） |
| `-upstream` | `http://127.0.0.1:3431` | 上游网关地址 |
| `-uphost` | `magpie.aiweb.xin` | 转发时覆盖的 Host 头 |
| `-cert` / `-key` | 必填 | TLS 证书与私钥 |
| `-spool-dir` | `/var/lib/reply9842/spool` | 流式 spool 根目录 |
| `-tmp-mb` | 512 | 活跃区磁盘预算（MB） |
| `-idem-mb` | 2048 | 幂等区磁盘预算（MB） |
| `-cache-mb` | 256 | 基版本字典缓存（内存，MB） |
| `-upstream-timeout` | 10m | 上游超时（**不随客户端断开取消**） |
| `-health-addr` | 空 | 健康检查监听；留空不启用 |
| `-health-probe` | false | 探针模式：GET 自己的 /healthz，成功 exit 0 |

## 网络要求

- **TCP 8443**：TCP/TLS 隧道
- **UDP 8443**：QUIC 隧道（客户端 `-transport auto` 会先试 QUIC）

只放通 TCP 也能工作——客户端会自动降级到 TCP。两组都放通则 QUIC 可用，
弱网下重连更快（0-RTT）。

## 磁盘与内存

- **spool** 是磁盘承载的响应缓冲，预算由 `-tmp-mb` / `-idem-mb` 控制。
  断线重连要靠它，建议放在持久卷上（Docker 的 named volume 或宿主目录）。
- **基版本缓存**（`-cache-mb`）在内存，存的是请求体字典（用于还原增量）。
  单条字典最大约等于单次请求体大小（典型 1–2MB）。

## 反向代理注意

若前面还有 nginx/openresty：

```nginx
location / {
    proxy_pass http://127.0.0.1:8443;
    proxy_http_version 1.1;
    proxy_buffering off;          # SSE 流式必须
    proxy_read_timeout 600s;      # 上游 LLM 可能跑很久
    proxy_send_timeout 600s;
}
```

**不要**在反代层再压缩响应——隧道内已经用 zstd 压过，重复压缩会浪费 CPU。
