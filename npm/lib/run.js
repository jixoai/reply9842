#!/usr/bin/env node
// reply9842 — npm 引导器
//
// 设计（2026-11-13 Owner 裁决）：这个 npm 包不含任何二进制，只有一个启动器和
// 下载脚本。首次运行时按「当前平台 + 想要的产品形态」从 GitHub Release 下载
// 对应文件，缓存到本地后直接执行。
//
// 为什么这样更好：
//   - 包体从 136MB 降到几 KB，npm 安装秒级完成
//   - 用户只为自己的平台付一次下载，而不是装一次拿到 13 个二进制
//   - 缓存按版本隔离，换版本不会串
//
// 国内加速：GitHub Release 直连在国内经常超时。按前缀顺序尝试（社区惯例的
// gh-proxy 系列），任一成功即用；全部失败才回退直连。前缀列表可用
// REPLY9842_MIRROR 环境变量覆盖，见 lib/mirror.js 的契约说明。
//
// 断点续传：见 lib/download.js —— 边收边写 .part，中断后下次启动带 Range 续上，
// 并在 rename 前核对字节数与 SHA256（防「半截二进制跑出 SEGV」）。

"use strict";

const fs = require("node:fs");
const path = require("node:path");
const os = require("node:os");
const { spawn } = require("node:child_process");

const PKG = require("../package.json");
const { MIRROR_ENV, mirrorLabel, resolveMirrors } = require("./mirror");
const { downloadArtifact, fetchChecksums, tempPath } = require("./download");

const REPO = "jixoai/reply9842";
const KINDS = ["cli", "gui", "server"];

// ---- 平台解析 ----

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

/** 产物名，与 release.yml 的命名严格一致 */
function artifactName(kind) {
  const o = goos();
  const a = goarch();
  if (!o || !a) return null;
  // GUI 只有 darwin / windows（mygo native UI 无 Linux 后端）
  if (kind === "gui" && o === "linux") return null;
  const ext = o === "windows" ? ".exe" : "";
  return `${kind}-${o}-${a}${ext}`;
}

// ---- 缓存 ----

/** 缓存根：Windows 用 LOCALAPPDATA，其它走 XDG_CACHE_HOME 或 ~/.cache */
function cacheRoot() {
  if (process.platform === "win32") {
    const base = process.env.LOCALAPPDATA ||
      path.join(os.homedir(), "AppData", "Local");
    return path.join(base, "reply9842");
  }
  const base = process.env.XDG_CACHE_HOME || path.join(os.homedir(), ".cache");
  return path.join(base, "reply9842");
}

function binaryPath(kind, version) {
  const name = artifactName(kind);
  if (!name) return null;
  return path.join(cacheRoot(), version, name);
}

// ---- 下载 ----

function releaseUrl(version, name) {
  return `https://github.com/${REPO}/releases/download/v${version}/${name}`;
}

function checksumsUrl(version) {
  return `https://github.com/${REPO}/releases/download/v${version}/SHA256SUMS.txt`;
}

/** 进度条（stderr，10% 一档，避免刷屏拖慢传输） */
function makeProgress(name) {
  let lastPct = -1;
  let drawn = false;
  const clear = () => {
    if (drawn) process.stderr.write("\r" + " ".repeat(52) + "\r");
    drawn = false;
  };
  return {
    onProgress(ev) {
      if (ev.phase === "info") {
        process.stderr.write(`    ${ev.message}\n`);
        return;
      }
      if (ev.phase === "start" && ev.resumed) {
        process.stderr.write(
          `    续传：从 ${(ev.written / 1048576).toFixed(1)}MB / ${(ev.total / 1048576).toFixed(1)}MB 继续\n`,
        );
        lastPct = -1;
      }
      if (ev.phase !== "progress" || !ev.total) return;
      const pct = Math.floor((ev.written / ev.total) * 100);
      if (pct === lastPct) return;
      if (pct % 10 !== 0 && pct !== 100) return;
      lastPct = pct;
      drawn = true;
      process.stderr.write(
        `\r    下载 ${name} ${String(pct).padStart(3)}% (${(ev.written / 1048576).toFixed(1)}MB)`,
      );
    },
    clear,
  };
}

