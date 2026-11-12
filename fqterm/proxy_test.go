package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 代理（D3）的测试策略：不连真实代理，而是在本机起「假代理」，由它按各协议的报文
// 规矩收下客户端字节、再决定回什么。这样既能验我们发出去的字节编码对不对（端口是
// 大端吗？域名长度字段对吗？userid 有没有终止符？），也能验各种坏对手（回的不是
// SOCKS5、拒绝建隧、口令不对、端口能连却一声不吭）会不会把连接拖死。

/* --------------------------------------------------------------------------- */
/* 夹具                                                                          */
/* --------------------------------------------------------------------------- */

// startTarget 起一个「目标服务器」：一连上就先发 banner，之后原样回显。
// banner 存在的意义很具体 —— 它用来验代理握手有没有把隧道里的第一批字节吃掉。
func startTarget(t *testing.T, banner string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起目标服务器失败: %v", err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				if banner != "" {
					c.Write([]byte(banner))
				}
				io.Copy(c, c)
			}(c)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return ln.Addr().String()
}

// startProxy 起一个假代理：每来一条连接就交给 handler。
func startProxy(t *testing.T, handler func(net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起假代理失败: %v", err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go handler(c)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return ln.Addr().String()
}

func splitHostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("夹具地址不对 %s: %v", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("夹具端口不对 %s: %v", addr, err)
	}
	return host, port
}

func portOf(t *testing.T, addr string) int {
	t.Helper()
	_, p := splitHostPort(t, addr)
	return p
}

// at 把假代理的地址填进一份代理配置，省得每个测试重算端口。
func at(t *testing.T, typ int, proxyAddr, user, pass string) ProxyConfig {
	return ProxyConfig{Type: typ, Host: "127.0.0.1", Port: portOf(t, proxyAddr), User: user, Pass: pass}
}

// tunnel 把两侧接通（假代理握手成功之后用）。
func tunnel(a, b net.Conn) {
	go io.Copy(a, b)
	io.Copy(b, a)
	a.Close()
	b.Close()
}

// readTunnelHead 校验隧道里第一批字节，再写一句回显验证反向通路。
func readTunnelHead(t *testing.T, c net.Conn, want string) {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	got := make([]byte, len(want))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatalf("读隧道首段失败: %v", err)
	}
	if string(got) != want {
		t.Fatalf("隧道首段不对：拿到 %q，期望 %q（握手阶段把目标数据吃掉了吗）", string(got), want)
	}
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatalf("写隧道失败: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("读回显失败: %v", err)
	}
	if string(buf) != "ping" {
		t.Errorf("回显不对: %q", string(buf))
	}
}

// recv 等夹具把「它看到了什么」送回来，最多 5 秒。宁可让测试带着上下文失败，
// 也不要整个包卡到 go test 的总超时。
func recv[T any](t *testing.T, ch chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("假代理没把它看到的内容送回来（握手没走完？）")
		var zero T
		return zero
	}
}

