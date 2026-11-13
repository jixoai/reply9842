## Why

改名提交（fa872d5，`🚚 项目改名 reply9842`）只覆盖了 Go 代码与 npm 包，官网、图标标题、
Windows 版本资源全部仍是旧名 `rfc9842-reply`。当前状态是**两个名字并存**：npm 包叫
`reply9842`、GitHub 仓库叫 `jixoai/reply9842`，而用户点开官网看到的品牌名和链接指向的是
`rfc9842-reply`（且那个链接已 404）。

依据（逐条可复核）：

```
rg -n 'rfc9842-reply' --glob '!*.png' --glob '!*.ico' --glob '!*.icns' --glob '!*.syso'
site/index.html:6    <title>rfc9842-reply — ...
site/index.html:13   <meta property="og:title" ...>
site/index.html:18   <meta property="og:url" content="https://rfc9842-reply.jixoai.com">
site/index.html:30   <span class="brand-name">rfc9842-reply</span>
site/index.html:38/73/489/490   https://github.com/jixoai/rfc9842-reply   ← 已失效链接
site/CNAME:1         rfc9842-reply.jixoai.com
site/style.css:2     rfc9842-reply — site styles
cmd/gui/assets/icon.svg:2     <title>rfc9842-reply</title>
cmd/gui/assets/tray.svg:2     <title>rfc9842-reply 托盘图标（template）</title>
cmd/gui/make_winres.sh:54/55  --product-name "rfc9842-reply" / --file-description "rfc9842-reply 客户端"
```

`README.md` 的旧域名已被并行改动清理（当前只剩 `https://reply9842.jixoai.com`），
但站点侧仍是旧名——文档与站点因此互相矛盾。

改名提交自己的裁决是「全部直接改，不保留旧路径」，因此这些残留属于**未执行完的承诺**，
不是刻意兼容。

## What Changes

- `site/index.html`：title / og:title / og:url / 品牌名（两处）/ 四处 GitHub 链接全部改为
  `reply9842`。
- `site/CNAME`：改为新域名（需 Owner 确认最终域名——见下「待定」）。
- `site/style.css` 头注释、`cmd/gui/assets/icon.svg` / `tray.svg` 的 `<title>` 同步改名。
- `cmd/gui/make_winres.sh` 的 `--product-name` / `--file-description` 改为 `reply9842`，
  并重跑 `make winres` 让 `.syso` 里的字符串真的更新（否则 exe 属性还是旧名）。
- 核对 `README.md` 的官网地址与新的 `site/CNAME` 一致（README 已清理，只需确认一致）。
- 决定 `RFC9842_BUNDLED` 环境变量是否改名：要么保留并**在文档里说明为什么**（迁移成本），
  要么改成 `REPLY9842_BUNDLED` 并保留旧名兼容读取。

**待 Owner 确认**：最终官网域名是 `reply9842.jixoai.com` 还是别的。CNAME 改错会让
Pages 自定义域失效，因此这一项不能在无人确认的情况下擅改。

## Capabilities

### New Capabilities

无。

### Modified Capabilities

- `brand-assets`: 追加「改名后全部对外标识必须一致，不得并存两个名字」需求；
  并把已知边界里那张残留表转成正式 Requirement。

## Impact

- 文件：`site/index.html`、`site/CNAME`、`site/style.css`、`cmd/gui/assets/icon.svg`、
  `cmd/gui/assets/tray.svg`、`cmd/gui/make_winres.sh`、`cmd/gui/rsrc_windows_amd64.syso`
  （重生成）、`README.md`。
- 部署：`site/CNAME` 变更涉及 DNS 与 GitHub Pages 自定义域，需要一次性窗口。
- 搜索可发现性：旧链接（含搜索引擎索引与外部引用）会失效；如需保留，应在旧域名做 301。
