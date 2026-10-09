package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/net/proxy"
	"sync/atomic"
	"time"
)

func calcMbps(nbytes int64, durationMS float64) float64 {
	if durationMS <= 0 {
		return 0
	}
	return ((float64(nbytes) * 8) / 1000.0 / 1000.0) / (durationMS / 1000.0)
}

func icmpPingMS(ip string, count int) float64 {
	args := []string{"-n", "-c", strconv.Itoa(count), "-W", "1"}
	if strings.Contains(ip, ":") {
		args = append(args, "-6")
	}
	args = append(args, ip)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(count+2)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ping", args...)
	out, err := cmd.Output()
	if err != nil {
		return -1
	}
	var times []float64
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		line := sc.Text()
		i := strings.Index(line, "time=")
		if i < 0 {
			continue
		}
		rest := line[i+5:]
		rest = strings.Fields(rest)[0]
		v, e := strconv.ParseFloat(rest, 64)
		if e == nil {
			times = append(times, v)
		}
	}
	if len(times) == 0 {
		return -1
	}
	sum := 0.0
	for _, t := range times {
		sum += t
	}
	return sum / float64(len(times))
}

// usingProxy 判断当前是否走了显式 HTTP 代理。
//
// 走代理时 ICMP 测的是「runner → 测速服务器」的直连延迟，完全不经过代理节点，
// 会严重误导（看起来很快、实际经节点很慢），必须改用经代理的 HTTP 往返测延迟。
func usingProxy() bool {
	for _, k := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "TAIER_SOCKS5"} {
		if os.Getenv(k) != "" {
			return true
		}
	}
	return false
}

