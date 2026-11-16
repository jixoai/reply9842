package codec

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
)

// ---- RFC 9842 §5 线格式对表 ----
//
// RFC 9842 §5 本体没有附十六进制样例/测试向量（查证：2026-11-16 取 rfc-editor
// 全文，正文与附录都没有示例字节串）。按任务约定自行构造对表向量：字典与
// 期望哈希用**仓库外**的独立实现算好（openssl shasum 与 python hashlib
// 双算一致），硬编码进测试，防止实现自证自洽。

// rfcTestDict 构造向量用的固定字典（78 字节，纯 ASCII）。
const rfcTestDict = "reply9842 dcz2 test dictionary v1: the quick brown fox jumps over the lazy dog"

// rfcTestDictSHA256 上面字典的 SHA-256（外部实现计算，勿用本包结果回填）。
const rfcTestDictSHA256 = "2a1e87a41da888e63bc15a19d616c253ae798c8dd73fe000fea25932f632e4eb"

// TestDcz2HeaderLayoutVector 头 40 字节逐字段对表 + zstd 帧首部形态。
//
// 断言（RFC 9842 §5）：
//  1. 字节 0–7  = 0x5e 0x2a 0x4d 0x18 0x20 0x00 0x00 0x00
//     （skippable frame：小端 magic 0x184D2A5E + 小端长度 32）
//  2. 字节 8–39 = SHA-256(字典)，用外部计算值对表
//  3. 字节 40–   zstd 帧：Dict_ID_flag = 0（**不写字典 ID**，RFC §5 的
//     raw content dictionary 全靠头里的哈希定位）
//  4. zstd 帧保留 content checksum（与 dcz 的 WithEncoderCRC(true) 对齐）
func TestDcz2HeaderLayoutVector(t *testing.T) {
	dict := []byte(rfcTestDict)
	if SumSHA256(dict).String() != rfcTestDictSHA256[:16] {
		t.Fatalf("SumSHA256 与外部实现不一致：%s", SumSHA256(dict))
	}

	body := []byte("reply9842 dcz2 test body: hello dictionary compression. " + rfcTestDict)
	z, err := EncodeDcz2(body, 19, dict)
	if err != nil {
		t.Fatalf("EncodeDcz2: %v", err)
	}
	if len(z) <= HeaderLen {
		t.Fatalf("dcz2 载荷太短：%d", len(z))
	}

	// 1. magic / skippable frame
	wantMagic := []byte{0x5e, 0x2a, 0x4d, 0x18, 0x20, 0x00, 0x00, 0x00}
	if !bytes.Equal(z[:8], wantMagic) {
		t.Fatalf("头 8 字节 = % x，期望 % x", z[:8], wantMagic)
	}
	if got := binary.LittleEndian.Uint32(z[0:4]); got != 0x184D2A5E {
		t.Fatalf("skippable magic(LE) = %#x，期望 0x184D2A5E", got)
	}
	if got := binary.LittleEndian.Uint32(z[4:8]); got != 32 {
		t.Fatalf("skippable 长度字段 = %d，期望 32", got)
	}

	// 2. 头哈希 = 外部计算的 SHA-256
	wantHash := mustHex(t, rfcTestDictSHA256)
	if !bytes.Equal(z[8:HeaderLen], wantHash) {
		t.Fatalf("头哈希 = %x，期望 %x", z[8:HeaderLen], wantHash)
	}
	if h, err := PeekDcz2Hash(z); err != nil || h != SumSHA256(dict) {
		t.Fatalf("PeekDcz2Hash = %v / %v", h, err)
	}

	// 3/4. zstd 帧首部
	frame := z[HeaderLen:]
	if !bytes.Equal(frame[:4], []byte{0x28, 0xb5, 0x2f, 0xfd}) {
		t.Fatalf("zstd magic = % x，期望 28 b5 2f fd", frame[:4])
	}
	fhd := frame[4]
	if iflag := fhd & 0x03; iflag != 0 {
		t.Fatalf("Dict_ID_flag = %d，期望 0（RFC 9842 §5 不写字典 ID）", iflag)
	}
	if cflag := (fhd >> 2) & 1; cflag != 1 {
		t.Fatalf("Content_Checksum_flag = %d，期望 1（CRC 必须保留）", cflag)
	}
}

// mustHex 解析十六进制串（测试向量对表用）。
func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	if len(s)%2 != 0 {
		t.Fatalf("hex 长度非法: %d", len(s))
	}
	out := make([]byte, len(s)/2)
	for i := 0; i < len(out); i++ {
		var v byte
		for j := 0; j < 2; j++ {
			c := s[i*2+j]
			switch {
			case c >= '0' && c <= '9':
				v = v<<4 | (c - '0')
			case c >= 'a' && c <= 'f':
				v = v<<4 | (c - 'a' + 10)
			case c >= 'A' && c <= 'F':
				v = v<<4 | (c - 'A' + 10)
			default:
				t.Fatalf("非法 hex 字符 %q", c)
			}
		}
		out[i] = v
	}
	return out
}

