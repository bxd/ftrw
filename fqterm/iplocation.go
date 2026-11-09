package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// IP 归属地查询（B8）。
//
// 原版的做法是「鼠标悬停命中屏幕上的 IPv4 → OSD 弹出 国家 城市」，数据来自本地
// 纯真库 qqwry.dat（见 fqterm_src/src/utilities/fqterm_ip_location.cpp）。这里保持
// 同样的数据源与同样的触发点，只是把 OSD 换成了状态区回显。
//
// 只用本地纯真库 qqwry.dat，不查在线归属地接口：BBS 年代这就是唯一来源，而在线
// 接口一要有外网、二要把用户正在看的 IP 发给第三方。库不存在时先从公开镜像自动
// 下一次（metowolf/qqwry.dat，每天最多自动一轮），下不动就明说让用户手动下载放到
// 设置里显示的路径 —— 绝不静默返回一个假归属地。

// qqwry 头 8 字节是索引区首/尾偏移，各占一个 4 字节小端整数（不是 3 字节！metowolf 那份
// 27MB 的库尾部偏移到了 0x01A2384C，按 3 字节读会把最高那一段 0x01 截掉，读出来是个
// 比头部还小的数，整份库都会被当成坏文件）。尾偏移指的是「最后一条索引
// 自己的位置」，不是它后面一个字节，条数 =（尾-首）/7 + 1。每条索引 7 字节 =
// 4 字节起始 IP + 3 字节记录偏移（区间上界不在索引里，是下一条的起始 IP）；
// 记录体第一个字节是字段 mode，0x01/0x02 后面跟着 3 字节重定向偏移。
const (
	qqWryHeaderSize  = 8
	qqWryIndexSize   = 7
	qqWryMaxDataSize = 128 << 20 // 纯真库实测 11~30MB（合库版本差异很大），超 128MB 的一定不是库文件
	qqWryMaxDepth    = 100       // 重定向递归上限，与原版的 100 一致
	qqWryFileName    = "qqwry.dat"
)

// qqWryLayoutSamples 是字节序 / 记录前缀这两项自检测各取多少个采样点。真库有几十万
// 条索引，几十个点足够把两种可能区分开了，而启动时全量扫一遍是白几十毫秒。
const qqWryLayoutSamples = 64

// qqwryAutoInterval 是自动更新的最小间隔：纯真库本身按天粒度发布，更频繁的去拉
// 只是白耗流量。间隔用 ipdb.stamp 文件的修改时间记账，不写进 prefs.json。
const qqwryAutoInterval = 24 * time.Hour

// qqwryStampName 记下「最近一次自动尝试」的时刻。下载失败也算尝试过：断网时每次
// 悬停都去撞一遍 GitHub 是最糟糕的体验。
const qqwryStampName = "ipdb.stamp"

// qqwryIdleTimeout 是「多久没收到新字节就算连接死了」。20 多 MB 的库经 GitHub
// 资产链下载，中途静默断流是实测会发生的，不能让它占着整体五分钟。
const qqwryIdleTimeout = 30 * time.Second

// qqwryDownloadTimeout 罩住整个传输（含重试），逐块 deadline 不设 —— 慢但一直在动的
// 下载应该让它跑完（库 20 多 MB，普通链路就是需要几十秒），卡死的判断交给连接与首字节超时。
const qqwryDownloadTimeout = 5 * time.Minute

// qqwrySources 是库文件的候选地址，按顺序试到成功为止。只走 release：metowolf/qqwry.dat
// 那个仓库本身只有一百多 KB，库里那个 20 多 MB 的 dat 是挂在 release 资产上的，
// raw.githubusercontent.com 与 jsDelivr 的 @main/qqwry.dat 两个直链都是 404（实际
// 试过）。releases/latest/download/… 是条稳定重定向，永远指向最新一版，不需要先问
// API。用户也可以在设置里填自己的地址，填了就只试那一个。
var qqwrySources = []string{
	"https://github.com/metowolf/qqwry.dat/releases/latest/download/qqwry.dat",
}

// qqwryAPIAsset 是兜底地点：仓库的 GitHub Releases API。github.com 直连被 reset
// 时，api.github.com 往往还能走；拿到 JSON 里的 browser_download_url 再下真文件。
const qqwryAPIAsset = "https://api.github.com/repos/metowolf/qqwry.dat/releases/latest"

// qqwryHTTP 只给下库用。不能拿 previewHTTP 复用：那个 client 没设整体 Timeout，
// 靠 context 罩着。这里的超时取 qqwryDownloadTimeout：库是 20 多 MB，不能再拿一个
// 默认一分钟的 client 去截它；卡死的判断交给连接与首字节超时。transport 用
// proxyAwareTransport，所以设置里配了代理之后这条最容易卡住的路也跟着通。
var qqwryHTTP = &http.Client{
	Timeout:   qqwryDownloadTimeout,
	Transport: proxyAwareTransport(2),
}

// IPUpdateProgress 是更新过程给界面看的一行字。进度文本全装在 Message 里（含百分比），
// 不再另发 done/total 让前端自己算：算法只该有一个，就在报数的那一层。
type IPUpdateProgress struct {
	State   string `json:"state"` // downloading | done | error
	Message string `json:"message"`
	Error   string `json:"error"`
}

// qqwryNoCountry 是库里的水印 / 广告字样。把它们当归属地显示比不显示更误事，
// 所以读到就等于没读到（原版同样把 CZ88.NET 剔掉）。
var qqwryNoCountry = []string{"CZ88.NET", "IPIP.CN", "净域IP"}

// qqwry 是整库读进内存后的只读视图。文件不在启动时加载：没放库的用户不该为这功能
// 付一次性读十几 MB 的代价，第一次真查 IP 时才打开。
//
// ipLE / endIPPfx 是两处库际差异的运行时自检结果，详见 detectIPOrder /
// detectEndIPPrefix：字节序读反会让二分查找彻底失序，结束 IP 前缀猜错则会把 GBK
// 字符串当成 IP 读 —— 两个都不该写死。
type qqwry struct {
	path       string
	data       []byte
	indexFirst uint32
	indexLast  uint32
	ipLE       bool
	endIPPfx   bool
}

