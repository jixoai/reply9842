// 断点续传下载器
//
// 事故背景（2026-11-13，见 scripts/deploy.sh 顶部）：跨境链路被短 timeout 打断，
// 传了 2.5MB / 9.9MB 的 ELF 就被部署，`file` 仍报 "ELF 64-bit LSB executable"
// （header 完好），真跑起来 status=11/SEGV。二进制下载是同一个风险面，所以这里：
//
//   1. 边收边写盘（createWriteStream 语义），大文件不全量驻留内存；
//   2. 重试时发 `Range: bytes=<已有长度>-`，.part 文件跨进程/跨镜像存活；
//   3. 结束时**必须**核对字节数（content-length / content-range 的 total），
//      不一致直接报错，绝不把半截文件 rename 成正式产物；
//   4. 可选 SHA256 校验（release 里的 SHA256SUMS.txt），能识别「换了镜像但内容不同」。

"use strict";

const fs = require("node:fs");
const path = require("node:path");
const crypto = require("node:crypto");
const { DIRECT } = require("./mirror");

/** 单次请求的空闲超时：多久收不到新字节就判定连接死了。 */
const DEFAULT_IDLE_TIMEOUT_MS = 30_000;

/** 每个镜像的尝试次数（首次 + 一次重试，重试会带 Range 续传）。 */
const DEFAULT_ATTEMPTS = 2;

// ---- 纯逻辑（可单测） ----

/**
 * 续传请求头。已有字节 > 0 才发 Range；否则发空对象（保持普通 GET）。
 * @returns {{Range?: string}}
 */
function buildRangeHeader(existingBytes) {
  if (!Number.isFinite(existingBytes) || existingBytes <= 0) return {};
  return { Range: `bytes=${existingBytes}-` };
}

/**
 * 解析 `Content-Range`。
 *   "bytes 100-200/300" → { start: 100, end: 200, total: 300 }
 *   "bytes 100-200/*"   → { start: 100, end: 200, total: null }
 *   "bytes *​/300"       → { start: null, end: null, total: 300 }   (416 用)
 */
function parseContentRange(header) {
  if (typeof header !== "string") return null;
  const m = /^bytes\s+(?:(\d+)-(\d+)|\*)\/(\d+|\*)$/i.exec(header.trim());
  if (!m) return null;
  return {
    start: m[1] === undefined ? null : Number(m[1]),
    end: m[2] === undefined ? null : Number(m[2]),
    total: m[3] === "*" ? null : Number(m[3]),
  };
}

/**
 * 决定「这次响应怎么处理」。纯函数，206/200/416 分支都在这。
 *
 * 关键区分：只有「body 的起点正好等于要写入的偏移」才能用这段 body。
 *   append  —— 206 且 start == 已有长度（真正的续传）
 *   restart —— body 从 0 开始，清空 .part 后可直接写（206 start=0 / 200）
 *   refetch —— body 起点与本地长度都对不上（206 start 既不等于已有长度也不是 0，
 *              或 416 且总长 ≠ 已有长度）：**这段 body 不可用**，必须删掉 .part
 *              重新发一次不带 Range 的请求
 *   complete —— 416 且已有长度正好等于服务端总长（.part 其实已完整）
 *
 * @param {{status:number, existingBytes:number, headers:{get:(n:string)=>string|null}}} args
 * @returns {{action:"append"|"restart"|"refetch"|"complete", total:number|null, reason?:string}}
 */
