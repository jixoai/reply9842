package main

import "testing"

func TestTransportChoicesByNetworkMode(t *testing.T) {
	t.Run("代理只提供自动/TCP", func(t *testing.T) {
		options, labels := transportChoices("proxy")
		if len(options) != 2 || options[0] != "auto" || options[1] != "tcp" {
			t.Fatalf("代理模式选项 = %v，期望 [auto tcp]", options)
		}
		for _, l := range labels {
			if l == "QUIC" {
				t.Fatalf("代理模式不得出现 QUIC 选项：%v", labels)
			}
		}
	})
	t.Run("其它模式保留三种", func(t *testing.T) {
		for _, mode := range []string{"direct", "system"} {
			options, _ := transportChoices(mode)
			if len(options) != 3 || options[1] != "quic" {
				t.Fatalf("%s 模式选项 = %v，期望含 QUIC 的三项", mode, options)
			}
		}
	})
}

func TestTransportForNetworkCoercesProxyQUIC(t *testing.T) {
	if got := transportForNetwork("proxy", "quic"); got != "auto" {
		t.Fatalf("代理 + QUIC 应纠正为 auto，实际 %q", got)
	}
	if got := transportForNetwork("proxy", "tcp"); got != "tcp" {
		t.Fatalf("代理 + TCP 不应改动，实际 %q", got)
	}
	if got := transportForNetwork("direct", "quic"); got != "quic" {
		t.Fatalf("直连模式应保留 QUIC，实际 %q", got)
	}
}
