# Tasks: fix-update-check-ratelimit

- [x] 1.1 `token()` 增加 `GH_TOKEN`、`GITHUB_TOKEN` 与本机 `gh auth token` 兜底
- [x] 1.2 `fetchLatest` 改为「直连带 token → 镜像不带 token」候选链，镜像 6s 超时
- [x] 1.3 直连 404 保持权威、限流错误优先返回、镜像 404 只当作该镜像不可用
- [x] 1.4 测试：限流后镜像成功且镜像收不到 Authorization；直连 404 不请求镜像；token 优先级
- [x] 2.1 README 说明 token 解析顺序与镜像边界
- [x] 3.1 `go test ./...` / `go vet ./...` / `go build ./...` 全绿
