package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

const (
	cyan   = "\033[36m"
	bold   = "\033[1m"
	green  = "\033[32m"
	yellow = "\033[33m"
	red    = "\033[31m"
	dim    = "\033[2m"
	nc     = "\033[0m"
)

func useColor() bool {
	st, _ := os.Stdout.Stat()
	return st.Mode()&os.ModeCharDevice != 0 && os.Getenv("TERM") != "dumb"
}

func c(code, s string) string {
	if !useColor() {
		return s
	}
	return code + s + nc
}

var isps = []string{"电信", "联通", "移动"}

var bsg = [][2]string{{"北京", "北京"}, {"上海", "上海"}, {"广东", "广州"}}

var provinceCity = map[string]string{
	"北京": "北京", "天津": "天津", "上海": "上海", "重庆": "重庆",
	"河北": "石家庄", "山西": "太原", "内蒙古": "呼和浩特",
	"辽宁": "沈阳", "吉林": "长春", "黑龙江": "哈尔滨",
	"江苏": "南京", "浙江": "杭州", "安徽": "合肥", "福建": "福州",
	"江西": "南昌", "山东": "济南", "河南": "郑州", "湖北": "武汉",
	"湖南": "长沙", "广东": "广州", "广西": "南宁", "海南": "海口",
	"四川": "成都", "贵州": "贵阳", "云南": "昆明", "西藏": "拉萨",
	"陕西": "西安", "甘肃": "兰州", "青海": "西宁", "宁夏": "银川",
	"新疆": "乌鲁木齐", "台湾": "台北", "香港": "香港", "澳门": "澳门",
}

var extraCity = map[string][2]string{
	"深圳": {"广东", "深圳"}, "苏州": {"江苏", "苏州"}, "宁波": {"浙江", "宁波"},
	"青岛": {"山东", "青岛"}, "厦门": {"福建", "厦门"}, "东莞": {"广东", "东莞"},
	"无锡": {"江苏", "无锡"}, "佛山": {"广东", "佛山"},
}

type ProbeResult struct {
	Region      string  `json:"region"`
	Family      string  `json:"family"`
	RTT         string  `json:"rtt"`
	SingleUp    string  `json:"single_up"`
	SingleDown  string  `json:"single_down"`
	MultiUp     string  `json:"multi_up"`
	MultiDown   string  `json:"multi_down"`
	SingleUpF   float64 `json:"single_up_f"`
	SingleDownF float64 `json:"single_down_f"`
	MultiUpF    float64 `json:"multi_up_f"`
	MultiDownF  float64 `json:"multi_down_f"`
}

