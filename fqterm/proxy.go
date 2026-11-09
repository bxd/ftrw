package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// 网络代理（D3）：SOCKS4/4a、SOCKS5、HTTP CONNECT，各自带认证。
//
// 原版把代理挂在 socket 上（fqterm_src/src/protocol/fqterm_socket_private.cpp 的
// socketConnected / socks5_connect / socks5_auth / socks5_reply），类型编号取自
// fqterm.h：NOPROXY=0 WINGATE=1 SOCKS4=2 SOCKS5=3 HTTP=4。这里延用同一套编号，
// 只少了 WINGATE —— 那是上世纪 WinGate 的私有握手，现在已经找不到对手方了。
// 编号 1 留给站点级的「强制直连」（见 ProxyDirect），跟原版的 WINGATE 撞不上，
// 因为本站点格式里从来不曾存过 Wingate。
//
// 与原版的一个实质差别：原版先在本地把主机名解析成 IPv4，再把 ATYP=0x01 发给代理；
// 这里把域名原样交给代理去解析（SOCKS5 的 ATYP=0x03）。会需要代理的场合，往往正是
// 本地 DNS 被污染或压根不通的场合，在本地解析等于一边把污染得来的 IP 当成目标，
// 一边把「代理能看到的名字」丢掉。只有 SOCKS4 没有域名扩展时才退回本地解析，
// 且解析不出来时自动改用 SOCKS4a 的写法。

// 代理类型编号。ProxyNone/ProxySocks4/ProxySocks5/ProxyHTTP 与原版一致。
const (
	ProxyNone   = 0 // 不用代理（全局设置里的「不用代理」，也是站点级的「跟随全局」）
	ProxyDirect = 1 // 仅站点级：本站点强制直连，即使全局配了代理
	ProxySocks4 = 2
	ProxySocks5 = 3
	ProxyHTTP   = 4
)

// 各类型的惯用端口。设置里端口留空时按这个补，省得用户每次都要回想。
var proxyDefaultPort = map[int]int{
	ProxySocks4: 1080,
	ProxySocks5: 1080,
	ProxyHTTP:   8080,
}

const (
	// proxyDialTimeout 连代理服务器本身。局域网代理通常毫秒级，15 秒是给
	//「地址填错成一个不存在的主机名」这种情形留的解析余量。
	proxyDialTimeout = 15 * time.Second
)

// proxyHandshakeTimeout 罩住 SOCKS/CONNECT 那几来一回的握手。代理活着但握手卡住
// （认证方式谈不拢、目标不可达时有些实现就是不发回包）比连不上更烦，必须有底线。
// 它是 var 而不是 const，只为了让单测能把这个等待从 15 秒压到几百毫秒。
var proxyHandshakeTimeout = 15 * time.Second

// proxyHeaderLimit HTTP 代理握手响应头的上限。正常响应不到 200 字节，
// 超出只可能是对端在灌垃圾，不能跟着长。
const proxyHeaderLimit = 8 << 10

// proxyTestTarget 是「测试连通」真正去连的地址。挑 api.github.com:443 而不是
// 某个 BBS：这条正好是 IP 库下载要用的路，这里通了 IP 库就下得动，不通也能一眼
// 看出卡在代理还是卡在出站。
const proxyTestTarget = "api.github.com:443"

// ProxyConfig 是一份代理设置（地址 + 凭据）。全局设置与站点覆盖共用它，
// JSON 字段名与前端 api.ts 里的 ProxyConfig 对齐。
type ProxyConfig struct {
	Type int    `json:"type"`
	Host string `json:"host"`
	Port int    `json:"port"`
	User string `json:"user"`
	Pass string `json:"pass"`
}

func (pc ProxyConfig) normalized() ProxyConfig {
	pc.Host = strings.TrimSpace(pc.Host)
	pc.User = strings.TrimSpace(pc.User)
	if !proxyTypeKnown(pc.Type) {
		// 认不出的编号（老配置里的 Wingate、手改 JSON 打错的）一律当没配：
		// 留着它只会让用户以为流量走了代理。地址与凭据原封不动留着 ——
		// 把代理临时切成「不用」还要重填地址是很理不壮的体验，能不能用由 usable 说。
		pc.Type = ProxyNone
		return pc
	}
	if pc.Port <= 0 || pc.Port > 65535 {
		pc.Port = proxyDefaultPort[pc.Type]
	}
	return pc
}

func proxyTypeKnown(t int) bool {
	switch t {
	case ProxySocks4, ProxySocks5, ProxyHTTP:
		return true
	}
	return false
}

