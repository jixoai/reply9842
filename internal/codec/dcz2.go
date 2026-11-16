package codec

// dcz2 — RFC 9842 §5 的正式线格式（与私有 dcz 并存）
//
// ── 为什么有两个格式 ─────────────────────────────────────────────────────
//
// dcz（dcz.go）是本仓库 2026-11 起的私有变体：头里放 BLAKE3-256(基线)，
// zstd 用**私有字典 ID** 0x52463938。线上有老客户端在跑，格式不能动。
//
// dcz2 是与 RFC 9842 §5 逐字节对齐的版本：
//
//	字节 0–7   0x5e 0x2a 0x4d 0x18 0x20 0x00 0x00 0x00
//	          （= Zstandard skippable frame：小端 magic 0x184D2A5E + 长度 0x20）
//	字节 8–39  SHA-256(字典) —— 32 字节
//	字节 40–   Zstandard 流；字典以**带外**方式提供，帧内**不写字典 ID**
//	          （ID 0 = 不写；RFC 8878 §3.1.1.1.3 允许私有约定，这里的约定就是
//	          「ID 0 + 头部哈希」，外部 dcz 解码器也认这个形态）
//
// 两个格式 magic 完全相同、头长度完全相同、头里的哈希长度也想同（32 字节），
// 因此**不可能靠嗅探区分**：dcz 的头是 BLAKE3(基线)，dcz2 的头是 SHA-256(字典)。
// 校验语义天然互斥（见 DecodeDcz/DecodeDcz2 里的哈希比对），但"能报错"不等于
// "能自动分辨"——正常路径必须靠**显式声明的编码**（帧里的 Encoding / 连接的
// ALPN 协商）来选解码器，绝不做「先试一个不行再试另一个」的嗅探。
//
// ── 哈希分工（重要，勿混）────────────────────────────────────────────────
//
//	BLAKE3-256(基版本 body)  = 隧道内部的**寻址**（store key、basefind、
//	                           幂等键派生、ResultHash 校验）。不变，避免线上
//	                           缓存冷启动（服务端 store 的 key 是 BLAKE3）。
//	SHA-256(字典)            = dcz2 线格式头里的**对外标识**（RFC 9842 §5）。
//	                           多算一次 SHA-256 是刻意的成本：本机 1MB ≈0.94ms、
//	                           香港机 ≈2.7ms，相对每请求百毫秒级的压缩耗时可忽略。
//
// 于是 dcz2 的帧里 BaseHash（BLAKE3）与 dcz 头哈希（SHA-256）各司其职：
// 前者给服务端查 store，后者给任何 RFC 实现校验字典。

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
)

// rfcDictID dcz2 的 zstd 字典 ID：0 = 帧头不写字典 ID。
//
// 不是「随便挑的 0」：RFC 9842 §5 的字典是 raw content dictionary，靠 dcz 头里的
// SHA-256 定位，流内不标识字典。klauspost/compress 的语义正好对齐——
// WithEncoderDictRaw(0, ...) 写出的帧头 Dict_ID_flag=0（无该字段），
// WithDecoderDictRaw(0, ...) 则在帧未声明 ID 时命中 dicts[0]。
const rfcDictID uint32 = 0

// SHA256 字典摘要（RFC 9842 §5 头里的 32 字节）。
//
// 刻意与 Hash（BLAKE3）分成两个类型：两者的值不通用，混用必须编译不过。
type SHA256 [32]byte

func (h SHA256) String() string { return fmt.Sprintf("%x", h[:8]) }

// SumSHA256 计算字典的 SHA-256（RFC 9842 §5 的头哈希）
func SumSHA256(b []byte) SHA256 { return SHA256(sha256.Sum256(b)) }

// EncodeDcz2 生成 dcz2：40 字节头（magic + SHA-256(dict)）+ zstd 流（无字典 ID）。
//
// 与 EncodeDcz 的签名差异：不接收外部传入的哈希。头哈希恒等于 SHA-256(dict)，
// 让调用方再传一遍只会多一个能传错的参数（EncodeDcz 需要 baseHash 是因为
// BLAKE3 既是头哈希又是 store key，dcz2 的头哈希没有第二用途）。
func EncodeDcz2(body []byte, level int, dict []byte) ([]byte, error) {
	if len(dict) == 0 {
		return nil, errors.New("codec: dcz2 requires a non-empty dictionary")
	}
	pay, err := compressDcz2(body, level, dict)
	if err != nil {
		return nil, err
	}
	h := SumSHA256(dict)
	out := make([]byte, 0, HeaderLen+len(pay))
	out = append(out, dczMagic[:]...)
	out = append(out, h[:]...)
	out = append(out, pay...)
	return out, nil
}

