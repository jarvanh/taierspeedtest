package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"os"
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

func imgSpeedColor(v float64, text string) color.RGBA {
	if text == "failed" || text == "-" || v < 0 {
		if text == "-" {
			return colMuted
		}
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

func loadFace(bold bool, size float64) font.Face {
	path := "/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc"
	if bold {
		path = "/usr/share/fonts/opentype/noto/NotoSansCJK-Bold.ttc"
	}
	b, err := os.ReadFile(path)
	if err != nil {
		b, err = os.ReadFile("/usr/share/fonts/truetype/droid/DroidSansFallbackFull.ttf")
		if err != nil {
			return nil
		}
	}
	coll, err := opentype.ParseCollection(b)
	var f *opentype.Font
	if err == nil && coll.NumFonts() > 0 {
		f, err = coll.Font(0)
	} else {
		f, err = opentype.Parse(b)
	}
	if err != nil {
		return nil
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
		w := d.MeasureString(s)
		x -= w.Round()
	}
	d.Dot = fixed.P(x, y)
	d.DrawString(s)
}

func drawDash(img *image.RGBA, x1, x2, y int, c color.RGBA) {
	for x := x1; x < x2; x += 10 {
		for i := 0; i < 7 && x+i < x2; i++ {
			img.Set(x+i, y, c)
			img.Set(x+i, y+1, c)
		}
	}
}

func renderReport(info ClientInfo, rows []ProbeResult, path string, modes []string) error {
	titleF := loadFace(true, 18)
	headF := loadFace(true, 16)
	cellF := loadFace(true, 15)
	smallF := loadFace(false, 13)
	if titleF == nil || cellF == nil {
		return fmt.Errorf("未找到中文字体，跳过出图")
	}

	grouped := map[string][]ProbeResult{}
	var families []string
	for _, r := range rows {
		if _, ok := grouped[r.Family]; !ok {
			families = append(families, r.Family)
		}
		grouped[r.Family] = append(grouped[r.Family], r)
	}

	showS := contains(modes, "single")
	showM := contains(modes, "multi")
	nCol := 2
	if showS {
		nCol += 2
	}
	if showM {
		nCol += 2
	}
	width := 160 + nCol*130
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

	y := 48
	drawText(img, titleF, width/2, y, "TaierSpeedtest  全球网测", colTeal, false)
	// center title roughly
	y = 48
	d := &font.Drawer{Face: titleF}
	tw := d.MeasureString("TaierSpeedtest  全球网测").Round()
	draw.Draw(img, image.Rect(0, 20, width, 60), &image.Uniform{colBG}, image.Point{}, draw.Src)
	drawText(img, titleF, (width-tw)/2, 48, "TaierSpeedtest  全球网测", colTeal, false)

	now := time.Now().Format("2006-01-02 15:04:05")
	loc := info.City + info.Oper
	if loc == info.Oper {
		loc = info.Province + info.Oper
	}
	sub := fmt.Sprintf("报告时间：%s    出口：%s %s", now, loc, info.IP)
	d.Face = smallF
	sw := d.MeasureString(sub).Round()
	drawText(img, smallF, (width-sw)/2, 72, sub, colMuted, false)
	drawDash(img, 40, width-40, 88, colMuted)

	y = 120
	cols := make([]int, 0, 6)
	cols = append(cols, 130, 210)
	x := 340
	if showS {
		cols = append(cols, x, x+140)
		x += 280
	}
	if showM {
		cols = append(cols, x, x+140)
	}

	headers := []string{"节点", "延迟"}
	if showS {
		headers = append(headers, "单线程上传", "单线程下载")
	}
	if showM {
		headers = append(headers, "多线程上传", "多线程下载")
	}

	for _, fam := range families {
		drawText(img, headF, 40, y, fam+" 测速", colTeal, false)
		y += 28
		for i, htxt := range headers {
			drawText(img, cellF, cols[i], y, htxt, colTeal, true)
		}
		y += lineH
		for _, r := range grouped[fam] {
			cells := []string{r.Region, r.RTT}
			fills := []color.RGBA{colTeal, colMuted}
			if r.RTT != "-" {
				fills[1] = colOK
			}
			if showS {
				cells = append(cells, r.SingleUp, r.SingleDown)
				fills = append(fills, imgSpeedColor(r.SingleUpF, r.SingleUp), imgSpeedColor(r.SingleDownF, r.SingleDown))
			}
			if showM {
				cells = append(cells, r.MultiUp, r.MultiDown)
				fills = append(fills, imgSpeedColor(r.MultiUpF, r.MultiUp), imgSpeedColor(r.MultiDownF, r.MultiDown))
			}
			for i := range cells {
				drawText(img, cellF, cols[i], y, cells[i], fills[i], true)
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

func contains(ss []string, x string) bool {
	for _, s := range ss {
		if s == x {
			return true
		}
	}
	return false
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

func uploadImage(path string) (string, error) {
	token, err := authToken()
	if err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile("image", filepath.Base(path))
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(fw, f); err != nil {
		return "", err
	}
	ctype := w.FormDataContentType()
	_ = w.Close()
	req, err := http.NewRequest(http.MethodPost, "https://i.111666.best/image", &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", ctype)
	req.Header.Set("Auth-Token", token)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		return "", fmt.Errorf("图床返回: %s", string(body))
	}
	src, _ := data["src"].(string)
	if src == "" {
		src, _ = data["url"].(string)
	}
	if src == "" {
		return "", fmt.Errorf("图床返回异常: %s", string(body))
	}
	if strings.HasPrefix(src, "http") {
		return src, nil
	}
	if !strings.HasPrefix(src, "/") {
		src = "/" + src
	}
	return "https://i.111666.best" + src, nil
}