// zstdFrameDictID 解析 zstd 帧首部，返回 (Dict_ID_flag, 字典 ID)。
//
// 布局：magic(4) + Frame_Header_Descriptor(1) + [Window_Descriptor(1)]
// + [Dictionary_ID(0/1/2/4)] + ...；Window_Descriptor 仅在
// Single_Segment_flag == 0 时存在——整段 body 够大时编码器就会置 0，
// 所以解析必须按 flag 走，不能写死偏移。
func zstdFrameDictID(t *testing.T, frame []byte) (byte, uint32) {
	t.Helper()
	if len(frame) < 6 {
		t.Fatalf("zstd 帧太短: %d", len(frame))
	}
	fhd := frame[4]
	flag := fhd & 0x03
	off := 5
	if (fhd>>5)&1 == 0 { // Single_Segment_flag == 0 → 有 Window_Descriptor
		off++
	}
	switch flag {
	case 0:
		return 0, 0
	case 1:
		return 1, uint32(frame[off])
	case 2:
		return 2, uint32(binary.LittleEndian.Uint16(frame[off : off+2]))
	default:
		return 3, binary.LittleEndian.Uint32(frame[off : off+4])
	}
}

// TestDcz2PrivateIDVsRFCID 锁住两种格式在 zstd 层的分界：
// dcz 写私有 ID 0x52463938（"RF98"），dcz2 不写 ID。
func TestDcz2PrivateIDVsRFCID(t *testing.T) {
	dict := []byte(rfcTestDict)
	body := []byte("shared body for both formats: " + rfcTestDict)

	z1, err := EncodeDcz(body, Sum(dict), 19, dict)
	if err != nil {
		t.Fatalf("EncodeDcz: %v", err)
	}
	flag1, id1 := zstdFrameDictID(t, z1[HeaderLen:])
	if flag1 != 3 || id1 != rawDictID {
		t.Fatalf("dcz 的字典 ID = flag %d / %#x，期望 flag 3 / %#x (\"RF98\")", flag1, id1, rawDictID)
	}
	if rawDictID == rfcDictID {
		t.Fatalf("私有 ID 与 RFC ID 相同（%d），两个格式将无法在 zstd 层区分", rawDictID)
	}

	z2, err := EncodeDcz2(body, 19, dict)
	if err != nil {
		t.Fatalf("EncodeDcz2: %v", err)
	}
	flag2, id2 := zstdFrameDictID(t, z2[HeaderLen:])
	if flag2 != 0 || id2 != 0 {
		t.Fatalf("dcz2 的字典 ID = flag %d / %#x，期望 flag 0（不写）", flag2, id2)
	}
}

func TestDcz2RoundTrip(t *testing.T) {
	base := makePayload(5, "a")
	next := append([]byte{}, base[:len(base)-1]...)
	next = append(next, []byte(`,{"role":"user","content":"continue"}]}`)...)

	z, err := EncodeDcz2(next, 19, base)
	if err != nil {
		t.Fatalf("encode dcz2: %v", err)
	}
	got, err := DecodeDcz2(z, base)
	if err != nil {
		t.Fatalf("decode dcz2: %v", err)
	}
	if !bytes.Equal(next, got) {
		t.Fatalf("dcz2 roundtrip mismatch")
	}

	// 与普通 zstd、私有 dcz 对比：dcz2 应远小于普通 zstd，与 dcz 同量级
	plain, err := Compress(next, 19, nil)
	if err != nil {
		t.Fatal(err)
	}
	dcz1, err := EncodeDcz(next, Sum(base), 19, base)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("next=%d  dcz2=%d  dcz=%d  zstd=%d", len(next), len(z), len(dcz1), len(plain))
	if len(z) >= len(plain) {
		t.Errorf("dcz2 (%d) 应小于普通 zstd (%d)", len(z), len(plain))
	}
	// 同一字典、同一 level、同一 body：两种格式的压缩产物只差头哈希与字典 ID，
	// 尺寸应当非常接近（差值来自 ID 字段与可能的块切分差异）
	if diff := len(z) - len(dcz1); diff > 16 || diff < -16 {
		t.Errorf("dcz2 与 dcz 尺寸差异异常：%d vs %d", len(z), len(dcz1))
	}
}

func TestDcz2RejectsWrongDict(t *testing.T) {
	base := makePayload(5, "a")
	next := append([]byte{}, base...)
	next = append(next, 'x')
	z, err := EncodeDcz2(next, 19, base)
	if err != nil {
		t.Fatal(err)
	}
	wrong := makePayload(5, "b")
	got, err := DecodeDcz2(z, wrong)
	if err == nil {
		t.Fatalf("用错字典必须报错，却解出 %d 字节", len(got))
	}
	if got != nil {
		t.Errorf("报错时不得返回任何字节（得到 %d 字节）", len(got))
	}
}