// httpTcpingMS 经 http.Client 测「到测速服务器」的往返延迟。
//
// 原实现是裸 socket 握手，在 TUN 下靠路由劫持才走代理；一旦弃用 TUN，
// 它就会退化成直连，测出来的不是经代理节点的真实延迟。
// 改用 http.Client 后走 HTTP_PROXY，经代理的隧道握手才是端到端真实 RTT。
func httpTcpingMS(ip string, port, count int) float64 {
	client := newHTTPClient(5*time.Second, false) // 延迟测量：每次新建连接，测的就是握手往返
	u := "http://" + hostPort(ip, port) + "/speed/"
	var samples []float64
	for i := 0; i < count+1; i++ {
		t0 := time.Now()
		req, err := http.NewRequest(http.MethodGet, u, nil)
		if err == nil {
			req.Header.Set("User-Agent", uaDalvik)
			if resp, err := client.Do(req); err == nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64))
				_ = resp.Body.Close()
				if resp.StatusCode < 500 {
					samples = append(samples, float64(time.Since(t0).Microseconds())/1000.0)
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(samples) == 0 {
		return -1
	}
	if len(samples) == 1 {
		return samples[0]
	}
	sum := 0.0
	// 丢弃第一次：含连接建立等一次性开销
	for _, v := range samples[1:] {
		sum += v
	}
	return sum / float64(len(samples)-1)
}

func measureLatency(ip string, port int) float64 {
	if !usingProxy() {
		if rtt := icmpPingMS(ip, 4); rtt > 0.1 {
			return rtt
		}
	}
	return httpTcpingMS(ip, port, 4)
}

func hasIPv6Internet() bool {
	d := net.Dialer{Timeout: 2 * time.Second}
	c, err := d.Dial("udp6", "[2001:4860:4860::8888]:53")
	if err != nil {
		c, err = d.Dial("tcp6", "[2400:3200::1]:53")
		if err != nil {
			return false
		}
	}
	_ = c.Close()
	return true
}

// newHTTPClient 构造一个「吃环境变量代理」的 HTTP 客户端。
//
// 这是弃用 TUN 的关键：数据面（上传/下载/延迟）原本是裸 socket 直连，
// 只能靠 mihomo TUN 靠路由劫持接管；改成标准 http.Client 后，
// HTTP_PROXY/HTTPS_PROXY 直接生效，不必再开 TUN。
//
// 为什么要弃 TUN：mihomo 的 TUN 用户态协议栈（gvisor）会贪婪收包并本地回 ACK，
// 客户端 Write() 几乎不阻塞 —— 上行读数实测虚高 756 倍（见 ampdemo 验证）。
// 走 HTTP_PROXY 时 mihomo 是标准 TCP accept：转不动就不读，
// TCP 窗口关闭、反压直接传导回客户端，写多快取决于真实出口速率。
// keepAlive=true 时复用连接（循环上传用）：避免每个请求都重新 TCP 握手。
func newHTTPClient(timeout time.Duration, keepAlive bool) *http.Client {
	tr := &http.Transport{
		Proxy:             http.ProxyFromEnvironment,
		ForceAttemptHTTP2: false, // 测速要稳定单流行为，避免 HTTP/2 多路复用干扰
		// 循环上传必须复用：每次新建连接会为每 1MB 请求付出一次完整握手（经代理 ~400ms），
		// 把上行锁死在「每秒 1 个 1MB 请求」= 8.39Mbps 的量子化台阶上。
		DisableKeepAlives: !keepAlive,
		// 关键：Go http.Client 会自动附加 "Accept-Encoding: gzip"，泰尔 WAF 实测对此回 403
		// （原版裸 socket 下载从不发此头；归因探针 proxied/direct 双 403 同页佐证）。
		DisableCompression:  true,
		TLSHandshakeTimeout: 10 * time.Second,
		DialContext: (&net.Dialer{
			Timeout:   8 * time.Second,
			KeepAlive: 0,
		}).DialContext,
	}
	// SOCKS5 隧道优先（TAIER_SOCKS5=host:port，mihomo mixed-port 同端口支持）。
	// 为什么不用 HTTP_PROXY 发数据面：HTTP 代理模式下 Go 发 absolute-form 请求行
	// （GET http://ip:port/path HTTP/1.1），泰尔服务端实测回 403；
	// SOCKS5 隧道里客户端以 origin-form（GET /path）直连目标，与 TUN 语义完全一致。
	if socks := os.Getenv("TAIER_SOCKS5"); socks != "" {
		if d, err := proxy.SOCKS5("tcp", socks, nil, proxy.Direct); err == nil {
			tr.Proxy = nil
			if cd, ok := d.(proxy.ContextDialer); ok {
				tr.DialContext = cd.DialContext
			} else {
				tr.Dial = d.Dial
			}
		}
	}
	return &http.Client{Timeout: timeout, Transport: tr}
}

type byteCounter struct {
	n atomic.Int64
}

func newByteCounter(_ int) *byteCounter { return &byteCounter{} }

func (c *byteCounter) add(n int)   { c.n.Add(int64(n)) }
func (c *byteCounter) snap() int64 { return c.n.Load() }

// uploadBody 作为 HTTP 请求体喂给 http.NewRequest。
//
// 计数发生在 Read() 里 —— Go 传输层按「对端 TCP 窗口实际能吞多少」来读 body，
// 链路堵住就不再读，计数自然停在真实进度上（反压直接传导，无本机缓冲放大）。
// 这比裸 socket 的 Write() 盲写准得多：Write() 返回只代表数据进了本机发送缓冲。
// uploadBody 作为 HTTP 请求体喂给 http.NewRequest。
//
// 计数发生在 Read() 里 —— 走标准反压代理（mihomo HTTP/SOCKS5）时，
// 传输层按「对端窗口实际能吞多少」来读 body，计数即真实出口速率。
//
// 两种模式（finite 区分，绝不能混）：
//
//	finite=false：上游谎言模式（Content-Length 900MB，靠 stop 结束）。
//	  实测经代理会被服务端 RST —— 保留只为对照，默认不用。
//	finite=true ：真实长度模式，发满即 EOF。⚠️ 曾经的爆表元凶：
//	  remaining==0 既是「未初始化的无限模式」又是「已发完」——
//	  两个语义撞在同一个值上，发完后 Read 掉进无限分支狂计数（实测 72万 Mbps）。
//	  现用 exhausted 粘性标志彻底分离两个语义。
type uploadBody struct {
	counter   *byteCounter
	prefix    []byte
	payload   []byte
	stop      <-chan struct{}
	finite    bool
	remaining int64
	exhausted bool
}

func (b *uploadBody) Read(p []byte) (int, error) {
	if b.exhausted {
		return 0, io.EOF
	}
	if !b.finite {
		select {
		case <-b.stop:
			return 0, io.EOF
		default:
		}
		n := copy(p, b.payload)
		b.counter.add(n)
		return n, nil
	}
	if len(b.prefix) > 0 {
		n := copy(p, b.prefix)
		if int64(n) > b.remaining {
			n = int(b.remaining)
		}
		b.prefix = b.prefix[n:]
		b.remaining -= int64(n)
		b.counter.add(n)
		if b.remaining == 0 {
			b.exhausted = true
		}
		return n, nil
	}
	n := copy(p, b.payload)
	if int64(n) > b.remaining {
		n = int(b.remaining)
	}
	b.remaining -= int64(n)
	b.counter.add(n)
	if b.remaining == 0 {
		b.exhausted = true
	}
	return n, nil
}

// redactURL 打印用：抹掉 query（key=uuid 是 dovalid 下发的会话凭据，
// jarvanh/actions 是公开仓库，日志不得泄漏 —— 与上游脚本「公开仓库日志勿泄漏」同规）。
func redactURL(u *url.URL) string {
	c := *u
	if c.RawQuery != "" {
		c.RawQuery = "key=***&r=…"
	}
	return c.String()
}

func downloadWorker(ctx context.Context, s Server, uuid string, counter *byteCounter) {
	addr := hostPort(s.HostIP, s.Port)
	path := fmt.Sprintf("/speed/File(1G).dl?r=%d&key=%s", time.Now().Unix(), uuid)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+path, nil)
	if err != nil {
		return
	}
	req.Header.Set("User-Agent", uaBrowser)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Connection", "close")

	// 走 http.Client 而非裸 socket：HTTP_PROXY 生效，不再需要 TUN 接管。
	resp, err := newHTTPClient(0, false).Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[debug-dl] do: %v\n", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		// 归因实验：同一 URL、同一 uuid、同一时刻，绕过一切代理直连重放一次。
		// proxied 403 + direct 200 → 节点出口 IP 被服务端封；
		// 两者都 403 → 服务端侧限制（时段窗口 / key 绑定），与代理实现无关。
		dreq := req.Clone(context.Background())
		directTr := &http.Transport{DisableKeepAlives: true}
		dc, err2 := (&http.Client{Timeout: 15 * time.Second, Transport: directTr}).Do(dreq)
		if err2 == nil {
			b2, _ := io.ReadAll(io.LimitReader(dc.Body, 200))
			fmt.Fprintf(os.Stderr, "[debug-dl] proxied=%d body=%q | direct-replay=%d body=%q | url=%s\n",
				resp.StatusCode, string(b), dc.StatusCode, string(b2), redactURL(req.URL))
			dc.Body.Close()
		} else {
			fmt.Fprintf(os.Stderr, "[debug-dl] proxied=%d body=%q | direct-replay err=%v | url=%s\n",
				resp.StatusCode, string(b), err2, redactURL(req.URL))
		}
		return
	}
	// 计数点是真实收到的数据：resp.Body 读到才算，与链路真实速率一致。
	buf := make([]byte, 65536)
	counted := false
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		n, err := resp.Body.Read(buf)
		if n > 0 {
			counter.add(n)
			counted = true
		}
		if err != nil {
			if !counted {
				fmt.Fprintf(os.Stderr, "[debug-dl] read-before-data: %v\n", err)
			}
			return
		}
	}
}