function classifyResponse({ status, existingBytes, headers }) {
  const get = (name) => (headers && typeof headers.get === "function"
    ? headers.get(name)
    : null);

  if (status === 206) {
    const range = parseContentRange(get("content-range"));
    if (!range || range.start === null) {
      throw new Error("服务端返回 206 但缺少合法的 Content-Range");
    }
    const length = Number(get("content-length") || 0);
    if (length > 0 && range.end !== null && length !== range.end - range.start + 1) {
      throw new Error(
        `206 响应的 content-length(${length}) 与 Content-Range(${range.start}-${range.end}) 不一致`,
      );
    }
    if (range.start === existingBytes) {
      return { action: "append", total: range.total };
    }
    if (range.start === 0) {
      // 服务端无视了 Range，但我们拿到的仍是从 0 开始的完整数据，清空重写即可
      return {
        action: "restart",
        total: range.total,
        reason: `服务端从 0 开始返回（本地已有 ${existingBytes}），从头写`,
      };
    }
    // 起点既不是 0 也不是本地长度：body 是中间片段，接到 .part 上会拼出坏文件
    return {
      action: "refetch",
      total: range.total,
      reason: `服务端从 ${range.start} 开始返回，本地已有 ${existingBytes}，需重新请求`,
    };
  }

  if (status === 200) {
    // 服务端不支持 Range（忽略了我们发的头），body 从 0 开始，清空重写
    const total = Number(get("content-length") || 0) || null;
    return {
      action: "restart",
      total,
      reason: existingBytes > 0 ? "服务端不支持 Range，从头下载" : undefined,
    };
  }

  if (status === 416) {
    const range = parseContentRange(get("content-range"));
    const total = range ? range.total : null;
    if (total !== null && total === existingBytes) {
      return { action: "complete", total };
    }
    // .part 比服务端文件还长（或总长未知）：这段残留不可信，丢掉重来
    return {
      action: "refetch",
      total,
      reason: `416：本地 .part(${existingBytes} 字节)与文件实际长度${total === null ? "不符" : `(${total} 字节)不符`}，丢弃重来`,
    };
  }

  throw new Error(`HTTP ${status}`);
}

/** 字节数校验。不一致要报错，不能静默接受半截文件。 */
function verifySize(actualBytes, expectedBytes, context) {
  if (expectedBytes == null) return; // 服务端没给长度，只能靠 SHA256 兜底
  if (actualBytes !== expectedBytes) {
    throw new Error(
      `下载不完整${context ? `（${context}）` : ""}: 实际 ${actualBytes} 字节，` +
        `服务端声明 ${expectedBytes} 字节，相差 ${expectedBytes - actualBytes}`,
    );
  }
}

/** 解析 SHA256SUMS.txt：`<hex>  <name>` 每行一条。 */
function parseChecksums(text) {
  const map = new Map();
  for (const line of String(text || "").split(/\r?\n/)) {
    const m = /^([0-9a-fA-F]{64})\s+\*?(.+?)\s*$/.exec(line);
    if (m) map.set(m[2], m[1].toLowerCase());
  }
  return map;
}

async function sha256File(file) {
  const hash = crypto.createHash("sha256");
  await new Promise((resolve, reject) => {
    const rs = fs.createReadStream(file);
    rs.on("data", (c) => hash.update(c));
    rs.on("error", reject);
    rs.on("end", resolve);
  });
  return hash.digest("hex");
}

// ---- 落盘 ----

function tempPath(dest) {
  return `${dest}.part`;
}

/** 记录「第一次下载时服务端声明的总长 / ETag」，用于识别换镜像后内容不同。 */
function metaPath(dest) {
  return `${dest}.part.json`;
}

async function readMeta(dest) {
  try {
    return JSON.parse(await fs.promises.readFile(metaPath(dest), "utf8"));
  } catch {
    return {};
  }
}

async function writeMeta(dest, meta) {
  try {
    await fs.promises.writeFile(metaPath(dest), JSON.stringify(meta));
  } catch {
    /* meta 只是优化，写不进去不影响正确性 */
  }
}

async function cleanupTemp(dest) {
  for (const p of [tempPath(dest), metaPath(dest)]) {
    await fs.promises.rm(p, { force: true });
  }
}

async function fileSize(file) {
  try {
    return (await fs.promises.stat(file)).size;
  } catch (e) {
    if (e.code === "ENOENT") return 0;
    throw e;
  }
}

/** 丢弃不再需要的响应体（不消费会占着连接，keep-alive 池里泄漏）。 */
async function discardBody(res) {
  if (!res || !res.body) return;
  try {
    if (typeof res.body.cancel === "function") await res.body.cancel();
  } catch {
    /* 已经结束/已取消 */
  }
}

// ---- 单次传输 ----

/**
 * 从单个前缀拉一次，成功返回 { bytes, total }。
 * 失败时 .part 保留（续传素材），只有内容明显不对才删。
 */