/** 依次尝试镜像下载，返回实际使用的镜像前缀 */
async function downloadWithMirrors(url, dest, { prefixes, expectedSha256 } = {}) {
  const list = prefixes || resolveMirrors();
  const prog = makeProgress(path.basename(dest));
  try {
    const { bytes, prefix } = await downloadArtifact({
      url,
      dest,
      prefixes: list,
      expectedSha256,
      onProgress: prog.onProgress,
      log: (line) => process.stderr.write(line + "\n"),
    });
    return { bytes, prefix, mirrors: list };
  } finally {
    prog.clear();
  }
}

// ---- 主流程 ----

/**
 * 解析镜像列表，失败时给出可操作提示并退出（不甩栈：这是配置错误，不是程序缺陷）。
 * @returns {string[]|null} 且在出错时已 process.exit
 */
function effectiveMirrors() {
  try {
    return resolveMirrors();
  } catch (e) {
    process.stderr.write(
      `reply9842: ${MIRROR_ENV} 配置有误 —— ${e.message}\n` +
        `  示例: ${MIRROR_ENV}=https://gh-proxy.com/  或  ${MIRROR_ENV}=direct\n`,
    );
    process.exit(1);
  }
}

function usage(kind) {
  const bins = {
    cli: "reply9842 [参数...]",
    gui: "reply9842-gui [参数...]",
    server: "reply9842-server [参数...]",
  };
  process.stderr.write([
    `reply9842 ${PKG.version} — RFC 9842 增量压缩隧道`,
    "",
    "用法:",
    `  ${bins[kind] || "reply9842 <cli|gui|server> [参数...]"}`,
    "",
    "首次运行会从 GitHub Release 下载对应平台的二进制（8-17MB），",
    "缓存后不再重复下载。中断后再次运行会从断点续传（.part 落盘）。",
    "",
    "镜像（加速前缀）配置:",
    `  ${MIRROR_ENV}=<前缀>[,<前缀>...]`,
    "",
    "  - 逗号分隔，顺序即优先级（回退链）；每项两侧空白会被忽略，空项跳过",
    "  - 前缀末尾的 / 可省略，会自动补全",
    "  - 写 direct 表示直连（如 REPLY9842_MIRROR=direct）；整串留空同样视为直连",
    "  - 只写前缀，程序会在后面拼完整 URL：<前缀>https://github.com/...",
    "  - 不设置该变量时使用下面内置的默认顺序",
    "",
    "  例:",
    `    ${MIRROR_ENV}=https://gh-proxy.com/ npx reply9842`,
    `    ${MIRROR_ENV}="https://my.mirror/,https://gh-proxy.com/,direct" npx reply9842`,
    "",
    "  当前生效顺序:",
    ...effectiveMirrors().map((p, i) => `    ${i + 1}. ${mirrorLabel(p)}${p ? `  (${p})` : ""}`),
    "",
    `缓存目录: ${cacheRoot()}`,
    "",
  ].join("\n"));
}

/** 根据调用名或首参数决定 kind */
function resolveKind(argv, argv1 = process.argv[1]) {
  const invoked = path.basename(argv1 || "");
  if (invoked === "reply9842-gui") return { kind: "gui", args: argv };
  if (invoked === "reply9842-server") return { kind: "server", args: argv };
  if (argv.length > 0 && KINDS.includes(argv[0])) {
    return { kind: argv[0], args: argv.slice(1) };
  }
  return { kind: "cli", args: argv };
}