func dw(s string) int {
	n := 0
	for _, r := range s {
		if r > 127 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

func padLeft(w int, s string) string {
	p := w - dw(s)
	if p < 0 {
		p = 0
	}
	return strings.Repeat(" ", p) + s
}

func fmtSpeed(v float64) string {
	if v < 0.001 {
		return "failed"
	}
	return fmt.Sprintf("%.2fMbps", v)
}

func fmtMS(v float64) string {
	if v < 0 {
		return "-"
	}
	if v < 10 {
		return fmt.Sprintf("%.1fms", v)
	}
	return fmt.Sprintf("%.0fms", v)
}

func speedColor(v float64) string {
	if v < 0 || v <= 20 {
		return red
	}
	if v <= 150 {
		return yellow
	}
	return green
}

func cell(text string, v float64, w int) string {
	if text == "-" || text == "" {
		return c(dim, padLeft(w, "-"))
	}
	col := red
	if text != "failed" {
		col = speedColor(v)
	}
	return c(col, padLeft(w, text))
}

func printHeader(family string, modes []string) {
	parts := []string{c(cyan, padLeft(12, family)), c(cyan, padLeft(8, "延迟"))}
	if contains(modes, "single") {
		parts = append(parts, c(cyan, padLeft(12, "单线程上传")), c(cyan, padLeft(12, "单线程下载")))
	}
	if contains(modes, "multi") {
		parts = append(parts, c(cyan, padLeft(12, "多线程上传")), c(cyan, padLeft(12, "多线程下载")))
	}
	fmt.Println("  " + strings.Join(parts, "  "))
}

func printRow(r ProbeResult, modes []string) {
	parts := []string{c(cyan, padLeft(12, r.Region)), padLeft(8, r.RTT)}
	if contains(modes, "single") {
		parts = append(parts, cell(r.SingleUp, r.SingleUpF, 12), cell(r.SingleDown, r.SingleDownF, 12))
	}
	if contains(modes, "multi") {
		parts = append(parts, cell(r.MultiUp, r.MultiUpF, 12), cell(r.MultiDown, r.MultiDownF, 12))
	}
	fmt.Println("  " + strings.Join(parts, "  "))
}

func normalizeProvince(s string) string {
	s = strings.TrimSpace(s)
	for _, suf := range []string{"特别行政区", "维吾尔自治区", "壮族自治区", "回族自治区", "自治区", "省", "市"} {
		s = strings.TrimSuffix(s, suf)
	}
	alias := map[string]string{"bj": "北京", "sh": "上海", "gd": "广东", "gz": "广东", "hb": "湖北", "js": "江苏"}
	if v, ok := alias[strings.ToLower(s)]; ok {
		return v
	}
	return s
}

func resolveLocation(name string) (prov, city string, err error) {
	loc := normalizeProvince(name)
	if loc == "" {
		return "", "", fmt.Errorf("空的测速点")
	}
	if v, ok := extraCity[loc]; ok {
		return v[0], v[1], nil
	}
	if v, ok := provinceCity[loc]; ok {
		return loc, v, nil
	}
	for p, cty := range provinceCity {
		if cty == loc {
			return p, cty, nil
		}
	}
	return "", "", fmt.Errorf("不支持的测速点: %s", name)
}

func targetsBSG() [][3]string {
	var jobs [][3]string
	for _, rc := range bsg {
		for _, isp := range isps {
			jobs = append(jobs, [3]string{rc[0], rc[1], isp})
		}
	}
	return jobs
}

func parsePoints(text string) ([][3]string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return targetsBSG(), nil
	}
	re := regexp.MustCompile(`[,，、;；]+`)
	parts := re.Split(text, -1)
	var jobs [][3]string
	for _, raw := range parts {
		part := strings.TrimSpace(raw)
		if part == "" {
			continue
		}
		var isp string
		for _, name := range isps {
			if strings.HasSuffix(part, name) {
				isp = name
				part = strings.TrimSpace(strings.TrimSuffix(part, name))
				break
			}
		}
		prov, city, err := resolveLocation(part)
		if err != nil {
			return nil, err
		}
		if isp != "" {
			jobs = append(jobs, [3]string{prov, city, isp})
		} else {
			for _, name := range isps {
				jobs = append(jobs, [3]string{prov, city, name})
			}
		}
	}
	if len(jobs) == 0 {
		return nil, fmt.Errorf("没有解析到测速点")
	}
	return jobs, nil
}

func isTTY() bool {
	st, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}

func prompt(r *bufio.Reader, msg, def string) string {
	fmt.Print(msg)
	line, _ := r.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}

func interactive(info ClientInfo) (jobs [][3]string, modes []string, err error) {
	r := bufio.NewReader(os.Stdin)
	v6 := hasIPv6Internet()
	loc := info.City + info.Oper
	if loc == info.Oper {
		loc = info.Province + info.Oper
	}
	fmt.Printf("%s  全球网测 Linux 客户端\n\n", c(bold+cyan, "TaierSpeedtest"))
	fmt.Printf("  出口    %s  %s\n", loc, info.IP)
	if v6 {
		fmt.Println("  IPv6    可通互联网，将加测 IPv6")
	} else {
		fmt.Println("  IPv6    不可用")
	}
	fmt.Println()
	fmt.Println("  测速点  回车 = 北上广省会 × 电信/联通/移动")
	fmt.Println("          可填省份、城市，多个用逗号分隔")
	fmt.Println("          例: 湖北")
	fmt.Println("          例: 北京,上海,浙江")
	fmt.Println("          例: 武汉电信,杭州联通")
	fmt.Println()
	raw := prompt(r, "  请输入测速点: ", "")
	jobs, err = parsePoints(raw)
	if err != nil {
		return nil, nil, err
	}
	fmt.Println()
	fmt.Println("  测速模式  回车 = 单线程 + 多线程对照")
	fmt.Println("    1) 单+多对照  (默认)")
	fmt.Println("    2) 只测单线程")
	fmt.Println("    3) 只测多线程")
	fmt.Println()
	ch := prompt(r, "  请选择 [1/2/3]: ", "1")
	switch ch {
	case "2":
		modes = []string{"single"}
	case "3":
		modes = []string{"multi"}
	default:
		modes = []string{"single", "multi"}
	}
	return jobs, modes, nil
}

func makeIMEI() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "TS" + strings.ToUpper(hex.EncodeToString(b))
}