func le24(b []byte, off int) uint32 {
	if off < 0 || off+3 > len(b) {
		return 0
	}
	return uint32(b[off]) | uint32(b[off+1])<<8 | uint32(b[off+2])<<16
}

func le32(b []byte, off int) uint32 {
	if off < 0 || off+4 > len(b) {
		return 0
	}
	return uint32(b[off]) | uint32(b[off+1])<<8 | uint32(b[off+2])<<16 | uint32(b[off+3])<<24
}

func be32(b []byte, off int) uint32 {
	if off < 0 || off+4 > len(b) {
		return 0
	}
	return uint32(b[off])<<24 | uint32(b[off+1])<<16 | uint32(b[off+2])<<8 | uint32(b[off+3])
}

// checkQQWryBytes 整份文件的结构校验：头部自洽之外，还得确认最后一条索引真的装得下。
// 一旦把 GitHub 的 404 页面、代理登录页之类的 HTML 存成库，之后每次查都会拿到
// 无意义的偏移然后读出乱码，比直接报失败难查得多。
func checkQQWryBytes(raw []byte) error {
	if err := checkQQWryHead(raw); err != nil {
		return err
	}
	// 真库把索引区放在文件末尾，所以头里那个「尾」是个很大但完全正常的数；
	// 只有拿到整份文件时才能拿它跟文件长度对。
	if last := int64(le32(raw, 4)); last+qqWryIndexSize > int64(len(raw)) {
		return fmt.Errorf("IP 库索引区尾落在文件外（%d..共 %d 字节）", last, len(raw))
	}
	return nil
}

// checkQQWryHead 只看头 8 字节自不自洽。下载刚开始手里只有一块、甚至只有 16 字节，
// 这时候拿不了文件长度（真库的索引区就在文件末尾，头部偏移必然远超 16）。
func checkQQWryHead(raw []byte) error {
	if len(raw) <= qqWryHeaderSize {
		return fmt.Errorf("IP 库过小（%d 字节）", len(raw))
	}
	first, last := le32(raw, 0), le32(raw, 4)
	if first < qqWryHeaderSize || last < first {
		return fmt.Errorf("IP 库头部索引区间非法（%d..%d / 共 %d 字节）", first, last, len(raw))
	}
	if (last-first)%qqWryIndexSize != 0 {
		return fmt.Errorf("IP 库索引区长度不是 %d 的整数倍（%d..%d）", qqWryIndexSize, first, last)
	}
	return nil
}

