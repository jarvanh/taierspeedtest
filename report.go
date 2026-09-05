package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

var (
	colBG    = color.RGBA{31, 30, 42, 255}
	colTeal  = color.RGBA{117, 184, 166, 255}
	colMuted = color.RGBA{141, 136, 123, 255}
	colOK    = color.RGBA{135, 216, 141, 255}
	colWarn  = color.RGBA{230, 195, 106, 255}
	colBad   = color.RGBA{224, 108, 117, 255}
)

const (
	hexBG    = "#1f1e2a"
	hexTeal  = "#75b8a6"
	hexMuted = "#8d887b"
	hexOK    = "#87d88d"
	hexWarn  = "#e6c36a"
	hexBad   = "#e06c75"
	fontURL  = "https://cdn.jsdelivr.net/gh/notofonts/noto-cjk@main/Sans/SubsetOTF/SC/NotoSansSC-Regular.otf"
)

func imgSpeedColor(v float64, text string) color.RGBA {
	if text == "-" {
		return colMuted
	}
	if text == "failed" || v < 0 {
		return colBad
	}
	if v <= 20 {
		return colBad
	}
	if v <= 150 {
		return colWarn
	}
	return colOK
}

func imgSpeedHex(v float64, text string) string {
	c := imgSpeedColor(v, text)
	return fmt.Sprintf("#%02x%02x%02x", c.R, c.G, c.B)
}

func contains(ss []string, x string) bool {
	for _, s := range ss {
		if s == x {
			return true
		}
	}
	return false
}

func exitLabel(info ClientInfo) string {
	loc := info.City + info.Oper
	if loc == info.Oper {
		loc = info.Province + info.Oper
	}
	if loc == "" {
		return "未知"
	}
	return loc
}

func groupedRows(rows []ProbeResult) (families []string, grouped map[string][]ProbeResult) {
	grouped = map[string][]ProbeResult{}
	for _, r := range rows {
		if _, ok := grouped[r.Family]; !ok {
			families = append(families, r.Family)
		}
		grouped[r.Family] = append(grouped[r.Family], r)
	}
	return families, grouped
}

func reportHeaders(modes []string) []string {
	h := []string{"节点", "延迟"}
	if contains(modes, "single") {
		h = append(h, "单线程上传", "单线程下载")
	}
	if contains(modes, "multi") {
		h = append(h, "多线程上传", "多线程下载")
	}
	return h
}

func reportCells(r ProbeResult, modes []string) []string {
	cells := []string{r.Region, r.RTT}
	if contains(modes, "single") {
		cells = append(cells, r.SingleUp, r.SingleDown)
	}
	if contains(modes, "multi") {
		cells = append(cells, r.MultiUp, r.MultiDown)
	}
	return cells
}

func reportFills(r ProbeResult, modes []string) []string {
	fills := []string{hexTeal, hexMuted}
	if r.RTT != "-" {
		fills[1] = hexOK
	}
	if contains(modes, "single") {
		fills = append(fills, imgSpeedHex(r.SingleUpF, r.SingleUp), imgSpeedHex(r.SingleDownF, r.SingleDown))
	}
	if contains(modes, "multi") {
		fills = append(fills, imgSpeedHex(r.MultiUpF, r.MultiUp), imgSpeedHex(r.MultiDownF, r.MultiDown))
	}
	return fills
}

func renderReportSVG(info ClientInfo, rows []ProbeResult, path string, modes []string) error {
	families, grouped := groupedRows(rows)
	headers := reportHeaders(modes)
	nCol := len(headers)
	colW := 130
	width := 80 + nCol*colW
	if width < 640 {
		width = 640
	}
	lineH := 26.0
	height := 130.0
	for _, f := range families {
		height += 40 + lineH*float64(1+len(grouped[f])) + 16
	}

	xs := make([]int, nCol)
	for i := 0; i < nCol; i++ {
		xs[i] = 40 + (i+1)*colW
		if xs[i] > width-30 {
			xs[i] = width - 30
		}
	}

	var b strings.Builder
	esc := html.EscapeString
	fontStack := `"PingFang SC","Microsoft YaHei","Noto Sans CJK SC","Source Han Sans SC","Noto Sans SC",sans-serif`
	fmt.Fprintf(&b, `<?xml version="1.0" encoding="UTF-8"?>`+"\n")
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%.0f" viewBox="0 0 %d %.0f">`+"\n", width, height, width, height)
	fmt.Fprintf(&b, `<rect width="100%%" height="100%%" fill="%s"/>`+"\n", hexBG)
	fmt.Fprintf(&b, `<style>text{font-family:%s;font-size:15px;font-weight:700;dominant-baseline:central}</style>`+"\n", fontStack)
	fmt.Fprintf(&b, `<text x="%d" y="32" text-anchor="middle" fill="%s" font-size="18">TaierSpeedtest  全球网测</text>`+"\n", width/2, hexTeal)
	now := time.Now().Format("2006-01-02 15:04:05")
	fmt.Fprintf(&b, `<text x="%d" y="56" text-anchor="middle" fill="%s" font-size="13">报告时间：%s    出口：%s</text>`+"\n",
		width/2, hexMuted, esc(now), esc(exitLabel(info)))
	fmt.Fprintf(&b, `<line x1="40" x2="%d" y1="76" y2="76" stroke="%s" stroke-width="1.5" stroke-dasharray="7 3"/>`+"\n", width-40, hexMuted)

	y := 110.0
	for _, fam := range families {
		fmt.Fprintf(&b, `<text x="40" y="%.1f" fill="%s" font-size="16">%s 测速</text>`+"\n", y, hexTeal, esc(fam))
		y += 28
		for i, h := range headers {
			fmt.Fprintf(&b, `<text x="%d" y="%.1f" text-anchor="end" fill="%s">%s</text>`+"\n", xs[i], y, hexTeal, esc(h))
		}
		y += lineH
		for _, r := range grouped[fam] {
			cells := reportCells(r, modes)
			fills := reportFills(r, modes)
			for i := range cells {
				fmt.Fprintf(&b, `<text x="%d" y="%.1f" text-anchor="end" fill="%s">%s</text>`+"\n", xs[i], y, fills[i], esc(cells[i]))
			}
			y += lineH
		}
		y += 16
	}
	b.WriteString("</svg>\n")
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func fontCachePath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = filepath.Join(os.TempDir(), "taierspeedtest")
	}
	return filepath.Join(dir, "taierspeedtest", "NotoSansSC-Regular.otf")
}

