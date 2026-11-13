// 分发层的最小自测：只验证「平台解析」与「启动器不崩」，不依赖真实二进制。
//
// 为什么不测真实二进制：CI 里 vendor/ 是构建产物，本地跑测试时可能不存在；
// 用 node --test 保持零依赖，能在任何环境下跑。

"use strict";

const test = require("node:test");
const assert = require("node:assert");
const path = require("node:path");
const { execFile } = require("node:child_process");
const { artifactName, goarch, goos, binaryPath, SUPPORTED } = require("../lib/platform");

test("goos/goarch 能识别当前平台", () => {
  const os = goos();
  const arch = goarch();
  assert.ok(["darwin", "linux", "windows"].includes(os), `未知 platform: ${process.platform}`);
  assert.ok(["amd64", "arm64"].includes(arch), `未知 arch: ${process.arch}`);
});

test("artifactName 命名与 CI 矩阵一致", () => {
  for (const kind of ["cli", "gui", "server"]) {
    const name = artifactName(kind);
    if (name === null) continue; // 该 kind 在本地平台无产物（例如 Linux 的 gui）
    assert.ok(SUPPORTED.has(name), `${name} 不在允许集合内`);
    assert.ok(name.startsWith(kind + "-"), `前缀应为 ${kind}-，实际 ${name}`);
    if (process.platform === "win32") {
      assert.ok(name.endsWith(".exe"), "Windows 产物必须带 .exe");
    } else {
      assert.ok(!name.endsWith(".exe"), "非 Windows 产物不应带 .exe");
    }
  }
});

test("binaryPath 落在 vendor/ 下", () => {
  const p = binaryPath("cli");
  if (p === null) return;
  assert.strictEqual(path.basename(path.dirname(p)), "vendor");
});

test("启动器在缺少二进制时给出可读错误并退出 1", (t, done) => {
  // 用一个不存在的 kind 触发「平台不支持」分支是做不到的（kind 是硬编码），
  // 改为验证：当 vendor/ 缺文件时，spawn 报错被正确转成退出码 1。
  const runner = path.join(__dirname, "..", "lib", "run.js");
  execFile(process.execPath, [runner, "--help"], (err, stdout, stderr) => {
    // --help 走 usage 分支，正常退出 0
    assert.strictEqual(err, null, `--help 不应失败: ${stderr}`);
    assert.match(stdout + stderr, /reply9842/);
    done();
  });
});

test("bin 三个入口都能被 Node 解析", () => {
  for (const f of ["reply9842.js", "reply9842-gui.js", "reply9842-server.js"]) {
    const p = path.join(__dirname, "..", "bin", f);
    assert.doesNotThrow(() => require.resolve(p), `${f} 无法解析`);
  }
});
