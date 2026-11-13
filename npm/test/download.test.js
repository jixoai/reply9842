// 断点续传下载器测试：全部用注入的 fetch mock，无网络。

"use strict";

const test = require("node:test");
const assert = require("node:assert");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const crypto = require("node:crypto");

const {
  buildRangeHeader,
  classifyResponse,
  downloadArtifact,
  fetchChecksums,
  parseChecksums,
  parseContentRange,
  tempPath,
  verifySize,
} = require("../lib/download");

// ---- 测试脚手架 ----

function mkTmpDir() {
  return fs.mkdtempSync(path.join(os.tmpdir(), "reply9842-dl-"));
}

function mkHeaders(obj) {
  const lower = new Map(
    Object.entries(obj).map(([k, v]) => [k.toLowerCase(), String(v)]),
  );
  return { get: (n) => (lower.has(n.toLowerCase()) ? lower.get(n.toLowerCase()) : null) };
}

const PAYLOAD = Buffer.from(
  Array.from({ length: 5000 }, (_, i) => String.fromCharCode(i % 251)),
  "utf8",
);

/** 构造一个响应：按 Range 语义从 payload 取片段。supportsRange=false 时忽略 Range 返回 200 全量。 */
function makeResponse({ payload, rangeHeader, supportsRange = true, etag = '"e1"' }) {
  const total = payload.length;
  if (supportsRange && rangeHeader) {
    const m = /^bytes=(\d+)-$/.exec(rangeHeader);
    const start = Number(m[1]);
    if (start >= total) {
      return {
        status: 416,
        headers: mkHeaders({ "content-range": `bytes */${total}` }),
        body: null,
      };
    }
    const slice = payload.subarray(start);
    return {
      status: 206,
      headers: mkHeaders({
        "content-length": slice.length,
        "content-range": `bytes ${start}-${total - 1}/${total}`,
        etag,
      }),
      body: (async function* () {
        yield slice;
      })(),
    };
  }
  return {
    status: 200,
    headers: mkHeaders({ "content-length": total, etag }),
    body: (async function* () {
      yield payload;
    })(),
  };
}

/**
 * 可编程 fetch mock。script 决定每次调用的行为：
 *  - { respond: (req) => response }
 *  - { throw: Error }
 *  - { truncateAt: N }  按 range 正常响应头，但流在中途断掉
 */
function makeFetchMock(script) {
  const calls = [];
  let i = 0;
  const fn = async (url, init = {}) => {
    const entry = script[Math.min(i, script.length - 1)];
    i++;
    const headers = init.headers || {};
    calls.push({ url, headers, method: init.method || "GET" });
    if (entry.throw) throw entry.throw;
    const res = entry.respond
      ? entry.respond({ url, headers })
      : makeResponse({
          payload: entry.payload,
          rangeHeader: headers.Range,
          supportsRange: entry.supportsRange !== false,
          etag: entry.etag || '"e1"',
        });
    if (entry.truncateAt != null && res.body) {
      const limit = entry.truncateAt;
      const inner = res.body;
      res.body = (async function* () {
        let n = 0;
        for await (const chunk of inner) {
          const take = Math.min(limit - n, chunk.length);
          if (take > 0) yield chunk.subarray(0, take);
          n += take;
          if (n >= limit) throw new Error("socket hang up");
        }
      })();
    }
    return res;
  };
  fn.calls = calls;
  return fn;
}

function rmdir(dir) {
  fs.rmSync(dir, { recursive: true, force: true });
}

// ---- 纯逻辑 ----

test("Range 头：仅当已有字节 > 0 才发", () => {
  assert.deepStrictEqual(buildRangeHeader(0), {});
  assert.deepStrictEqual(buildRangeHeader(-1), {});
  assert.deepStrictEqual(buildRangeHeader(NaN), {});
  assert.deepStrictEqual(buildRangeHeader(1024), { Range: "bytes=1024-" });
});