func ensureCJKFont() string {
	candidates := []string{
		"/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc",
		"/usr/share/fonts/opentype/noto/NotoSansSC-Regular.otf",
		"/usr/share/fonts/truetype/droid/DroidSansFallbackFull.ttf",
		"/usr/share/fonts/truetype/wqy/wqy-zenhei.ttc",
		fontCachePath(),
	}
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && st.Size() > 1000 {
			return p
		}
	}
	dst := fontCachePath()
	_ = os.MkdirAll(filepath.Dir(dst), 0o755)
	client := &http.Client{Timeout: 45 * time.Second}
	resp, err := client.Get(fontURL)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return ""
	}
	tmp := dst + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return ""
	}
	_, err = io.Copy(f, resp.Body)
	_ = f.Close()
	if err != nil {
		return ""
	}
	if err := os.Rename(tmp, dst); err != nil {
		return ""
	}
	return dst
}

func loadFace(path string, size float64) font.Face {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var f *opentype.Font
	if coll, err := opentype.ParseCollection(b); err == nil && coll.NumFonts() > 0 {
		f, err = coll.Font(0)
		if err != nil {
			return nil
		}
	} else {
		f, err = opentype.Parse(b)
		if err != nil {
			return nil
		}
	}
	face, err := opentype.NewFace(f, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		return nil
	}
	return face
}

func drawText(img *image.RGBA, face font.Face, x, y int, s string, c color.Color, right bool) {
	if face == nil {
		return
	}
	d := &font.Drawer{Dst: img, Src: image.NewUniform(c), Face: face}
	if right {
		x -= d.MeasureString(s).Round()
	}
	d.Dot = fixed.P(x, y)
	d.DrawString(s)
}

func hexToRGBA(h string) color.RGBA {
	var r, g, b uint8
	fmt.Sscanf(h, "#%02x%02x%02x", &r, &g, &b)
	return color.RGBA{r, g, b, 255}
}