// dialVia 经假代理拨号，失败即 fatal。
func dialVia(t *testing.T, pc ProxyConfig, target string) net.Conn {
	t.Helper()
	c, err := proxyDial(context.Background(), pc, "tcp", target)
	if err != nil {
		t.Fatalf("经 %s 拨 %s 失败: %v", pc.label(), target, err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// expectFail 拨号必须失败，并且错误文案里要带上关键词 —— 用户看到的就是这行字，
// 只断言「失败了」等于没测。
func expectFail(t *testing.T, pc ProxyConfig, target, wantSub string) {
	t.Helper()
	_, err := proxyDial(context.Background(), pc, "tcp", target)
	if err == nil {
		t.Fatalf("本该失败，却拨通了 %s", target)
	}
	if !strings.Contains(err.Error(), wantSub) {
		t.Errorf("错误里该有 %q，实际: %v", wantSub, err)
	}
}

/* --------------------------------------------------------------------------- */
/* 配置层                                                                        */
/* --------------------------------------------------------------------------- */

func TestProxyDialWithoutProxyGoesDirect(t *testing.T) {
	target := startTarget(t, "HELLO")
	readTunnelHead(t, dialVia(t, ProxyConfig{Type: ProxyNone}, target), "HELLO")
}

func TestProxyConfigUsableAndNormalize(t *testing.T) {
	cases := []struct {
		name string
		in   ProxyConfig
		want bool
		port int
	}{
		{name: "空配置", in: ProxyConfig{}, want: false},
		{name: "类型 none", in: ProxyConfig{Type: ProxyNone, Host: "h", Port: 1}, want: false},
		{name: "类型 direct 是站点级语义", in: ProxyConfig{Type: ProxyDirect, Host: "h", Port: 1}, want: false},
		{name: "选了类型没填地址", in: ProxyConfig{Type: ProxySocks5, Port: 1080}, want: false},
		{name: "socks5 齐全", in: ProxyConfig{Type: ProxySocks5, Host: " 127.0.0.1 ", Port: 7890}, want: true, port: 7890},
		{name: "端口填 0 按类型补", in: ProxyConfig{Type: ProxySocks5, Host: "h"}, want: true, port: 1080},
		{name: "http 端口填 0 补 8080", in: ProxyConfig{Type: ProxyHTTP, Host: "h"}, want: true, port: 8080},
		{name: "端口越界也补默认", in: ProxyConfig{Type: ProxyHTTP, Host: "h", Port: 99999}, want: true, port: 8080},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.usable(); got != tc.want {
				t.Errorf("usable = %v，期望 %v", got, tc.want)
			}
			n := tc.in.normalized()
			if n.Host != strings.TrimSpace(tc.in.Host) {
				t.Errorf("normalized 把地址改成 %q 了", n.Host)
			}
			if tc.want && n.Port != tc.port {
				t.Errorf("normalized 端口 = %d，期望 %d", n.Port, tc.port)
			}
		})
	}
	// 编号 1 在原版是 Wingate。这里当「站点级直连」用，所以作为全局代理类型时必须
	// 等于不代理 —— 否则一份老配置会静默走上一条谁也没实现过的路。
	for _, bogus := range []int{1, 7, -3} {
		if (ProxyConfig{Type: bogus, Host: "h", Port: 1}).usable() {
			t.Errorf("编号 %d 不该被当成可用代理", bogus)
		}
		got := (ProxyConfig{Type: bogus, Host: "h", Port: 1}).normalized()
		if got.Type != ProxyNone {
			t.Errorf("编号 %d 该被归一化成「没配」，实际 %+v", bogus, got)
		}
		// 地址不能跟着一起抹：把代理暂时切成「不用」还要重填地址是很理不壮的体验。
		if got.Host != "h" {
			t.Errorf("编号 %d 被抹掉了地址: %+v", bogus, got)
		}
	}
	// label 带用户但不带口令：日志里漏口令是最不该发生的错。
	lbl := ProxyConfig{Type: ProxySocks5, Host: "127.0.0.1", Port: 7890, User: "me", Pass: "s3cret"}.label()
	if !strings.Contains(lbl, "SOCKS5") || !strings.Contains(lbl, "me@") {
		t.Errorf("label 缺信息: %q", lbl)
	}
	if strings.Contains(lbl, "s3cret") {
		t.Errorf("label 漏了口令: %q", lbl)
	}
}

func TestProxyForSiteLevels(t *testing.T) {
	global := Preferences{ProxyType: ProxyHTTP, ProxyHost: "g.example", ProxyPort: 3128, ProxyUser: "gu", ProxyPass: "gp"}
	// 跟随全局（旧 sites.json 缺键就是这个形状）
	if got := proxyForSite(Site{}, global); got.Host != "g.example" || got.Type != ProxyHTTP {
		t.Errorf("站点没填代理时应跟随全局，实际 %+v", got)
	}
	// 强制直连
	if got := proxyForSite(Site{ProxyType: ProxyDirect}, global); got.usable() {
		t.Errorf("站点选直连还拿到可用代理: %+v", got)
	}
	// 站点自带一份就完全按自己填的走，不蹭全局凭据
	got := proxyForSite(Site{ProxyType: ProxySocks5, ProxyHost: "s.example", ProxyPort: 1080}, global)
	if got.Type != ProxySocks5 || got.Host != "s.example" || got.User != "" {
		t.Errorf("站点级代理没按自己填的走: %+v", got)
	}
	// 站点选了类型却没填地址 = 等于不用代理，而不是拿空地址去撞
	if s := proxyForSite(Site{ProxyType: ProxySocks5}, global); s.usable() {
		t.Errorf("地址为空不该算可用代理: %+v", s)
	}
}

/* --------------------------------------------------------------------------- */
/* SOCKS5（RFC 1928 + RFC 1929）                                                 */
/* --------------------------------------------------------------------------- */

// socks5Probe 记下假代理看到的请求内容，交回主测试 goroutine 断言（在别的 goroutine
// 里调 t.Errorf 有「测试结束后再写日志」的 panic 风险）。
type socks5Probe struct {
	methods      []byte
	authUser     string
	authPass     string
	host         string
	port         int
	gotBindQuery bool
}

// fakeSocks5 是一个真能干活的 SOCKS5 代理：user 非空即要求用户名口令认证；成功后
// 固定带一段「绑定地址」（ATYP=0x01），顺带验客户端会把它读干净。
func fakeSocks5(t *testing.T, dialTo, user, pass string) (string, chan socks5Probe) {
	t.Helper()
	ch := make(chan socks5Probe, 1)
	report := func(p socks5Probe) {
		select {
		case ch <- p:
		default:
		}
	}
	addr := startProxy(t, func(c net.Conn) {
		defer c.Close()
		var p socks5Probe
		var head [2]byte
		if _, err := io.ReadFull(c, head[:]); err != nil || head[0] != 0x05 {
			return
		}
		p.methods = make([]byte, head[1])
		if _, err := io.ReadFull(c, p.methods); err != nil {
			return
		}
		sel := byte(0x00)
		if user != "" {
			sel = 0x02
		}
		if _, err := c.Write([]byte{0x05, sel}); err != nil {
			return
		}
		if user != "" {
			var v [1]byte
			if _, err := io.ReadFull(c, v[:]); err != nil || v[0] != 0x01 {
				return
			}
			field := func() string {
				var l [1]byte
				if _, err := io.ReadFull(c, l[:]); err != nil {
					return ""
				}
				b := make([]byte, l[0])
				if _, err := io.ReadFull(c, b); err != nil {
					return ""
				}
				return string(b)
			}
			p.authUser, p.authPass = field(), field()
			ok := byte(0x00)
			if p.authUser != user || p.authPass != pass {
				ok = 0xff
			}
			if _, err := c.Write([]byte{0x01, ok}); err != nil {
				return
			}
			if ok != 0x00 {
				report(p)
				return
			}
		}
		var req [5]byte // VN CD RSV ATYP + 后面那段地址的长度
		if _, err := io.ReadFull(c, req[:]); err != nil {
			return
		}
		switch req[3] {
		case 0x03:
			name := make([]byte, req[4])
			if _, err := io.ReadFull(c, name); err != nil {
				return
			}
			p.host = string(name)
		case 0x01:
			ip := make([]byte, 4)
			if _, err := io.ReadFull(c, ip); err != nil {
				return
			}
			p.host = net.IP(ip).String()
		default:
			return
		}
		var pb [2]byte
		if _, err := io.ReadFull(c, pb[:]); err != nil {
			return
		}
		p.port = int(pb[0])<<8 | int(pb[1])
		p.gotBindQuery = true
		target, err := net.Dial("tcp", dialTo)
		if err != nil {
			c.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
			report(p)
			return
		}
		if _, err := c.Write([]byte{0x05, 0x00, 0x00, 0x01, 127, 0, 0, 1, 0, 0}); err != nil {
			target.Close()
			return
		}
		report(p)
		tunnel(c, target)
	})
	return addr, ch
}

func TestProxySocks5NoAuth(t *testing.T) {
	const banner = "SOCKS5BANNER"
	target := startTarget(t, banner)
	th, tp := splitHostPort(t, target)
	proxy, ch := fakeSocks5(t, target, "", "")

	readTunnelHead(t, dialVia(t, at(t, ProxySocks5, proxy, "", ""), target), banner)

	p := recv(t, ch)
	if p.host != th || p.port != tp {
		t.Errorf("代理看到的目标 = %s:%d，期望 %s:%d", p.host, p.port, th, tp)
	}
	// 没填用户名时只该 offer 无认证；多 offer 一种会让部分代理去选 0x02 然后要凭据。
	if len(p.methods) != 1 || p.methods[0] != 0x00 {
		t.Errorf("认证方式清单该只有 0x00，实际 %v", p.methods)
	}
}

func TestProxySocks5UserPass(t *testing.T) {
	const banner = "SOCKS5AUTH"
	target := startTarget(t, banner)
	proxy, ch := fakeSocks5(t, target, "allen", "pw!")

	readTunnelHead(t, dialVia(t, at(t, ProxySocks5, proxy, "allen", "pw!"), target), banner)

	p := recv(t, ch)
	if p.authUser != "allen" || p.authPass != "pw!" {
		t.Errorf("认证子协商里的凭据不对: %q/%q", p.authUser, p.authPass)
	}
	// 填了口令也必须同时 offer 无认证：本地那堆客户端的 SOCKS 口大多不支持 0x02，
	// 只给一种就会被回 0xff 判死。
	if !containsByte(p.methods, 0x02) || !containsByte(p.methods, 0x00) {
		t.Errorf("认证方式清单该含 0x02 与 0x00，实际 %v", p.methods)
	}
}

func TestProxySocks5WrongCredentials(t *testing.T) {
	target := startTarget(t, "X")
	proxy, _ := fakeSocks5(t, target, "right-user", "right-pass")
	expectFail(t, at(t, ProxySocks5, proxy, "wrong", "wrong"), target, "用户名口令")
}

func TestProxySocks5NeedsAuthButNoneGiven(t *testing.T) {
	target := startTarget(t, "X")
	proxy, _ := fakeSocks5(t, target, "someone", "secret")
	expectFail(t, at(t, ProxySocks5, proxy, "", ""), target, "没填")
}

func TestProxySocks5NoAcceptableMethod(t *testing.T) {
	target := startTarget(t, "X")
	proxy := startProxy(t, func(c net.Conn) {
		defer c.Close()
		var head [2]byte
		if _, err := io.ReadFull(c, head[:]); err != nil {
			return
		}
		io.CopyN(io.Discard, c, int64(head[1]))
		c.Write([]byte{0x05, 0xff}) // 一种都不接受
	})
	expectFail(t, at(t, ProxySocks5, proxy, "u", "p"), target, "不接受")
}

func TestProxySocks5ConnectRefused(t *testing.T) {
	target := startTarget(t, "X")
	proxy := startProxy(t, func(c net.Conn) {
		defer c.Close()
		var head [2]byte
		if _, err := io.ReadFull(c, head[:]); err != nil {
			return
		}
		io.CopyN(io.Discard, c, int64(head[1]))
		c.Write([]byte{0x05, 0x00})
		var req [5]byte
		if _, err := io.ReadFull(c, req[:]); err != nil {
			return
		}
		io.CopyN(io.Discard, c, int64(req[4])+2)
		// 0x05 = 连接被目标拒绝。错误码要翻成人话，用户才知道该去查目标还是查代理。
		c.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	})
	expectFail(t, at(t, ProxySocks5, proxy, "", ""), target, "连接被目标拒绝")
}

func TestProxySocks5SilentProxyTimesOut(t *testing.T) {
	// 端口能连上但一个字节都不回 —— 这是最容易把界面钉死的形态，必须有底线。
	target := startTarget(t, "X")
	proxy := startProxy(t, func(c net.Conn) {
		defer c.Close()
		time.Sleep(5 * time.Second)
	})
	restore := proxyHandshakeTimeout
	proxyHandshakeTimeout = 300 * time.Millisecond
	defer func() { proxyHandshakeTimeout = restore }()

	start := time.Now()
	_, err := proxyDial(context.Background(), at(t, ProxySocks5, proxy, "", ""), "tcp", target)
	if err == nil {
		t.Fatal("对方沉默却成功了")
	}
	if !strings.Contains(err.Error(), "超时") {
		t.Errorf("该报超时: %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("等了 %s，底线没生效", d)
	}
}

func TestProxySocks5AgainstNonSocksServer(t *testing.T) {
	// 把一个 HTTP 服务器当 SOCKS5 用：一连上就回 "HTTP/1.1 400..."，第一个字节 'H' = 72。
	target := startTarget(t, "X")
	proxy := startProxy(t, func(c net.Conn) {
		defer c.Close()
		// 先读丢客户端握手再回 HTTP 响应：带着未读数据 close 在 Windows 上
		// 发 RST，客户端读不到 'H' 字节，断言会 flaky。
		var head [2]byte
		if _, err := io.ReadFull(c, head[:]); err != nil {
			return
		}
		io.CopyN(io.Discard, c, int64(head[1]))
		c.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\n"))
	})
	expectFail(t, at(t, ProxySocks5, proxy, "", ""), target, "不是 SOCKS5")
}

// 成功回包后面按规矩还跟着「绑定地址」。它没有分隔符，少发了不能当没事发生：
// 接着读就是在猜隧道数据到哪里为止，猜错的后果是横幅少一截、IAC 被掐一半。
func TestProxySocks5TruncatedReplyFails(t *testing.T) {
	target := startTarget(t, "TAILLESS")
	proxy := startProxy(t, func(c net.Conn) {
		defer c.Close()
		var head [2]byte
		if _, err := io.ReadFull(c, head[:]); err != nil {
			return
		}
		io.CopyN(io.Discard, c, int64(head[1]))
		c.Write([]byte{0x05, 0x00})
		var req [5]byte
		if _, err := io.ReadFull(c, req[:]); err != nil {
			return
		}
		io.CopyN(io.Discard, c, int64(req[4])+2)
		// 地址类型那个字节还没发完就接上了隧道数据：客户端不能拿它当 atyp 继续读
		c.Write([]byte{0x05, 0x00, 0x00, 'T', 'A', 'I', 'L'})
		time.Sleep(2 * time.Second)
	})
	expectFail(t, at(t, ProxySocks5, proxy, "", ""), target, "地址类型")
}

/* --------------------------------------------------------------------------- */
/* SOCKS4 / SOCKS4a                                                             */
/* --------------------------------------------------------------------------- */

type socks4Probe struct {
	vn, cd byte
	port   int
	ip     net.IP
	userid string
	host   string // 仅在 4a（IP 字段写成 0.0.0.x）时有值
}

// fakeSocks4 是一个真能干活的 SOCKS4/4a 代理：认得 4a 的域名写法，按 targets 里
// 的名字去连对应目标；连不上就回 91。
func fakeSocks4(t *testing.T, targets map[string]string) (string, chan socks4Probe) {
	t.Helper()
	ch := make(chan socks4Probe, 1)
	addr := startProxy(t, func(c net.Conn) {
		defer c.Close()
		var head [8]byte
		if _, err := io.ReadFull(c, head[:]); err != nil {
			return
		}
		p := socks4Probe{vn: head[0], cd: head[1], port: int(head[2])<<8 | int(head[3]), ip: net.IP(head[4:8])}
		rd := bufio.NewReader(c)
		readZ := func() string {
			var sb strings.Builder
			for {
				b, err := rd.ReadByte()
				if err != nil {
					return sb.String()
				}
				if b == 0 {
					return sb.String()
				}
				sb.WriteByte(b)
			}
		}
		p.userid = readZ()
		host := p.ip.String()
		if p.ip[0] == 0 && p.ip[1] == 0 && p.ip[2] == 0 && p.ip[3] != 0 {
			host = readZ()
			p.host = host
		}
		target, err := net.Dial("tcp", targets[host])
		if err != nil {
			c.Write([]byte{0x00, 91, 0, 0, 0, 0, 0, 0})
			return
		}
		c.Write([]byte{0x00, 90, 0, 0, 0, 0, 0, 0})
		select {
		case ch <- p:
		default:
		}
		tunnel(c, target)
	})
	return addr, ch
}

func TestProxySocks4WithIPv4Literal(t *testing.T) {
	const banner = "SOCKS4BANNER"
	target := startTarget(t, banner)
	th, tp := splitHostPort(t, target)
	proxy, ch := fakeSocks4(t, map[string]string{th: target})

	readTunnelHead(t, dialVia(t, at(t, ProxySocks4, proxy, "", ""), target), banner)

	p := recv(t, ch)
	if p.vn != 0x04 || p.cd != 0x01 {
		t.Errorf("版本/命令不对: vn=%d cd=%d（4=SOCKS4、1=CONNECT）", p.vn, p.cd)
	}
	if p.port != tp {
		t.Errorf("端口不是大端编码: 代理看到 %d，期望 %d", p.port, tp)
	}
	if !p.ip.Equal(net.ParseIP(th)) {
		t.Errorf("IP 字段不对: %v，期望 %s", p.ip, th)
	}
	// 没填用户名时 userid 是空串，但那一个终止字节必须在 —— 少了它后面的字节会被
	// 当成 userid 吃掉，报出来的错就成了莫名的「代理拒绝」。
	if p.userid != "" {
		t.Errorf("userid 该是空串: %q", p.userid)
	}
}

func TestProxySocks4FallsBackToSocks4a(t *testing.T) {
	const banner = "SOCKS4AOK"
	const name = "ftrw-proxy-test.invalid"
	target := startTarget(t, banner)
	_, tp := splitHostPort(t, target)
	proxy, ch := fakeSocks4(t, map[string]string{name: target})

	// 这个名字本地解析不了，正好逼出 4a 分支：域名必须原样交给代理。
	addr := fmt.Sprintf("%s:%d", name, tp)
	readTunnelHead(t, dialVia(t, at(t, ProxySocks4, proxy, "", ""), addr), banner)

	p := recv(t, ch)
	if p.host != name {
		t.Errorf("域名该交给代理解析，代理看到的是 %q", p.host)
	}
	if !(p.ip[0] == 0 && p.ip[1] == 0 && p.ip[2] == 0 && p.ip[3] != 0) {
		t.Errorf("4a 的 IP 写法该是 0.0.0.x（x≠0），实际 %v", p.ip)
	}
}

func TestProxySocks4Rejected(t *testing.T) {
	target := startTarget(t, "X")
	// targets 空表：任何目标都连不上，代理只会回 91。
	proxy, _ := fakeSocks4(t, map[string]string{})
	expectFail(t, at(t, ProxySocks4, proxy, "", ""), target, "SOCKS4")
}

/* --------------------------------------------------------------------------- */
/* HTTP CONNECT                                                                 */
/* --------------------------------------------------------------------------- */

type httpFakeMode int

const (
	httpFakeTunnel           httpFakeMode = iota // 200 + 接通目标
	httpFakeTunnelPushBanner                     // 200 与目标首段数据一次写出
	httpFakeRefused                              // 403
	httpFakeGarbage                              // 根本不是状态行
)

// fakeHTTP 起一个 HTTP CONNECT 代理，并把收到的请求头整份送回测试。
// dialTo 为空时按请求行里写的目标去连（给 http.Client 那条接线用）。
func fakeHTTP(t *testing.T, dialTo string, banner string, mode httpFakeMode) (string, chan string) {
	t.Helper()
	ch := make(chan string, 4)
	addr := startProxy(t, func(c net.Conn) {
		defer c.Close()
		rd := bufio.NewReader(c)
		var sb strings.Builder
		for {
			line, err := rd.ReadString('\n')
			sb.WriteString(line)
			if err != nil || strings.TrimRight(line, "\r\n") == "" {
				break
			}
		}
		select {
		case ch <- sb.String():
		default:
		}
		head := "HTTP/1.1 200 Connection established\r\nProxy-Agent: ftrw-fake\r\n\r\n"
		switch mode {
		case httpFakeRefused:
			c.Write([]byte("HTTP/1.1 403 Forbidden\r\n\r\n"))
			return
		case httpFakeGarbage:
			c.Write([]byte("这不是 HTTP 响应，只是随手写的一段话\r\n\r\n"))
			return
		}
		dest := dialTo
		if dest == "" {
			line := strings.SplitN(sb.String(), "\r\n", 2)[0]
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return
			}
			dest = fields[1]
		}
		target, err := net.Dial("tcp", dest)
		if err != nil {
			c.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
			return
		}
		if mode == httpFakeTunnelPushBanner {
			// 响应头与目标的第一段数据挤在同一个 Write 里 —— 客户端要是敢预读，
			// 这段 banner 就永远丢了。
			c.Write(append([]byte(head), banner...))
			tunnel(c, target)
			return
		}
		c.Write([]byte(head))
		tunnel(c, target)
	})
	return addr, ch
}

func TestProxyHTTPConnect(t *testing.T) {
	const banner = "CONNECTBANNER"
	target := startTarget(t, banner)
	th, tp := splitHostPort(t, target)
	proxy, ch := fakeHTTP(t, target, "", httpFakeTunnel)

	readTunnelHead(t, dialVia(t, at(t, ProxyHTTP, proxy, "u1", "p1"), target), banner)

	req := recv(t, ch)
	if !strings.HasPrefix(req, "CONNECT ") || !strings.Contains(req, " HTTP/1.") {
		t.Errorf("请求行不对: %q", req)
	}
	if !strings.Contains(req, fmt.Sprintf("%s:%d", th, tp)) {
		t.Errorf("CONNECT 的目标该是 %s:%d，实际:\n%s", th, tp, req)
	}
	wantAuth := base64.StdEncoding.EncodeToString([]byte("u1:p1"))
	if !strings.Contains(req, "Proxy-Authorization: Basic "+wantAuth) {
		t.Errorf("没带上 Basic 认证头:\n%s", req)
	}
}

// 隧道第一口字节绝不能丢：BBS 的开场横幅常常正好压在 200 那一行后面。
func TestProxyHTTPConnectKeepsTunnelHeadBytes(t *testing.T) {
	const banner = "WELCOME\x1b[0m"
	target := startTarget(t, "")
	proxy, _ := fakeHTTP(t, target, banner, httpFakeTunnelPushBanner)

	c := dialVia(t, at(t, ProxyHTTP, proxy, "", ""), target)
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	got := make([]byte, len(banner))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatalf("读隧道首段失败: %v", err)
	}
	if string(got) != banner {
		t.Errorf("隧道首段被吃了: % q", got)
	}
}