// loadQQWry 读库并校验头部。头不对（索引尾落在文件外、索引区倒挂）就当不是纯真库。
func loadQQWry(path string) (*qqwry, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.Size() > qqWryMaxDataSize {
		return nil, fmt.Errorf("IP 库大小异常（%d 字节）", fi.Size())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := checkQQWryBytes(raw); err != nil {
		return nil, err
	}
	db := &qqwry{path: path, data: raw, indexFirst: le32(raw, 0), indexLast: le32(raw, 4)}
	db.ipLE = detectIPOrder(db)
	db.endIPPfx = detectEndIPPrefix(db)
	return db, nil
}

// indexCount 索引条数。头部那个「尾」就是最后一条自己的位置，所以要 +1。
func (db *qqwry) indexCount() uint32 {
	return (db.indexLast-db.indexFirst)/qqWryIndexSize + 1
}

func (db *qqwry) entryOff(i uint32) int {
	return int(db.indexFirst) + int(i)*qqWryIndexSize
}

// ipAt 按自检出来的字节序读一个 4 字节 IP。
func (db *qqwry) ipAt(off int) uint32 {
	if db.ipLE {
		return le32(db.data, off)
	}
	return be32(db.data, off)
}

// detectIPOrder 判断索引里那 4 字节 IP 是不是小端存的。真库从头到尾都是小端（连 IP
// 也是），但字节序读反的后果不是「数字变小」而是整个序列不再递增，二分查找会稳定
// 地落到错的条目上 —— 与其赌一个，不如数一遍倒序次数。
func detectIPOrder(db *qqwry) bool {
	n := db.indexCount()
	if n < 2 {
		return true
	}
	var leBad, beBad int
	step := max(uint32(1), n/qqWryLayoutSamples)
	for i := uint32(0); i+step < n; i += step {
		a, b := db.entryOff(i), db.entryOff(i+step)
		if le32(db.data, a) > le32(db.data, b) {
			leBad++
		}
		if be32(db.data, a) > be32(db.data, b) {
			beBad++
		}
	}
	return leBad <= beBad
}

// detectEndIPPrefix 判断记录体开头有没有重复的那 4 字节结束 IP：早年的纯真库有（LumaQQ
// 和原版 FQTerm 那代代码因此一律 +4 跳过），后来的库把它去掉了、索引里那 3 字节直接
// 指向国家字段的 mode。判据很硬：段与段首尾相接，所以老格式下这 4 个字节按索引同样
// 的字节序解出来，正好等于下一条索引的起始 IP 减一。
func detectEndIPPrefix(db *qqwry) bool {
	n := db.indexCount()
	if n < 3 {
		return false
	}
	hits, total := 0, 0
	step := max(uint32(1), (n-1)/qqWryLayoutSamples)
	for i := uint32(0); i+1 < n; i += step {
		rec := int(le24(db.data, db.entryOff(i)+4))
		if rec < 0 || rec+4 >= len(db.data) {
			continue
		}
		total++
		if db.ipAt(rec)+1 == db.ipAt(db.entryOff(i+1)) {
			hits++
		}
	}
	if total == 0 {
		return false
	}
	// 采样点很少，不要求全中：过半就定性，免得个别写歪的条目把整个库判错
	return hits*2 > total
}

// resolve 查一个 IPv4。found=false 表示库里没有覆盖这段地址的索引项，或者字段
// 本身就落在水印区（CZ88.NET 之类），两种情况界面都只能报「无结果」。
func (db *qqwry) resolve(ip uint32) (country, city string, found bool) {
	// 先二分出「起始 IP 不超过查询 IP 的最后一条」。结束 IP 不存在索引项里，真库的段
	// 与段首尾相接，所以上界就是下一条的起始 IP；最后一条没有「下一条」，无上界。
	lo, hi := uint32(0), db.indexCount()-1
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if db.ipAt(db.entryOff(mid)) <= ip {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	entry := db.entryOff(lo)
	if entry+qqWryIndexSize > len(db.data) || db.ipAt(entry) > ip {
		return "", "", false // 查询 IP 比库里第一条还小
	}
	if lo+1 < db.indexCount() && ip >= db.ipAt(entry+qqWryIndexSize) {
		return "", "", false // 落在两段的空缺里，就是没覆盖
	}
	// 索引项的 3 字节记录偏移（entry+4 起）指的就是这条记录的头
	rec := int(le24(db.data, entry+4))
	if db.endIPPfx {
		rec += 4 // 老库在那里重复了一遍结束 IP，国家字段在它后面
	}
	country, city = db.readFields(rec)
	if country == "" && city == "" {
		return "", "", false
	}
	return country, city, true
}

// readFields 解析记录体的两个字段：记录体开头就是国家字段的 mode。mode 0x01 =
// 两个字段都跳到别处（背靠背放在落点）；mode 0x02 = 只有国家跳走，城市跟着
// 当前指针。国家字段最多被跳两次，而且第二跳一定是 0x02 —— 那种情况下地区就紧跟
// 在第二个标记字节后面，而不是跟着第一个落点的字符串（原版与 LumaQQ 都是这么读的）。
func (db *qqwry) readFields(rec int) (string, string) {
	if rec < 0 || rec >= len(db.data) {
		return "", ""
	}
	var country, city string
	switch db.dataAt(rec) {
	case 0x01:
		off := int(le24(db.data, rec+1))
		if db.dataAt(off) == 0x02 {
			country = db.strAt(db.deref(off))
			city = db.stringAt(off + 4)
		} else {
			country = db.stringAt(off)
			city = db.stringAt(db.nextString(off))
		}
	case 0x02:
		country = db.stringAt(db.deref(rec))
		city = db.stringAt(db.deref(rec + 4))
	default:
		country = db.stringAt(rec)
		city = db.stringAt(db.deref(db.nextString(rec)))
	}
	if city == "CZ88.NET" { // 纯真水印当不得地区名用
		city = ""
	}
	return strings.TrimSpace(country), strings.TrimSpace(city)
}

// stringAt 读一个字段起点上的字符串：0x00 是「空字段」（纯真把空地区写成裸 NUL），
// 0x01/0x02 先跟跳转，剩下的交给 strAt。字段长度、下一字段起点这些算术一律按
// 原始 GBK 字节算，不能拿解码后的 UTF-8 字符串长度算：一个中文 3 字节，差很多。
func (db *qqwry) stringAt(pos int) string {
	switch db.dataAt(pos) {
	case 0x00:
		return ""
	case 0x01, 0x02:
		return db.strAt(db.deref(pos))
	}
	return db.strAt(pos)
}

// deref 把 0x01/0x02 重定向链走到终点，返回实际字段的起点；落点非法（没写、越界、
// 或者跳成一个环）返回 -1，上层拿到 -1 就当这个字段是空的。上限 qqWryMaxDepth：
// 库写坏了会跳成环，原版靠 100 层计数挡住，这里一样。
func (db *qqwry) deref(pos int) int {
	for depth := 0; depth < qqWryMaxDepth; depth++ {
		if db.dataAt(pos) != 0x01 && db.dataAt(pos) != 0x02 {
			return pos
		}
		next := int(le24(db.data, pos+1))
		if next <= 0 || next >= len(db.data) || next == pos {
			return -1
		}
		pos = next
	}
	return -1
}

// nextString 返回紧跟在 pos 那个字符串之后的下一个字符串起点（跳过终止 NUL）。
// mode 0x01 重定向处的「国家+地区」就是这种背靠背写法。
func (db *qqwry) nextString(pos int) int {
	end := db.endOfString(pos)
	if end == -1 {
		return pos
	}
	return end + 1
}

// endOfString 给出 pos 处 NUL 终止字符串的终止字节下标；没 terminator 返 -1。
func (db *qqwry) endOfString(pos int) int {
	if pos < 0 {
		return -1
	}
	for i := pos; i < len(db.data); i++ {
		if db.data[i] == 0x00 {
			return i
		}
	}
	return -1
}

func (db *qqwry) dataAt(pos int) byte {
	if pos < 0 || pos >= len(db.data) {
		return 0
	}
	return db.data[pos]
}

// strAt 读一个以 0x00 结尾的 GBK 字段。越界、或字段开头就是终止符都返回空串。
// 注：这里不能拿「不小于 indexFirst」当合法性判据 —— 真库的记录区在索引区之前。
func (db *qqwry) strAt(pos int) string {
	if pos < 0 || pos >= len(db.data) {
		return ""
	}
	end := pos
	for end < len(db.data) && db.data[end] != 0x00 {
		end++
	}
	s := decodeGBK(db.data[pos:end])
	for _, bad := range qqwryNoCountry {
		if s == bad {
			return ""
		}
	}
	return s
}

// decodeGBK 把库里的 GBK 字节转 UTF-8。纯 ASCII 直接返回，非法字节交给 GB18030 兜底。
func decodeGBK(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	ascii := true
	for _, c := range b {
		if c >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		return string(b)
	}
	return decodeCharset(string(b), "gbk")
}

// --- 查询入口 ---

// ipUint32 解析点分 IPv4。打码段按原版补成 '1'：水木最常见的是末段打码
// （202.38.64.*），但不少站端连中间段一起打（114.242.*.*），也有用 x 打码的
// （114.242.x.x）—— 两种都认。全打码（*.*.*.*）问不出任何信息，不当它是地址。
func ipUint32(s string) (uint32, bool) {
	s = strings.TrimSpace(s)
	ip := net.ParseIP(s)
	if ip == nil || ip.To4() == nil {
		filled, ok := fillMaskedIPv4(s)
		if !ok {
			return 0, false
		}
		if ip = net.ParseIP(filled); ip == nil || ip.To4() == nil {
			return 0, false
		}
	}
	b := ip.To4()
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]), true
}

// isMaskToken 这一段是不是打码。除 * 之外还要认 x/X：国内站端两种写法都在用，
// 只认一种的结果就是鼠标停在地址上什么也不弹，而用户无从知道为什么不弹。
func isMaskToken(p string) bool { return p == "*" || p == "x" || p == "X" }