async function downloadOnce({ url, prefix, dest, fetchImpl, onProgress, idleTimeoutMs }) {
  const existing = await fileSize(tempPath(dest));
  const controller = new AbortController();
  let idleTimer = null;
  const armIdle = () => {
    if (!idleTimeoutMs) return;
    if (idleTimer) clearTimeout(idleTimer);
    idleTimer = setTimeout(
      () => controller.abort(new Error(`连接空闲超过 ${idleTimeoutMs}ms`)),
      idleTimeoutMs,
    );
  };

  armIdle();
  let res;
  try {
    res = await fetchImpl(prefix + url, {
      redirect: "follow",
      headers: buildRangeHeader(existing),
      signal: controller.signal,
    });
  } catch (e) {
    if (idleTimer) clearTimeout(idleTimer);
    throw e;
  }

  try {
    const plan = classifyResponse({
      status: res.status,
      existingBytes: existing,
      headers: res.headers,
    });

    if (plan.action === "complete") {
      await discardBody(res);
      return { bytes: existing, total: plan.total };
    }
    if (plan.reason) {
      onProgress({ phase: "info", message: plan.reason });
    }
    if (plan.action === "refetch") {
      // 这段 body 的起点和我们盘上的字节对不上，接到 .part 上会拼出坏文件。
      // 丢掉残留后让上层重发一次不带 Range 的请求（416 / 起点错位都走这里）。
      await discardBody(res);
      await cleanupTemp(dest);
      throw Object.assign(new Error(plan.reason || "需要重新请求"), { restart: true });
    }

    // 换镜像/服务端声明的总长变了 → 之前下的那段不是这个文件，必须丢
    const meta = await readMeta(dest);
    if (plan.action === "append" && meta.total != null && plan.total != null && meta.total !== plan.total) {
      await cleanupTemp(dest);
      throw Object.assign(
        new Error(`服务端文件大小(${plan.total})与已下载部分(${meta.total})不符，已丢弃重来`),
        { restart: true },
      );
    }
    const etag = res.headers.get("etag");
    if (plan.action === "append" && meta.etag && etag && meta.etag !== etag) {
      await cleanupTemp(dest);
      throw Object.assign(
        new Error("镜像返回的 ETag 与已下载部分不一致，已丢弃重来"),
        { restart: true },
      );
    }

    const startingAt = plan.action === "append" ? existing : 0;
    const tmp = tempPath(dest);
    await fs.promises.mkdir(path.dirname(dest), { recursive: true });
    const handle = await fs.promises.open(tmp, plan.action === "append" ? "a" : "w");

    if (plan.action !== "append") {
      // 从头写：旧 meta 已失效，避免下一次续传拿它做总长比对
      await fs.promises.rm(metaPath(dest), { force: true });
    }
    if (plan.total == null) {
      onProgress({
        phase: "info",
        message: "服务端未声明文件长度，只能靠 SHA256 校验完整性",
      });
    }

    let written = startingAt;
    try {
      if (plan.total != null) {
        await writeMeta(dest, { total: plan.total, etag: etag || meta.etag || null });
        onProgress({ phase: "start", written, total: plan.total, resumed: startingAt > 0 });
      }
      if (res.body) {
        for await (const chunk of res.body) {
          armIdle();
          await handle.write(chunk);
          written += chunk.length;
          if (plan.total) onProgress({ phase: "progress", written, total: plan.total });
        }
      }
    } catch (e) {
      // 网络中断：把已有进度刷盘，交给下一次尝试续传
      await handle.close();
      throw new Error(
        `传输中断（已写入 ${written} 字节${plan.total ? ` / ${plan.total}` : ""}）：${e.message}`,
      );
    }
    const st = await handle.stat();
    await handle.close();

    // ★ 完整性闸门：字节数对不上绝不 rename
    verifySize(st.size, plan.total, prefix ? new URL(prefix).hostname : "直连");
    if (written !== st.size) {
      throw new Error(`写入计数(${written})与磁盘大小(${st.size})不一致`);
    }
    return { bytes: st.size, total: plan.total };
  } catch (e) {
    await discardBody(res); // 复核失败/HTTP 非 2xx：别把响应体挂在那里
    throw e;
  } finally {
    if (idleTimer) clearTimeout(idleTimer);
  }
}