async function ensureBinary(kind, version) {
  const name = artifactName(kind);
  if (!name) {
    process.stderr.write(
      `reply9842: 当前平台（${process.platform}/${process.arch}）没有 ${kind} 的预编译产物。\n` +
        (kind === "gui"
          ? "GUI 只提供 macOS 与 Windows 版本；Linux 请用 CLI 或从源码构建。\n"
          : `请到 https://github.com/${REPO}/releases 下载，或用 go install 安装。\n`),
    );
    process.exit(1);
  }

  const dest = binaryPath(kind, version);
  try {
    const st = await fs.promises.stat(dest);
    if (st.size > 0) return dest; // 已缓存，零网络开销
  } catch {
    /* 未缓存，继续下载 */
  }

  const url = releaseUrl(version, name);
  const prefixes = effectiveMirrors();
  const partialBytes = await fs.promises
    .stat(tempPath(dest))
    .then((s) => s.size)
    .catch(() => 0);

  process.stderr.write(
    `reply9842: 首次运行，下载 ${name}（v${version}）` +
      (partialBytes > 0
        ? `，检测到未完成的下载 ${(partialBytes / 1048576).toFixed(1)}MB，将续传\n`
        : "\n"),
  );

  // SHA256SUMS.txt 是 best-effort：取不到就只靠字节数校验，不阻塞下载
  let expectedSha256 = null;
  const sums = await fetchChecksums({ url: checksumsUrl(version), prefixes });
  if (sums) expectedSha256 = sums.get(name) || null;

  try {
    const { bytes, prefix } = await downloadWithMirrors(url, dest, {
      prefixes,
      expectedSha256,
    });
    process.stderr.write(
      `  已缓存到 ${dest}（${(bytes / 1048576).toFixed(1)}MB，来源 ${mirrorLabel(prefix)}` +
        `${expectedSha256 ? "，SHA256 已校验" : ""}）\n`,
    );
    return dest;
  } catch {
    process.stderr.write(
      `\nreply9842: 下载失败。\n` +
        `  手动下载：${url}\n` +
        `  放到：${dest}\n` +
        `  或源码构建：go install github.com/${REPO}/cmd/${kind}@v${version}\n` +
        `  镜像加速：${MIRROR_ENV}=https://gh-proxy.com/ npx reply9842\n`,
    );
    process.exit(1);
  }
}

async function main(argv = process.argv.slice(2)) {
  const { kind, args } = resolveKind(argv);

  if (args[0] === "-h" || args[0] === "--help") {
    usage(kind);
    return;
  }

  // 版本跟随 npm 包版本：包升级即视为「想要新版本二进制」，缓存按版本隔离。
  const bin = await ensureBinary(kind, PKG.version);

  const child = spawn(bin, args, { stdio: "inherit" });

  for (const sig of ["SIGINT", "SIGTERM", "SIGHUP"]) {
    process.on(sig, () => {
      if (!child.killed) child.kill(sig);
    });
  }

  child.on("error", (err) => {
    process.stderr.write(`reply9842: 启动失败：${err.message}\n`);
    process.exit(1);
  });

  child.on("exit", (code, signal) => {
    if (signal) {
      const n = { SIGINT: 2, SIGTERM: 15, SIGHUP: 1 }[signal] || 0;
      process.exit(128 + n);
    }
    process.exit(code == null ? 1 : code);
  });
}

/** 只有被 bin/*.js 或直接 node lib/run.js 启动时才跑主流程，测试里 require 保持纯函数可测 */
function invokedAsEntry() {
  if (require.main === module) return true;
  const name = path.basename((require.main && require.main.filename) || "");
  return (
    name === "reply9842.js" ||
    name === "reply9842-gui.js" ||
    name === "reply9842-server.js"
  );
}

if (invokedAsEntry()) {
  main().catch((e) => {
    process.stderr.write(`reply9842: ${e.stack || e.message}\n`);
    process.exit(1);
  });
}

module.exports = {
  artifactName,
  binaryPath,
  cacheRoot,
  ensureBinary,
  goarch,
  goos,
  main,
  releaseUrl,
  resolveKind,
  usage,
};
