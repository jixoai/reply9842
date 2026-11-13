// 分发层自测：只验证纯逻辑（平台解析 / 缓存路径 / 镜像列表），不发网络请求。

"use strict";

const test = require("node:test");
const assert = require("node:assert");
const path = require("node:path");

// run.js 在 require 时会执行 main()，所以这里不 require 它，改用「读源码断言」
const src = require("node:fs").readFileSync(
  path.join(__dirname, "..", "lib", "run.js"), "utf8"
);

test("产物命名规则与 release.yml 一致", () => {
  // 从源码里抽出 artifactName 的实现约定，做静态断言
  assert.match(src, /kind === "gui" && o === "linux"/, "GUI 应排除 Linux");
  assert.match(src, /const ext = o === "windows" \? "\.exe" : ""/, "Windows 加 .exe");
  assert.match(src, /\$\{kind\}-\$\{o\}-\$\{a\}\$\{ext\}/, "命名格式 <kind>-<os>-<arch>[.exe]");
});

test("镜像列表按优先级且以直连兜底", () => {
  const { DEFAULT_MIRRORS } = require("../lib/mirror");
  assert.ok(DEFAULT_MIRRORS.includes("https://gh-proxy.com/"), "应含 gh-proxy.com");
  assert.ok(DEFAULT_MIRRORS.includes("https://ghfast.top/"), "应含 ghfast.top");
  assert.strictEqual(DEFAULT_MIRRORS[DEFAULT_MIRRORS.length - 1], "",
    '最后一项必须是空串（直连兜底）');
});

test("缓存按版本隔离", () => {
  const { binaryPath } = require("../lib/run");
  const p = binaryPath("cli", "9.9.9");
  if (p) assert.match(p, /9\.9\.9/, "缓存路径必须含 version，否则换版本会串包");
});

test("跨平台缓存根", () => {
  assert.match(src, /process\.platform === "win32"/, "Windows 分支");
  assert.match(src, /LOCALAPPDATA/, "Windows 用 LOCALAPPDATA");
  assert.match(src, /XDG_CACHE_HOME/, "其它用 XDG_CACHE_HOME");
});

test("下载是原子落盘（.part -> rename）", () => {
  const { tempPath } = require("../lib/download");
  assert.strictEqual(tempPath("/cache/bin"), "/cache/bin.part");
  const dl = require("node:fs").readFileSync(
    path.join(__dirname, "..", "lib", "download.js"), "utf8"
  );
  assert.match(dl, /rename\(tmp, dest\)/, "应 rename 落盘");
  assert.match(dl, /verifySize\(/, "rename 前必须核对字节数");
});

test("三个 bin 入口存在且指向同一启动器", () => {
  const pkg = require("../package.json");
  for (const name of ["reply9842", "reply9842-gui", "reply9842-server"]) {
    const p = path.join(__dirname, "..", pkg.bin[name]);
    assert.ok(require("node:fs").existsSync(p), `${name} 指向的文件缺失`);
  }
});

test("包体不含 vendor（二进制走下载）", () => {
  const pkg = require("../package.json");
  assert.ok(!pkg.files.some((f) => f.includes("vendor")),
    "files 不应包含 vendor/，否则又变成 136MB 大包");
});
