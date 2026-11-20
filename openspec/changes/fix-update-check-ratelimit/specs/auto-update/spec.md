## MODIFIED Requirements

### Requirement: 版本检查与 semver 比较

系统 SHALL 通过 GitHub API（`releases/latest`）查询最新版本，与当前版本做 **semver
比较**而非字符串比较（`v0.10.0` 必须大于 `v0.2.1`）。API 限流时必须返回明确错误而非
崩溃，并按以下顺序解析 token：`Options.Token` → `GH_TOKEN` → `GITHUB_TOKEN` →
本机 `gh auth token`。直连失败（网络错误或限流）时 SHALL 使用已配置的镜像前缀
重试同一 API 路径（每个镜像 6s 超时），且镜像请求**不得**携带 Authorization。
直连 404 是权威结论，不得回落到镜像。

落地证据：`internal/update/check.go`（Check / fetchLatest / 限流分类）、
`internal/update/token.go`（gh CLI 兜底）、`internal/update/check_mirror_test.go`、
`internal/update/token_test.go`、`internal/update/semver.go`。

#### Scenario: 两位数版本号

- **WHEN** 当前 `v0.2.1`、最新 `v0.10.0`
- **THEN** 判定有新版本（字符串比较会得出相反结论）

#### Scenario: GitHub API 限流

- **WHEN** 未认证配额（60 次/小时）用尽，直连 API 返回 403/429
- **THEN** 依次尝试配置的镜像前缀；任一镜像返回合法 release JSON 则检查成功；
  全部失败时返回可读的限流错误（含重置时间），不 panic、不阻塞界面

#### Scenario: token 不能泄露给镜像

- **WHEN** 直连返回限流且请求携带 token
- **THEN** 后续镜像请求的 `Authorization` 头为空

#### Scenario: 仓库确实没有 release

- **WHEN** 直连 API 返回 404
- **THEN** 直接返回「没有 release」，不请求镜像