// usable 判断这份配置是不是真能拿去拨号。类型认得但地址为空（用户选了类型还没填
// 地址）时按直连处理，不能拿空地址去撞。
func (pc ProxyConfig) usable() bool {
	pc = pc.normalized()
	return proxyTypeKnown(pc.Type) && pc.Host != ""
}

func (pc ProxyConfig) addr() string {
	pc = pc.normalized()
	return net.JoinHostPort(pc.Host, strconv.Itoa(pc.Port))
}

// label 是给日志和界面看的一行短描述，带上认证信息但不含口令。
func (pc ProxyConfig) label() string {
	pc = pc.normalized()
	name := proxyTypeName(pc.Type)
	if pc.User != "" {
		return fmt.Sprintf("%s %s@%s", name, pc.User, pc.addr())
	}
	return fmt.Sprintf("%s %s", name, pc.addr())
}

func proxyTypeName(t int) string {
	switch t {
	case ProxySocks4:
		return "SOCKS4"
	case ProxySocks5:
		return "SOCKS5"
	case ProxyHTTP:
		return "HTTP"
	case ProxyDirect:
		return "直连"
	default:
		return "不用代理"
	}
}

// activeProxy 是当前生效的全局代理。之所以要单独存一份内存：http.Transport 的
// DialContext 回调是标准库在自己起的 goroutine 里调的，那两条路（IP 库下载、
// 链接预览）拿不到 store。站点级的覆盖不走这里，由拨号前现算的 proxyForSite 负责。
var activeProxy atomic.Pointer[ProxyConfig]

func setGlobalProxy(pc ProxyConfig) {
	p := pc.normalized()
	activeProxy.Store(&p)
	if p.usable() {
		log.Printf("全局代理已启用：%s", p.label())
	}
}

func globalProxy() ProxyConfig {
	if p := activeProxy.Load(); p != nil {
		return *p
	}
	return ProxyConfig{}
}

// proxyFromPrefs 取全局那一份。
func proxyFromPrefs(p Preferences) ProxyConfig {
	return ProxyConfig{Type: p.ProxyType, Host: p.ProxyHost, Port: p.ProxyPort, User: p.ProxyUser, Pass: p.ProxyPass}
}

// proxyForSite 算出一个站点最终该用哪个代理：站点选「跟随全局」（0，也是旧
// sites.json 缺键时的值）就用全局那份；选「强制直连」（1）一定不用代理；
// 选了具体类型就完全按站点自己填的字段走（不回落到全局凭据 —— 两处字段猜来猜去
// 谁赢，不如让界面填了什么就是什么）。
func proxyForSite(site Site, p Preferences) ProxyConfig {
	switch site.ProxyType {
	case ProxyNone:
		return proxyFromPrefs(p)
	case ProxyDirect:
		return ProxyConfig{Type: ProxyNone}
	default:
		return ProxyConfig{Type: site.ProxyType, Host: site.ProxyHost, Port: site.ProxyPort, User: site.ProxyUser, Pass: site.ProxyPass}
	}
}

// proxyDial 拨通 addr 并走完代理握手，返回的连接已经是隧道里的裸流；没配代理时
// 就是普通直拨。ctx 只约束「拨号 + 握手」，返回的连接不会被它的取消掐断（握手
// 期间设的读写 deadline 会被清掉），所以调用方可以放心拿它做超时。
func proxyDial(ctx context.Context, pc ProxyConfig, network, addr string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" {
		if pc.usable() {
			return nil, fmt.Errorf("代理只支持 TCP，不支持 %s", network)
		}
	}
	pc = pc.normalized()
	d := net.Dialer{Timeout: proxyDialTimeout, KeepAlive: 30 * time.Second}
	if !pc.usable() {
		return d.DialContext(ctx, network, addr)
	}
	c, err := d.DialContext(ctx, "tcp", pc.addr())
	if err != nil {
		return nil, fmt.Errorf("连不上代理 %s：%w", pc.addr(), err)
	}
	if err := proxyHandshake(ctx, c, pc, addr); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// proxyHandshake 在已连上代理的连接上走完建隧流程，成功时连接已可当作目标主机的