// fillMaskedIPv4 补齐打码的四段。补 '1' 而不是 '0'：补 0 会把 202.38.0.0 这种推向
// 保留段附近，而纯真库是按区间起点二分的，补 1 更稳地落在打码前缀自己那一格。
// 头一段就打码（*.*.*.*、*.38.64.1）问不出东西，直接拒。
func fillMaskedIPv4(s string) (string, bool) {
	parts := strings.Split(s, ".")
	if len(parts) != 4 || isMaskToken(parts[0]) {
		return "", false
	}
	for i, p := range parts {
		if isMaskToken(p) {
			parts[i] = "1"
		}
	}
	return strings.Join(parts, "."), true
}

// IPLocationResult 是一次归属地查询的回执。Detail 是给界面直接显示的一行字，
// 前端不需要自己拼「国家 城市」，也不需要知道错误是怎么分级的。
type IPLocationResult struct {
	Query   string `json:"query"`
	IP      string `json:"ip"`
	Country string `json:"country"`
	City    string `json:"city"`
	Detail  string `json:"detail"`
	Found   bool   `json:"found"`
	Error   string `json:"error"`
}

// IPService 暴露给前端：悬停查（可能一秒来好几次）与命令手查（ip.lookup:地址）。
// 库里查是纯内存二分，纳秒级；真正会阻塞的两件事是「域名 → IP」和「首次下库」，
// 前者靠 TTL 缓存，后者靠后台任务 + 每天最多自动一轮。
type IPService struct {
	store *Store
	win   *application.WebviewWindow

	mu       sync.Mutex
	db       *qqwry
	dbLoaded bool
	dbErr    error
	dbMTime  time.Time
	autodir  string
	updating bool
	task     int
	lastTry  time.Time
	dns      map[string]dnsCache
}

type dnsCache struct {
	ip      string
	expires time.Time
}

const (
	ipDNSPosTTL = 10 * time.Minute
	ipDNSNegTTL = time.Minute
)

func NewIPService(store *Store) *IPService {
	return &IPService{store: store, dns: make(map[string]dnsCache)}
}

// Bind 在 main 建好主窗口之后注入进度事件的去处。不绑也能查库，只是下载进度没人听。
//
//wails:ignore
func (s *IPService) Bind(win *application.WebviewWindow) {
	s.mu.Lock()
	s.win = win
	s.mu.Unlock()
}

// Lookup 查一个 IPv4 字面量或主机名。名字里的打码末段会被补全（原版对 202.38.64.*
// 也是这么处理的），补完仍不是合法 IP 才去解析域名。
func (s *IPService) Lookup(query string) IPLocationResult {
	q := strings.TrimSpace(query)
	if q == "" {
		return s.fail(q, "没有可查询的地址", false)
	}
	res := IPLocationResult{Query: q, IP: q}
	u32, ok := ipUint32(q)
	if !ok {
		// 库里还没有就不白等解析：水木那种打码 IP（末段 *）补成的 1.1.1.1 是个真
		// 地址，去 DNS 查它既慢又答的是 Cloudflare 的归属地
		if _, missing, err := s.database(); err != nil {
			if missing {
				go s.MaybeUpdate()
			}
			return s.fail(q, err.Error(), missing)
		}
		ip, err := s.resolveHost(q)
		if err != nil {
			return s.fail(q, fmt.Sprintf("无法解析主机名 %s：%v", q, err), false)
		}
		res.IP = ip
		if u32, ok = ipUint32(ip); !ok {
			return s.fail(q, "解析结果不是 IPv4 地址", false)
		}
	}
	db, missing, err := s.database()
	if err != nil {
		if missing {
			go s.MaybeUpdate()
		}
		return s.fail(q, err.Error(), missing)
	}
	country, city, found := db.resolve(u32)
	if !found {
		return s.fail(q, "库里没有这条记录", false)
	}
	res.Country, res.City = country, city
	res.Detail = fmt.Sprintf("%s  %s", res.IP, strings.TrimSpace(country+" "+city))
	res.Found = true
	// 成功也要留一行：查库在内存里、不写日志的话，「界面什么都没弹」就再也分不清
	// 是前端没认出地址，还是认出来了但显示环节掉了。
	log.Printf("IP 归属地 %q -> %s", q, res.Detail)
	return res
}

// Note 把前端那一格看到的东西写进日志。Alt+单击靠它报「这一行到底认出了什么」：
// 归属地没弹时光看界面分不出是没识别、没坐标还是没显示，而这三件事要改的地方完全不同。
func (s *IPService) Note(msg string) {
	log.Printf("IP 前端诊断 %s", strings.TrimSpace(msg))
}

// fail 保留已经填好的字段：错误信息要能说明「用户看的那个地址哪儿不对」，
// 而不是只剩一个「查询失败」。missing 只用来告诉界面「该不该去自动下库」，
// 它不进返回体：旧的前端拿到多的 JSON 字段无害，反过来才要命。
func (s *IPService) fail(query, msg string, missing bool) IPLocationResult {
	if missing {
		s.markAttempt()
	}
	log.Printf("IP 归属地查询失败 query=%q: %s", query, msg)
	return IPLocationResult{Query: query, IP: query, Error: msg, Detail: msg}
}

// resolveHost 只取第一个 A 记录，并且按结果缓存（失败的负缓存更短，免得解析恢复后
// 还有一分钟查不出来）。悬停会在同一个主机名上反复触发，这一层缓存就是给它的。
func (s *IPService) resolveHost(host string) (string, error) {
	host = strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
	if strings.ContainsAny(host, "/:?@") {
		return "", errors.New("看起来不像主机名")
	}
	s.mu.Lock()
	item, ok := s.dns[host]
	s.mu.Unlock()
	if ok && time.Now().Before(item.expires) {
		if item.ip == "" {
			return "", errors.New("上次解析就没有结果")
		}
		return item.ip, nil
	}
	var ip string
	var err error
	if raw, lookupErr := net.LookupIP(host); lookupErr == nil {
		for _, candidate := range raw {
			if v4 := candidate.To4(); v4 != nil {
				ip = v4.String()
				break
			}
		}
	} else {
		err = lookupErr
	}
	s.mu.Lock()
	if ip == "" {
		s.dns[host] = dnsCache{expires: time.Now().Add(ipDNSNegTTL)}
	} else {
		s.dns[host] = dnsCache{ip: ip, expires: time.Now().Add(ipDNSPosTTL)}
	}
	s.mu.Unlock()
	if ip == "" {
		if err == nil {
			err = errors.New("没有 A 记录")
		}
		return "", err
	}
	return ip, nil
}