func renderReportPNG(info ClientInfo, rows []ProbeResult, path string, modes []string) error {
	fontPath := ensureCJKFont()
	if fontPath == "" {
		return fmt.Errorf("无中文字体")
	}
	titleF := loadFace(fontPath, 18)
	headF := loadFace(fontPath, 16)
	cellF := loadFace(fontPath, 15)
	smallF := loadFace(fontPath, 13)
	if titleF == nil || cellF == nil {
		return fmt.Errorf("无法解析字体")
	}
	families, grouped := groupedRows(rows)
	headers := reportHeaders(modes)
	nCol := len(headers)
	colW := 140
	width := 80 + nCol*colW
	if width < 720 {
		width = 720
	}
	lineH := 26
	h := 140
	for _, f := range families {
		h += 40 + lineH*(1+len(grouped[f])) + 16
	}
	img := image.NewRGBA(image.Rect(0, 0, width, h))
	draw.Draw(img, img.Bounds(), &image.Uniform{colBG}, image.Point{}, draw.Src)

	d := &font.Drawer{Face: titleF}
	title := "TaierSpeedtest  全球网测"
	drawText(img, titleF, (width-d.MeasureString(title).Round())/2, 48, title, colTeal, false)
	now := time.Now().Format("2006-01-02 15:04:05")
	sub := fmt.Sprintf("报告时间：%s    出口：%s", now, exitLabel(info))
	d.Face = smallF
	drawText(img, smallF, (width-d.MeasureString(sub).Round())/2, 72, sub, colMuted, false)
	for x := 40; x < width-40; x += 10 {
		for i := 0; i < 7 && x+i < width-40; i++ {
			img.Set(x+i, 88, colMuted)
			img.Set(x+i, 89, colMuted)
		}
	}

	xs := make([]int, nCol)
	for i := 0; i < nCol; i++ {
		xs[i] = 40 + (i+1)*colW
		if xs[i] > width-30 {
			xs[i] = width - 30
		}
	}
	y := 120
	for _, fam := range families {
		drawText(img, headF, 40, y, fam+" 测速", colTeal, false)
		y += 28
		for i, hd := range headers {
			drawText(img, cellF, xs[i], y, hd, colTeal, true)
		}
		y += lineH
		for _, r := range grouped[fam] {
			cells := reportCells(r, modes)
			fills := reportFills(r, modes)
			for i := range cells {
				drawText(img, cellF, xs[i], y, cells[i], hexToRGBA(fills[i]), true)
			}
			y += lineH
		}
		y += 16
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}

func convertSVGToPNG(svgPath, pngPath string) error {
	cmd := exec.Command("convert", "-background", hexBG, svgPath, pngPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("convert: %v %s", err, string(out))
	}
	st, err := os.Stat(pngPath)
	if err != nil || st.Size() < 100 {
		return fmt.Errorf("convert 未生成有效 PNG")
	}
	return nil
}

func authToken() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	p := filepath.Join(dir, "taierspeedtest", "auth-token")
	if b, err := os.ReadFile(p); err == nil {
		s := strings.TrimSpace(string(b))
		if s != "" {
			return s, nil
		}
	}
	tok := fmt.Sprintf("%x%x", time.Now().UnixNano(), os.Getpid())
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(p, []byte(tok+"\n"), 0o600); err != nil {
		return "", err
	}
	return tok, nil
}

func postMultipart(url, field, path string, headers map[string]string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile(field, filepath.Base(path))
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(fw, f); err != nil {
		return nil, err
	}
	ctype := w.FormDataContentType()
	_ = w.Close()
	req, err := http.NewRequest(http.MethodPost, url, &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", ctype)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func upload111666(path string) (string, error) {
	token, err := authToken()
	if err != nil {
		return "", err
	}
	body, err := postMultipart("https://i.111666.best/image", "image", path, map[string]string{"Auth-Token": token})
	if err != nil {
		return "", err
	}
	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		return "", fmt.Errorf("111666 返回: %s", string(body))
	}
	if ok, _ := data["ok"].(bool); !ok && data["src"] == nil {
		return "", fmt.Errorf("111666 拒绝: %s", string(body))
	}
	src, _ := data["src"].(string)
	if src == "" {
		src, _ = data["url"].(string)
	}
	if src == "" {
		return "", fmt.Errorf("111666 无地址: %s", string(body))
	}
	if strings.HasPrefix(src, "http") {
		return src, nil
	}
	if !strings.HasPrefix(src, "/") {
		src = "/" + src
	}
	return "https://i.111666.best" + src, nil
}

func uploadCatbox(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("reqtype", "fileupload")
	fw, err := w.CreateFormFile("fileToUpload", filepath.Base(path))
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(fw, f); err != nil {
		return "", err
	}
	ctype := w.FormDataContentType()
	_ = w.Close()
	req, err := http.NewRequest(http.MethodPost, "https://catbox.moe/user/api.php", &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", ctype)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	u := strings.TrimSpace(string(b))
	if !strings.HasPrefix(u, "http") {
		return "", fmt.Errorf("catbox: %s", u)
	}
	return u, nil
}

func publishReport(info ClientInfo, rows []ProbeResult, modes []string) (string, error) {
	stamp := time.Now().Unix()
	svgPath := fmt.Sprintf("/tmp/taierspeedtest-%d.svg", stamp)
	pngPath := fmt.Sprintf("/tmp/taierspeedtest-%d.png", stamp)
	if err := renderReportSVG(info, rows, svgPath, modes); err != nil {
		return "", err
	}
	pngOK := renderReportPNG(info, rows, pngPath, modes) == nil
	if !pngOK {
		pngOK = convertSVGToPNG(svgPath, pngPath) == nil
	}

	var errs []string
	if pngOK {
		if u, err := upload111666(pngPath); err == nil {
			return u, nil
		} else {
			errs = append(errs, "111666: "+err.Error())
		}
		if u, err := uploadCatbox(pngPath); err == nil {
			return u, nil
		} else {
			errs = append(errs, "catbox png: "+err.Error())
		}
	} else {
		errs = append(errs, "png 渲染失败，改传 SVG")
	}
	if u, err := uploadCatbox(svgPath); err == nil {
		return u, nil
	} else {
		errs = append(errs, "catbox svg: "+err.Error())
	}
	return "", fmt.Errorf("%s；本地 SVG: %s", strings.Join(errs, "；"), svgPath)
}
