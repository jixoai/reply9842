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
| `UPSTREAM` | `http://127.0.0.1:3425` | 上游网关地址 |
| `UPHOST` | `magpie.aiweb.xin` | 转发时覆盖的 Host 头 |

### 3. 验证

```bash
# 健康检查
curl -s localhost:8080/healthz          # 期望 ok

# 容器健康状态
docker inspect reply9842-server --format '{{.State.Health.Status}}'
```

### 上游在同机怎么办

`UPSTREAM` 默认是 `127.0.0.1:3425`，但容器内的 `127.0.0.1` 是容器自己。
两种情况：

**上游也是容器**（推荐）：放进同一个 compose 网络，`UPSTREAM=http://magpie:3425`。

**上游是宿主机服务**：

```yaml
    extra_hosts:
      - "host.docker.internal:host-gateway"
```

然后 `UPSTREAM=http://host.docker.internal:3425`。

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
  -upstream http://127.0.0.1:3425
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl enable --now reply9842-server
```

## 二·五、中继机（多线路的第二条路径）

客户端的「多线路」里，`relay` 类型指向**另一台** server：客户端连到中继，
中继再把请求转发给真正的上游。用途是「本机到香港这一段劣化时换一条路径」——
中继机通常放在网络条件不同的机房（如上海），与主服务器互为兜底。

中继机与主服务器是**同一个二进制**，三处必须特别处理：

### 1. 证书：复用主服务器的证书

中继指向的是另一台 **IP**，但客户端仍用主服务器的域名做 SNI 校验，所以中继机上
放的必须是**同一张证书**（`magpie.aiweb.xin`），不是为 IP 签的新证书。

客户端侧对应要填 `server_name`（CLI 的 JSON 字段 / GUI 线路编辑区的「证书域名」）。
不填的表现是 `x509: cannot validate certificate for <ip> ... no IP SANs`。

### 2. 上游：可以指公网域名，不必是本机网关

主服务器的 `-upstream` 指向本机 magpie 网关（`http://127.0.0.1:3425,...`）。
中继机上通常**没有**那个网关，把上游指向公网域名即可：

```bash
-upstream https://magpie.aiweb.xin:443 -uphost magpie.aiweb.xin
```

**端口不能省**。写成 `-upstream https://magpie.aiweb.xin`（无 `:443`）时 Go 的
dialer 会报 `dial tcp: address magpie.aiweb.xin: missing port in address`，
客户端侧看到的是 502——而且日志里只有这一行，很容易误判成网络不通。

### 3. 资源：按兜底定位给较小的预算

中继是备用路径，不是主力，无需与主服务器同规格：

```bash
-cache-mb 64 -tmp-mb 128 -idem-mb 256
```

### 4. systemd unit（实测可用）

```ini
[Unit]
Description=reply9842 relay (fallback route)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=admin
WorkingDirectory=/opt/reply9842-2
ExecStart=/opt/reply9842-2/reply9842-server \
  -listen 0.0.0.0:28443 \
  -upstream https://magpie.aiweb.xin:443 \
  -uphost magpie.aiweb.xin \
  -cert /opt/reply9842-2/certs/cert.pem \
  -key /opt/reply9842-2/certs/key.pem \
  -cache-mb 64 -tmp-mb 128 -idem-mb 256 \
  -spool-dir /opt/reply9842-2/spool \
  -log /opt/reply9842-2/log/server.jsonl \
  -stats /opt/reply9842-2/log/stats.json
Restart=always
RestartSec=5
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
```

注意：**换 unit 之前先停掉手起的进程**。`nohup` 起过的实例会继续占着
28443，systemd 启动时只报 `listen: quic listen: bind: address already in use`
然后进入 restart 循环，`systemctl status` 显示 `activating` 而不是 `failed`，
很容易误判成「正在启动」。

### 5. 客户端的代价：跨 server 的 delta 基线不共享

delta 基线是 **server 侧状态**。中继与主服务器各存各的，跨 server 切换会偶发
一次 `delta-miss` → 客户端全量重发。数据正确，只多一次带宽。客户端启动日志
会明确打印这条提示。

## 三、npm

`reply9842` 是**一个** npm 包，内含三个 bin：`reply9842` / `reply9842-gui` /
`reply9842-server`。**没有**一个叫 `reply9842-server` 的独立包，所以
`npx reply9842-server` 会 404——必须用 `-p` 指定包名再点名 bin：

```bash
# 服务端
npx -y -p reply9842 reply9842-server -listen 0.0.0.0:8443 -cert ... -key ...

# 客户端 CLI
npx -y -p reply9842 reply9842 -listen 127.0.0.1:18899 -server magpie.aiweb.xin:28443

# 固定版本（生产建议）
npx -y -p reply9842@0.4.0 reply9842-server ...
```

npm 包只含启动器，首次运行自动下载对应平台的二进制（见 [npm 说明](./npm/README.md)）。

## 参数

| 参数 | 默认 | 说明 |
|---|---|---|
| `-listen` | `0.0.0.0:28443` | 隧道监听（QUIC 与 TCP/TLS 同端口） |
| `-upstream` | `http://127.0.0.1:3425,http://127.0.0.1:3426` | 上游网关地址；逗号分隔多个候选，按序拨号（蓝绿双槽） |
| `-uphost` | `magpie.aiweb.xin` | 转发时覆盖的 Host 头 |
| `-cert` / `-key` | 必填 | TLS 证书与私钥 |
| `-spool-dir` | `/var/lib/reply9842/spool` | 流式 spool 根目录 |
| `-tmp-mb` | 512 | 活跃区磁盘预算（MB） |
| `-idem-mb` | 2048 | 幂等区磁盘预算（MB） |
| `-spool-hydrate` | true | 监听就绪后后台补水幂等区；启动 O(1)，可用性不依赖它 |
| `-spool-hydrate-batch` | 200 | 补水每批条数（批间让路 10ms） |
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
  启动不扫描幂等区（与条目数无关）：续传按 key 冷查文件，目录维护在监听
  就绪后由 `-spool-hydrate` 后台完成。
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