// database 懒加载并缓存。missing 用来区分「根本还没有库」与「有文件但读不成」：
// 前者该去自动下载，后者重试一万次也不会好，只会把用户自己的文件越写越坏。
//
// 失败原因只记住「没找到」：库坏了的情况下次调用要重新探测 —— 自动更新刚写下新
// 文件、或者用户手工把库放进目录之后，都必须在同一个进程里能立即查到。
func (s *IPService) database() (*qqwry, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dbLoaded && s.db != nil {
		if fi, err := os.Stat(s.db.path); err == nil && !fi.ModTime().After(s.dbMTime) {
			return s.db, false, nil
		}
		s.db, s.dbLoaded, s.dbErr = nil, false, nil // 文件被换过（手动替换 / 刚自动更新完）
	}
	s.dbLoaded = true
	path, err := findQQWry(s.prefDBPath(), executableDir(), s.homeDirLocked())
	if err != nil {
		s.dbErr = err
		return nil, true, err
	}
	db, err := loadQQWry(path)
	if err != nil {
		s.dbErr = err
		log.Printf("IP 库打开失败 path=%s: %v", path, err)
		return nil, false, fmt.Errorf("IP 库不可用（%s）：%v", path, err)
	}
	s.db = db
	s.dbMTime = db.mtime()
	s.dbErr = nil
	log.Printf("纯真 IP 库已载入 path=%s size=%d 字节序=%s 记录前缀=%v", path, len(db.data), db.orderName(), db.endIPPfx)
	return db, false, nil
}

// orderName 把自检出来的字节序拼成一句日志可读的话。
func (db *qqwry) orderName() string {
	if db.ipLE {
		return "小端"
	}
	return "大端"
}

func (db *qqwry) mtime() time.Time {
	fi, err := os.Stat(db.path)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

func (s *IPService) homeDirLocked() string {
	if s.store == nil {
		return ""
	}
	return s.store.Dir()
}

// targetLocked 决定新库写哪儿。用户在设置里填了路径就写那儿（写坏了也是他自己选的）；
// 否则写配置目录。autodir 记下「自动下载落点」，给失败提示用。
func (s *IPService) targetLocked() string {
	if p := strings.TrimSpace(s.prefDBPath()); p != "" {
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			return filepath.Join(p, qqWryFileName)
		}
		return p
	}
	return s.autoPathLocked()
}

func (s *IPService) autoPathLocked() string {
	s.autodir = s.homeDirLocked()
	if s.autodir == "" {
		return qqWryFileName
	}
	return filepath.Join(s.autodir, qqWryFileName)
}

// prefDBPath 读偏好里显式指定的库路径。直接摸 store.Prefs 而不是调 GetPreferences：
// 后者会拿 store 的读锁，而我们可能已经持着自己的锁。
func (s *IPService) prefDBPath() string {
	if s.store == nil {
		return ""
	}
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	return s.store.Prefs.IPLocationDb
}

func (s *IPService) prefURL() string {
	if s.store == nil {
		return ""
	}
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	return strings.TrimSpace(s.store.Prefs.IPLocationUrl)
}

// --- 在线更新 ---

// StartUpdate 立刻起一轮更新，返回任务号。前端先注册好进度事件再调它，
// 重复调用会拿到同一个任务号（正在跑的那一轮）。
func (s *IPService) StartUpdate() int {
	s.mu.Lock()
	if s.updating {
		id := s.task
		s.mu.Unlock()
		return id
	}
	s.task++
	id := s.task
	s.updating = true
	s.mu.Unlock()
	go s.performUpdate(id)
	return id
}

// MaybeUpdate 给启动与悬停自触发用：只有「本地压根没有库」或「距上次尝试超过一天」
// 才真去跑一轮，剩下的静默返 0。功能被关掉时不跑 —— 那等于用户明说了不要这个能力，
// 不该还去碰 GitHub。
func (s *IPService) MaybeUpdate() int {
	if !s.enabled() || !s.needsAutoUpdate() {
		return 0
	}
	return s.StartUpdate()
}

func (s *IPService) enabled() bool {
	if s.store == nil {
		return true
	}
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	return s.store.Prefs.IPLocationEnabled
}

func (s *IPService) needsAutoUpdate() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	target := s.targetLocked()
	if _, err := os.Stat(target); err != nil {
		return true // 没有库：不管之前试过没试过，都得再来一次（看闸门）
	}
	if !s.lastTry.IsZero() && time.Since(s.lastTry) < qqwryAutoInterval {
		return false
	}
	stamp, err := os.Stat(filepath.Join(s.homeDirLocked(), qqwryStampName))
	if err != nil {
		return true
	}
	return time.Since(stamp.ModTime()) >= qqwryAutoInterval
}

// markAttempt 给「查库时发现没有库」记一笔，防止断网时每次悬停都去碰一下 GitHub。
func (s *IPService) markAttempt() {
	s.mu.Lock()
	s.lastTry = time.Now()
	s.mu.Unlock()
}

func (s *IPService) performUpdate(id int) {
	msg, err := s.downloadQQWry(id)
	s.mu.Lock()
	s.updating = false
	s.lastTry = time.Now()
	s.touchStampLocked()
	s.db, s.dbLoaded, s.dbErr = nil, false, nil // 本轮无论成败都重评，成了下一次查就拿到新库
	s.mu.Unlock()
	res := IPUpdateProgress{State: "done", Message: msg}
	if err != nil {
		res.State = "error"
		res.Error = err.Error()
	}
	s.publish(res)
	log.Printf("IP 库更新结束 task=%d: %s %v", id, msg, err)
}

