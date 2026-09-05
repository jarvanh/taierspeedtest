package main

import (
	"os"
	"strings"
	"testing"
)

func TestRenderSVGHidesIP(t *testing.T) {
	info := ClientInfo{IP: "117.150.27.193", Province: "湖北", City: "宜昌", Oper: "移动"}
	rows := []ProbeResult{{
		Region: "湖北电信", Family: "IPv4", RTT: "9.9ms",
		SingleUp: "12.00Mbps", SingleDown: "200.00Mbps",
		MultiUp: "50.00Mbps", MultiDown: "1400.00Mbps",
		SingleUpF: 12, SingleDownF: 200, MultiUpF: 50, MultiDownF: 1400,
	}}
	svg := t.TempDir() + "/r.svg"
	if err := renderReportSVG(info, rows, svg, []string{"single", "multi"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(svg)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Contains(s, "117.150") || strings.Contains(s, info.IP) {
		t.Fatalf("svg leaked ip")
	}
	if !strings.Contains(s, "宜昌移动") {
		t.Fatalf("missing exit label")
	}
}
