package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

//go:embed fonts/NotoSansSC-subset.otf
var cjkFontBytes []byte

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
	host111  = "https://i.111666.best"
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

func hexToRGBA(h string) color.RGBA {
	var r, g, b uint8
	fmt.Sscanf(h, "#%02x%02x%02x", &r, &g, &b)
	return color.RGBA{r, g, b, 255}
}

func parseCJKFont() (*opentype.Font, error) {
	if coll, err := opentype.ParseCollection(cjkFontBytes); err == nil && coll.NumFonts() > 0 {
		return coll.Font(0)
	}
	return opentype.Parse(cjkFontBytes)
}

func loadFace(size float64) (font.Face, error) {
	f, err := parseCJKFont()
	if err != nil {
		return nil, err
	}
	return opentype.NewFace(f, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull})
}

func closeFace(face font.Face) {
	if c, ok := face.(io.Closer); ok {
		_ = c.Close()
	}
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

func renderReportPNG(info ClientInfo, rows []ProbeResult, path string, modes []string) error {
	titleF, err := loadFace(18)
	if err != nil {
		return fmt.Errorf("字体: %w", err)
	}
	defer closeFace(titleF)
	headF, err := loadFace(16)
	if err != nil {
		return err
	}
	defer closeFace(headF)
	cellF, err := loadFace(14)
	if err != nil {
		return err
	}
	defer closeFace(cellF)
	smallF, err := loadFace(13)
	if err != nil {
		return err
	}
	defer closeFace(smallF)

	families, grouped := groupedRows(rows)
	headers := reportHeaders(modes)
	nCol := len(headers)
	colW := 140
	width := 80 + nCol*colW
	if width < 720 {
		width = 720
	}
	lineH := 26
	h := 150
	for _, f := range families {
		h += 40 + lineH*(1+len(grouped[f])) + 16
	}
	img := image.NewRGBA(image.Rect(0, 0, width, h))
	draw.Draw(img, img.Bounds(), &image.Uniform{colBG}, image.Point{}, draw.Src)

	d := &font.Drawer{Face: titleF}
	title := "TaierSpeedtest  全球网测"
	drawText(img, titleF, (width-d.MeasureString(title).Round())/2, 44, title, colTeal, false)
	now := time.Now().Format("2006-01-02 15:04:05")
	sub := fmt.Sprintf("报告时间：%s    出口：%s", now, exitLabel(info))
	d.Face = smallF
	drawText(img, smallF, (width-d.MeasureString(sub).Round())/2, 68, sub, colMuted, false)
	for x := 40; x < width-40; x += 10 {
		for i := 0; i < 7 && x+i < width-40; i++ {
			img.Set(x+i, 84, colMuted)
			img.Set(x+i, 85, colMuted)
		}
	}

	xs := make([]int, nCol)
	for i := 0; i < nCol; i++ {
		xs[i] = 40 + (i+1)*colW
		if xs[i] > width-30 {
			xs[i] = width - 30
		}
	}
	y := 116
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
	d.Face = smallF
	foot := "github.com/MiaM1ku/taierspeedtest"
	drawText(img, smallF, (width-d.MeasureString(foot).Round())/2, h-18, foot, colMuted, false)

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
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
		return tok, nil
	}
	_ = os.WriteFile(p, []byte(tok+"\n"), 0o600)
	return tok, nil
}

func mimeByExt(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	default:
		return "application/octet-stream"
	}
}

func postMultipart(url, field, path string, extra map[string]string, headers map[string]string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range extra {
		if err := w.WriteField(k, v); err != nil {
			return nil, err
		}
	}
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, field, filepath.Base(path)))
	h.Set("Content-Type", mimeByExt(path))
	fw, err := w.CreatePart(h)
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(fw, f); err != nil {
		return nil, err
	}
	ctype := w.FormDataContentType()
	if err := w.Close(); err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, url, &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", ctype)
	req.Header.Set("User-Agent", "TaierSpeedtest/"+version)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 45 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d %s", resp.StatusCode, truncate(string(body), 180))
	}
	return body, nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func upload111666(path string) (string, error) {
	token, err := authToken()
	if err != nil {
		return "", err
	}
	body, err := postMultipart(host111+"/image", "image", path, nil, map[string]string{"Auth-Token": token})
	if err != nil {
		return "", err
	}
	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		return "", fmt.Errorf("返回: %s", truncate(string(body), 180))
	}
	ok, _ := data["ok"].(bool)
	src, _ := data["src"].(string)
	if src == "" {
		src, _ = data["url"].(string)
	}
	if !ok && src == "" {
		return "", fmt.Errorf("拒绝: %s", truncate(string(body), 180))
	}
	if src == "" {
		return "", fmt.Errorf("无地址: %s", truncate(string(body), 180))
	}
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		return src, nil
	}
	if !strings.HasPrefix(src, "/") {
		src = "/" + src
	}
	return host111 + src, nil
}

func uploadCatbox(path string) (string, error) {
	body, err := postMultipart("https://catbox.moe/user/api.php", "fileToUpload", path, map[string]string{"reqtype": "fileupload"}, nil)
	if err != nil {
		return "", err
	}
	u := strings.TrimSpace(string(body))
	if !strings.HasPrefix(u, "http") {
		return "", fmt.Errorf("%s", truncate(u, 180))
	}
	return u, nil
}

func upload0x0(path string) (string, error) {
	body, err := postMultipart("https://0x0.st", "file", path, nil, map[string]string{"Accept": "text/plain"})
	if err != nil {
		return "", err
	}
	u := strings.TrimSpace(string(body))
	if !strings.HasPrefix(u, "http") {
		return "", fmt.Errorf("%s", truncate(u, 180))
	}
	return u, nil
}

func publishReport(info ClientInfo, rows []ProbeResult, modes []string) (string, error) {
	dir := os.TempDir()
	pngPath := filepath.Join(dir, fmt.Sprintf("taierspeedtest-%d.png", time.Now().Unix()))
	if err := renderReportPNG(info, rows, pngPath, modes); err != nil {
		return "", fmt.Errorf("渲染 PNG 失败: %w", err)
	}
	type up struct {
		name string
		fn   func(string) (string, error)
	}
	var errs []string
	for _, u := range []up{
		{"111666", upload111666},
		{"catbox", uploadCatbox},
		{"0x0", upload0x0},
	} {
		url, err := u.fn(pngPath)
		if err == nil {
			return url, nil
		}
		errs = append(errs, u.name+": "+err.Error())
	}
	return "", fmt.Errorf("%s；本地 PNG: %s", strings.Join(errs, "；"), pngPath)
}