// uploadWorker 上行测速：走 http.Client（吃 TAIER_SOCKS5 / HTTP_PROXY）。
//
// finite 模式下**循环上传**：单请求发满即结束（服务端 200 = 真实收到），
// 立即再发下一个直到窗口结束 —— ztelliot 验证过的正确模式。
// 字节数由 ContentLength 严格界定、服务端逐个确认，杜绝计数爆炸；
// 也顺带解决「单请求发完后窗口内空转、median 被 0 采样拉爆」的问题。
func uploadWorker(ctx context.Context, s Server, uuid string, counter *byteCounter) {
	addr := hostPort(s.HostIP, s.Port)
	fn := time.Now().Format("SPEED_20060102_150405.000")
	payload := make([]byte, 16384)
	_, _ = rand.Read(payload)

	upLen := int64(0)
	if v := os.Getenv("TAIER_UPLOAD_LEN"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			upLen = n
		}
	}
	finite := upLen > 0
	contentLen := upLen
	if !finite {
		contentLen = 900000000 // 上游谎言模式（仅对照；经代理会被 RST）
	}

	// ⚠️ 循环上传**共享同一个 client**：连接复用生效的前提是同一个 Transport/连接池。
	// 若每个请求新建 client，即便 DisableKeepAlives=false 也各自建池、照样重新握手 ——
	// 上行仍会被锁死在「每秒 1 个 1MB 请求」= 8.39Mbps 的量子化台阶上。
	upClient := newHTTPClient(0, true)

	oneShot := func(idx int) bool {
		body := &uploadBody{
			counter: counter,
			prefix: []byte(fmt.Sprintf(
				"--%s\r\nContent-Disposition: form-data; name=\"upload\";filename=\"%s.%d\"\r\n\r\n",
				boundary, fn, idx)),
			payload:   payload,
			stop:      ctx.Done(),
			finite:    finite,
			remaining: upLen,
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			"http://"+addr+"/speed/doAnalsLoad.do", body)
		if err != nil {
			return false
		}
		req.Header.Set("User-Agent", uaUpload)
		req.Header.Set("Charset", "UTF-8")
		req.Header.Set("Key", uuid)
		req.Header.Set("Cache-Control", "no-cache")
		req.Header.Set("Content-Type", "multipart/form-data;boundary="+boundary)
		req.ContentLength = contentLen

		resp, err := upClient.Do(req)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[debug-ul] #%d do: %v\n", idx, err)
			return false
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		fmt.Fprintf(os.Stderr, "[debug-ul] #%d status=%d body=%q\n", idx, resp.StatusCode, string(b))
		return resp.StatusCode >= 200 && resp.StatusCode < 400
	}

	// ── 持续供给模式（默认，与下载同构 = 测真实带宽）──
	// 不设 TAIER_UPLOAD_LEN 即走这里：单个请求撑满整个测速窗口，
	// body 持续产数据直到 ctx 结束。计数点在 Read()，而 Read() 会被
	// TCP 反压卡住 —— 计数由真实流量驱动，不再依赖「包完成」事件。
	// 这与下载 downloadWorker 的 resp.Body.Read() 完全同源，故能测真实带宽。
	//
	// 实测判据（可控带宽 SOCKS5 限速代理对照）：
	//   定长循环包  限速30→67.11  限速60→67.11  ← 两次完全相同，不跟随带宽
	//               67.11 = 7.999 阶（整阶 = 仍在数包）
	//   持续供给    限速30→23.20  限速60→37.49  ← 上升 1.62x
	//               代理真值同步 8.914→14.653 (1.64x)，几乎完全吻合 ✓
	//
	// ⚠️ 曾试过 chunked（不声明长度）连续流，服务端不响应
	//    （实测每连接仅走 149 字节后 8 秒超时），故仍用声明长度的形态。
	if !finite {
		if oneShot(1) {
			return
		}
		// ⚠️ ctx 已取消 = 测速窗口正常结束（Do 返回 context canceled），不是失败。
		// 原实现把它当「持续供给失败」误报（2026-10-09 对照实验实锤：成功测出
		// 125Mbps 的同时 stderr 照打「持续供给失败，放弃该节点上行」），随后的重试
		// 也只会立刻再次 canceled。窗口结束直接返回，计数已进采样，读数不受影响。
		if ctx.Err() != nil {
			return
		}
		// 偶发抖动重试一次（仍是持续供给，不产生假值）。
		time.Sleep(100 * time.Millisecond)
		if oneShot(2) {
			return
		}
		// 持续供给彻底失败：直接放弃该节点上行（2026-10-08 定案：移除回退保险）。
		// 原保险：失败回退 1MB 定长循环（历史背景：「900MB 谎言经代理被 RST」时
		// 宁可拿台阶值也不能没有值）—— 但定长循环读数是「每包一跳」的数包台阶值，
		// 不是真实带宽，回流订阅会拿假速度误导选路。宁缺毋假：失败就不给值。
		fmt.Fprintf(os.Stderr, "[debug-ul] 持续供给失败，放弃该节点上行（回退保险已移除）\n")
		uploadAttributionProbe(ctx, s, uuid)
		return
	}
	// ── 定长循环模式（仅回退：显式设了 TAIER_UPLOAD_LEN 才走）──
	// ⚠️ 这条路测不出真实带宽 —— 计数是「每包一跳」，节拍由包完成事件定：
	//    小包被 socket 缓冲 + mihomo 缓冲瞬间吞下，Read() 立刻跑完，
	//    然后干等服务端 200（≈RTT）期间计数完全停摆。
	//    实测读数恒为 8.39 的整数倍（0/8.39/12.58/16.78/…/67.11/83.89），
	//    这与「下载值是连续的」形成鲜明对比 —— 是数包而非测速。
	//    仅当持续供给模式在真实代理链路上出现大面积 RST 时才回退到此。
	for i := 1; ; i++ {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if !oneShot(i) {
			time.Sleep(100 * time.Millisecond) // 失败退避，防 tight-loop
		}
	}
}

