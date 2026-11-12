// Package codec — 请求体的内容寻址压缩（RFC 9842 dcz 语义）
//
// 正交意图（3 个，2026-11-12 kzf 需求：「本质上，我们其实是在做自定义的有状态
// 压缩技术」「先做块级去重（消除重复传输），再压缩」）：
//   1. 哈希：BLAKE3-256 内容寻址（RFC 9842 用 SHA-256，此处用 BLAKE3 更快，
//      仅作本地/隧道内标识，不对接外部 dcz 解码器）
//   2. dcz 编解码：40 字节 skippable 帧头 + zstd 流，与 RFC 9842 §5 线格式一致
//   3. 自适应 level：按 body 大小选 zstd level（-19 慢 150 倍，30MB 会爆）
//
// 关键约束（调研实测，勿改）：
//   - frame checksum 必须开：关掉后错字典解出「可解码但内容错乱」的静默损坏
//   - zstd 字典必须由调用方按 hash 精确匹配，绝不做「近似选一个」
package codec

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
	"lukechampine.com/blake3"
)

// HashSize 与 meta.HashSize 保持一致
const HashSize = 32

// rawDictID 原始内容字典的私有 ID（RFC 8878 §3.1.1.1.3 允许私有约定）
// 编码端与解码端必须一致，否则 "magic number mismatch"
const rawDictID uint32 = 0x52463938 // "RF98"

// Hash 内容摘要
type Hash [HashSize]byte

func (h Hash) IsZero() bool {
	for _, b := range h {
		if b != 0 {
			return false
		}
	}
	return true
}

func (h Hash) String() string { return fmt.Sprintf("%x", h[:8]) }

// Sum 计算内容的 BLAKE3-256
func Sum(b []byte) Hash {
	var h Hash
	d := blake3.Sum256(b)
	copy(h[:], d[:])
	return h
}

// ---- zstd 编码器池 ----
//
// zstd.Encoder 复用需注意：带字典的请求必须用独立的 Encoder 实例
// （EncodeAll 会重置内部状态，但字典绑定在 Encoder 上）。
// 这里按 (level, dictionary) 组合池化，避免每请求重建。

type encKey struct {
	level int
	dictH Hash
}

type encPool struct {
	m map[encKey]*zstd.Encoder
}

func newEncPool() *encPool { return &encPool{m: make(map[encKey]*zstd.Encoder)} }

var pool = newEncPool()

func (p *encPool) get(level int, dict []byte) (*zstd.Encoder, error) {
	var dh Hash
	if len(dict) > 0 {
		dh = Sum(dict)
	}
	k := encKey{level: level, dictH: dh}
	if e, ok := p.m[k]; ok {
		return e, nil
	}
	opts := []zstd.EOption{
		zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(level)),
		// 窗口：dcz 允许到 128MB；普通 zstd coding 被 RFC 9659 限到 8MB，
		// 我们走隧道自定义协议，不受该限，但按 body 大小自适应（见 PickLevel）
		zstd.WithEncoderCRC(true), // 强制 checksum，防静默损坏
		zstd.WithEncoderConcurrency(1),
	}
	if len(dict) > 0 {
		opts = append(opts, zstd.WithEncoderDictRaw(rawDictID, dict))
	}
	e, err := zstd.NewWriter(nil, opts...)
	if err != nil {
		return nil, err
	}
	p.m[k] = e
	return e, nil
}

// PickLevel 按 body 大小选 zstd level。
//
// 实测（调研子代理，1.86MB→1.89MB 增量）：
//
//	-1  → 11651 B / ~0s
//	-9  →  2615 B / 0.01s
//	-19 →  1297 B / 1.51s
//
// 且 41MB 文档 -19 需 34.5s / 328MB RSS。故按尺寸降级。
func PickLevel(bodySize int) int {
	switch {
	case bodySize <= 2<<20: // ≤2MB：追求压缩率
		return 19
	case bodySize <= 8<<20: // ≤8MB：折中
		return 12
	default: // >8MB：保速度与内存
		return 9
	}
}