test("解析 Content-Range：完整 / 未知总长 / 416 形态", () => {
  assert.deepStrictEqual(parseContentRange("bytes 100-200/300"), {
    start: 100, end: 200, total: 300,
  });
  assert.deepStrictEqual(parseContentRange("bytes 100-200/*"), {
    start: 100, end: 200, total: null,
  });
  assert.deepStrictEqual(parseContentRange("bytes */300"), {
    start: null, end: null, total: 300,
  });
  assert.strictEqual(parseContentRange("garbage"), null);
  assert.strictEqual(parseContentRange(null), null);
});

test("206 且起点吻合 → append", () => {
  const plan = classifyResponse({
    status: 206,
    existingBytes: 100,
    headers: mkHeaders({ "content-length": 200, "content-range": "bytes 100-299/300" }),
  });
  assert.strictEqual(plan.action, "append");
  assert.strictEqual(plan.total, 300);
});

test("206 但起点不吻合 → refetch（拼不上，不能硬接）", () => {
  const plan = classifyResponse({
    status: 206,
    existingBytes: 50,
    headers: mkHeaders({ "content-length": 250, "content-range": "bytes 100-349/350" }),
  });
  assert.strictEqual(plan.action, "refetch");
  assert.match(plan.reason, /本地已有 50/);
});

test("206 起点为 0 且本地有残留 → restart（body 从 0 开始，清空即可用）", () => {
  const plan = classifyResponse({
    status: 206,
    existingBytes: 500,
    headers: mkHeaders({ "content-length": 5000, "content-range": "bytes 0-4999/5000" }),
  });
  assert.strictEqual(plan.action, "restart");
  assert.strictEqual(plan.total, 5000);
});

test("206 缺少 Content-Range → 抛错", () => {
  assert.throws(
    () => classifyResponse({ status: 206, existingBytes: 0, headers: mkHeaders({}) }),
    /缺少合法的 Content-Range/,
  );
});

test("206 的 content-length 与 Content-Range 不一致 → 抛错", () => {
  assert.throws(
    () =>
      classifyResponse({
        status: 206,
        existingBytes: 100,
        headers: mkHeaders({ "content-length": 999, "content-range": "bytes 100-299/300" }),
      }),
    /不一致/,
  );
});

test("200 且服务端忽略 Range → restart（清空从头写，body 从 0 开始可用）", () => {
  const plan = classifyResponse({
    status: 200,
    existingBytes: 500,
    headers: mkHeaders({ "content-length": 5000 }),
  });
  assert.strictEqual(plan.action, "restart");
  assert.match(plan.reason, /不支持 Range/);
});

test("200 且本地无残留 → restart（清空写，语义与首次下载相同）", () => {
  const plan = classifyResponse({
    status: 200,
    existingBytes: 0,
    headers: mkHeaders({ "content-length": 5000 }),
  });
  assert.strictEqual(plan.action, "restart");
  assert.strictEqual(plan.total, 5000);
});

test("206 起点既非 0 也非本地长度 → refetch（body 是中间片段，不可用）", () => {
  const plan = classifyResponse({
    status: 206,
    existingBytes: 10,
    headers: mkHeaders({ "content-length": 190, "content-range": "bytes 100-289/300" }),
  });
  assert.strictEqual(plan.action, "refetch");
  assert.match(plan.reason, /需重新请求/);
});

test("416 且 .part 比真实文件长 → refetch（残留不可信）", () => {
  const plan = classifyResponse({
    status: 416,
    existingBytes: 9000,
    headers: mkHeaders({ "content-range": "bytes */5000" }),
  });
  assert.strictEqual(plan.action, "refetch");
  assert.strictEqual(plan.total, 5000);
});

test("416 且已有字节等于总长 → complete（.part 其实已完整）", () => {
  const plan = classifyResponse({
    status: 416,
    existingBytes: 5000,
    headers: mkHeaders({ "content-range": "bytes */5000" }),
  });
  assert.strictEqual(plan.action, "complete");
});

test("其它状态码 → 抛错", () => {
  assert.throws(
    () => classifyResponse({ status: 404, existingBytes: 0, headers: mkHeaders({}) }),
    /HTTP 404/,
  );
});