// uploadAttributionProbe 在持续供给彻底失败后定位「断在哪一跳」，只打日志不改测速。
//
// 背景（2026-10-09 三方对照实验定案）：机场节点链路上传 POST 100% 秒断 RST，
// 而 runner 直连（S1）与 runner 经 mihomo DIRECT（S2）全部成功 —— 断点在节点隧道内。
// 但 [debug-ul] 只能看到 loopback 视角的 write RST，无法区分：
//
//	节点对 doAnalsLoad.do 一律断 / 节点只限大 Content-Length / 节点隧道本身已死
//
// 三个探测各 ≤4s、用独立 context（不随窗口取消）与独立 dummy counter（不污染读数）：
//
//	small-proxied : 经 TAIER_SOCKS5/HTTP_PROXY 发 1KB 定长 POST —— RST = 节点对该端点
//	                一律断；任何 HTTP 响应（含 4xx）= 链路通，断的是大包/持续流
//	small-direct  : 绕过一切代理直连重放 —— 对照（key 可能因绑定节点出口 IP 而 4xx，
//	                网络层通断仍是有效信号）
//	tunnel-get    : 经代理 GET /speed/ —— 区分「节点隧道已死」与「对 POST 挑食」
func uploadAttributionProbe(ctx context.Context, s Server, uuid string) {
	pctx, cancel := context.WithTimeout(context.Background(), 16*time.Second)
	defer cancel()
	addr := hostPort(s.HostIP, s.Port)
	dummy := newByteCounter(1)
	runSmall := func(label string, client *http.Client) {
		body := &uploadBody{
			counter:   dummy,
			prefix:    []byte(fmt.Sprintf("--%s\r\nContent-Disposition: form-data; name=\"upload\";filename=\"attr.%s\"\r\n\r\n", boundary, label)),
			payload:   make([]byte, 4096),
			stop:      pctx.Done(),
			finite:    true,
			remaining: 1024,
		}
		req, err := http.NewRequestWithContext(pctx, http.MethodPost, "http://"+addr+"/speed/doAnalsLoad.do", body)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[debug-ul-attr] %s newreq: %v\n", label, err)
			return
		}
		req.Header.Set("User-Agent", uaUpload)
		req.Header.Set("Charset", "UTF-8")
		req.Header.Set("Key", uuid)
		req.Header.Set("Cache-Control", "no-cache")
		req.Header.Set("Content-Type", "multipart/form-data;boundary="+boundary)
		req.ContentLength = 1024
		resp, err := client.Do(req)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[debug-ul-attr] %s err=%v\n", label, err)
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 80))
		fmt.Fprintf(os.Stderr, "[debug-ul-attr] %s status=%d body=%q\n", label, resp.StatusCode, string(b))
	}
	runSmall("small-proxied", newHTTPClient(4*time.Second, false))
	runSmall("small-direct", &http.Client{Timeout: 4 * time.Second,
		Transport: &http.Transport{DisableKeepAlives: true}})
	if req, err := http.NewRequestWithContext(pctx, http.MethodGet, "http://"+addr+"/speed/", nil); err == nil {
		req.Header.Set("User-Agent", uaDalvik)
		if resp, err := newHTTPClient(4*time.Second, false).Do(req); err == nil {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 80))
			fmt.Fprintf(os.Stderr, "[debug-ul-attr] tunnel-get status=%d body=%q\n", resp.StatusCode, string(b))
			resp.Body.Close()
		} else {
			fmt.Fprintf(os.Stderr, "[debug-ul-attr] tunnel-get err=%v\n", err)
		}
	}
}

