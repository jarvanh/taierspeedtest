package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
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

func httpTcpingMS(ip string, port, count int) float64 {
	req := []byte(fmt.Sprintf("GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", hostPort(ip, port)))
	var samples []float64
	for i := 0; i < count+1; i++ {
		t0 := time.Now()
		c, err := net.DialTimeout("tcp", hostPort(ip, port), 2*time.Second)
		if err != nil {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		_ = c.SetDeadline(time.Now().Add(2 * time.Second))
		_, err = c.Write(req)
		if err != nil {
			_ = c.Close()
			continue
		}
		buf := make([]byte, 64)
		n, err := c.Read(buf)
		_ = c.Close()
		if err == nil && n > 0 {
			samples = append(samples, float64(time.Since(t0).Microseconds())/1000.0)
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
	for _, v := range samples[1:] {
		sum += v
	}
	return sum / float64(len(samples)-1)
}

func measureLatency(ip string, port int) float64 {
	if rtt := icmpPingMS(ip, 4); rtt > 0.1 {
		return rtt
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

type byteCounter struct {
	n atomic.Int64
}

func (c *byteCounter) add(n int) { c.n.Add(int64(n)) }
func (c *byteCounter) snap() int64 { return c.n.Load() }

func parseHTTPHeader(buf []byte) (code int, bodyOff int, ok bool) {
	idx := strings.Index(string(buf), "\r\n\r\n")
	if idx < 0 {
		return 0, 0, false
	}
	first := strings.SplitN(string(buf[:idx]), "\r\n", 2)[0]
	if !strings.HasPrefix(first, "HTTP/1.") {
		return 0, 0, false
	}
	fs := strings.Fields(first)
	if len(fs) < 2 {
		return 0, 0, false
	}
	fmt.Sscanf(fs[1], "%d", &code)
	return code, idx + 4, true
}

func downloadWorker(ctx context.Context, s Server, uuid string, counter *byteCounter) {
	addr := hostPort(s.HostIP, s.Port)
	path := fmt.Sprintf("/speed/File(1G).dl?r=%d&key=%s", time.Now().Unix(), uuid)
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nAccept: */*\r\nConnection: close\r\nUser-Agent: %s\r\nHost:%s\r\n\r\n",
		path, uaBrowser, addr)
	c, err := net.DialTimeout("tcp", addr, 8*time.Second)
	if err != nil {
		return
	}
	defer c.Close()
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	if _, err := c.Write([]byte(req)); err != nil {
		return
	}
	buf := make([]byte, 0, 8192)
	tmp := make([]byte, 65536)
	headerDone := false
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
		n, err := c.Read(tmp)
		if n > 0 {
			if !headerDone {
				buf = append(buf, tmp[:n]...)
				code, off, ok := parseHTTPHeader(buf)
				if !ok {
					if err != nil {
						return
					}
					continue
				}
				if code < 200 || code >= 400 {
					return
				}
				if len(buf) > off {
					counter.add(len(buf) - off)
				}
				headerDone = true
				buf = nil
			} else {
				counter.add(n)
			}
		}
		if err != nil {
			return
		}
	}
}

func uploadWorker(ctx context.Context, s Server, uuid string, counter *byteCounter) {
	addr := hostPort(s.HostIP, s.Port)
	fn := time.Now().Format("SPEED_20060102_150405.000")
	header := fmt.Sprintf(
		"POST /speed/doAnalsLoad.do HTTP/1.1\r\nConnection: close\r\nCache-Control: no-cache\r\nCharset: UTF-8\r\nKey: %s\r\nContent-Type: multipart/form-data;boundary=%s\r\nUser-Agent: %s\r\nHost: %s\r\nAccept-Encoding: gzip\r\nContent-Length: 900000000\r\n\r\n--%s\r\nContent-Disposition: form-data; name=\"upload\";filename=\"%s\"\r\n\r\n",
		uuid, boundary, uaUpload, addr, boundary, fn,
	)
	payload := make([]byte, 16384)
	_, _ = rand.Read(payload)
	c, err := net.DialTimeout("tcp", addr, 8*time.Second)
	if err != nil {
		return
	}
	defer c.Close()
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	n, err := c.Write([]byte(header))
	if err != nil {
		return
	}
	counter.add(n)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		_ = c.SetWriteDeadline(time.Now().Add(3 * time.Second))
		n, err := c.Write(payload)
		if n > 0 {
			counter.add(n)
		}
		if err != nil {
			return
		}
	}
}

func avgTop3(speeds []float64) float64 {
	if len(speeds) == 0 {
		return 0
	}
	cp := append([]float64(nil), speeds...)
	for i := 0; i < len(cp); i++ {
		for j := i + 1; j < len(cp); j++ {
			if cp[j] < cp[i] {
				cp[i], cp[j] = cp[j], cp[i]
			}
		}
	}
	n := 3
	if len(cp) < n {
		n = len(cp)
	}
	sum := 0.0
	for _, v := range cp[len(cp)-n:] {
		sum += v
	}
	return sum / float64(n)
}

func runPhase(s Server, uuid string, down bool, threads, lengthS, intervalMS int) float64 {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var counter byteCounter
	var wg sync.WaitGroup
	for i := 0; i < threads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if down {
				downloadWorker(ctx, s, uuid, &counter)
			} else {
				uploadWorker(ctx, s, uuid, &counter)
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
	return avgTop3(samples)
}