test("大小校验：不符报错，缺省放行", () => {
  assert.doesNotThrow(() => verifySize(100, null));
  assert.doesNotThrow(() => verifySize(100, 100));
  assert.throws(() => verifySize(99, 100), /实际 99 字节.*声明 100 字节/);
});

test("parseChecksums 支持二进制模式的 * 前缀", () => {
  const map = parseChecksums(
    "c04688fee9ed9c4c3333bdc5f9381c8e6d3a4deb1073327545d2870a1630c9c9  cli-darwin-amd64\n" +
      "3d504f3e13c1928b7997e05018d693df5159edd841fd13c552ca85160430b186 *cli-darwin-arm64\n" +
      "\n不被识别的行\n",
  );
  assert.strictEqual(map.size, 2);
  assert.strictEqual(map.get("cli-darwin-amd64").slice(0, 6), "c04688");
  assert.strictEqual(map.get("cli-darwin-arm64").slice(0, 6), "3d504f");
});

// ---- 端到端（mock fetch） ----

test("全新下载：流式写盘 + 原子 rename + 不留 .part", async () => {
  const dir = mkTmpDir();
  try {
    const dest = path.join(dir, "bin");
    const mock = makeFetchMock([{ payload: PAYLOAD }]);
    const { bytes, prefix } = await downloadArtifact({
      url: "https://example/app",
      dest,
      prefixes: [""],
      fetchImpl: mock,
    });
    assert.strictEqual(bytes, PAYLOAD.length);
    assert.strictEqual(prefix, "");
    assert.deepStrictEqual(fs.readFileSync(dest), PAYLOAD);
    assert.ok(!fs.existsSync(tempPath(dest)), ".part 应被 rename 掉");
    assert.ok(!fs.existsSync(tempPath(dest) + ".json"), "meta 应被清理");
    assert.deepStrictEqual(mock.calls[0].headers, {}, "无残留时不应发 Range");
  } finally {
    rmdir(dir);
  }
});

test("续传：预置 .part → 发 Range，拼出完整文件", async () => {
  const dir = mkTmpDir();
  try {
    const dest = path.join(dir, "bin");
    const half = 2000;
    fs.writeFileSync(tempPath(dest), PAYLOAD.subarray(0, half));
    const mock = makeFetchMock([{ payload: PAYLOAD }]);

    const { bytes } = await downloadArtifact({
      url: "https://example/app",
      dest,
      prefixes: [""],
      fetchImpl: mock,
    });
    assert.strictEqual(bytes, PAYLOAD.length);
    assert.strictEqual(mock.calls[0].headers.Range, `bytes=${half}-`);
    assert.deepStrictEqual(fs.readFileSync(dest), PAYLOAD);
  } finally {
    rmdir(dir);
  }
});

test("传输中途断掉 → 保留 .part，下一次尝试续传成功", async () => {
  const dir = mkTmpDir();
  try {
    const dest = path.join(dir, "bin");
    // 第一次：正常 200 头，但流只给 1500 字节就断（模拟 socket hang up）
    // 第二次：带着 Range=1500 续传，成功
    const mock = makeFetchMock([
      { payload: PAYLOAD, truncateAt: 1500 },
      { payload: PAYLOAD },
    ]);
    const { bytes } = await downloadArtifact({
      url: "https://example/app",
      dest,
      prefixes: [""],
      fetchImpl: mock,
      attempts: 2,
    });
    assert.strictEqual(bytes, PAYLOAD.length);
    assert.strictEqual(mock.calls.length, 2, "应重试一次");
    assert.deepStrictEqual(mock.calls[1].headers, { Range: "bytes=1500-" },
      "第二次必须从断点续");
    assert.deepStrictEqual(fs.readFileSync(dest), PAYLOAD);
  } finally {
    rmdir(dir);
  }
});