// median 取采样中位数。
// 原实现 avgTop3 是「排序后取最大的 3 个求平均」——只挑峰值，
// 缓冲造成的瞬时突发必定被选中并当成稳态速率，是读数虚高的放大器。
// 中位数对孤立尖峰不敏感，能反映窗口内的典型速率。
func median(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	cp := append([]float64(nil), vals...)
	sort.Float64s(cp)
	n := len(cp)
	if n%2 == 1 {
		return cp[n/2]
	}
	return (cp[n/2-1] + cp[n/2]) / 2
}

type phaseResult struct {
	written float64
}

func runPhase(s Server, uuid string, down bool, threads, lengthS, intervalMS int) phaseResult {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	counter := newByteCounter(threads)
	var wg sync.WaitGroup
	for i := 0; i < threads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if down {
				downloadWorker(ctx, s, uuid, counter)
			} else {
				uploadWorker(ctx, s, uuid, counter)
			}
		}()
		time.Sleep(40 * time.Millisecond)
	}
	points := (lengthS * 1000) / intervalMS
	if points < 1 {
		points = 1
	}
	preheat := 2000 / intervalMS
	if preheat < 1 {
		preheat = 1
	}
	var lastDelta, lastTotal int64
	var samples []float64
	ticker := time.NewTicker(time.Duration(intervalMS) * time.Millisecond)
	defer ticker.Stop()
	for index := 1; index <= points+preheat; index++ {
		<-ticker.C
		total := counter.snap()
		delta := total - lastTotal
		if delta < 0 {
			delta = 0
		}
		lastTotal = total
		var speed float64
		if lastDelta > 0 {
			speed = calcMbps(lastDelta+delta, float64(intervalMS*2))
		} else {
			speed = calcMbps(delta, float64(intervalMS))
		}
		lastDelta = delta
		if index > preheat {
			samples = append(samples, speed)
		}
	}
	cancel()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(1 * time.Second):
	}
	return phaseResult{written: median(samples)}
}