// downloadQQWry 把库下到一个 .part 文件、校验头部结构、然后才改名覆盖。
// 下一半断、或者拿回一个 HTML 错误页，都不能把原来能用的库挡掉。
func (s *IPService) downloadQQWry(id int) (string, error) {
	if !qqwryNetAvailable() {
		return "", errors.New("没有可用网络，自动更新跳过了；可自行下载 qqwry.dat 放到设置里显示的路径")
	}
	s.mu.Lock()
	target := s.targetLocked()
	s.mu.Unlock()
	if dir := filepath.Dir(target); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("无法创建库目录 %s：%v", dir, err)
		}
	}
	// 半截文件的名字必须是定死的：库有 20 多 MB，慢链路上一轮跑不完是常态，
	// 带上 pid 就等于每轮从零开始。传输失败时也不删它，下一轮接着写。
	part := target + ".part"
	// 另一半身份：.part 属于哪个下载地址，续传前要靠它判断（换源就不能接）。
	marker := part + ".src"
	// 早先的版本把 pid 写进了后缀，那些遗留物不会自己消失，而一份就是 20 多 MB。
	// qqwry.dat.part 不匹配这个 glob，正在续传的那份不会被误删。
	if ms, err := filepath.Glob(target + ".*.part"); err == nil {
		for _, m := range ms {
			_ = os.Remove(m)
		}
	}

	names := s.sourceNames()
	prog := newIPProgress(s)
	var lastErr error
	for i, u := range names {
		s.publish(IPUpdateProgress{
			State:   "downloading",
			Message: fmt.Sprintf("正在下载 IP 库（%d/%d）：%s", i+1, len(names), shortURL(u)),
		})
		got, skipped, err := fetchQQWry(u, part, target, prog.report)
		if err != nil {
			lastErr = err
			log.Printf("IP 库下载失败 url=%s: %v", u, err)
			continue
		}
		if skipped {
			// 对方说没更新过，半截文件就再也没机会补全（它对应的不是任何一个完整版本）
			_ = os.Remove(part)
			_ = os.Remove(marker)
			return "本地库已是最新，无需更新", nil
		}
		if err := os.Rename(part, target); err != nil {
			return "", fmt.Errorf("写入库文件失败 %s：%v", target, err)
		}
		return fmt.Sprintf("IP 库已更新（%s，%s）", filepath.Base(u), humanSize(got)), nil
	}
	if lastErr == nil {
		lastErr = errors.New("所有地址都没能下到库")
	}
	return "", fmt.Errorf("自动更新失败：%v；可手动下载 qqwry.dat 放到 %s", lastErr, target)
}

// sourceNames 拼地址列表：设置里填了就只试那一个，没填则用 release 直链加一个
// GitHub API 兜底（github.com 被劫持成 ERR_CONNECTION_RESET 时，api 域名往往还在）。
func (s *IPService) sourceNames() []string {
	if custom := s.prefURL(); custom != "" {
		return []string{custom}
	}
	out := append([]string(nil), qqwrySources...)
	return append(out, qqwryAPIAsset)
}

// touchStampLocked 记下「最近一次自动尝试」。写不写成功都不影响查询，只是下一次
// 启动可能多一次无效重试。
func (s *IPService) touchStampLocked() {
	home := s.homeDirLocked()
	if home == "" {
		return
	}
	_ = os.WriteFile(filepath.Join(home, qqwryStampName), []byte(time.Now().Format(time.RFC3339)), 0o644)
}

// Status 给设置页看：现在用的库在哪、多大、最后一次更新时间，以及自动下载会落到哪。
func (s *IPService) Status() (string, error) {
	db, _, err := s.database()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s（%s，更新于 %s）", db.path, humanSize(int64(len(db.data))), db.mtime().Format("2006-01-02")), nil
}

// Reload 丢掉缓存（库换了文件、或者第一次没找到现在放好了）。设置里改了库路径或
// 下载地址之后必须叫一次，不然这一轮还是拿着旧文件。
func (s *IPService) Reload() {
	s.mu.Lock()
	s.db, s.dbLoaded, s.dbErr, s.dbMTime = nil, false, nil, time.Time{}
	s.mu.Unlock()
}

func (s *IPService) publish(p IPUpdateProgress) {
	s.mu.Lock()
	win := s.win
	s.mu.Unlock()
	if win == nil {
		return
	}
	win.EmitEvent("ip:update", p)
}

// fetchQQWry 下一个库到 dst。condPath（当前正在用着的那份库）的 mtime 会被当成
// If-Modified-Since 发出去：对方回了 304 就原样保留旧文件（skipped=true），从而不把
// 整份库重下一遍。地址指向 GitHub API 时先问一跳拿 release 里的真下载落点。
func fetchQQWry(rawURL, dst, condPath string, onProgress func(int64, int64)) (int64, bool, error) {
	if !strings.HasPrefix(rawURL, "https://api.github.com/") {
		return fetchQQWryURL(rawURL, dst, condPath, onProgress)
	}
	// 走了 API 兜底这条路之后本身还得有兜底：browser_download_url 仍然要连
	// github.com，而 github.com 被 reset、api.github.com 还能走的网络并不少见
	// （取证时实打实撞上了）。第二个落点是 api 域名下的资产端点，带上了
	// Accept: application/octet-stream 就直接回文件本体。
	urls, err := releaseAssetURLs(rawURL)
	if err != nil {
		return 0, false, err
	}
	var lastErr error
	for _, u := range urls {
		got, skipped, err := fetchQQWryURL(u, dst, condPath, onProgress)
		if err == nil {
			return got, skipped, nil
		}
		lastErr = err
		log.Printf("IP 库经 API 兜底下载失败 url=%s: %v", u, err)
	}
	return 0, false, lastErr
}