func TestProxyHTTPConnectRefused(t *testing.T) {
	target := startTarget(t, "X")
	proxy, _ := fakeHTTP(t, target, "", httpFakeRefused)
	// 403 要带着状态码报出来：用户拿这行字去搜才知道是代理策略挡了。
	expectFail(t, at(t, ProxyHTTP, proxy, "", ""), target, "403")
}

func TestProxyHTTPGarbageResponse(t *testing.T) {
	target := startTarget(t, "X")
	proxy, _ := fakeHTTP(t, target, "", httpFakeGarbage)
	expectFail(t, at(t, ProxyHTTP, proxy, "", ""), target, "看不懂")
}

func TestProxyHTTPDialsRequestedTarget(t *testing.T) {
	// dialTo 留空 → 按 CONNECT 行里的地址去连。这条正是 http.Client 用法依赖的行为。
	const banner = "ANYTARGET"
	target := startTarget(t, banner)
	proxy, _ := fakeHTTP(t, "", "", httpFakeTunnel)
	readTunnelHead(t, dialVia(t, at(t, ProxyHTTP, proxy, "", ""), target), banner)
}

/* --------------------------------------------------------------------------- */
/* 各种坏对手                                                                    */
/* --------------------------------------------------------------------------- */

func TestProxyDialUnreachableProxy(t *testing.T) {
	// 占一个端口再关掉，确保「连不上代理」报的是代理而不是目标。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	expectFail(t, at(t, ProxySocks5, addr, "", ""), "127.0.0.1:23", "连不上代理")
}

