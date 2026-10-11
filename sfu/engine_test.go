package sfu

import (
	"reflect"
	"testing"
)

// TestParseAdvertiseIPs 钉住一个真实事故：SFU_ADVERTISE_IP 支持逗号分隔的多个地址。
//
// 线上故障链：服务器上有一份未进版本库的本地补丁把逗号拆成列表，重新构建时丢失 →
// 整串被当成一个 IP 传给 Pion → 地址改写表无效 → 每个远端候选都被拒
// （"invalid address rewrite mapping"）→ 所有客户端都连不上。
func TestParseAdvertiseIPs(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"单个地址", "104.168.149.15", []string{"104.168.149.15"}},
		{"多个地址", "192.168.2.202,10.1.0.202,104.168.149.15",
			[]string{"192.168.2.202", "10.1.0.202", "104.168.149.15"}},
		{"带空格的多个地址", " 1.2.3.4 , 5.6.7.8 ", []string{"1.2.3.4", "5.6.7.8"}},
		{"忽略空项", "1.2.3.4,,5.6.7.8,", []string{"1.2.3.4", "5.6.7.8"}},
		{"全是空白", "  ,  ", []string{}},
		{"空字符串", "", []string{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseAdvertiseIPs(c.in)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("parseAdvertiseIPs(%q) = %v，期望 %v", c.in, got, c.want)
			}
		})
	}
}