func fetchQQWryURL(rawURL, dst, condPath string, onProgress func(int64, int64)) (int64, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), qqwryDownloadTimeout)
	defer cancel()

	// 断在半途的连接不会自己报错：对方只是不再发字节（GitHub 在部分链路就是这副
	// 长相 —— 下到 600KB 就停住不发）。所以另加一层「多久没新数据就算死」的判断，
	// 好让下一块地点有机会，而不是干等到整体超时才回来。
	idleCtx, cancelIdle := context.WithCancel(ctx)
	defer cancelIdle()
	var lastByteAt atomic.Int64
	var stalled atomic.Bool
	lastByteAt.Store(time.Now().UnixNano())
	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		tk := time.NewTicker(2 * time.Second)
		defer tk.Stop()
		for {
			select {
			case <-watchDone:
				return
			case <-tk.C:
				if time.Since(time.Unix(0, lastByteAt.Load())) > qqwryIdleTimeout {
					stalled.Store(true)
					cancelIdle()
					return
				}
			}
		}
	}()
	stallErr := func(err error) error {
		if stalled.Load() {
			return fmt.Errorf("传输停滞（%s 没收到新字节）", qqwryIdleTimeout)
		}
		return err
	}

	// 续传：先验手里这份 .part 值不值得接着写。两个条件缺一不可 —— 开头 16 字节确实
	// 像一份纯真库（上次下的其实是 HTML 错误页的话，头部自检就过不去），而且上次下
	// 的就是这个地址。换镜像等于换一份字节流，拿半截去接另一份的尾巴，拼出来的东西
	// 长度像样、头部也自洽，查起来全是错答案 —— 比下载失败糟得多。
	marker := dst + ".src"
	var resume int64
	var savedHead [16]byte
	if fi, err := os.Stat(dst); err == nil && fi.Size() > qqWryHeaderSize {
		from, readErr := os.ReadFile(marker)
		if readErr != nil || strings.TrimSpace(string(from)) != rawURL {
			log.Printf("IP 库半截文件作废（地址从 %q 换成了 %q）", strings.TrimSpace(string(from)), rawURL)
			_ = os.Remove(dst)
			_ = os.Remove(marker)
		} else if h, err := readHeadOf(dst, len(savedHead)); err != nil || len(h) < len(savedHead) || checkQQWryHead(h) != nil {
			_ = os.Remove(dst)
			_ = os.Remove(marker)
		} else {
			resume = fi.Size()
			copy(savedHead[:], h)
		}
	}

	req, err := http.NewRequestWithContext(idleCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, false, err
	}
	req.Header.Set("User-Agent", previewUserAgent)
	// 只有这一条是 github API 资产端点必需的（不给就回 JSON）；拿它扫普通静态
	// 文件地址也无害，对方最多是不理这个头。
	req.Header.Set("Accept", "application/octet-stream")
	if resume > 0 {
		// 续传时不发 If-Modified-Since：拿旧库的 mtime 去问只会换来一个 304，把这份
		// 马上要补全的半截文件判死 —— 它对应的并不是任何一个完整版本。
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", resume))
	} else if fi, err := os.Stat(condPath); err == nil && !fi.ModTime().IsZero() {
		req.Header.Set("If-Modified-Since", fi.ModTime().UTC().Format(http.TimeFormat))
	}
	resp, err := qqwryHTTP.Do(req)
	if err != nil {
		return 0, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return 0, true, nil
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return 0, false, fmt.Errorf("HTTP %s", resp.Status)
	}
	// 对方回了 200 全量 = 理都不理 Range。这时绝不能往 .part 后面追加，归零整份重下。
	if resume > 0 && !respHasRange(resp, resume) {
		log.Printf("IP 库续传未被接受 url=%s resume=%d status=%s，整份重下", rawURL, resume, resp.Status)
		resume = 0
	}
	f, err := openQQWryPart(dst, resume)
	if err != nil {
		return 0, false, err
	}
	defer f.Close()
	if resume > 0 {
		log.Printf("IP 库续传 url=%s 从 %d 字节接着下", rawURL, resume)
	}
	// 记下这份 .part 属于哪个地址（上面决定要不要续传时读的就是它）。
	_ = os.WriteFile(marker, []byte(rawURL), 0o644)
	buf := make([]byte, 256*1024)
	got := resume // 磁盘上已有的字节数，含续传起点
	var head [16]byte
	headed := resume > 0
	if headed {
		copy(head[:], savedHead[:])
	}
	// 进度总数：206 的 Content-Length 是「还差多少」，得加上已有的；200 就是全量。
	// 对方不给长度（分块传输）时传 0，让上层只显示已下字节数。
	var total int64
	if resp.ContentLength > 0 {
		total = resume + resp.ContentLength
	}
	for {
		if err := idleCtx.Err(); err != nil {
			return got, false, stallErr(err)
		}
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			lastByteAt.Store(time.Now().UnixNano())
			if !headed {
				if n >= len(head) {
					copy(head[:], buf[:len(head)])
					if err := checkQQWryHead(head[:]); err != nil {
						os.Remove(dst) // 留着它下一轮还会想去续传一个 HTML
						return got, false, fmt.Errorf("下回来的不是纯真库：%v", err)
					}
					headed = true
				} else if n > 4 && looksLikeTextHead(buf[:n]) {
					os.Remove(dst)
					return got, false, errors.New("下回来的是网页而不是库文件（可能被代理或防火墙拦了）")
				}
			}
			if _, err := f.Write(buf[:n]); err != nil {
				return got, false, err
			}
			got += int64(n)
			if onProgress != nil {
				onProgress(got, total)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return got, false, stallErr(readErr)
		}
	}
	if err := f.Close(); err != nil {
		return got, false, err
	}
	if !headed || got <= qqWryHeaderSize {
		os.Remove(dst)
		return got, false, fmt.Errorf("下载不完整（%d 字节）", got)
	}
	// 收尾再补上「索引尾落在文件内」这一条：开头那 16 字节判不了，但现在知道总长了
	if last := int64(le32(head[:], 4)); last+qqWryIndexSize > got {
		os.Remove(dst)
		return got, false, fmt.Errorf("下回来的库结构不对：索引尾 %d 超出了文件长度 %d", last, got)
	}
	_ = os.Remove(marker)
	return got, false, nil
}

// openQQWryPart 按「有没有续传起点」决定打开方式：有就接在后面写，没有就清空重写。
// 少了 O_TRUNC 这一支，一旦对方不理 Range 就会拼出「旧半截 + 新全量」的文件，
// 头部自检与索引尾检查都可能照样通过，查出来的归属地却是彻底错的。
func openQQWryPart(dst string, resume int64) (*os.File, error) {
	flags := os.O_WRONLY | os.O_CREATE
	if resume > 0 {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	return os.OpenFile(dst, flags, 0o644)
}

// readHeadOf 读文件开头 n 字节，不足 n 就返回实际拿到的那些（长度不够交给调用方判）。
func readHeadOf(path string, n int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, n)
	got, err := io.ReadFull(f, buf)
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		err = nil
	}
	if err != nil {
		return nil, err
	}
	return buf[:got], nil
}

