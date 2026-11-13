// 镜像（加速前缀）配置解析
//
// 契约（npm 下载器 / Go update 包 / GUI 设置页三处共用，改这里要同步三处）：
//
//   优先级：REPLY9842_MIRROR 环境变量 > 内置默认列表
//   格式：  逗号分隔的前缀列表，顺序即优先级（回退链）
//   特殊值：direct 或空值 = 直连（不加前缀）
//   拼接：  <前缀><完整 url>，前缀末尾自动补 "/"，已带则不动
//
// 为什么不复用 npm_config_*：npm 会吃掉自己前缀的变量并做一层 config 解析，
// 用户从 .npmrc 传下来的值语义会变形；REPLY9842_ 前缀无歧义。

"use strict";

/** 内置默认列表：先国内镜像，最后官方直连兜底。顺序是 2026-11 实测下来的。 */
const DEFAULT_MIRRORS = [
  "https://gh-proxy.com/",
  "https://ghfast.top/",
  "https://ghproxy.net/",
  "", // 空串 = 直连
];

/** 覆盖用的环境变量名 */
const MIRROR_ENV = "REPLY9842_MIRROR";

/** 直连前缀（空串，拼接后仍是原 url） */
const DIRECT = "";

function isDirect(token) {
  return token === DIRECT || token.toLowerCase() === "direct";
}

/**
 * 规范化单个前缀：末尾补 "/"。
 * 代理契约是 `<prefix><url>` 直接字符串拼接，少一个斜杠就会拼出
 * `https://my.mirrorhttps://github.com/...` 这种坏 URL。
 */
function normalizePrefix(prefix) {
  if (prefix === DIRECT) return DIRECT;
  return prefix.endsWith("/") ? prefix : `${prefix}/`;
}

/**
 * 校验前缀是 http(s) URL。
 *
 * 校验放在补斜杠**之后**：`https://m.example.com` 补成 `https://m.example.com/`
 * 才是真正参与拼接的形态，用最终形态校验才等价于 Go 侧。
 */
function validatePrefix(value, raw) {
  // WHATWG URL 会把 "https:///x" 的额外斜杠当空 authority 丢弃、解析成 host=x，
  // 而 Go 的 url.Parse 得到 Host=""（拒绝）。为了让两侧接受集合**完全相同**，
  // 这里显式要求 authority 非空，不依赖 URL 解析器的宽松行为。
  const authority = /^[a-z][a-z0-9+.-]*:\/\/([^/?#]*)/i.exec(value);
  if (authority && authority[1].trim() === "") {
    throw new Error(
      `${MIRROR_ENV} 里的镜像前缀缺少主机名：${JSON.stringify(raw)}`,
    );
  }

  let parsed;
  try {
    parsed = new URL(value);
  } catch {
    if (/^https?:\/\/$/i.test(value)) {
      throw new Error(
        `${MIRROR_ENV} 里的镜像前缀缺少主机名：${JSON.stringify(raw)}`,
      );
    }
    throw new Error(
      `${MIRROR_ENV} 里的镜像前缀不是合法 URL：${JSON.stringify(raw)}`,
    );
  }
  if (parsed.protocol !== "http:" && parsed.protocol !== "https:") {
    throw new Error(
      `${MIRROR_ENV} 只接受 http(s) 前缀，收到：${JSON.stringify(raw)}`,
    );
  }
  if (!parsed.hostname) {
    throw new Error(
      `${MIRROR_ENV} 里的镜像前缀缺少主机名：${JSON.stringify(raw)}`,
    );
  }
  return value;
}

/**
 * 解析一行镜像配置文本，返回前缀数组（含 "" 表示直连）。
 *
 * 规则：
 *  - 逗号切分，每项 trim；
 *  - 空项跳过（`a,,b` → [a, b]，直连必须显式写 direct）；
 *  - `direct`（大小写不敏感）与整串为空 → 直连；
 *  - 无任何有效项 → 直连（尊重用户显式设置，不回退默认）；
 *  - 保持顺序去重。
 */
function parseMirrorList(raw) {
  const text = String(raw == null ? "" : raw).trim();
  if (text === "") return [DIRECT];

  const out = [];
  const seen = new Set();
  const push = (prefix) => {
    if (seen.has(prefix)) return;
    seen.add(prefix);
    out.push(prefix);
  };

  for (const piece of text.split(",")) {
    const token = piece.trim();
    if (token === "") continue; // 容忍 `a,,b` 与结尾逗号
    if (isDirect(token)) {
      push(DIRECT);
      continue;
    }
    push(normalizePrefix(validatePrefix(token, piece)));
  }

  return out.length > 0 ? out : [DIRECT];
}

/**
 * 解析当前生效的镜像列表。
 * @param {NodeJS.ProcessEnv} [env]
 */
function resolveMirrors(env = process.env) {
  const raw = env ? env[MIRROR_ENV] : undefined;
  if (raw === undefined) return DEFAULT_MIRRORS.slice(); // 未设置 = 默认行为
  return parseMirrorList(raw);
}

/** 给人看的镜像名：直连显示 github.com，其余显示 hostname。 */
function mirrorLabel(prefix) {
  if (prefix === DIRECT) return "github.com（直连）";
  try {
    return new URL(prefix).hostname;
  } catch {
    return prefix;
  }
}

module.exports = {
  DEFAULT_MIRRORS,
  DIRECT,
  MIRROR_ENV,
  mirrorLabel,
  normalizePrefix,
  parseMirrorList,
  resolveMirrors,
};
