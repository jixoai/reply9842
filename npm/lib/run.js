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
// 国内加速：GitHub Release 直连在国内经常超时。按下列前缀顺序尝试（社区惯例的
// gh-proxy 系列），任一成功即用；全部失败才回退直连。

"use strict";

const fs = require("node:fs");
const path = require("node:path");
const os = require("node:os");
const { spawn } = require("node:child_process");

const PKG = require("../package.json");

// 加速前缀。顺序即优先级：先国内镜像，最后官方直连兜底。
// 这类代理的契约是 `<prefix><完整 url>`，社区通用。
const MIRRORS = [
  "https://gh-proxy.com/",
  "https://ghfast.top/",
  "https://ghproxy.net/",
  "", // 空串 = 直连
];

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

/** 下载到 dest，带进度。mirror 为前缀，空串表示直连。 */
async function download(url, dest, mirror) {
  const res = await fetch(mirror + url, { redirect: "follow" });
  if (!res.ok) throw new Error(`HTTP ${res.status}`);

  const tmp = `${dest}.part`;
  await fs.promises.mkdir(path.dirname(dest), { recursive: true });

  const total = Number(res.headers.get("content-length") || 0);
  const chunks = [];
  let got = 0;
  let lastPct = -1;

  for await (const chunk of res.body) {
    chunks.push(chunk);
    got += chunk.length;
    if (total > 0) {
      const pct = Math.floor((got / total) * 100);
      if (pct !== lastPct && pct % 10 === 0) {
        process.stderr.write(`\r  下载 ${pct}% (${(got / 1048576).toFixed(1)}MB)`);
        lastPct = pct;
      }
    }
  }
  if (total > 0) process.stderr.write("\r" + " ".repeat(40) + "\r");

  await fs.promises.writeFile(tmp, Buffer.concat(chunks));
  await fs.promises.rename(tmp, dest); // 原子落盘
  if (process.platform !== "win32") await fs.promises.chmod(dest, 0o755);
}

/** 依次尝试镜像，任一成功即返回 */
async function downloadWithMirrors(url, dest) {
  let lastErr;
  for (const m of MIRRORS) {
    const label = m ? new URL(m).hostname : "github.com（直连）";
    process.stderr.write(`  尝试 ${label} …\n`);
    try {
      await download(url, dest, m);
      return true;
    } catch (e) {
      lastErr = e;
      process.stderr.write(`    失败：${e.message}\n`);
    }
  }
  throw lastErr || new Error("全部镜像失败");
}

// ---- 主流程 ----

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
    "缓存后不再重复下载。国内网络自动走 gh-proxy 加速。",
    "",
    `缓存目录: ${cacheRoot()}`,
    "",
  ].join("\n"));
}

/** 根据调用名或首参数决定 kind */
function resolveKind(argv) {
  const invoked = path.basename(process.argv[1] || "");
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

  process.stderr.write(`reply9842: 首次运行，下载 ${name}（v${version}）\n`);
  const url = releaseUrl(version, name);
  try {
    await downloadWithMirrors(url, dest);
    process.stderr.write(`  已缓存到 ${dest}\n`);
    return dest;
  } catch {
    process.stderr.write(
      `\nreply9842: 下载失败。\n` +
        `  手动下载：${url}\n` +
        `  放到：${dest}\n` +
        `  或源码构建：go install github.com/${REPO}/cmd/${kind}@v${version}\n`,
    );
    process.exit(1);
  }
}

async function main() {
  const { kind, args } = resolveKind(process.argv.slice(2));

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

main().catch((e) => {
  process.stderr.write(`reply9842: ${e.stack || e.message}\n`);
  process.exit(1);
});
