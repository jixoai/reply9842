## ADDED Requirements

### Requirement: 对外标识必须单一，不得并存两个名字

改名后，所有对外可见的标识（站点 title/og/brand、GitHub 链接、CNAME、图标资源内的
title、Windows PE 版本字符串、文档中的域名）SHALL 统一为 `reply9842` 及其对应域名。
任何残留旧名必须在文档中显式说明保留理由（例如为兼容已有用户环境而保留的环境变量名）。

#### Scenario: 搜索旧名无残留

- **WHEN** 执行
  `rg -n 'rfc9842-reply' --glob '!*.png' --glob '!*.ico' --glob '!*.icns' --glob '!*.syso'`
- **THEN** 输出为空，或每一处命中都在 `openspec/specs/brand-assets/spec.md` 的
  「允许保留」清单里有理由

#### Scenario: 站点链接可用

- **WHEN** 打开官网并点击任一 GitHub 链接
- **THEN** 跳到 `https://github.com/jixoai/reply9842` 且返回 200，不是 404

#### Scenario: Windows 产物属性为新名

- **WHEN** 右键 `gui-windows-amd64.exe` 查看「属性 → 详细信息」
- **THEN** 产品名是 `reply9842`，不是 `rfc9842-reply`

### Requirement: CNAME 是部署的单一事实来源

`site/CNAME` SHALL 只包含最终域名一行，且与 README 与 DNS 记录一致。修改 CNAME 属于
需要一次性切换窗口的部署动作（Pages 自定义域会短暂失效并重新申请 TLS）。

#### Scenario: 三处一致

- **WHEN** 对比 `site/CNAME`、`README.md` 里的官网地址、DNS 的 CNAME 记录
- **THEN** 三者指向同一个域名

#### Scenario: 切换期可观测

- **WHEN** CNAME 变更并部署后
- **THEN** `curl -sI https://<域名>` 返回 200 且证书覆盖该域名