func applyMode(r *ProbeResult, mode string, up, down phaseResult) {
	us, ds := fmtSpeed(up.written), fmtSpeed(down.written)
	if mode == "single" {
		r.SingleUp, r.SingleDown = us, ds
		r.SingleUpF, r.SingleDownF = up.written, down.written
		return
	}
	r.MultiUp, r.MultiDown = us, ds
	r.MultiUpF, r.MultiDownF = up.written, down.written
}

// upStaggerMS 上传线程错峰间隔：多会话模式下让 N 个独立会话先后建立，
// 摊薄同会话/同 IP 的 POST 请求密度（服务端限次对速率也敏感）。
func upStaggerMS() int {
	if v := os.Getenv("TAIER_UPLOAD_STAGGER_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return 500
}

// threadEnvInt 允许经环境变量覆盖 --down-threads/--up-threads（workflow 侧
// 不需要暴露新 flag；默认值不变，TAIER_UP_THREADS/TAIER_DOWN_THREADS 可调）。
func threadEnvInt(env string, fallback int) int {
	if v := os.Getenv(env); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}

func runOne(base, ip, prov, city, isp, imei string, lengthS, intervalMS int, modes []string, downTh, upTh int, ipv6 bool) ProbeResult {
	family := "IPv4"
	if ipv6 {
		family = "IPv6"
	}
	r := ProbeResult{Region: prov + isp, Family: family, RTT: "-", SingleUp: "-", SingleDown: "-", MultiUp: "-", MultiDown: "-"}
	servers, err := matchServers(base, ip, prov, city, isp, ipv6)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[debug-flow] matchServers: %v\n", err)
		return r
	}
	s := pickServer(servers, prov, city, isp)
	if s == nil {
		fmt.Fprintf(os.Stderr, "[debug-flow] pickServer: 候选 %d 个全部不可用\n", len(servers))
		return r
	}
	fmt.Fprintf(os.Stderr, "[debug-flow] server=%s:%d (%s)\n", s.HostIP, s.Port, s.HostName)
	uuid, err := enqueue(*s, imei, enqueueBandwidth())
	if err != nil {
		fmt.Fprintf(os.Stderr, "[debug-flow] enqueue: %v\n", err)
		return r
	}
	defer dequeue(*s, uuid)
	r.RTT = fmtMS(measureLatency(s.HostIP, s.Port))
	for _, mode := range modes {
		dth, uth := downTh, upTh
		if mode == "single" {
			dth, uth = 1, 1
		}
		down := runPhase(*s, uuid, true, dth, lengthS, intervalMS, 40)
		up := runPhase(*s, uuid, false, uth, lengthS, intervalMS, upStaggerMS())
		// 数据面已改为 http.Client（吃 HTTP_PROXY）：上传计数发生在 body 的 Read() 里，
		// 传输层按对端窗口读、链路堵住就停止读，读数即真实出口速率。
		fmt.Fprintf(os.Stderr, "[probe] mode=%s up=%.2fMbps down=%.2fMbps\n",
			mode, up.written, down.written)
		applyMode(&r, mode, up, down)
	}
	return r
}