test("大小不符（流提前结束但没抛错）→ 报错，绝不 rename", async () => {
  const dir = mkTmpDir();
  try {
    const dest = path.join(dir, "bin");
    const mock = makeFetchMock([{ payload: PAYLOAD }]);
    // 让截断不抛错：截断后正常结束（content-length 仍声称全量）
    const silent = async (url, init) => {
      const res = await mock(url, init);
      const inner = res.body;
      res.body = (async function* () {
        let n = 0;
        for await (const chunk of inner) {
          const take = Math.max(0, Math.min(100 - n, chunk.length));
          if (take > 0) yield chunk.subarray(0, take);
          n += take;
          if (n >= 100) return;
        }
      })();
      return res;
    };
    await assert.rejects(
      downloadArtifact({
        url: "https://example/app",
        dest,
        prefixes: [""],
        fetchImpl: silent,
        attempts: 1,
      }),
      /下载不完整|传输中断/,
    );
    assert.ok(!fs.existsSync(dest), "半截文件不得成为正式产物");
  } finally {
    rmdir(dir);
  }
});

test("服务端不支持 Range（返回 200 全量）→ 丢弃 .part 从头写，结果正确", async () => {
  const dir = mkTmpDir();
  try {
    const dest = path.join(dir, "bin");
    fs.writeFileSync(tempPath(dest), Buffer.from("garbage-from-other-server"));
    const mock = makeFetchMock([{ payload: PAYLOAD, supportsRange: false }]);

    const { bytes } = await downloadArtifact({
      url: "https://example/app",
      dest,
      prefixes: [""],
      fetchImpl: mock,
      attempts: 1,
    });
    assert.strictEqual(bytes, PAYLOAD.length);
    assert.deepStrictEqual(fs.readFileSync(dest), PAYLOAD, "不得把垃圾前缀拼进产物");
  } finally {
    rmdir(dir);
  }
});

test("多镜像回退：第一个镜像下到一半失败，第二个镜像续传成功", async () => {
  const dir = mkTmpDir();
  try {
    const dest = path.join(dir, "bin");
    const mock = makeFetchMock([
      { payload: PAYLOAD, truncateAt: 1200 }, // mirror A 挂在一半
      { payload: PAYLOAD }, // mirror B 续传（同一内容）
    ]);
    const { bytes, prefix } = await downloadArtifact({
      url: "https://example/app",
      dest,
      prefixes: ["https://a.com/", "https://b.com/"],
      fetchImpl: mock,
      attempts: 1, // 每个镜像各试一次，回退靠换镜像
    });
    assert.strictEqual(bytes, PAYLOAD.length);
    assert.strictEqual(prefix, "https://b.com/");
    assert.strictEqual(mock.calls[0].url, "https://a.com/https://example/app");
    assert.strictEqual(mock.calls[1].url, "https://b.com/https://example/app");
    assert.strictEqual(mock.calls[1].headers.Range, "bytes=1200-");
    assert.deepStrictEqual(fs.readFileSync(dest), PAYLOAD);
  } finally {
    rmdir(dir);
  }
});

test("换镜像时总长变了 → 识别为不同内容，丢弃 .part 从头重来", async () => {
  const dir = mkTmpDir();
  try {
    const dest = path.join(dir, "bin");
    // mirror A 声称 5000 字节，但传到 1200 断掉（meta 记下 total=5000）
    // mirror B 的文件其实是另一个内容（长度 4000）→ 必须丢弃重下
    const other = PAYLOAD.subarray(0, 4000);
    const mock = makeFetchMock([
      { payload: PAYLOAD, truncateAt: 1200 },
      { payload: other }, // B 忽略 Range？不，B 也支持 Range 但 total 不同
    ]);
    const { bytes, prefix } = await downloadArtifact({
      url: "https://example/app",
      dest,
      prefixes: ["https://a.com/", "https://b.com/"],
      fetchImpl: mock,
      attempts: 1,
    });
    assert.strictEqual(prefix, "https://b.com/");
    assert.strictEqual(bytes, other.length);
    assert.deepStrictEqual(fs.readFileSync(dest), other, "不同内容必须从头拼，不能混");
  } finally {
    rmdir(dir);
  }
});