// 裸字节流使用。握手全程带 deadline：代理端口能连但不应答（端口转发到了一台死机器）
// 是最容易把人钉死在界面上的情形。
func proxyHandshake(ctx context.Context, c net.Conn, pc ProxyConfig, addr string) error {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("目标地址不好解析 %s：%w", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return fmt.Errorf("目标端口不合法：%s", portStr)
	}
	// 握手的底线取「整体期限」与 proxyHandshakeTimeout 里更早的那个：只看 ctx 会让
	// 一个很宽的整体超时把单步握手拖长，只看常量又会让调用方给的短 ctx 失效。
	beat := time.Now().Add(proxyHandshakeTimeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(beat) {
		beat = dl
	}
	c.SetDeadline(beat)
	defer c.SetDeadline(time.Time{})
	switch pc.Type {
	case ProxySocks5:
		return socks5Handshake(ctx, c, pc, host, port)
	case ProxySocks4:
		return socks4Handshake(ctx, c, pc, host, port)
	case ProxyHTTP:
		return httpConnectHandshake(c, pc, host, port)
	default:
		return fmt.Errorf("不支持的代理类型编号 %d", pc.Type)
	}
}

/* --------------------------------------------------------------------------- */
/* SOCKS5（RFC 1928 + 用户名口令扩展 RFC 1929）                                   */
/* --------------------------------------------------------------------------- */

func socks5Handshake(ctx context.Context, c net.Conn, pc ProxyConfig, host string, port int) error {
	// 认证方式清单：填了用户名口令就把 0x02 排在前面，但仍带上 0x00 —— 不少本地
	// 代理（各种客户端的 HTTP/SOCKS 混合监听口）根本不支持 0x02，只报 0x00 表示
	//「不需要认证」。只给一种会让这种代理直接回 0xff 把会话判死。
	methods := []byte{0x05, 0x01, 0x00}
	if pc.User != "" {
		methods = []byte{0x05, 0x02, 0x02, 0x00}
	}
	if _, err := c.Write(methods); err != nil {
		return fmt.Errorf("代理握手（送认证方式）失败：%w", err)
	}
	var sel [2]byte
	if _, err := io.ReadFull(c, sel[:]); err != nil {
		return fmt.Errorf("代理握手（等认证方式）失败：%w", socksTimeoutHint(err))
	}
	if sel[0] != 0x05 {
		return fmt.Errorf("对方不是 SOCKS5 代理（回了版本号 %d）", sel[0])
	}
	switch sel[1] {
	case 0x00:
		// 对方选了「不认证」，直接进建隧
	case 0x02:
		if pc.User == "" {
			return errors.New("代理要求用户名口令认证，设置里没填")
		}
		if err := socks5Auth(c, pc.User, pc.Pass); err != nil {
			return err
		}
	case 0xff:
		return errors.New("代理不接受任何一种认证方式（填的用户名口令可能没对上）")
	default:
		return fmt.Errorf("代理选了不明的认证方式 0x%02x", sel[1])
	}

	// 建隧请求：域名直发（ATYP=0x03），交给代理解析，理由见文件头。
	if len(host) > 255 {
		return fmt.Errorf("主机名过长（%d 字节）", len(host))
	}
	req := make([]byte, 0, 7+len(host))
	req = append(req, 0x05, 0x01, 0x00, 0x03, byte(len(host)))
	req = append(req, host...)
	req = append(req, byte(port>>8), byte(port))
	if _, err := c.Write(req); err != nil {
		return fmt.Errorf("代理握手（连 %s）失败：%w", host, err)
	}
	var rep [4]byte
	if _, err := io.ReadFull(c, rep[:]); err != nil {
		return fmt.Errorf("代理握手（等 %s 结果）失败：%w", host, socksTimeoutHint(err))
	}
	if rep[0] != 0x05 {
		return fmt.Errorf("对方不是 SOCKS5 代理（回了版本号 %d）", rep[0])
	}
	if rep[1] != 0x00 {
		return fmt.Errorf("代理连 %s:%d 失败：%s", host, port, socks5RepText(rep[1]))
	}
	// 回包后面还跟着「绑定的地址」，长度看 atyp。这一段没有分隔符，少读几个字节就是
	// 把隧道数据的前几个字节当成了地址 —— 症状是终端开头少一截、或者 telnet 的 IAC
	// 被掐一半，比直接报错难查得多，所以按规矩读满、读不到就报错。
	switch rep[3] {
	case 0x01:
		return socks5BindTail(c, 4+2)
	case 0x03:
		var l [1]byte
		if _, err := io.ReadFull(c, l[:]); err != nil {
			return fmt.Errorf("代理回包不完整（绑定域名长度）：%w", err)
		}
		return socks5BindTail(c, int(l[0])+2)
	case 0x04:
		return socks5BindTail(c, 16+2)
	default:
		return fmt.Errorf("代理回了不明的地址类型 0x%02x", rep[3])
	}
}

func socks5BindTail(c net.Conn, n int) error {
	if _, err := io.ReadFull(c, make([]byte, n)); err != nil {
		return fmt.Errorf("代理回包不完整（绑定地址）：%w", err)
	}
	return nil
}

// socks5Auth 是 RFC 1929 那一小段用户名口令子协商。
func socks5Auth(c net.Conn, user, pass string) error {
	if len(user) > 255 || len(pass) > 255 {
		return errors.New("代理用户名或口令过长（单段最多 255 字节）")
	}
	pkt := make([]byte, 0, 3+len(user)+len(pass))
	pkt = append(pkt, 0x01, byte(len(user)))
	pkt = append(pkt, user...)
	pkt = append(pkt, byte(len(pass)))
	pkt = append(pkt, pass...)
	if _, err := c.Write(pkt); err != nil {
		return fmt.Errorf("代理认证（送凭据）失败：%w", err)
	}
	var st [2]byte
	if _, err := io.ReadFull(c, st[:]); err != nil {
		return fmt.Errorf("代理认证（等结果）失败：%w", socksTimeoutHint(err))
	}
	if st[0] != 0x01 {
		return fmt.Errorf("代理认证回包格式不对（版本 0x%02x）", st[0])
	}
	if st[1] != 0x00 {
		return fmt.Errorf("代理拒绝了这组用户名口令（状态 0x%02x）", st[1])
	}
	return nil
}

func socks5RepText(code byte) string {
	switch code {
	case 0x01:
		return "请求被拒或一般失败"
	case 0x02:
		return "规则不允许连接"
	case 0x03:
		return "网络不可达"
	case 0x04:
		return "目标主机不可达"
	case 0x05:
		return "连接被目标拒绝"
	case 0x06:
		return "目标超时"
	case 0x07:
		return "该命令不被支持"
	case 0x08:
		return "地址类型不支持"
	default:
		return fmt.Sprintf("未知错误码 0x%02x", code)
	}
}

/* --------------------------------------------------------------------------- */
/* SOCKS4 / SOCKS4a                                                             */
/* --------------------------------------------------------------------------- */

func socks4Handshake(ctx context.Context, c net.Conn, pc ProxyConfig, host string, port int) error {
	// SOCKS4 的报文只能装 IPv4 字面量，所以域名得先在本地解析。这一步失败不代表
	// 连不上：SOCKS4a 用「IP 填 0.0.0.x（x≠0）+ 末尾附域名」的写法把解析推给代理，
	// 现代还活着的 SOCKS4 实现基本都认它。
	ip, err := resolveIPv4(ctx, host)
	b := make([]byte, 0, 16+len(host)+len(pc.User))
	b = append(b, 0x04, 0x01, byte(port>>8), byte(port))
	if err == nil {
		b = append(b, ip[0], ip[1], ip[2], ip[3])
	} else {
		b = append(b, 0x00, 0x00, 0x00, 0x01)
	}
	// userid 必须以 0 结束，即使为空 —— 少这个终止符会让后面的域名字节被当成
	// userid 吃掉，报出来的错就成了莫名的「代理拒绝」。
	b = append(b, pc.User...)
	b = append(b, 0x00)
	if err != nil {
		b = append(b, host...)
		b = append(b, 0x00)
	}
	if _, werr := c.Write(b); werr != nil {
		return fmt.Errorf("代理握手（SOCKS4 连 %s）失败：%w", host, werr)
	}
	var rep [8]byte
	if _, rerr := io.ReadFull(c, rep[:]); rerr != nil {
		return fmt.Errorf("代理握手（等 SOCKS4 结果）失败：%w", socksTimeoutHint(rerr))
	}
	if rep[0] != 0x00 {
		return fmt.Errorf("对方不是 SOCKS4 代理（回了版本字段 %d）", rep[0])
	}
	switch rep[1] {
	case 90:
		return nil
	case 91:
		return errors.New("SOCKS4 代理拒绝了请求（clientinfo：用户名或口令不对）")
	case 92:
		return errors.New("SOCKS4 代理连不上目标（identd 通了但被目标拒绝）")
	case 93:
		return errors.New("SOCKS4 代理与目标之间连接已断")
	default:
		return fmt.Errorf("SOCKS4 代理回了状态 %d", rep[1])
	}
}

func resolveIPv4(ctx context.Context, host string) (net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			return v4, nil
		}
		return nil, fmt.Errorf("%s 是 IPv6 地址，SOCKS4 装不下", host)
	}
	rctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIP(rctx, "ip4", host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("%s 没有 A 记录", host)
	}
	return ips[0].To4(), nil
}

