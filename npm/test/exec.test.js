const test = require("node:test");
const assert = require("node:assert");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const { ensureExecutable } = require("../lib/run");

// 下载器是「rename 之后才 chmod」，中间被杀 / 被 cp、解压、备份丢 mode 都会
// 留下「大小非 0 但不可执行」的缓存文件。缓存命中必须能自愈，否则 spawn EACCES
// 永远不会自己好（2026-11-16 提出）。
test("丢了执行位的缓存二进制会被补回 0755", { skip: process.platform === "win32" }, async () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "reply-exec-"));
  const f = path.join(dir, "reply9842");
  fs.writeFileSync(f, "#!/bin/sh\nexit 0\n");
  fs.chmodSync(f, 0o644);

  assert.equal(await ensureExecutable(f), true);
  const mode = fs.statSync(f).mode & 0o777;
  assert.ok(mode & 0o100, `执行位没有补回：${mode.toString(8)}`);
});

test("本来就可执行的返回 true 且不改 mode", { skip: process.platform === "win32" }, async () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "reply-exec-"));
  const f = path.join(dir, "reply9842");
  fs.writeFileSync(f, "#!/bin/sh\nexit 0\n");
  fs.chmodSync(f, 0o755);

  assert.equal(await ensureExecutable(f), true);
  assert.equal(fs.statSync(f).mode & 0o777, 0o755);
});

test("文件不存在返回 false（调用方退回下载）", async () => {
  assert.equal(await ensureExecutable("/nonexistent/reply9842-test"), false);
});
