package main

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	pkgName   = "com.cnspeedtest.globalspeed"
	appName   = "globalspeed"
	randSalt  = "12345555"
	uaDalvik  = "Dalvik/2.1.0 (Linux; U; Android 14; NE2210 Build/TP1A.220624.014)"
	uaBrowser = "Mozilla/5.0 (Windows NT 10.0; WOW64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/100.0.4896.60 Safari/537.36"
	uaUpload  = "Dalvik/1.6.0 (Linux; U; Android 4.2.2; GT-I9505 Build/JDQ39)"
	boundary  = "00content0boundary00"
)

var ctrlServers = []string{
	"https://dlcv2.cnspeedtest.cn:8443",
	"http://dlc.duoweisoft.com:8096",
	"http://dlcv2.duoweisoft.com:8088",
}

type ClientInfo struct {
	IP       string
	Province string
	City     string
	Oper     string
}

type Server struct {
	HostID   string
	HostName string
	HostIP   string
	Port     int
	PName    string
	City     string
	Oper     string
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func enqueueToken(imei, ts string, bandwidth int) string {
	h1 := md5Hex("model=Android&imei=" + imei)
	h2 := md5Hex(fmt.Sprintf("stime=%s&band=%d&rand=%s", ts, bandwidth, randSalt))
	return md5Hex(h1 + h2)
}

func hostPort(ip string, port int) string {
	if strings.Contains(ip, ":") && !strings.HasPrefix(ip, "[") {
		return fmt.Sprintf("[%s]:%d", ip, port)
	}
	return fmt.Sprintf("%s:%d", ip, port)
}

func httpGet(raw string, timeout time.Duration) (string, error) {
	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequest(http.MethodGet, raw, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", uaDalvik)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func httpPost(raw string, timeout time.Duration) (string, error) {
	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequest(http.MethodPost, raw, strings.NewReader(""))
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", uaDalvik)
	req.Header.Set("Charset", "utf-8")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func fetchClient() (base string, info ClientInfo, err error) {
	var last error
	for _, b := range ctrlServers {
		body, e := httpGet(b+"/dataServer/getIpLocSP.php", 6*time.Second)
		if e != nil {
			last = e
			continue
		}
		parts := strings.Split(strings.TrimSpace(body), "|")
		if len(parts) == 0 || parts[0] == "" {
			continue
		}
		info.IP = parts[0]
		if len(parts) > 1 {
			var loc []string
			if json.Unmarshal([]byte(parts[1]), &loc) == nil {
				if len(loc) > 1 {
					info.Province = loc[1]
				}
				if len(loc) > 2 {
					info.City = loc[2]
				}
				if len(loc) > 4 {
					info.Oper = loc[4]
				}
			}
		}
		if info.Oper == "" && len(parts) > 3 {
			info.Oper = parts[3]
		}
		return b, info, nil
	}
	if last == nil {
		last = fmt.Errorf("无法获取出口 IP")
	}
	return "", ClientInfo{}, last
}

func matchServers(base, ip, province, city, oper string, ipv6 bool) ([]Server, error) {
	v := url.Values{}
	v.Set("ip", ip)
	v.Set("network", "4")
	v.Set("province", province)
	v.Set("city", city)
	v.Set("wifioper", oper)
	v.Set("mobileoperid", "")
	if ipv6 {
		v.Set("ipv6", "1")
	} else {
		v.Set("ipv6", "0")
	}
	v.Set("model", "Android")
	v.Set("pkg", pkgName)
	body, err := httpGet(base+"/dataServer/mobilematch_many.php?"+v.Encode(), 10*time.Second)
	if err != nil {
		return nil, err
	}
	var arr []map[string]any
	if err := json.Unmarshal([]byte(body), &arr); err != nil {
		return nil, err
	}
	out := make([]Server, 0, len(arr))
	for _, item := range arr {
		port := 0
		switch p := item["port"].(type) {
		case string:
			fmt.Sscanf(p, "%d", &port)
		case float64:
			port = int(p)
		}
		out = append(out, Server{
			HostID:   fmt.Sprint(item["hostid"]),
			HostName: fmt.Sprint(item["hostname"]),
			HostIP:   fmt.Sprint(item["hostip"]),
			Port:     port,
			PName:    fmt.Sprint(item["pname"]),
			City:     fmt.Sprint(item["city"]),
			Oper:     oper,
		})
	}
	return out, nil
}

func tcpOK(ip string, port int) bool {
	c, err := net.DialTimeout("tcp", hostPort(ip, port), 2*time.Second)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

func pickServer(servers []Server, province, city, oper string) *Server {
	if len(servers) == 0 {
		return nil
	}
	for i := range servers {
		s := &servers[i]
		if strings.Contains(s.HostName, oper) && (strings.Contains(s.HostName, city) || s.City == city || s.PName == province) {
			if tcpOK(s.HostIP, s.Port) {
				return s
			}
		}
	}
	for i := range servers {
		if tcpOK(servers[i].HostIP, servers[i].Port) {
			return &servers[i]
		}
	}
	return &servers[0]
}

func enqueue(s Server, imei string, bandwidth int) (string, error) {
	ts := fmt.Sprintf("%d", time.Now().Unix())
	token := enqueueToken(imei, ts, bandwidth)
	raw := fmt.Sprintf(
		"http://%s/speed/dovalid?key=&flag=true&bandwidth=%d&model=Android&imei=%s&time=%s&app=%s&token=%s&pkg=%s",
		hostPort(s.HostIP, s.Port), bandwidth, url.QueryEscape(imei), ts, appName, token, pkgName,
	)
	var last string
	for i := 0; i < 3; i++ {
		body, err := httpGet(raw, 5*time.Second)
		if err != nil {
			last = err.Error()
			time.Sleep(200 * time.Millisecond)
			continue
		}
		body = strings.TrimSpace(body)
		last = body
		if strings.HasPrefix(body, "0") {
			return "", fmt.Errorf("服务器忙")
		}
		if strings.HasPrefix(body, "2") {
			time.Sleep(400 * time.Millisecond)
			continue
		}
		if strings.HasPrefix(body, "-1") {
			return "", fmt.Errorf("dovalid 参数错误")
		}
		if len(body) > 2 {
			return body[2:], nil
		}
	}
	return "", fmt.Errorf("enqueue 失败: %s", last)
}

func dequeue(s Server, uuid string) {
	_, _ = httpPost("http://"+hostPort(s.HostIP, s.Port)+"/speed/dovalid?key="+uuid, 5*time.Second)
}
