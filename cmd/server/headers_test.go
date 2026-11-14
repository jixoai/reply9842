package main

import "testing"

// TestHeaderMap 可重复 -header 的解析：同名后者覆盖、Host 大小写不敏感。
func TestHeaderMap(t *testing.T) {
	c := &cfg{headers: headerList{
		"Host: one.example.com",
		"X-Foo: bar",
		"host: two.example.com", // 同名后者覆盖（Host 大小写不同也要覆盖）
		"  X-Spaced :  v  ",
	}}
	m := c.headerMap()
	// 规范化后只有 Host 一个键，值是最后一条（同名后者覆盖）
	if got := m["Host"]; got != "two.example.com" {
		t.Errorf("Host = %q，期望最后一条 two.example.com（map=%v）", got, m)
	}
	if got := m["X-Foo"]; got != "bar" {
		t.Errorf("X-Foo = %q", got)
	}
	if got := m["X-Spaced"]; got != "v" {
		t.Errorf("X-Spaced 未 trim: %q", got)
	}
	if len(m) != 3 {
		t.Errorf("期望 3 个头，得到 %d: %v", len(m), m)
	}
}

// TestHeaderListSetRejects 缺冒号的写法要立刻报错，而不是静默丢掉。
func TestHeaderListSetRejects(t *testing.T) {
	var h headerList
	if err := h.Set("NoColonHere"); err == nil {
		t.Error("缺冒号应当报错")
	}
	if err := h.Set(": value"); err == nil {
		t.Error("缺名字应当报错")
	}
	if err := h.Set("X-Ok: 1"); err != nil {
		t.Errorf("合法写法被拒: %v", err)
	}
}