func TestProxyDialRejectsNonTCPWithProxy(t *testing.T) {
	_, err := proxyDial(context.Background(), ProxyConfig{Type: ProxySocks5, Host: "h", Port: 1}, "udp", "1.2.3.4:53")
	if err == nil || !strings.Contains(err.Error(), "只支持 TCP") {
		t.Fatalf("配了代理还想走 UDP 要被挡住: %v", err)
	}
	// 没配代理时 udp 该原样放行（本机 shell 之外的用途将来也许用得上）
	if _, err := proxyDial(context.Background(), ProxyConfig{}, "udp", "127.0.0.1:53"); err != nil && strings.Contains(err.Error(), "只支持 TCP") {
		t.Errorf("无代理时不该拦 udp: %v", err)
	}
}

func TestProxyBadTargetAddr(t *testing.T) {
	target := startTarget(t, "X")
	proxy, _ := fakeSocks5(t, target, "", "")
	pc := at(t, ProxySocks5, proxy, "", "")
	if _, err := proxyDial(context.Background(), pc, "tcp", "just-a-name"); err == nil {
		t.Error("缺端口的目标地址该报错")
	}
	if _, err := proxyDial(context.Background(), pc, "tcp", target+":88888"); err == nil {
		t.Error("端口越界该报错")
	}
}

