// Package downlink — 下行响应体的流式压缩（上行 dcz 的对偶）
//
// 正交意图（2 个，2026-11-13 kzf 需求：「下行完全没有压缩」「必须保留流式语义」）：
//  1. 流式：上游 chunk 进来就压，绝不攒完整个响应（EncodeAll 一次性压缩等于
//     把流变批，SSE 会变成「憋到结束才吐」）
//  2. 连续：输出是一条连续 zstd 流，客户端一个 decoder 顺读即可
//
// 为什么是「字节阈值 + 时间阈值」双触发（实测 396 条真实 SSE 响应，13.4MB，
// 96558 个事件，事件均 139B，中位到达速率 3.7KB/s）：
//
//	逐事件 flush        63.9%   ← 每个事件 flush 成小块，压缩窗口全浪费
//	1KB / 200ms        28.1%
//	4KB / 200ms        15.9%   ← 本实现（双触发等价于此）
//	不 flush（整条）      8.5%
//
// 时间阈值是 SSE 实时性的保证：上游每 38ms 吐一个事件，若只按 4KB 触发，
// 3.7KB/s 下要攒 1.1 秒才吐一次，用户看到的是「一段一段跳」。200ms 阈值让
// 延迟代价恒定在可感知阈值以下（现有 TTFB 中位数 7.6s，占比 2.6%）。
//
// 级别选 SpeedFastest 而非 SpeedDefault：同批样本 10.16% vs 10.00%（差 0.16pp），
// 而每 encoder 堆占用 0.64MB vs 1.76MB、CPU 24ms vs 38ms。下行延迟敏感且多
// 请求并发，省内存比省 0.16pp 值。
package downlink

import (
	"errors"
	"io"
	"time"

	"github.com/klauspost/compress/zstd"
)

// 默认阈值（Compressor 字段可覆盖，便于测试）
const (
	DefaultFlushBytes = 4 << 10
	DefaultFlushIdle  = 200 * time.Millisecond
)

// MinUseful 小于这个体积的响应不值得压缩。
//
// 实测（线上真实 401 错误体 172 字节）：压缩后连同帧头 333 字节，是明文的
// 1.9 倍。zstd 最小帧头+校验约 13-17 字节，而隧道每帧还带 10 字节帧头 +
// 17 字节 ResponseHead —— 小响应上压缩是纯亏。误差/短错误体走明文。
const MinUseful = 512

// Compressor 把明文字节流压成一条连续 zstd 流写进 sink。
//
// 非并发安全：Write/Flush/Close 由调用方串行化（spool.Entry 用自己的锁）。
type Compressor struct {
	// FlushBytes 攒够这么多明文字节就 flush
	FlushBytes int
	// FlushIdle 距上次 flush 超过这么久就 flush（SSE 实时性下限）
	FlushIdle time.Duration

	zw      *zstd.Encoder
	pending int
	last    time.Time
	closed  bool
}

// New 创建压缩器，压缩后的字节写入 sink
func New(sink io.Writer) (*Compressor, error) {
	zw, err := zstd.NewWriter(sink,
		zstd.WithEncoderLevel(zstd.SpeedFastest),
		// 强制 frame checksum：与 codec 侧同理，关掉后坏块会解出「可解码但内容错乱」
		zstd.WithEncoderCRC(true),
		zstd.WithEncoderConcurrency(1),
	)
	if err != nil {
		return nil, err
	}
	return &Compressor{
		FlushBytes: DefaultFlushBytes, FlushIdle: DefaultFlushIdle,
		zw: zw, last: time.Now(),
	}, nil
}

// Write 写入明文，达到字节阈值或时间阈值即 flush
func (c *Compressor) Write(p []byte) (int, error) {
	if c.closed {
		return 0, errors.New("downlink: compressor closed")
	}
	n, err := c.zw.Write(p)
	c.pending += n
	if err != nil {
		return n, err
	}
	if c.pending >= c.FlushBytes || time.Since(c.last) >= c.FlushIdle {
		if err := c.Flush(); err != nil {
			return n, err
		}
	}
	return n, nil
}

// Flush 把已写入但还没输出的数据推出去（无待输出数据时是空操作）
func (c *Compressor) Flush() error {
	if c.closed || c.pending == 0 {
		return nil
	}
	c.pending = 0
	c.last = time.Now()
	return c.zw.Flush()
}

// Pending 还攒在压缩器内部、尚未推成可读字节的明文量。
// 读取方用它判断「哪些明文此刻读得到」，不能越界读。
func (c *Compressor) Pending() int { return c.pending }

// Close 结束 zstd 流（写出帧尾）。之后不可再 Write。
func (c *Compressor) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	c.pending = 0
	return c.zw.Close()
}
