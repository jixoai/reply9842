#!/usr/bin/env node
// npm 分发层的统一启动器：三个 bin（reply9842 / reply9842-gui / reply9842-server）
// 共用这一份代码，靠 argv[1] 的 basename 或第一个参数决定启动哪个二进制。
//
// 设计取舍：不做 shell 包装、不做 spawn 转发，而是用 spawn 继承 stdio 后透传
// 退出码与信号。原因是要让 GUI 也在同一个入口工作，且 Ctrl-C 行为与原生一致。

"use strict";

const { spawn } = require("node:child_process");
const { binaryPath } = require("./platform");

// 支持的子命令 → 产物 kind
const KINDS = {
  cli: "cli",
  gui: "gui",
  server: "server",
};

function usage() {
  process.stderr.write(
    [
      "reply9842 — RFC 9842 增量压缩隧道",
      "",
      "用法:",
      "  reply9842 [cli 参数...]        启动本地转发客户端（默认）",
      "  reply9842 gui                  启动图形界面",
      "  reply9842 server [参数...]     启动服务端",
      "",
      "也可以直接用独立入口: reply9842-gui / reply9842-server",
      "",
    ].join("\n"),
  );
}

/** 根据调用方式决定 kind 与剩余参数 */
function resolve(argv) {
  const invoked = require("node:path").basename(process.argv[1] || "");

  // 独立入口：reply9842-gui / reply9842-server
  if (invoked === "reply9842-gui") return { kind: "gui", args: argv };
  if (invoked === "reply9842-server") return { kind: "server", args: argv };

  // 统一入口：第一个参数是子命令才消费它
  if (argv.length > 0 && KINDS[argv[0]]) {
    return { kind: KINDS[argv[0]], args: argv.slice(1) };
  }
  // 无子命令 → 默认 cli（保持 `npx reply9842 -listen ...` 这种旧习惯可用）
  if (argv.length > 0 && (argv[0] === "-h" || argv[0] === "--help")) {
    usage();
    process.exit(0);
  }
  return { kind: "cli", args: argv };
}

function main() {
  const { kind, args } = resolve(process.argv.slice(2));
  const bin = binaryPath(kind);

  if (!bin) {
    const os = process.platform;
    const arch = process.arch;
    process.stderr.write(
      `reply9842: 当前平台（${os}/${arch}）没有 ${kind} 的预编译产物。\n` +
        (kind === "gui"
          ? "GUI 只提供 macOS 与 Windows 版本；Linux 请用 CLI 或从源码构建。\n"
          : "请从 https://github.com/jixoai/rfc9842-reply/releases 下载，或用 go install 安装。\n"),
    );
    process.exit(1);
  }

  // 先检查文件真的存在：spawn 的 ENOENT 消息对用户毫无意义
  try {
    require("node:fs").accessSync(bin, require("node:fs").constants.X_OK);
  } catch {
    process.stderr.write(
      `reply9842: 找不到可执行的 ${kind} 产物：\n  ${bin}\n` +
        "npm 包可能不完整，请重装：npm i -g reply9842@latest\n",
    );
    process.exit(1);
  }

  // 继承 stdio：CLI 需要真实终端（进度、颜色、Ctrl-C）；
  // GUI 在 macOS 上不需要终端，但继承也无害。
  const child = spawn(bin, args, { stdio: "inherit" });

  // 信号透传：让 Ctrl-C / kill 直达子进程，而不是先杀 Node
  for (const sig of ["SIGINT", "SIGTERM", "SIGHUP"]) {
    process.on(sig, () => {
      if (!child.killed) child.kill(sig);
    });
  }

  child.on("error", (err) => {
    process.stderr.write(`reply9842: 启动失败：${err.message}\n`);
    process.exitCode = 1;
    process.exit(1);
  });

  // 透传退出码；被信号杀掉时 Node 的惯例是 128+signal
  child.on("exit", (code, signal) => {
    if (signal) {
      const n = { SIGINT: 2, SIGTERM: 15, SIGHUP: 1 }[signal] || 0;
      process.exit(128 + n);
    }
    process.exit(code == null ? 1 : code);
  });
}

main();
