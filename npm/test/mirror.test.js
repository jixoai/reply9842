// 镜像配置解析测试（纯逻辑，无网络）

"use strict";

const test = require("node:test");
const assert = require("node:assert");

const {
  DEFAULT_MIRRORS,
  MIRROR_ENV,
  mirrorLabel,
  normalizePrefix,
  parseMirrorList,
  resolveMirrors,
} = require("../lib/mirror");

test("未设置环境变量 → 默认列表，顺序与默认直连兜底不变", () => {
  assert.deepStrictEqual(resolveMirrors({}), [
    "https://gh-proxy.com/",
    "https://ghfast.top/",
    "https://ghproxy.net/",
    "",
  ]);
  // 默认列表来自常量，不被调用方修改
  const a = resolveMirrors({});
  a.push("https://evil/");
  assert.strictEqual(DEFAULT_MIRRORS.length, 4, "默认列表不应被污染");
});

test("单值覆盖", () => {
  assert.deepStrictEqual(
    resolveMirrors({ [MIRROR_ENV]: "https://my.mirror/" }),
    ["https://my.mirror/"],
  );
});

test("多值按顺序，即回退链优先级", () => {
  assert.deepStrictEqual(
    parseMirrorList("https://a/,https://b/,direct"),
    ["https://a/", "https://b/", ""],
  );
});

test("两侧空白 trim，空项跳过", () => {
  assert.deepStrictEqual(
    parseMirrorList("  https://a/ , https://b/  "),
    ["https://a/", "https://b/"],
  );
  // `a,,b` 与结尾逗号都不产生直连项——直连必须显式写 direct
  assert.deepStrictEqual(parseMirrorList("https://a/,,https://b/,"), [
    "https://a/",
    "https://b/",
  ]);
});

test("空值 / 纯空白 / 只有分隔符 → 直连（不回退默认列表）", () => {
  assert.deepStrictEqual(resolveMirrors({ [MIRROR_ENV]: "" }), [""]);
  assert.deepStrictEqual(resolveMirrors({ [MIRROR_ENV]: "   " }), [""]);
  assert.deepStrictEqual(resolveMirrors({ [MIRROR_ENV]: " , , " }), [""]);
});

test("direct 大小写不敏感，且可与真实镜像混用", () => {
  assert.deepStrictEqual(parseMirrorList("DIRECT"), [""]);
  assert.deepStrictEqual(parseMirrorList("Direct"), [""]);
  assert.deepStrictEqual(parseMirrorList("https://a/,Direct"), ["https://a/", ""]);
});

test("末尾斜杠自动补全，已有的不重复补", () => {
  assert.strictEqual(normalizePrefix("https://my.mirror"), "https://my.mirror/");
  assert.strictEqual(normalizePrefix("https://my.mirror/"), "https://my.mirror/");
  assert.strictEqual(normalizePrefix("https://my.mirror/proxy"), "https://my.mirror/proxy/");
  assert.strictEqual(normalizePrefix(""), "");
  assert.deepStrictEqual(parseMirrorList("https://my.mirror"), ["https://my.mirror/"]);
});

test("保持顺序去重", () => {
  assert.deepStrictEqual(
    parseMirrorList("https://a/,https://b/,https://a/,https://a"),
    ["https://a/", "https://b/"],
  );
});

test("非法前缀直接报错（不静默忽略）", () => {
  assert.throws(() => parseMirrorList("not-a-url"), /不是合法 URL/);
  assert.throws(() => parseMirrorList("ftp://mirror/"), /只接受 http\(s\)/);
  assert.throws(() => parseMirrorList("https://a/,nope"), /不是合法 URL/);
});

test("缺主机名的前缀报错（与 Go url.Parse 的接受集合一致）", () => {
  // WHATWG URL 对 "https:///x" 很宽松（会把 x 当成 host），Go 不接受；
  // 两侧必须一致，否则同一份配置在 npm 能跑、在 Go 里报错。
  assert.throws(() => parseMirrorList("https://"), /缺少主机名/);
  assert.throws(() => parseMirrorList("https:///x"), /缺少主机名/);
  assert.throws(() => parseMirrorList("http://"), /缺少主机名/);
});

test("带端口 / 查询串 / userinfo 的前缀按原样拼接（只补斜杠）", () => {
  assert.deepStrictEqual(parseMirrorList("https://m:8080/p"), ["https://m:8080/p/"]);
  assert.deepStrictEqual(
    parseMirrorList("https://user:pass@m.example.com/"),
    ["https://user:pass@m.example.com/"],
  );
});

test("标签：直连显示 github.com，镜像显示 hostname", () => {
  assert.match(mirrorLabel(""), /github\.com/);
  assert.strictEqual(mirrorLabel("https://gh-proxy.com/"), "gh-proxy.com");
});
