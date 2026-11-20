# fix-update-check-ratelimit

## Why

2026-11-20 实测：本机直连 `api.github.com/repos/jixoai/reply9842/releases/latest`
返回 403（`API rate limit exceeded for 35.78.235.132`）。未认证配额是
60 次/小时/IP，公司 NAT/机场出口很容易被别人打满；更新检查因此失败，
而下载用的镜像链完全没有参与这一步。

同一时间实测三个默认镜像对 API 路径的支持：

| 镜像 | `mirror + https://api.github.com/...` |
|---|---|
| `https://gh-proxy.com/` | **200，返回完整 release JSON** |
| `https://ghfast.top/` | 连接失败 |
| `https://ghproxy.net/` | 连接失败 |

即：镜像加速 API 是**部分可行**的，不能作为唯一方案；本地 `gh` CLI 的登录
token（5000 次/小时）才是稳定解法。

## What Changes

- token 解析顺序扩展为：`Options.Token` → `GH_TOKEN` → `GITHUB_TOKEN` →
  本机 `gh auth token`（3s 超时，失败静默为空，不缓存、不落盘、不写日志）。
- `fetchLatest` 改为候选链：直连（带 token）→ 配置的镜像前缀 + `api.github.com`
  （**不带** Authorization，6s/个）→ 返回直连的原始错误。
- 直连 404 仍是权威结论（仓库没有 release），不再请求镜像。
- 限流错误优先返回（提示限额重置时间），而不是被镜像的 404/连接失败覆盖。
- README 补充 token/镜像的边界说明。

## 安全边界

`Authorization` 只发往直连的 `api.github.com`；镜像请求永不带 token。