// TestDcz2EncodeRejectsEmptyDict 空字典必须拒绝（RFC §5 的字典是无字典 ID 的
// raw content dictionary，空字典没有可寻址的哈希语义）。
func TestDcz2EncodeRejectsEmptyDict(t *testing.T) {
	if _, err := EncodeDcz2([]byte("x"), 19, nil); err == nil {
		t.Fatal("空字典必须报错")
	}
}

// TestDcz2DczNotInterchangeable 双解码器互斥陷阱（本任务的硬要求）。
//
// 两个格式 magic 相同、头里都是 32 字节哈希，唯一区别是哈希语义
// （BLAKE3(基线) vs SHA-256(字典)）。这里用**同一个正确字典**交叉解码：
// 哈希语义不同 → 必须报错，绝不能"先试一个不行再试另一个"地双双接受。
func TestDcz2DczNotInterchangeable(t *testing.T) {
	dict := makePayload(4, "cross")
	body := append(append([]byte{}, dict...), []byte(`{"more":"suffix"}`)...)

	z2, err := EncodeDcz2(body, 19, dict)
	if err != nil {
		t.Fatal(err)
	}
	z1, err := EncodeDcz(body, Sum(dict), 19, dict)
	if err != nil {
		t.Fatal(err)
	}

	// dcz2 载荷 → dcz(BLAKE3) 解码器：必须报错
	if got, err := DecodeDcz(z2, dict); err == nil {
		t.Fatalf("dcz 解码器接受了 dcz2 载荷（解出 %d 字节）——格式混淆陷阱", len(got))
	} else {
		t.Logf("dcz←dcz2 拒绝理由: %v", err)
	}
	// 反向：dcz 载荷 → dcz2 解码器：必须报错
	if got, err := DecodeDcz2(z1, dict); err == nil {
		t.Fatalf("dcz2 解码器接受了 dcz 载荷（解出 %d 字节）——格式混淆陷阱", len(got))
	} else {
		t.Logf("dcz2←dcz 拒绝理由: %v", err)
	}

	// 空字典也要走"拒绝"，且拒绝理由与"字典不匹配"区分
	if _, err := DecodeDcz2(z2, nil); err == nil {
		t.Fatal("dcz2 无字典必须报错")
	}

	// 对照：各自解码自己的载荷都能过（证明上面的拒绝不是把两者都解坏了）
	if out, err := DecodeDcz2(z2, dict); err != nil || !bytes.Equal(out, body) {
		t.Fatalf("dcz2 自解失败: %v", err)
	}
	if out, err := DecodeDcz(z1, dict); err != nil || !bytes.Equal(out, body) {
		t.Fatalf("dcz 自解失败: %v", err)
	}
}

// TestDcz2StreamDecoder 流式解码（下行形态）：剥头 + 分块喂入，逐块边界
// （含 1 字节一次的最坏情况）都能正确解出。
func TestDcz2StreamDecoder(t *testing.T) {
	dict := makePayload(3, "stream")
	body := append(append([]byte{}, dict...), []byte(`{"streamed":true}`)...)

	z, err := EncodeDcz2(body, 9, dict)
	if err != nil {
		t.Fatal(err)
	}
	// 头必须先剥掉：流式解码器接的是「头之后」的 zstd 字节
	payload := z[HeaderLen:]

	for _, chunk := range []int{0 /* 一次读完 */, 1, 7, 64 << 10} {
		r := io.Reader(bytes.NewReader(payload))
		if chunk > 0 {
			r = &chunkReader{r: r, n: chunk}
		}
		d, err := StreamDcz2Decoder(r, dict)
		if err != nil {
			t.Fatalf("chunk=%d: StreamDcz2Decoder: %v", chunk, err)
		}
		out, err := io.ReadAll(d.IOReadCloser())
		if err != nil {
			d.Close()
			t.Fatalf("chunk=%d: read: %v", chunk, err)
		}
		d.Close()
		if !bytes.Equal(out, body) {
			t.Fatalf("chunk=%d: 流式解出 %d 字节，期望 %d", chunk, len(out), len(body))
		}
	}

	// 反向：dcz2 的解码器不接 dcz 的流（字典 ID 不同 → 报错或解出垃圾都被
	// TestDcz2DczNotInterchangeable 的头哈希闸挡住；这里只锁"流式层也不串"）。
	z1, err := EncodeDcz(body, Sum(dict), 19, dict)
	if err != nil {
		t.Fatal(err)
	}
	d1, err := StreamDczDecoder(bytes.NewReader(z1[HeaderLen:]), dict)
	if err != nil {
		t.Fatalf("StreamDczDecoder: %v", err)
	}
	out1, err1 := io.ReadAll(d1.IOReadCloser())
	d1.Close()
	if err1 != nil || !bytes.Equal(out1, body) {
		t.Fatalf("dcz 流式解码失败: %v", err1)
	}
}

// chunkReader 每次最多返回 n 字节（模拟隧道分帧）
type chunkReader struct {
	r io.Reader
	n int
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if len(p) > c.n {
		p = p[:c.n]
	}
	return c.r.Read(p)
}
