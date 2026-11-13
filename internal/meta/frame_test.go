package meta

import (
	"testing"
)

// 能力位必须向后兼容：不含该位的请求帧（旧客户端发出的）必须能被解析，
// 且解析结果 AcceptDownZstd=false —— 这是「旧客户端不会被推压缩流」的
// 唯一保障。
func TestRequestCapabilityBitBackwardCompatible(t *testing.T) {
	body := []byte(`{"hello":"world"}`)
	hdrs := EncodeHeaders(map[string][]string{"Content-Type": {"application/json"}})

	// 1) 新客户端：带能力位
	hNew := &RequestHead{
		Method: "POST", Path: "/v1/messages", Encoding: EncIdentity,
		BodyLen: int64(len(body)), AcceptDownZstd: true,
	}
	raw, err := EncodeRequest(hNew, hdrs, body)
	if err != nil {
		t.Fatal(err)
	}
	got, gh, gb, err := DecodeRequest(raw)
	if err != nil {
		t.Fatalf("新帧解析失败: %v", err)
	}
	if !got.AcceptDownZstd {
		t.Error("带能力位的新帧解析出 AcceptDownZstd=false")
	}
	if string(gb) != string(body) || string(gh) != string(hdrs) {
		t.Error("新帧 headers/body 解析错位")
	}

	// 2) 旧客户端：同样的 head 但不带能力位（模拟老版本二进制）
	hOld := *hNew
	hOld.AcceptDownZstd = false
	rawOld, err := EncodeRequest(&hOld, hdrs, body)
	if err != nil {
		t.Fatal(err)
	}
	if len(rawOld) != len(raw)-1 {
		t.Fatalf("能力位应为 1 字节追加：new=%d old=%d", len(raw), len(rawOld))
	}
	gotOld, _, gbOld, err := DecodeRequest(rawOld)
	if err != nil {
		t.Fatalf("旧帧解析失败（向后兼容被破坏）: %v", err)
	}
	if gotOld.AcceptDownZstd {
		t.Error("不带能力位的旧帧解析出 AcceptDownZstd=true —— 会向旧客户端推压缩流")
	}
	if string(gbOld) != string(body) {
		t.Errorf("旧帧 body 解析错位: %q", gbOld)
	}
}

// 旧服务端（当它收到带能力位的新帧时）必须不 panic 且不影响 body 解析：
// 这是「新客户端 + 旧服务端」的过渡期形态。旧服务端的解码器只按
// bodyLen 截 body、不做尾部检查，等价于忽略这一字节。
func TestRequestCapabilityBitIgnoredByOldDecoder(t *testing.T) {
	body := []byte("payload")
	hdrs := []byte("K:v\n")
	raw, err := EncodeRequest(&RequestHead{
		Method: "POST", Path: "/x", Encoding: EncIdentity,
		BodyLen: int64(len(body)), AcceptDownZstd: true,
	}, hdrs, body)
	if err != nil {
		t.Fatal(err)
	}
	// 模拟旧解码器：截掉尾部能力位后再解析
	if _, _, gb, err := DecodeRequest(raw[:len(raw)-1]); err != nil || string(gb) != string(body) {
		t.Fatalf("旧解码器路径失败: err=%v body=%q", err, gb)
	}
}

// 续传请求的能力位同样必须向后兼容
func TestResumeCapabilityBitBackwardCompatible(t *testing.T) {
	var key [32]byte
	key[0] = 1

	newReq := &ResumeReq{IdemKey: key, From: 4096, EventID: "42", AcceptDownZstd: true}
	rawNew := EncodeResumeReq(newReq)
	gotNew, err := DecodeResumeReq(rawNew)
	if err != nil {
		t.Fatal(err)
	}
	if !gotNew.AcceptDownZstd || gotNew.From != 4096 || gotNew.EventID != "42" {
		t.Fatalf("新续传帧解析错: %+v", gotNew)
	}

	oldReq := &ResumeReq{IdemKey: key, From: 4096, EventID: "42"}
	rawOld := EncodeResumeReq(oldReq)
	if len(rawOld) != len(rawNew)-1 {
		t.Fatalf("能力位应为 1 字节追加: new=%d old=%d", len(rawNew), len(rawOld))
	}
	gotOld, err := DecodeResumeReq(rawOld)
	if err != nil {
		t.Fatalf("旧续传帧解析失败: %v", err)
	}
	if gotOld.AcceptDownZstd {
		t.Error("旧续传帧解析出 AcceptDownZstd=true —— 会向旧客户端推压缩流")
	}
	if gotOld.From != 4096 || gotOld.EventID != "42" {
		t.Fatalf("旧续传帧字段解析错: %+v", gotOld)
	}
}