// ---- 多镜像编排 ----

function hostOf(prefix) {
  if (prefix === DIRECT) return "github.com（直连）";
  try {
    return new URL(prefix).hostname;
  } catch {
    return prefix;
  }
}

/**
 * 依次尝试镜像下载到 dest（原子 rename）。返回 { bytes, prefix }。
 *
 * 每个镜像默认尝试两次：第一次失败留下的 .part 会被第二次用 Range 续上；
 * 换镜像同样续，因为同一版本同一文件内容一致——但服务端声明的总长或 ETag
 * 变化时判定为不同内容，丢弃重下。
 */
async function downloadArtifact({
  url,
  dest,
  prefixes,
  fetchImpl = globalThis.fetch,
  onProgress = () => {},
  log = () => {},
  attempts = DEFAULT_ATTEMPTS,
  idleTimeoutMs = DEFAULT_IDLE_TIMEOUT_MS,
  expectedSha256 = null,
}) {
  const tmp = tempPath(dest);
  let lastErr;
  const list = prefixes && prefixes.length ? prefixes : [DIRECT];

  for (const prefix of list) {
    const label = hostOf(prefix);
    let attempt = 0;
    let refunded = 0; // 「内容不符 → 丢弃重来」不占用尝试次数，但每个镜像最多退一次
    while (attempt < attempts) {
      attempt++;
      const has = await fileSize(tmp);
      log(`  ${label}${attempt > 1 ? `（第 ${attempt} 次尝试）` : ""}${has > 0 ? `，已有 ${(has / 1048576).toFixed(1)}MB 续传` : ""} …`);
      try {
        const { bytes } = await downloadOnce({
          url, prefix, dest, fetchImpl, onProgress, idleTimeoutMs,
        });

        if (expectedSha256) {
          const actual = await sha256File(tmp);
          if (actual !== expectedSha256) {
            await cleanupTemp(dest);
            throw Object.assign(
              new Error(`SHA256 校验失败（期望 ${expectedSha256.slice(0, 12)}…，实际 ${actual.slice(0, 12)}…），已丢弃`),
              { restart: true },
            );
          }
        }

        await fs.promises.rename(tmp, dest); // 原子落盘
        await fs.promises.rm(metaPath(dest), { force: true });
        if (process.platform !== "win32") await fs.promises.chmod(dest, 0o755);
        return { bytes, prefix };
      } catch (e) {
        lastErr = e;
        log(`    失败：${e.message}`);
        if (!e.restart) continue; // 网络/HTTP 错误：留给同一镜像的下一次尝试续传

        // 内容层面的错误（大小/ETag/SHA256 不符）：.part 已删，值得立刻从头重来一次
        if (refunded === 0) {
          refunded++;
          attempt--; // 这次不算数
        }
        // 同一个镜像连续两次内容都不对 → 换镜像
      }
    }
  }
  throw lastErr || new Error("全部镜像失败");
}

/** 取 SHA256SUMS.txt（best-effort，任何失败都返回 null，不阻塞下载）。 */
async function fetchChecksums({ url, prefixes, fetchImpl = globalThis.fetch, idleTimeoutMs = 15_000 }) {
  for (const prefix of prefixes && prefixes.length ? prefixes : [DIRECT]) {
    const controller = new AbortController();
    const timer = idleTimeoutMs
      ? setTimeout(() => controller.abort(new Error("timeout")), idleTimeoutMs)
      : null;
    try {
      const res = await fetchImpl(prefix + url, { redirect: "follow", signal: controller.signal });
      if (!res.ok) continue;
      const text = await res.text();
      const map = parseChecksums(text);
      if (map.size > 0) return map;
    } catch {
      /* 忽略，继续下一个前缀 */
    } finally {
      if (timer) clearTimeout(timer);
    }
  }
  return null;
}

module.exports = {
  DEFAULT_ATTEMPTS,
  DEFAULT_IDLE_TIMEOUT_MS,
  buildRangeHeader,
  classifyResponse,
  cleanupTemp,
  downloadArtifact,
  downloadOnce,
  fetchChecksums,
  metaPath,
  parseChecksums,
  parseContentRange,
  sha256File,
  tempPath,
  verifySize,
};