/* --------------------------------------------------------------------------- */
/* HTTP CONNECT                                                                 */
/* --------------------------------------------------------------------------- */

func httpConnectHandshake(c net.Conn, pc ProxyConfig, host string, port int) error {
	var sb strings.Builder
	fmt.Fprintf(&sb, "CONNECT %s:%d HTTP/1.1\r\n", host, port)
	fmt.Fprintf(&sb, "Host: %s:%d\r\n", host, port)
	if pc.User != "" {
		sb.WriteString("Proxy-Authorization: Basic ")
		sb.WriteString(base64.StdEncoding.EncodeToString([]byte(pc.User + ":" + pc.Pass)))
		sb.WriteString("\r\n")
	}
	sb.WriteString("\r\n")
	if _, err := c.Write([]byte(sb.String())); err != nil {
		return fmt.Errorf("代理握手（CONNECT %s）失败：%w", host, err)
	}
	// 逐字节读到空行，不能拿 bufio.Reader 去读：它会预读一整块，把隧道里
	// 紧随其后的目标数据吸进自己的缓冲区再也没人取出来 —— BBS 的开场横幅常常正好
	// 就压在 200 那一行的后面，症状是每次连都少开头一截。
	var hdr []byte
	one := make([]byte, 1)
	for {
		if len(hdr) >= proxyHeaderLimit {
			return errors.New("代理响应头过长，不像正常响应")
		}
		if _, err := c.Read(one); err != nil {
			return fmt.Errorf("代理握手（等 CONNECT 结果）失败：%w", socksTimeoutHint(err))
		}
		hdr = append(hdr, one[0])
		if len(hdr) >= 4 && string(hdr[len(hdr)-4:]) == "\r\n\r\n" {
			break
		}
	}
	first := hdr
	if i := strings.Index(string(hdr), "\r\n"); i > 0 {
		first = hdr[:i]
	}
	code, err := httpStatusCode(string(first))
	if err != nil {
		return fmt.Errorf("代理响应看不懂：%s", strings.TrimSpace(string(first)))
	}
	if code < 200 || code > 299 {
		return fmt.Errorf("代理拒绝建隧：HTTP %d（%s）", code, strings.TrimSpace(string(first)))
	}
	return nil
}