/* --------------------------------------------------------------------------- */
/* http.Client 接线                                                             */
/* --------------------------------------------------------------------------- */

// 配了全局代理之后，IP 库下载与链接预览那两条 HTTP 出路要真的经由代理出去。
// 这条打的是整条接线（activeProxy → Transport.Proxy/DialContext → CONNECT），
// 少接一环都过不去。
func TestProxyAwareTransportRoutesHTTP(t *testing.T) {
	restore := globalProxy()
	defer setGlobalProxy(restore)

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, "pong")
	}))
	defer srv.Close()
	srvHost, srvPort := splitHostPort(t, strings.TrimPrefix(srv.URL, "http://"))

	proxy, ch := fakeHTTP(t, "", "", httpFakeTunnel)
	setGlobalProxy(ProxyConfig{Type: ProxyHTTP, Host: "127.0.0.1", Port: portOf(t, proxy)})

	tr := proxyAwareTransport(2)
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 15 * time.Second}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("经代理发 HTTP 失败: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "pong" {
		t.Errorf("响应不对: %q", body)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("目标服务器命中 %d 次，期望 1", n)
	}
	req := <-ch
	if !strings.Contains(req, fmt.Sprintf("%s:%d", srvHost, srvPort)) {
		t.Errorf("代理该看到目标地址 %s:%d，实际:\n%s", srvHost, srvPort, req)
	}
}

