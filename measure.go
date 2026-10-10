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
func (c *byteCounter) sub(n int)   { c.n.Add(-int64(n)) }
func (c *byteCounter) snap() int64 { return c.n.Load() }

// uploadBody 作为 HTTP 请求体喂给 http.NewRequest。
//
// 计数发生在 Read() 里 —— Go 传输层按「对端 TCP 窗口实际能吞多少」来读 body，
// 链路堵住就不再读，计数自然停在真实进度上（反压直接传导，无本机缓冲放大）。
// 这比裸 socket 的 Write() 盲写准得多：Write() 返回只代表数据进了本机发送缓冲。
//
// 两种模式（finite 区分，绝不能混）：
//
//	finite=false：900MB 谎言持续供给（TAIER_UPLOAD_SUSTAINED=1 的对照模式）。
//	  读数经机场节点链路 100% 秒断 RST —— 节点网关大 POST 阈值 16MB~128MB
//	  （2026-10-10 归因探针二分定案，与 CF 免费版 100MB 兼容）；直连正常
//	  （S1/S2/沙箱实测 125/118/156Mbps），故仅作对照、默认不用。
//	finite=true ：真实长度模式（默认 8MB/请求，发满即 EOF）。⚠️ 曾经的爆表元凶：
//	  remaining==0 既是「未初始化的无限模式」又是「已发完」——
//	  两个语义撞在同一个值上，发完后 Read 掉进无限分支狂计数（实测 72万 Mbps）。
//	  现用 exhausted 粘性标志彻底分离两个语义。
//
// ⚠️ 计数口径（2026-10-10，v1.0.4-jh.12）：Read 计的是「交给传输层」的字节，
// 半途断连时其中一部分只到了本机/代理缓冲、服务端并没收到 —— 旧实现失败后
// 「已计数字节不撤销」，这是上行读数仅存的高估源。现改为**确认对账**：
// 每个 finite 请求记 ownSent，请求失败（非 2xx/3xx / Do 出错）时把 ownSent
// 从共享 counter 精确扣回（sub），只留服务端确认过的字节。窗口正常结束时
// 在途请求不扣（那部分反压驱动的字节是真实流出的，见 oneShot 内注释）。
type uploadBody struct {
	counter   *byteCounter
	prefix    []byte
	payload   []byte
	stop      <-chan struct{}
	finite    bool
	remaining int64
	exhausted bool
	// ownSent：本请求从 Read 侧计入共享 counter 的字节数（对账用）。
	// ⚠️ 必须是原子类型：Read 由 transport 的写协程调用，而服务端**提前响应**
	// （如 403 早于 body 写完返回）时 oneShot 在 Do 返回后即刻读它——两边无
	// happens-before，-race 实测会报竞争。
	// 持续供给模式（finite=false）不记账——那是对照模式，读数语义本就不同。
	ownSent atomic.Int64
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
		b.ownSent.Add(int64(n))
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
	b.ownSent.Add(int64(n))
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
// enqueueBandwidth 返回 dovalid 会话的 bandwidth 声明值（参与 token 计算）。
// 实测（2026-10-10）：单连接 8MiB/s 限速档来自**机场节点链路**（直连无此档），
// 与该参数无映射关系；保留可调仅作排查开关。
func enqueueBandwidth() int {
	if v := os.Getenv("TAIER_ENQUEUE_BANDWIDTH"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 200
}

// uploadWorker(ctx, s, uuid, counter)：uuid 为共享会话；多会话模式（默认开）下
// 本连接会**独立 enqueue** 换成自己的 uuid —— 服务端对同一会话的并发 POST 有限次
// 拒绝（multi 4 并发时 up=0 率 43%），每连接独立会话让服务端看到 N 个温和的独立
// 用户，配合 runPhase 的错峰启动，为加连接数铺路。
func uploadWorker(ctx context.Context, s Server, uuid string, counter *byteCounter) {
	if os.Getenv("TAIER_UPLOAD_MULTI_SESSION") != "0" {
		if u2, err := enqueue(s, makeIMEI(), enqueueBandwidth()); err == nil {
			uuid = u2
			defer dequeue(s, u2)
		} else {
			fmt.Fprintf(os.Stderr, "[debug-ul] multi-session enqueue failed, fallback shared: %v\n", err)
		}
	}
	addr := hostPort(s.HostIP, s.Port)
	fn := time.Now().Format("SPEED_20060102_150405.000")
	payload := make([]byte, 16384)
	_, _ = rand.Read(payload)

	// 声明值决策（2026-10-10 归因探针二分实测，22 节点无一例外）：
	//   1KB/64KB/1MB/16MB 声明 → 全部有 HTTP 响应（200/400/403），零 RST；
	//   128MB 声明 → 22/22 write RST 秒断 —— 节点链路网关对大 POST 的阈值在
	//   16MB~128MB（与 Cloudflare 免费版 100MB 兼容；行为是直接断连而非 413）。
	//   而 900MB 谎言持续供给（原默认）在机场节点上 100% 秒断 ⇒ 上行恒 0。
	// 故默认改为 **8MB 有限长度循环**（阈值下留 2 倍余量）：
	//   - 单请求 8MB：@1MB/s 慢节点 ≈8s > 窗口剩余，等效撑满窗口，无台阶问题；
	//     快节点 1~3 个请求/窗口，RTT 停摆占比 ≤20%，中位数读数接近真实。
	//   - 服务端逐个确认（200 = 真实收到），计数与服务端确认挂钩，宁真勿假。
	// 旧行为保留为对照模式：TAIER_UPLOAD_SUSTAINED=1 → 900MB 谎言持续供给；
	// TAIER_UPLOAD_LEN=<bytes> → 人工定长循环（原语义不变）。
	finite := true
	contentLen := int64(8 << 20)
	if v := os.Getenv("TAIER_UPLOAD_LEN"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			contentLen = n
		}
	}
	switch os.Getenv("TAIER_UPLOAD_SUSTAINED") {
	case "1", "true", "yes", "on":
		finite = false
		contentLen = 900000000
	}

	// ⚠️ 循环上传**共享同一个 client**：连接复用生效的前提是同一个 Transport/连接池。
	// 若每个请求新建 client，即便 DisableKeepAlives=false 也各自建池、照样重新握手 ——
	// 上行仍会被锁死在「每秒 1 个 1MB 请求」= 8.39Mbps 的量子化台阶上。
	upClient := newHTTPClient(0, true)

	// 确认台账（v1.0.4-jh.12）：本连接「交给传输层 vs 服务端确认」的对账。
	// sent=Read 侧计入共享 counter 的字节；confirmed=服务端 2xx/3xx 确认请求的字节；
	// clawback=失败请求扣回的字节。窗口结束打一行 [debug-ul-ack] ——
	// ratio 长期 ≈100% 说明读数全部由服务端确认支撑，诚实度可审计。
	// 持续供给模式（finite=false）不记账，ownSent 恒 0，台账自然不触发。
	var sentTotal, confirmedTotal, clawedTotal int64

	oneShot := func(idx int) bool {
		body := &uploadBody{
			counter: counter,
			prefix: []byte(fmt.Sprintf(
				"--%s\r\nContent-Disposition: form-data; name=\"upload\";filename=\"%s.%d\"\r\n\r\n",
				boundary, fn, idx)),
			payload:   payload,
			stop:      ctx.Done(),
			finite:    finite,
			remaining: contentLen,
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
			own := body.ownSent.Load()
			fmt.Fprintf(os.Stderr, "[debug-ul] #%d do: %v sent=%d\n", idx, err, own)
			// ctx 已取消 = 测速窗口正常结束：在途字节由 TCP 反压驱动、真实流出本机，
			// 不扣回也不计入确认（它们没有服务端确认，但不属于「造假」）。
			// ctx 还活着 = 真失败：ownSent 是「服务端没确认收到」的字节，精确扣回
			// （确认对账口径：只留确认过的，宁缺毋假）。
			if ctx.Err() == nil && own > 0 {
				counter.sub(int(own))
				clawedTotal += own
			}
			return false
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		own := body.ownSent.Load()
		fmt.Fprintf(os.Stderr, "[debug-ul] #%d status=%d body=%q sent=%d\n",
			idx, resp.StatusCode, string(b), own)
		sentTotal += own
		if resp.StatusCode >= 200 && resp.StatusCode < 400 {
			confirmedTotal += own
			return true
		}
		// 4xx/5xx：服务端明确拒收 —— 读到的字节同样扣回。
		if own > 0 {
			counter.sub(int(own))
			clawedTotal += own
		}
		return false
	}

	// ── 900MB 谎言持续供给（对照模式：TAIER_UPLOAD_SUSTAINED=1 才走）──
	// 单请求撑满整个测速窗口，body 持续产数据直到 ctx 结束。计数点在 Read()，
	// 由 TCP 反压驱动，与下载同源 —— 直连（S1/S2、沙箱 A/B）下读数真实。
	// ⚠️ 但经机场节点 100% 秒断 RST（阈值探针已定案），线上默认已弃用。
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
	// ── 有限长度循环模式（默认路径：8MB/请求，TAIER_UPLOAD_LEN 可覆盖）──
	// 单请求发满 8MB 即 EOF、服务端确认，立即再发下一个直到窗口结束。
	// 为什么不再是"数包台阶"：旧 1MB 小包循环的台阶（8.39Mbps 量子化）根源是
	// 「包完成事件节拍 + 等确认期间计数停摆」占窗口比例过大；8MB 请求在慢节点
	// 等效撑满窗口（传输 >> RTT），快节点停摆占比也 ≤20%，中位数读数接近真实。
	// 失败（服务端 4xx/连接断）退避重试：v1.0.4-jh.12 起失败请求的已计字节**精确
	// 扣回**（确认对账，见 oneShot 内注释），窗口内成功请求的真实速率照常进采样。
	// runPhase 对负 delta 钳 0：扣回表现为该采样点 0 速，不会被当成负尖峰。
	defer func() {
		if sentTotal > 0 || clawedTotal > 0 {
			ratio := 100.0
			if sentTotal > 0 {
				ratio = float64(confirmedTotal) * 100.0 / float64(sentTotal)
			}
			fmt.Fprintf(os.Stderr, "[debug-ul-ack] sent=%d confirmed=%d clawed=%d ratio=%.1f%%\n",
				sentTotal, confirmedTotal, clawedTotal, ratio)
		}
	}()
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
// 三个固定探测 + 声明值二分（64KB/1MB/16MB/128MB），各 ≤4s、独立 context（不随窗口
// 取消）与独立 dummy counter（不污染读数）：
//
//	small-proxied      : 经 TAIER_SOCKS5/HTTP_PROXY 发 1KB 定长 POST —— RST = 节点对该端点
//	                     一律断；任何 HTTP 响应（含 4xx）= 链路通
//	len{64KB,1MB,16MB,128MB}-proxied : 递增声明值定位「大上传被断」的阈值 ——
//	                     RST = 该声明值被断；HTTP 响应 = 放行；写不完超时 = 放行（声明被接受）
//	small-direct       : 绕过一切代理直连重放 —— 对照（key 可能因绑定节点出口 IP 而 4xx，
//	                     网络层通断仍是有效信号）
//	tunnel-get         : 经代理 GET /speed/ —— 区分「节点隧道已死」与「对 POST 挑食」
func uploadAttributionProbe(ctx context.Context, s Server, uuid string) {
	pctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	addr := hostPort(s.HostIP, s.Port)
	dummy := newByteCounter(1)
	runSmall := func(label string, client *http.Client, contentLen int64) {
		body := &uploadBody{
			counter:   dummy,
			prefix:    []byte(fmt.Sprintf("--%s\r\nContent-Disposition: form-data; name=\"upload\";filename=\"attr.%s\"\r\n\r\n", boundary, label)),
			payload:   make([]byte, 4096),
			stop:      pctx.Done(),
			finite:    true,
			remaining: contentLen,
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
		req.ContentLength = contentLen
		resp, err := client.Do(req)
		if err != nil {
			// RST/broken pipe = 该声明值被断；timeout = 声明被接受、还在慢写 = 放行
			fmt.Fprintf(os.Stderr, "[debug-ul-attr] %s err=%v\n", label, err)
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 80))
		fmt.Fprintf(os.Stderr, "[debug-ul-attr] %s status=%d body=%q\n", label, resp.StatusCode, string(b))
	}
	// 声明值二分：small(1KB) 全通、900MB 全断（2026-10-09 实测），中间档定位阈值。
	// 判定：RST=断；HTTP 响应（含 4xx）=放行；写不完超时=放行（声明被接受）。
	runSmall("small-proxied", newHTTPClient(4*time.Second, false), 1024)
	runSmall("len64KB-proxied", newHTTPClient(4*time.Second, false), 64*1024)
	runSmall("len1MB-proxied", newHTTPClient(4*time.Second, false), 1<<20)
	runSmall("len16MB-proxied", newHTTPClient(4*time.Second, false), 16<<20)
	runSmall("len128MB-proxied", newHTTPClient(4*time.Second, false), 128<<20)
	runSmall("small-direct", &http.Client{Timeout: 4 * time.Second,
		Transport: &http.Transport{DisableKeepAlives: true}}, 1024)
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

func runPhase(s Server, uuid string, down bool, threads, lengthS, intervalMS, staggerMS int) phaseResult {
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
		time.Sleep(time.Duration(staggerMS) * time.Millisecond)
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
