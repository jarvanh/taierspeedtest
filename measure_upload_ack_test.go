package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// captureStderr 把 os.Stderr 换成管道收本轮输出（uploadWorker 直接写 os.Stderr）。
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	old := os.Stderr
	os.Stderr = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	os.Stderr = old
	w.Close()
	return <-done
}

func envForUploadTest(t *testing.T) {
	t.Helper()
	t.Setenv("TAIER_UPLOAD_MULTI_SESSION", "0") // 不打 enqueue，假服务器没有 dovalid
	t.Setenv("TAIER_SOCKS5", "")
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("http_proxy", "")
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("https_proxy", "")
	t.Setenv("TAIER_UPLOAD_SUSTAINED", "")
	t.Setenv("TAIER_UPLOAD_STAGGER_MS", "0")
}

// uploadBody 自身记账：finite 模式下 counter 与 ownSent 严格同步、发满即 EOF；
// 持续供给模式（finite=false）不记账（ownSent 恒 0）。
func TestUploadBodyOwnSentAccounting(t *testing.T) {
	c := newByteCounter(1)
	b := &uploadBody{
		counter:   c,
		prefix:    []byte("HEAD"),
		payload:   make([]byte, 100),
		finite:    true,
		remaining: 250,
	}
	buf := make([]byte, 60)
	total := 0
	for i := 0; i < 100; i++ {
		n, err := b.Read(buf)
		total += n
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	}
	if total != 250 {
		t.Fatalf("sent %d bytes, want 250", total)
	}
	if c.snap() != 250 {
		t.Fatalf("counter=%d, want 250", c.snap())
	}
	if b.ownSent.Load() != 250 {
		t.Fatalf("ownSent=%d, want 250", b.ownSent.Load())
	}

	// 持续供给模式：计数进 counter，ownSent 不动
	c2 := newByteCounter(1)
	stop := make(chan struct{})
	b2 := &uploadBody{counter: c2, payload: make([]byte, 100), stop: stop, finite: false}
	_, _ = b2.Read(buf)
	if c2.snap() != 60 {
		t.Fatalf("sustained counter=%d, want 60", c2.snap())
	}
	if b2.ownSent.Load() != 0 {
		t.Fatalf("sustained ownSent=%d, want 0（对照模式不记账）", b2.ownSent.Load())
	}
	close(stop)
}

// 核心回归：服务端 403 拒收的请求，其已计字节必须在共享 counter 中被扣回。
// 旧实现「失败不撤销」——N 次失败就留下 N×ownSent 的幻影字节；新实现 counter
// 只留「正在途的那一个请求」的余量（≤ 2×单请求长度）。
func TestUploadWorkerClawsBackOnReject(t *testing.T) {
	envForUploadTest(t)
	// TAIER_UPLOAD_LEN 覆盖单请求长度（64KB），服务器读走 32KB 后 403，
	// 保证每次失败都有 ownSent ≥ 32KB 可扣。
	t.Setenv("TAIER_UPLOAD_LEN", "65536")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 32768)
		_, _ = io.ReadFull(r.Body, buf) // 吃掉 32KB，制造真实的 ownSent
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	host, port := mustHostPort(t, srv.URL)

	counter := newByteCounter(1)
	stderr := captureStderr(t, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
		defer cancel()
		uploadWorker(ctx, Server{HostIP: host, Port: port}, "test-uuid", counter)
	})

	// 1.2s ÷（~几十 ms/请求 + 100ms 失败退避）≈ 6~10 次失败。
	// 不扣回的话 counter ≈ 6~10 × ≥32KB ≈ ≥192KB；扣回后只剩取消瞬间的在途余量。
	if got := counter.snap(); got > 2*65536 {
		t.Fatalf("counter=%d 未经扣回（幻影字节残留），应 ≤ %d\nstderr:\n%s", got, 2*65536, stderr)
	}
	if !strings.Contains(stderr, "[debug-ul-ack]") {
		t.Fatalf("缺确认台账 [debug-ul-ack]\nstderr:\n%s", stderr)
	}
	if !strings.Contains(stderr, "clawed=") || strings.Contains(stderr, "clawed=0 ") {
		t.Fatalf("台账应显示 clawed>0\nstderr:\n%s", stderr)
	}
}

// 服务端全 200 时：确认率必须精确 100%（sent 全部 confirmed），
// 且窗口内真实产出字节 > 0。
func TestUploadWorkerAckRatioFullConfirm(t *testing.T) {
	envForUploadTest(t)
	t.Setenv("TAIER_UPLOAD_LEN", "262144") // 256KB/请求，窗口内能跑多个完整请求

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) // 全量接收
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	host, port := mustHostPort(t, srv.URL)

	counter := newByteCounter(1)
	stderr := captureStderr(t, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
		defer cancel()
		uploadWorker(ctx, Server{HostIP: host, Port: port}, "test-uuid", counter)
	})

	if counter.snap() <= 0 {
		t.Fatalf("正常链路 counter 应 > 0\nstderr:\n%s", stderr)
	}
	if !strings.Contains(stderr, "ratio=100.0%") {
		t.Fatalf("全 200 场景确认率应为 100.0%%\nstderr:\n%s", stderr)
	}
}

func mustHostPort(t *testing.T, url string) (string, int) {
	t.Helper()
	s := strings.TrimPrefix(url, "http://")
	idx := strings.LastIndex(s, ":")
	if idx < 0 {
		t.Fatalf("bad url %q", url)
	}
	host := s[:idx]
	port := 0
	for _, c := range s[idx+1:] {
		port = port*10 + int(c-'0')
	}
	return host, port
}