// 没配代理时不能凭空插入一跳：这条路要能直连（这里只验结果，不验内部字段）。
func TestProxyAwareTransportWithoutProxy(t *testing.T) {
	restore := globalProxy()
	defer setGlobalProxy(restore)
	setGlobalProxy(ProxyConfig{Type: ProxyNone})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") }))
	defer srv.Close()
	tr := proxyAwareTransport(2)
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 15 * time.Second}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("直连失败: %v", err)
	}
	resp.Body.Close()
}

func TestSetGlobalProxyRoundTrip(t *testing.T) {
	restore := globalProxy()
	defer setGlobalProxy(restore)

	setGlobalProxy(ProxyConfig{Type: ProxySocks5, Host: " 10.0.0.1 ", Port: 0})
	got := globalProxy()
	if got.Host != "10.0.0.1" || got.Port != 1080 {
		t.Errorf("存进去的该是归一化后的值，实际 %+v", got)
	}
	setGlobalProxy(ProxyConfig{})
	if globalProxy().usable() {
		t.Error("清空之后还可用")
	}
}

/* --------------------------------------------------------------------------- */
/* 真代理（默认跳过）                                                           */
/* --------------------------------------------------------------------------- */

// TestProxyAgainstRealProxy 拿一个真代理跑一遭完整的 CONNECT 建隧。假代理能验报文
// 格式，验不了「对手真是个现代代理软件时能不能用」（版本兼容、Keep-Alive 行为、
// 它对 CONNECT 之外的请求怎么反应），这些只能打真货。
//
//	$env:FTRW_PROXY_TEST = "http://127.0.0.1:9870"   # 也认 socks5://、socks4://、带账号的写法
//	go test -run TestProxyAgainstReal -v
func TestProxyAgainstRealProxy(t *testing.T) {
	raw := strings.TrimSpace(os.Getenv("FTRW_PROXY_TEST"))
	if raw == "" {
		t.Skip("未设 FTRW_PROXY_TEST，跳过真代理校验")
	}
	pc, err := parseProxySpec(raw)
	if err != nil {
		t.Fatalf("FTRW_PROXY_TEST 读不懂：%v", err)
	}
	msg, err := proxyTest(pc)
	if err != nil {
		t.Fatalf("经 %s 建隧失败: %v", pc.label(), err)
	}
	t.Log(msg)

	// 再拿它下一次真的 HTTPS：只看建隧成功不够，得确认 TLS 与内容也能穿过去。
	restore := globalProxy()
	defer setGlobalProxy(restore)
	setGlobalProxy(pc)
	tr := proxyAwareTransport(2)
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 30 * time.Second}
	resp, err := client.Get("https://api.github.com/repos/metowolf/qqwry.dat/releases/latest")
	if err != nil {
		t.Fatalf("经代理发 HTTPS 失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 || !strings.Contains(string(body), "browser_download_url") {
		t.Fatalf("代理拿回的不是预期内容: HTTP %d，%d 字节", resp.StatusCode, len(body))
	}
	t.Logf("经代理拿到了 release JSON（%d 字节）", len(body))
}

