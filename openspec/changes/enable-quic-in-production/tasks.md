# Tasks: enable-quic-in-production

## 1. 先实测，再决定（阻塞后续所有项）

- [ ] 1.1 在目标生产链路（当前 `-server` 默认值）上用 `-transport quic` 连 20 次，
      记录成功率、握手耗时、失败错误文本
- [ ] 1.2 同样测 `-transport auto`，记录「每次连接多付的探测超时」总量
      （`QuicProbe` 默认 1 s）
- [ ] 1.3 把两组数据写进 proposal 的「实测」小节，据此在 (a) 启用 / (b) 不启用 中裁决

## 2. 按裁决落地

### 若选 (a) 启用

- [ ] 2.1 服务器防火墙放通 UDP 28443（或生产端口），`docker-compose.server.yml` 的
      UDP 映射确认生效
- [ ] 2.2 `quic.Config` 显式加 `Allow0RTT: true`，并补一个 0-RTT 行为的 e2e
- [ ] 2.3 `DefaultTransport` 保持 `auto`，并在代码注释里写下「为什么推翻 0194704 的
      tcp 决定」+ 实测数据引用
- [ ] 2.4 README 已知限制第 3 条改写为「QUIC 已启用，实测成功率 X%」

### 若选 (b) 不启用

- [ ] 2.5 `DefaultTransport` 改为 `"tcp"`，注释记录实测依据（链路上 QUIC 从未握手成功）
- [ ] 2.6 `README.md` 已知限制第 3 条与 `DEPLOY.md` 的网络要求改写为「QUIC 已实现但默认
      不启用；启用需自行验证链路」
- [ ] 2.7 **删掉 DEPLOY.md 里「0-RTT」的措辞**（当前代码没有 `Allow0RTT`，该能力不存在）
- [ ] 2.8 GUI 的 transport 分段控件保留 quic 选项（手动可选），但默认档在注释里说明原因

## 3. 护栏

- [ ] 3.1 `internal/relayclient/default_test.go` 新增 `TestDefaultTransport`：断言当前
      默认值，并在注释里写明「改这个值必须同步改 README 与 DEPLOY」
- [ ] 3.2 `rg -n '0-RTT' README.md DEPLOY.md` 的结果与代码里的 `Allow0RTT` 配置一致
      （有配置才能写进文档）

## 验收标准

- 「默认 transport 是什么」在一处（代码常量 + 一条断言 + 文档一句）说得清，且三者一致。
- 文档里不再出现代码不支持的协议能力（0-RTT 是当前的违例）。
- 若启用 QUIC：连续 20 次 `-transport quic` 全部成功，且 `TestQUICRoundTrip` 与新增
  0-RTT e2e 通过。