test("SHA256 不符 → 丢弃并重试，最后报错", async () => {
  const dir = mkTmpDir();
  try {
    const dest = path.join(dir, "bin");
    const mock = makeFetchMock([{ payload: PAYLOAD }]);
    const good = crypto.createHash("sha256").update(PAYLOAD).digest("hex");
    const bad = "0".repeat(64);

    await assert.rejects(
      downloadArtifact({
        url: "https://example/app",
        dest,
        prefixes: [""],
        fetchImpl: mock,
        attempts: 1,
        expectedSha256: bad,
      }),
      /SHA256 校验失败/,
    );
    assert.ok(!fs.existsSync(dest));

    // 正确的 SHA256 能通过
    const ok = await downloadArtifact({
      url: "https://example/app",
      dest,
      prefixes: [""],
      fetchImpl: mock,
      attempts: 1,
      expectedSha256: good,
    });
    assert.strictEqual(ok.bytes, PAYLOAD.length);
  } finally {
    rmdir(dir);
  }
});

test(".part 比真实文件长 → 416 触发丢弃，第二次请求不带 Range 并成功", async () => {
  const dir = mkTmpDir();
  try {
    const dest = path.join(dir, "bin");
    // 真实文件只有 5000 字节，本地残留 9000 → 服务端必然 416
    fs.writeFileSync(tempPath(dest), Buffer.alloc(9000, 0x41));
    const mock = makeFetchMock([{ payload: PAYLOAD }]);

    const { bytes } = await downloadArtifact({
      url: "https://example/app",
      dest,
      prefixes: [""],
      fetchImpl: mock,
      attempts: 2,
    });
    assert.strictEqual(bytes, PAYLOAD.length);
    assert.strictEqual(mock.calls.length, 2, "416 后必须重新发一次请求");
    assert.deepStrictEqual(mock.calls[0].headers, { Range: "bytes=9000-" });
    assert.deepStrictEqual(mock.calls[1].headers, {}, "重来那次不能再带 Range");
    assert.deepStrictEqual(fs.readFileSync(dest), PAYLOAD);
  } finally {
    rmdir(dir);
  }
});

test(".part 已完整时收到 416 → 直接 rename 成功，不重复下载", async () => {
  const dir = mkTmpDir();
  try {
    const dest = path.join(dir, "bin");
    fs.writeFileSync(tempPath(dest), PAYLOAD);
    // 手工造一个 416 响应
    const mock = async () => ({
      status: 416,
      headers: mkHeaders({ "content-range": `bytes */${PAYLOAD.length}` }),
      body: null,
    });
    const { bytes } = await downloadArtifact({
      url: "https://example/app",
      dest,
      prefixes: [""],
      fetchImpl: mock,
      attempts: 1,
    });
    assert.strictEqual(bytes, PAYLOAD.length);
    assert.deepStrictEqual(fs.readFileSync(dest), PAYLOAD);
  } finally {
    rmdir(dir);
  }
});

test("fetchChecksums：取不到就返回 null，不抛错", async () => {
  const missing = await fetchChecksums({
    url: "https://example/SHA256SUMS.txt",
    prefixes: [""],
    fetchImpl: async () => ({ ok: false, status: 404 }),
  });
  assert.strictEqual(missing, null);

  const throwing = await fetchChecksums({
    url: "https://example/SHA256SUMS.txt",
    prefixes: ["", "https://a/"],
    fetchImpl: async () => {
      throw new Error("ENOTFOUND");
    },
  });
  assert.strictEqual(throwing, null);
});

test("fetchChecksums：第一个镜像失败时用下一个", async () => {
  let n = 0;
  const map = await fetchChecksums({
    url: "https://example/SHA256SUMS.txt",
    prefixes: ["https://dead/", "https://ok/"],
    fetchImpl: async () => {
      n++;
      if (n === 1) throw new Error("boom");
      return {
        ok: true,
        status: 200,
        text: async () => `${"a".repeat(64)}  bin\n`,
      };
    },
  });
  assert.strictEqual(n, 2);
  assert.strictEqual(map.get("bin"), "a".repeat(64));
});