// parseProxySpec 只给上面的测试用：“http://127.0.0.1:9870”、"socks5://user:pw@host:1080”
// 这种写法读成人能看懂的形式。界面上是分开填的字段，所以这份解析不进生产代码。
func parseProxySpec(raw string) (ProxyConfig, error) {
	if !strings.Contains(raw, "://") {
		raw = "socks5://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ProxyConfig{}, err
	}
	var typ int
	switch strings.ToLower(u.Scheme) {
	case "socks5", "socks":
		typ = ProxySocks5
	case "socks4", "socks4a":
		typ = ProxySocks4
	case "http", "https":
		typ = ProxyHTTP
	default:
		return ProxyConfig{}, fmt.Errorf("不认的代理前缀 %q", u.Scheme)
	}
	pc := ProxyConfig{Type: typ, Host: u.Hostname()}
	if p := u.Port(); p != "" {
		pc.Port, _ = strconv.Atoi(p)
	}
	if u.User != nil {
		pc.User = u.User.Username()
		pc.Pass, _ = u.User.Password()
	}
	return pc.normalized(), nil
}

/* --------------------------------------------------------------------------- */
/* 小工具                                                                       */
/* --------------------------------------------------------------------------- */

func containsByte(b []byte, v byte) bool {
	for _, x := range b {
		if x == v {
			return true
		}
	}
	return false
}