// DecodeDcz2 解 dcz2：校验 40 字节头里的 SHA-256 == 提供字典的 SHA-256，再解压。
//
// 用 dcz（BLAKE3 语义）的头解 dcz2 必在这里报 "dictionary mismatch"——
// 哈希语义不同，不可能碰巧相等（这是"双解码器不会都接受"的第一道闸）。
func DecodeDcz2(payload []byte, dict []byte) ([]byte, error) {
	if len(payload) < HeaderLen {
		return nil, errors.New("codec: payload shorter than dcz2 header")
	}
	if !bytes.Equal(payload[:8], dczMagic[:]) {
		return nil, errors.New("codec: bad dcz2 magic")
	}
	var declared SHA256
	copy(declared[:], payload[8:HeaderLen])

	if len(dict) == 0 {
		return nil, errors.New("codec: no dictionary available for dcz2 payload")
	}
	if got := SumSHA256(dict); got != declared {
		return nil, fmt.Errorf("codec: dcz2 dictionary mismatch: have %s want %s", got, declared)
	}
	return decompressDcz2(payload[HeaderLen:], dict)
}

// PeekDcz2Hash 只看 dcz2 头里的 SHA-256，不触碰正文（用于校验与诊断）。
func PeekDcz2Hash(payload []byte) (SHA256, error) {
	if len(payload) < HeaderLen {
		return SHA256{}, errors.New("codec: short payload")
	}
	if !bytes.Equal(payload[:8], dczMagic[:]) {
		return SHA256{}, errors.New("codec: bad dcz2 magic")
	}
	var h SHA256
	copy(h[:], payload[8:HeaderLen])
	return h, nil
}

// ---- dcz2 的 zstd 编解码 ----

// compressDcz2 用「raw 字典 + 不写字典 ID」压一条 zstd 流。
//
// 参数与 dcz 对齐（level 由调用方按 body 大小选、CRC 强制开、并发 1），
// 但不复用 dcz 的编码器池：池的 key 是 (level, BLAKE3(dict))，不含字典 ID——
// 同一个字典内容在两种格式下 key 相同，会串用 v1（私有 ID）与 v2（ID 0）的
// 编码器实例。改动池的 key 就动了 dcz 的既有代码（约束：dcz 一行不改），
// 所以这里单开一条路径。
//
// 内存语义与现状一致：dcz 的池当前是 encPoolCap=0（不复用，用完即关），
// 本函数同样每次新建并 Close。若日后要开池复用，必须把字典 ID 并入池 key。
func compressDcz2(body []byte, level int, dict []byte) ([]byte, error) {
	enc, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(level)),
		// checksum 必须开：关掉后错字典解出「可解码但内容错乱」的静默损坏
		zstd.WithEncoderCRC(true),
		zstd.WithEncoderConcurrency(1),
		zstd.WithEncoderDictRaw(rfcDictID, dict),
	)
	if err != nil {
		return nil, err
	}
	defer enc.Close()
	return enc.EncodeAll(body, nil), nil
}

// decompressDcz2 解上面那条流：字典以 ID 0 注册，帧未声明 ID 时命中。
func decompressDcz2(payload []byte, dict []byte) ([]byte, error) {
	d, err := zstd.NewReader(nil,
		// 与 dcz 侧同参数：大文档容忍更高内存
		zstd.WithDecoderMaxMemory(512<<20),
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderDictRaw(rfcDictID, dict),
	)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	// 报错时不得返回部分输出：DecodeAll 在 CRC 校验失败时会先吐出已解出的
	// 字节再返回错误（实测 len=16427 + "CRC check failed"）。调用方按
	// 「err==nil 才用 out」写，但把不变量落在实现里更稳——下游若有人漏判
	// err，错误明文就已经流到用户手里了。
	out, err := d.DecodeAll(payload, nil)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ---- 流式解码（下行响应体） ----
//
// 下行是「一条连续流被切成多帧」，头（40 字节）只在流最前面出现一次，
// 且可能被切在任意帧边界上；调用方负责先攒够头、定位字典，再把**头之后**
// 的 zstd 字节接给下面这两个函数。

// StreamDcz2Decoder 建 dcz2（ID 0 字典）的流式解码器；r 里是**头已剥掉**的 zstd 字节。
func StreamDcz2Decoder(r io.Reader, dict []byte) (*zstd.Decoder, error) {
	if len(dict) == 0 {
		return nil, errors.New("codec: dcz2 requires a non-empty dictionary")
	}
	return zstd.NewReader(r,
		zstd.WithDecoderMaxMemory(512<<20),
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderDictRaw(rfcDictID, dict),
	)
}

// StreamDczDecoder 建 dcz（私有 ID 字典）的流式解码器；r 里是头之后的 zstd 字节。
//
// 存在的理由：把「哪种格式用哪个字典 ID」的知识留在本包一处，
// 下行解码端（clientstream）不必自己拼 WithDecoderDictRaw 的参数。
func StreamDczDecoder(r io.Reader, dict []byte) (*zstd.Decoder, error) {
	if len(dict) == 0 {
		return nil, errors.New("codec: dcz requires a non-empty dictionary")
	}
	return zstd.NewReader(r,
		zstd.WithDecoderMaxMemory(512<<20),
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderDictRaw(rawDictID, dict),
	)
}
