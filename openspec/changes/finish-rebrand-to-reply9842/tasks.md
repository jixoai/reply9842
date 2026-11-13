# Tasks: finish-rebrand-to-reply9842

## 1. 待 Owner 裁决（阻塞后续）

- [ ] 1.1 确认最终官网域名（候选：`reply9842.jixoai.com`）。这是 `site/CNAME` 的唯一
      输入，改错会让 Pages 自定义域失效
- [ ] 1.2 确认旧域名是否需要 301 保留（涉及 DNS + Pages 双域配置）

## 2. 代码与站点

- [ ] 2.1 `site/CNAME` 写入最终域名（单行，无换行尾随空格）
- [ ] 2.2 `site/index.html`：`<title>` / `og:title` / `og:url` / `.brand-name`（两处）/
      四处 `https://github.com/jixoai/rfc9842-reply` 全部改为 `reply9842`
- [ ] 2.3 `site/style.css` 头注释改名
- [ ] 2.4 `cmd/gui/assets/icon.svg` 与 `tray.svg` 的 `<title>` 改名
- [ ] 2.5 `cmd/gui/make_winres.sh`：`--product-name` 与 `--file-description` 改为
      `reply9842`，然后 `make winres` 重生成 `.syso`
- [ ] 2.6 `README.md`：官网地址与新 `site/CNAME` 一致（旧域名已被清理，只需核对一致）
- [ ] 2.7 `RFC9842_BUNDLED` 决策落地（保留+说明 / 改名+兼容读取），并同步 `INSTALL.md`

## 3. 验证

- [ ] 3.1 `rg -n 'rfc9842-reply' --glob '!*.png' --glob '!*.ico' --glob '!*.icns'
      --glob '!*.syso'` 输出为空（唯一允许存在的例外要在文档里写明理由）
- [ ] 3.2 `file dist/gui-windows-amd64.exe` 后查 exe 版本信息，`ProductName` 为新名
- [ ] 3.3 `site/` 本地 `python3 -m http.server` 走查：品牌名、页脚链接、GitHub 链接可点
- [ ] 3.4 Pages 部署后 `curl -sI https://<最终域名>` 返回 200

## 验收标准

- 全仓库（排除二进制资源）不再出现 `rfc9842-reply`，或每一处残留都在文档里说明理由。
- 官网品牌名、GitHub 链接、CNAME 三处指向同一个新域名。
- Windows 产物属性里的产品名是 `reply9842`。
