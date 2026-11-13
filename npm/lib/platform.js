// 平台检测：把 Node 的 process.platform/arch 映射到发布产物命名。
//
// 产物命名规则（与 .github/workflows/release.yml 严格一致）：
//   <kind>-<goos>-<goarch>[.exe]
// 例：cli-darwin-arm64、gui-windows-amd64.exe、server-linux-amd64
//
// 全部产物都打在同一个 npm 包的 vendor/ 目录下（不拆分 optionalDependencies），
// 用户装一次就有全平台二进制。代价是包体大，换来的是「零解析、零下载失败」。

"use strict";

const path = require("node:path");

/** 支持的产物名，与 CI 构建矩阵一致 */
const SUPPORTED = new Set([
  "cli-darwin-amd64",
  "cli-darwin-arm64",
  "cli-linux-amd64",
  "cli-linux-arm64",
  "cli-windows-amd64.exe",
  "gui-darwin-amd64",
  "gui-darwin-arm64",
  "gui-windows-amd64.exe",
  "server-darwin-amd64",
  "server-darwin-arm64",
  "server-linux-amd64",
  "server-linux-arm64",
  "server-windows-amd64.exe",
]);

function goos() {
  switch (process.platform) {
    case "darwin":
      return "darwin";
    case "linux":
      return "linux";
    case "win32":
      return "windows";
    default:
      return null;
  }
}

function goarch() {
  switch (process.arch) {
    case "x64":
      return "amd64";
    case "arm64":
      return "arm64";
    default:
      return null;
  }
}

/**
 * 当前平台的产物名（不含目录），平台不支持时返回 null。
 * @param {"cli"|"gui"|"server"} kind
 */
function artifactName(kind) {
  const os = goos();
  const arch = goarch();
  if (!os || !arch) return null;
  const ext = os === "windows" ? ".exe" : "";
  const name = `${kind}-${os}-${arch}${ext}`;
  return SUPPORTED.has(name) ? name : null;
}

/**
 * 某个 kind 在当前平台的可执行文件绝对路径。
 * 返回 null 表示该平台不支持（例如 Linux 上没有 GUI 产物）。
 */
function binaryPath(kind) {
  const name = artifactName(kind);
  if (!name) return null;
  return path.join(__dirname, "..", "vendor", name);
}

module.exports = { binaryPath, artifactName, goos, goarch, SUPPORTED };