var version = "dev"

func main() {
	bsgF := flag.Bool("bsg", false, "北上广三网")
	points := flag.String("points", "", "测速点，逗号分隔")
	province := flag.String("province", "", "指定省份")
	duration := flag.Int("duration", 5, "每方向秒数")
	interval := flag.Int("interval", 500, "采样间隔毫秒")
	downTh := flag.Int("down-threads", 8, "多线程下行连接数")
	upTh := flag.Int("up-threads", 4, "多线程上行连接数")
	modeF := flag.String("mode", "", "both|single|multi，交互时可不填")
	jsonF := flag.Bool("json", false, "JSON 输出")
	ipv6F := flag.Bool("ipv6", false, "强制 IPv6")
	noIPv6 := flag.Bool("no-ipv6", false, "不测 IPv6")
	noImage := flag.Bool("no-image", false, "不出图")
	flag.Parse()
	if *duration < 5 {
		*duration = 5
	}
	if *duration > 13 {
		*duration = 13
	}

	base, info, err := fetchClient()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s[X] %v%s\n", red, err, nc)
		os.Exit(1)
	}

	var jobs [][3]string
	var modes []string
	if *points != "" {
		jobs, err = parsePoints(*points)
		modes = []string{"single", "multi"}
	} else if *province != "" {
		jobs, err = parsePoints(*province)
		modes = []string{"single", "multi"}
	} else if *bsgF || !isTTY() {
		jobs = targetsBSG()
		modes = []string{"single", "multi"}
	} else {
		jobs, modes, err = interactive(info)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s[X] %v%s\n", red, err, nc)
		os.Exit(1)
	}
	if *modeF == "single" {
		modes = []string{"single"}
	} else if *modeF == "multi" {
		modes = []string{"multi"}
	} else if *modeF == "both" {
		modes = []string{"single", "multi"}
	}

	imei := makeIMEI()
	loc := info.City + info.Oper
	fmt.Println()
	fmt.Println(c(bold+cyan, "泰尔测速"))
	fmt.Printf("%s  出口 %s  %s%s\n", dim, info.IP, loc, nc)
	fmt.Println()

	wantV6 := !*noIPv6 && (*ipv6F || hasIPv6Internet())
	if !*jsonF {
		printHeader("IPv4", modes)
	}

	var rows []ProbeResult
	total := len(jobs)
	if wantV6 {
		total *= 2
	}
	total *= len(modes)
	done := 0

	runFamily := func(ipv6 bool, family string) {
		if !*jsonF && ipv6 {
			fmt.Println()
			printHeader(family, modes)
		}
		for _, j := range jobs {
			if !*jsonF {
				tag := ""
				if ipv6 {
					tag = "IPv6 "
				}
				fmt.Fprintf(os.Stderr, "\r%s  测速进度%s  %d/%d  %s%s%s   ", cyan, nc, done, total, tag, j[0], j[2])
			}
			row := runOne(base, info.IP, j[0], j[1], j[2], imei, *duration, *interval, modes,
				threadEnvInt("TAIER_DOWN_THREADS", *downTh), threadEnvInt("TAIER_UP_THREADS", *upTh), ipv6)
			rows = append(rows, row)
			done += len(modes)
			if !*jsonF {
				fmt.Fprint(os.Stderr, "\r\033[2K")
				printRow(row, modes)
			}
		}
	}

	runFamily(false, "IPv4")
	if wantV6 {
		runFamily(true, "IPv6")
	}

	if *jsonF {
		b, _ := json.MarshalIndent(rows, "", "  ")
		fmt.Println(string(b))
		return
	}
	fmt.Println()
	if *noImage {
		return
	}
	url, err := publishReport(info, rows, modes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s[!] 结果图失败: %v%s\n", yellow, err, nc)
		return
	}
	fmt.Printf("%s结果图%s  %s\n", cyan, nc, url)
}