// Compress 用 zstd 压缩（无字典或带字典）
func Compress(body []byte, level int, dict []byte) ([]byte, error) {
	e, err := pool.get(level, dict)
	if err != nil {
		return nil, err
	}
	return e.EncodeAll(body, nil), nil
}

// ---- dcz 线格式 ----
//
// RFC 9842 §5：
//   8 字节 magic  0x5e 0x2a 0x4d 0x18 0x20 0x00 0x00 0x00
//   （即 Zstandard skippable frame，magic 0x184D2A5E 小端 + 长度 0x20 小端）
//   + 32 字节字典哈希
//   + 其后是 zstd 流
//
// 40 字节头是「skippable frame」，标准 zstd 解码器会自动跳过，
// 因此本格式对不认识 dcz 的解码器仍然安全（只是解出裸 zstd 流）。

var dczMagic = [8]byte{0x5e, 0x2a, 0x4d, 0x18, 0x20, 0x00, 0x00, 0x00}

// HeaderLen dcz 头长度
const HeaderLen = 40

// EncodeDcz 生成 dcz：40 字节头 + zstd(dict) 流
func EncodeDcz(body []byte, baseHash Hash, level int, dict []byte) ([]byte, error) {
	if len(dict) == 0 {
		return nil, errors.New("codec: dcz requires a non-empty dictionary")
	}
	// 字典哈希必须与声明的 baseHash 一致，否则解码端必错
	if got := Sum(dict); got != baseHash {
		return nil, fmt.Errorf("codec: dict hash mismatch: have %s want %s", got, baseHash)
	}
	pay, err := Compress(body, level, dict)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, HeaderLen+len(pay))
	out = append(out, dczMagic[:]...)
	out = append(out, baseHash[:]...)
	out = append(out, pay...)
	return out, nil
}

// DecodeDcz 解 dcz：校验 40 字节头里的哈希 == 提供字典的哈希，再解压
func DecodeDcz(payload []byte, dict []byte) ([]byte, error) {
	if len(payload) < HeaderLen {
		return nil, errors.New("codec: payload shorter than dcz header")
	}
	if !bytes.Equal(payload[:8], dczMagic[:]) {
		return nil, errors.New("codec: bad dcz magic")
	}
	var declared Hash
	copy(declared[:], payload[8:HeaderLen])

	if len(dict) == 0 {
		return nil, errors.New("codec: no dictionary available for dcz payload")
	}
	if got := Sum(dict); got != declared {
		return nil, fmt.Errorf("codec: dictionary mismatch: have %s want %s", got, declared)
	}
	return Decompress(payload[HeaderLen:], dict)
}

// ---- 解压 ----

func newDecoder(dict []byte) (*zstd.Decoder, error) {
	opts := []zstd.DOption{
		// 大文档容忍更高内存；与编码端自适应 level 对应
		zstd.WithDecoderMaxMemory(512 << 20),
		zstd.WithDecoderConcurrency(1),
	}
	if len(dict) > 0 {
		opts = append(opts, zstd.WithDecoderDictRaw(rawDictID, dict))
	}
	return zstd.NewReader(nil, opts...)
}

// Decompress 解压（无字典或带字典）
func Decompress(payload []byte, dict []byte) ([]byte, error) {
	d, err := newDecoder(dict)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	return d.DecodeAll(payload, nil)
}

// ---- 流式辅助（响应体） ----

// StreamDecompress 把上游响应体流式解压（响应走 EncZstd 时使用）
func StreamDecompress(r io.Reader) (io.Reader, error) {
	d, err := zstd.NewReader(r, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	return d.IOReadCloser(), nil
}

// PeekDczHash 只看 dcz 头里的哈希，不触碰正文（用于「先验哈希再决定要不要读字典」）
func PeekDczHash(payload []byte) (Hash, error) {
	if len(payload) < HeaderLen {
		return Hash{}, errors.New("codec: short payload")
	}
	if !bytes.Equal(payload[:8], dczMagic[:]) {
		return Hash{}, errors.New("codec: bad dcz magic")
	}
	var h Hash
	copy(h[:], payload[8:HeaderLen])
	return h, nil
}

// 保留 binary 引用（dcz 头长度字段解析可能后续扩展）
var _ = binary.BigEndian