func httpStatusCode(line string) (int, error) {
	fields := strings.Fields(line)
	if len(fields) < 2 || !strings.HasPrefix(fields[0], "HTTP/") {
		return 0, errors.New("不是状态行")
	}
	return strconv.Atoi(fields[1])
}

// socksTimeoutHint 把「超时」翻译成一句人话：裸的 context deadline exceeded 用户
// 分不清是自己网断了还是代理地址填错了。
func socksTimeoutHint(err error) error {
	if errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("等代理回话超时（%s）", proxyHandshakeTimeout)
	}
	return err
}

/* --------------------------------------------------------------------------- */
/* 给 http.Client 用                                                             */
/* --------------------------------------------------------------------------- */

// proxyAwareTransport 造一个会按当前全局代理设置出站的 transport，IP 库下载与链接
// 预览共用它 —— 用户在设置里配好代理，这两条路和终端连接就该一起通，不该各处再配
// 一遍。改设置不必重启：DialContext 每次拨号现读 activeProxy。
func proxyAwareTransport(idlePerHost int) *http.Transport {
	return &http.Transport{
		// 应用内配了代理就把环境变量那套让开：两层代理叠在一起时，报出来的错
		// 完全看不出流量究竟走了哪条路，不如一次只信一个来源。
		Proxy: func(req *http.Request) (*url.URL, error) {
			if globalProxy().usable() {
				return nil, nil
			}
			return http.ProxyFromEnvironment(req)
		},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return proxyDial(ctx, globalProxy(), network, addr)
		},
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		MaxIdleConnsPerHost:   idlePerHost,
	}
}

// proxyTest 拿一份代理设置去连 proxyTestTarget，走完整握手，成功返一句给界面显示的
// 话。它只验「能不能通过代理建一条隧到 GitHub」，不下载任何东西 —— 用户按「测试」
// 时想知道的就是这一步成不成。
func proxyTest(pc ProxyConfig) (string, error) {
	if !pc.usable() {
		return "", errors.New("还没填代理地址（类型或地址为空）")
	}
	ctx, cancel := context.WithTimeout(context.Background(), proxyDialTimeout+proxyHandshakeTimeout+10*time.Second)
	defer cancel()
	c, err := proxyDial(ctx, pc, "tcp", proxyTestTarget)
	if err != nil {
		return "", err
	}
	defer c.Close()
	return fmt.Sprintf("经 %s 连上了 %s", pc.label(), proxyTestTarget), nil
}