// respHasRange 判对方是不是真按我们要的起点回了半截。只看 206 不够 —— 得对上
// Content-Range 里的起始字节，个别服务会回一个从别处开始的 206。
func respHasRange(resp *http.Response, resume int64) bool {
	if resp.StatusCode != http.StatusPartialContent {
		return false
	}
	cr := resp.Header.Get("Content-Range")
	if cr == "" {
		return true // 206 却不给 Content-Range：少见，只能当我们请求的那段
	}
	fields := strings.Fields(cr)
	if len(fields) < 2 || !strings.EqualFold(fields[0], "bytes") {
		return false
	}
	spec := fields[1]
	if slash := strings.IndexByte(spec, '/'); slash >= 0 {
		spec = spec[:slash]
	}
	dash := strings.IndexByte(spec, '-')
	if dash < 0 {
		return false
	}
	start, err := strconv.ParseInt(spec[:dash], 10, 64)
	return err == nil && start == resume
}

// looksLikeTextHead 粗判「这是 HTML/JSON 而不是一堆定长偏移」：开头都是可打印 ASCII
// 而且没一个 NULL，基本就是错误页。纯真库头两个偏移虽然也是小整数，但紧跟的一堆
// 索引字节里必然有 0x00，不会整片都是可打印字符。
func looksLikeTextHead(b []byte) bool {
	printable := 0
	for _, c := range b {
		if c == 0 {
			return false
		}
		if c >= 0x20 && c < 0x7f {
			printable++
		}
	}
	return printable == len(b)
}

// releaseAssetURLs 从 GitHub API 的 latest release 里拿 qqwry.dat 的下载落点，按可达性
// 排序：先是浏览器直链（走 github.com），后是 api 域名下的资产端点（同样的文件，
// 换一个域名）。拿到手仍是个 JSON，所以这一跳只在「直链挂了」才值得走。
func releaseAssetURLs(apiURL string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", previewUserAgent)
	resp, err := qqwryHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}
	var payload struct {
		Assets []struct {
			URL                string `json:"url"`
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return nil, err
	}
	var out []string
	for _, a := range payload.Assets {
		if !strings.EqualFold(a.Name, qqWryFileName) {
			continue
		}
		if a.BrowserDownloadURL != "" {
			out = append(out, a.BrowserDownloadURL)
		}
		if a.URL != "" {
			out = append(out, a.URL)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("release 里没有 qqwry.dat")
	}
	return out, nil
}

// qqwryNetAvailable 只回答「有没有非回环的、_up_的网络接口」，不去拨号验证能不能出
// 网。它的价值在于：拔了网线 / 没连 WiFi 时给用户一句明说的提示，而不是一堆
// getaddrinfo 超时拼出来的技术错误。
func qqwryNetAvailable() bool {
	ifaces, err := net.Interfaces()
	if err != nil {
		return true // 拿不到接口名单不代表没网，照常去试
	}
	for _, i := range ifaces {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		return true
	}
	return false
}

// ipProgressReporter 把「已下载字节数」节流成事件：每读一块就发一次会把 webview 的事件
// 通道淹死（10MB / 256KB 也有 40 次），而 512KB 或 300ms 一跳已经足够看出速度了。
type ipProgressReporter struct {
	svc   *IPService
	step  int64
	until time.Time
}

func newIPProgress(svc *IPService) *ipProgressReporter {
	return &ipProgressReporter{svc: svc, step: 512 * 1024}
}

func (r *ipProgressReporter) report(done, total int64) {
	if done < r.step && done != total && time.Now().Before(r.until) {
		return
	}
	r.step = done + 512*1024
	r.until = time.Now().Add(300 * time.Millisecond)
	text := fmt.Sprintf("已下载 %s", humanSize(done))
	if total > 0 {
		text = fmt.Sprintf("已下载 %s / %s（%d%%）", humanSize(done), humanSize(total), done*100/total)
	}
	r.svc.publish(IPUpdateProgress{State: "downloading", Message: text})
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return strconv.FormatFloat(float64(n)/float64(div), 'f', 1, 64) + " " + "KMGTPE"[exp:exp+1] + "B"
}

func shortURL(u string) string {
	if i := strings.Index(u, "://"); i >= 0 {
		rest := u[i+3:]
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			rest = rest[j+1:]
		}
		if len(rest) > 48 {
			rest = rest[:48] + "…"
		}
		return rest
	}
	return u
}

// findQQWry 依次试：偏好里的显式路径 → 配置目录 → exe 同目录 → 原版配置目录。
// 目录里找文件名大小写不敏感（原版本身就是大小写不敏感匹配 qqwry.dat）。
// 提示只列目录不列文件名，免得出路一长串看不过来。
func findQQWry(prefPath, exeDir, home string) (string, error) {
	var tried []string
	seen := make(map[string]bool)
	tryFile := func(path string) (string, bool) {
		path = strings.TrimSpace(path)
		if path == "" || seen[strings.ToLower(path)] {
			return "", false
		}
		seen[strings.ToLower(path)] = true
		tried = append(tried, path)
		fi, err := os.Stat(path)
		if err != nil || fi.IsDir() {
			return "", false
		}
		return path, true
	}
	if prefPath != "" {
		if fi, err := os.Stat(prefPath); err == nil && fi.IsDir() {
			if p, ok := qqwryInDir(prefPath); ok {
				return p, nil
			}
			tried = append(tried, filepath.Join(prefPath, qqWryFileName))
		} else if p, ok := tryFile(prefPath); ok {
			return p, nil
		}
	}
	dirs := []string{}
	if home != "" {
		dirs = append(dirs, home)
	}
	if exeDir != "" {
		dirs = append(dirs, exeDir)
	}
	if base, err := os.UserConfigDir(); err == nil {
		dirs = append(dirs, filepath.Join(base, "FQTerm"))
	}
	dirs = append(dirs, ".")
	for _, dir := range dirs {
		if p, ok := qqwryInDir(dir); ok {
			return p, nil
		}
		tried = append(tried, filepath.Join(dir, qqWryFileName))
	}
	return "", fmt.Errorf("未找到纯真 IP 库 %s；可在设置里指定路径或手动下载放到：%s", qqWryFileName, strings.Join(tried, "；"))
}

func qqwryInDir(dir string) (string, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.EqualFold(e.Name(), qqWryFileName) {
			return filepath.Join(dir, e.Name()), true
		}
	}
	return "", false
}

func executableDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}
