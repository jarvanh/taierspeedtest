package main

import (
	"bytes"
	"image/png"
	"os"
	"testing"
)

func sampleInfo() ClientInfo {
	return ClientInfo{IP: "117.150.27.193", Province: "湖北", City: "宜昌", Oper: "移动"}
}

func sampleRows() []ProbeResult {
	return []ProbeResult{{
		Region: "湖北电信", Family: "IPv4", RTT: "9.9ms",
		SingleUp: "12.00Mbps", SingleDown: "200.00Mbps",
		MultiUp: "50.00Mbps", MultiDown: "1400.00Mbps",
		SingleUpF: 12, SingleDownF: 200, MultiUpF: 50, MultiDownF: 1400,
	}}
}

func TestExitLabelHidesIP(t *testing.T) {
	info := sampleInfo()
	got := exitLabel(info)
	if got != "宜昌移动" {
		t.Fatalf("exitLabel=%q", got)
	}
	if bytes.Contains([]byte(got), []byte("117.150")) {
		t.Fatal("label leaked ip")
	}
}

func TestRenderPNG(t *testing.T) {
	path := t.TempDir() + "/r.png"
	if err := renderReportPNG(sampleInfo(), sampleRows(), path, []string{"single", "multi"}); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	b := img.Bounds()
	if b.Dx() < 600 || b.Dy() < 120 {
		t.Fatalf("png too small: %dx%d", b.Dx(), b.Dy())
	}
}

func TestFontCoversReportChars(t *testing.T) {
	f, err := parseCJKFont()
	if err != nil {
		t.Fatal(err)
	}
	face, err := loadFace(14)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFace(face)
	_ = f
	var missing []rune
	check := func(s string) {
		for _, r := range s {
			if r < 32 {
				continue
			}
			if _, ok := face.GlyphAdvance(r); !ok {
				missing = append(missing, r)
			}
		}
	}
	check("宜昌移动湖北武汉")
	check("报告时间：出口节点延迟单线程上传下载多线程电信联通未知")
	for p, c := range provinceCity {
		check(p)
		check(c)
	}
	for city, v := range extraCity {
		check(city)
		check(v[0])
		check(v[1])
	}
	if len(missing) > 0 {
		t.Fatalf("missing glyphs: %q", string(missing))
	}
}

func TestEmbeddedFontParses(t *testing.T) {
	if len(cjkFontBytes) < 1000 {
		t.Fatalf("embedded font too small: %d", len(cjkFontBytes))
	}
	if _, err := parseCJKFont(); err != nil {
		t.Fatal(err)
	}
}
