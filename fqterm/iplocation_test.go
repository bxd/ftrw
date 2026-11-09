package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

// 这些测试一律不碰网络：库的内容用代码合成，覆盖「三种重定向写法 + GBK + 水印 +
// 边界」这些纯真格式的全部花样。只有 TestQQWryRealDatabase 是个例外：它读一份用户
// 指定的本地真库，不设环境变量就静默跳过。

// gbkForTest 把字符串编成 GBK 字节。不带终止符：测试里自己补 0x00，这样「空字段只写
// 一个裸 NUL」这种真库写法才表达得出来。
func gbkForTest(t *testing.T, s string) []byte {
	t.Helper()
	if s == "" {
		return nil
	}
	b, _, err := transform.String(simplifiedchinese.GB18030.NewEncoder(), s)
	if err != nil {
		t.Fatalf("GBK 编码 %q 失败: %v", s, err)
	}
	return []byte(b)
}

func put24(b []byte, off int, v uint32) {
	b[off] = byte(v)
	b[off+1] = byte(v >> 8)
	b[off+2] = byte(v >> 16)
}

// put24Append 往下追加一个 3 字节小端偏移。put24 只能写已分配好的空间，而记录区
// 是一路 append 长大的，直接拿它写 len(rec) 会越界。
func put24Append(b []byte, v uint32) []byte {
	var t [3]byte
	put24(t[:], 0, v)
	return append(b, t[:]...)
}

type qqWryEntry struct {
	firstIP, lastIP uint32
	country, city   string
}

// qqWryOpts 要合成哪种库。真库的两处代际差异（索引 IP 的字节序、记录体开头那 4 字节
// 结束 IP）都靠 loadQQWry 自检，所以这里得能把两种都摆出来让自检去认。
type qqWryOpts struct {
	entries  []qqWryEntry
	bothAt   int  // 这一条改用 0x01 背靠背写法；-1 表示不要这个样本
	chainAt  int  // 这一条的重定向落点指向上一个重定向标记，逼出两跳；-1 表示不要
	endIPPfx bool // 老格式：记录体开头重复一遍结束 IP
	ipBE     bool // 索引 IP 按网络字节序写（真库不是，用来验证字节序自检）
}

// ipBytes 编一个 4 字节 IP。真库连 IP 都是小端存的（首字节在最高位那个数值
// 直接 little-endian 落盘），所以默认走小端；ipBE 是为了让自检能被「没自检」证伪。
func ipBytes(v uint32, be bool) []byte {
	if be {
		return []byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}
	}
	return []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)}
}

func putIP(b []byte, off int, v uint32, be bool) {
	copy(b[off:off+4], ipBytes(v, be))
}

// buildQQWryBytes 造一个结构合法的迷你库：先把记录区一条条拼出来并记下每条偏移，
// 再回头写索引区。国家字段按「第二次出现改用 0x02 重定向」写；bothAt 那条整条记录
// 只有「0x01 + 3 字节偏移」，落点是记录区末尾另附的「国家+地区」背靠背字串（真库就是
// 这么复用字段的）；chainAt 那条把重定向指向上一个重定向标记，真库里这几种写法都有。
//
// 本测试把索引区排在记录区之前，跟真库相反；解析器只认头里那两个偏移和记录里的绝对
// 偏移，不关心谁前谁后（strAt 也特意不拿 indexFirst 当合法性判据）。
func buildQQWryBytes(t *testing.T, o qqWryOpts) []byte {
	t.Helper()
	entries := o.entries
	if len(entries) == 0 {
		t.Fatal("至少要有一条索引")
	}
	recArea := qqWryHeaderSize + len(entries)*qqWryIndexSize
	rec := make([]byte, 0, 256)
	offsets := make([]int, len(entries))
	countryAt := make([]int, len(entries)) // 每条记录国家字段的起点（重定向都指向这里）
	redirectAt := map[string]int{}         // 该国家最近一个 0x02 标记的位置，链式重定向指这里
	seenCountry := map[string]int{}
	bothPatch := -1 // 0x01 那条的落点字段在 rec 里的下标，循环末尾回填
	for i, e := range entries {
		offsets[i] = recArea + len(rec)
		if o.endIPPfx {
			rec = append(rec, ipBytes(e.lastIP, o.ipBE)...)
		}
		countryAt[i] = recArea + len(rec)
		if i == o.bothAt {
			rec = append(rec, 0x01)
			bothPatch = len(rec)
			rec = put24Append(rec, 0) // 落点还没算出来，先占位
			continue
		}
		if at, ok := seenCountry[e.country]; ok {
			target := countryAt[at]
			if i == o.chainAt {
				if mark, ok := redirectAt[e.country]; ok {
					target = mark // 落点自己也是个 0x02，逼 deref 走两跳
				}
			}
			redirectAt[e.country] = recArea + len(rec)
			rec = append(rec, 0x02)
			rec = put24Append(rec, uint32(target))
		} else {
			seenCountry[e.country] = i
			rec = append(rec, gbkForTest(t, e.country)...)
			rec = append(rec, 0x00)
		}
		rec = append(rec, gbkForTest(t, e.city)...)
		rec = append(rec, 0x00)
	}
	if bothPatch >= 0 {
		e := entries[o.bothAt]
		put24(rec, bothPatch, uint32(recArea+len(rec)))
		rec = append(rec, gbkForTest(t, e.country)...)
		rec = append(rec, 0x00)
		rec = append(rec, gbkForTest(t, e.city)...)
		rec = append(rec, 0x00)
	}
	out := make([]byte, recArea+len(rec))
	put24(out, 0, uint32(qqWryHeaderSize))
	// 头里的「尾」是最后一条索引自己的位置，不是它后面一个字节
	put24(out, 4, uint32(recArea-qqWryIndexSize))
	for i, e := range entries {
		at := qqWryHeaderSize + i*qqWryIndexSize
		putIP(out, at, e.firstIP, o.ipBE)
		// 记录偏移占 7 字节里的后 3 个（从 at+4 起）。写偏一个字节就会盖住下一条
		// 的起始 IP，整张表全错位
		put24(out, at+4, uint32(offsets[i]))
	}
	copy(out[recArea:], rec)
	return out
}

// qqWryLayoutEntries 是后面几个测试共用的索引表：段与段首尾相接（真库就是这样），而且
// 第 3/4 个字节得真有变化 —— 全在最高一字节上不同的话，两种字节序读出来都是递增，
// 字节序自检从原理上就分不出两者，拿它就没法测。
func qqWryLayoutEntries() []qqWryEntry {
	return []qqWryEntry{
		{ipRange("1.0.0.0"), ipRange("1.255.255.255"), "北京市", "海淀区"},
		{ipRange("2.0.0.0"), ipRange("3.0.0.255"), "广东省", "深圳市"},
		{ipRange("3.0.1.0"), ipRange("3.255.255.255"), "北京市", "朝阳区"},  // 0x02 重定向到首条国家
		{ipRange("4.0.0.0"), ipRange("5.0.1.255"), "天津市", ""},         // 空城市：字段直接接 NUL
		{ipRange("5.0.2.0"), ipRange("5.255.255.255"), "上海市", "浦东新区"}, // 0x01 背靠背
		{ipRange("6.0.0.0"), ipRange("7.0.8.255"), "北京市", "CZ88.NET"}, // 水印要剔掉
		{ipRange("7.0.9.0"), ipRange("7.255.255.255"), "湖北省", "武汉市"},
	}
}

func qqWryLayoutCases() []struct {
	ip            string
	country, city string
} {
	return []struct {
		ip            string
		country, city string
	}{
		{"1.2.3.4", "北京市", "海淀区"},
		{"2.100.0.1", "广东省", "深圳市"},
		{"3.3.3.3", "北京市", "朝阳区"},
		{"4.4.4.4", "天津市", ""},
		{"5.5.5.5", "上海市", "浦东新区"},
		{"6.6.6.6", "北京市", ""},
		{"7.7.7.7", "湖北省", "武汉市"},
	}
}

func checkQQWryLayout(t *testing.T, db *qqwry) {
	t.Helper()
	for _, tc := range qqWryLayoutCases() {
		u, ok := ipUint32(tc.ip)
		if !ok {
			t.Fatalf("ipUint32(%q) 失败", tc.ip)
		}
		country, city, found := db.resolve(u)
		if !found {
			t.Errorf("resolve(%s) 说库里没有", tc.ip)
			continue
		}
		if country != tc.country || city != tc.city {
			t.Errorf("resolve(%s) = %q/%q，想要 %q/%q", tc.ip, country, city, tc.country, tc.city)
		}
	}
	// 比库里第一条还小：没覆盖就是没覆盖，不能给个假归属地
	if _, _, found := db.resolve(ipRange("0.9.9.9")); found {
		t.Errorf("0.9.9.9 不在库里却也报 found")
	}
}

func TestQQWryResolveLayouts(t *testing.T) {
	raw := buildQQWryBytes(t, qqWryOpts{entries: qqWryLayoutEntries(), bothAt: 4, chainAt: -1})
	if err := checkQQWryBytes(raw); err != nil {
		t.Fatalf("合成库自校验失败: %v", err)
	}
	db, err := loadQQWry(writeTempDB(t, raw))
	if err != nil {
		t.Fatalf("loadQQWry: %v", err)
	}
	if db.indexCount() != uint32(len(qqWryLayoutEntries())) {
		t.Errorf("条数 = %d，想要 %d", db.indexCount(), len(qqWryLayoutEntries()))
	}
	if !db.ipLE {
		t.Errorf("小端写的库却被自检判成了大端")
	}
	if db.endIPPfx {
		t.Errorf("记录体开头没写结束 IP，却被自检判成了老格式")
	}
	checkQQWryLayout(t, db)
}

// 早年的库在记录体开头多写一遍结束 IP（原版 FQTerm 那代代码就是按这个读法写的），
// 自检得认出来并且跳过 4 字节。
func TestQQWryOldLayoutEndIPPrefix(t *testing.T) {
	raw := buildQQWryBytes(t, qqWryOpts{entries: qqWryLayoutEntries(), bothAt: 4, endIPPfx: true})
	db, err := loadQQWry(writeTempDB(t, raw))
	if err != nil {
		t.Fatalf("loadQQWry: %v", err)
	}
	if !db.endIPPfx {
		t.Fatalf("老格式的结束 IP 前缀没被自检出来")
	}
	checkQQWryLayout(t, db)
}

// 字节序自检：把索引 IP 按网络字节序写（不是真库做法），resolve 仍然得给出正确结果。
func TestQQWryDetectIPOrder(t *testing.T) {
	raw := buildQQWryBytes(t, qqWryOpts{entries: qqWryLayoutEntries(), bothAt: 4, ipBE: true})
	db, err := loadQQWry(writeTempDB(t, raw))
	if err != nil {
		t.Fatalf("loadQQWry: %v", err)
	}
	if db.ipLE {
		t.Fatalf("大端写的库没被自检出来")
	}
	checkQQWryLayout(t, db)
}

// 重定向的目标自己又是个重定向（真库有这种写法），deref 得跟着链走到终点。
func TestQQWryChainedRedirect(t *testing.T) {
	entries := []qqWryEntry{
		{ipRange("1.0.0.0"), ipRange("1.255.255.255"), "北京市", "海淀区"},
		{ipRange("2.0.0.0"), ipRange("2.255.255.255"), "上海市", "徐汇区"},
		{ipRange("3.0.0.0"), ipRange("3.255.255.255"), "上海市", "浦东新区"}, // 0x02 → 字面量
		{ipRange("4.0.0.0"), ipRange("4.255.255.255"), "上海市", "黄浦区"},  // 0x02 → 上一个 0x02
	}
	raw := buildQQWryBytes(t, qqWryOpts{entries: entries, chainAt: 3})
	db, err := loadQQWry(writeTempDB(t, raw))
	if err != nil {
		t.Fatalf("loadQQWry: %v", err)
	}
	for _, tc := range []struct {
		ip, country, city string
	}{
		{"3.1.1.1", "上海市", "浦东新区"},
		{"4.4.4.4", "上海市", "黄浦区"},
	} {
		country, city, found := db.resolve(ipRange(tc.ip))
		if !found || country != tc.country || city != tc.city {
			t.Errorf("resolve(%s) = %q/%q found=%v，想要 %s/%s", tc.ip, country, city, found, tc.country, tc.city)
		}
	}
}

// deref 跳成一个环（库写坏了）时不能死循环：qqWryMaxDepth 必须兜得住。
func TestQQWryRedirectLoopIsBounded(t *testing.T) {
	entries := []qqWryEntry{
		{ipRange("1.0.0.0"), ipRange("1.255.255.255"), "北京市", "海淀区"},
	}
	raw := buildQQWryBytes(t, qqWryOpts{entries: entries})
	recOff := int(le24(raw, qqWryHeaderSize+4))
	raw[recOff] = 0x02                   // 国家字段改成重定向
	put24(raw, recOff+1, uint32(recOff)) // 指向自己：一个环
	db, err := loadQQWry(writeTempDB(t, raw))
	if err != nil {
		t.Fatalf("loadQQWry: %v", err)
	}
	country, _, found := db.resolve(ipRange("1.1.1.1"))
	if found && country != "" {
		t.Errorf("环状重定向读出了结论: %q", country)
	}
}

func TestCheckQQWryBytesRejectsGarbage(t *testing.T) {
	if err := checkQQWryBytes([]byte("<html><head><title>404</title></head></html>")); err == nil {
		t.Errorf("HTML 被当成了合法 IP 库")
	}
	if err := checkQQWryBytes(nil); err == nil {
		t.Errorf("空字节被当成了合法 IP 库")
	}
	// 真库把索引区放在文件末尾，头里两个偏移都是大整数。下载刚开始手里只有 16 字节，
	// 这时候拿「索引尾越界」当理由会把好库错杀成非法，所以两条规则必须分开。
	bigFirst, bigLast := uint32(1952768), uint32(1952768+7*400000)
	frag := make([]byte, 16)
	put24(frag, 0, bigFirst)
	put24(frag, 4, bigLast)
	if err := checkQQWryHead(frag); err != nil {
		t.Errorf("真库那种大偏移的头被当成非法: %v", err)
	}
	if err := checkQQWryBytes(frag); err == nil {
		t.Errorf("只有 16 字节却装得下索引尾，整文件校验应该拦下")
	}
	skew := make([]byte, 16)
	put24(skew, 0, 8)
	put24(skew, 4, 17) // 索引区长度不是 7 的整数倍
	if err := checkQQWryHead(skew); err == nil {
		t.Errorf("索引区长度歪的库头被放过了")
	}
	if looksLikeTextHead([]byte("<html>oops</html>")) != true {
		t.Errorf("HTML 开头没被判成文本")
	}
	if looksLikeTextHead([]byte{0x08, 0x00, 0x00, 0x40, 0x00, 0x00, 0x01, 0x00}) {
		t.Errorf("库头被误判成文本")
	}
}

func TestIPUint32(t *testing.T) {
	cases := []struct {
		in   string
		want uint32
		ok   bool
	}{
		{"1.2.3.4", 0x01020304, true},
		{" 202.38.64.* ", 0xCA264001, true}, // 打码末段按原版补 '1'
		{"114.242.*.*", 0x72F20101, true},   // 中间段一起打码（不少站端这么显）
		{"114.242.x.x", 0x72F20101, true},   // 用 x 而不是 * 打码的站端
		{"1.*.3.4", 0x01010304, true},
		{"*.*.*.*", 0, false},   // 全打码问不出任何东西
		{"x.38.64.1", 0, false}, // 头一段就打码也算问不出
		{"*.38.64.1", 0, false},
		{"202.38.x.999", 0, false}, // 打码能补，但没打的那段仍然得合法
		{"255.255.255.255", 0xFFFFFFFF, true},
		{"1.2.3.999", 0, false},
		{"1.2.3", 0, false},
		{"::1", 0, false},
		{"waterwaterwater.site", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		got, ok := ipUint32(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("ipUint32(%q) = %#x,%v，想要 %#x,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestFindQQWrySearchOrderAndHints(t *testing.T) {
	home := t.TempDir()
	exe := t.TempDir()
	// 只有 exe 目录里有库（大小写不同的文件名也要认）
	if err := os.WriteFile(filepath.Join(exe, "QQWRY.DAT"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := findQQWry("", exe, home)
	if err != nil {
		t.Fatalf("没找到 exe 目录里的库: %v", err)
	}
	if !bytes.Equal([]byte(filepath.Base(got)), []byte("QQWRY.DAT")) {
		t.Errorf("findQQWry 返回 %s，想要 exe 目录里那份", got)
	}
	// 显式指定的目录优先
	if err := os.WriteFile(filepath.Join(home, qqWryFileName), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err = findQQWry(home, exe, home)
	if err != nil || filepath.Dir(got) != home {
		t.Errorf("显式目录没生效: got=%s err=%v", got, err)
	}
	// 哪儿都没有：错误里必须带上可放路径，用户照着放就行
	_, err = findQQWry("", t.TempDir(), t.TempDir())
	if err == nil {
		t.Fatal("空目录本该报找不到")
	}
	for _, want := range []string{qqWryFileName, "设置", "手动下载"} {
		if !stringsContains(err.Error(), want) {
			t.Errorf("找不到库的提示里没有 %q：%s", want, err)
		}
	}
}

func stringsContains(s, sub string) bool {
	return len(s) >= len(sub) && bytes.Contains([]byte(s), []byte(sub))
}

func ipRange(s string) uint32 {
	v, ok := ipUint32(s)
	if !ok {
		panic("bad test IP " + s)
	}
	return v
}

func writeTempDB(t *testing.T, raw []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), qqWryFileName)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestQQWryRealDatabase 拿一份真库把上面所有假设再跑一遍。两项自检（索引 IP 的
// 字节序、老格式的记录体结束 IP 前缀）只能对真数据验：合成库只能证明「两种写法
// 都能读」，证不了「真库是哪一种」。
//
// 默认跳过：真库 20 多 MB，不该进常规测试。手工验证时指一份本地库（程序自动更新
// 下来的那份就能直接用）：
//
//	$env:FTRW_IPDB_TEST_FILE = "$env:APPDATA\FTRW\qqwry.dat"
//	go test -run TestQQWryRealDatabase -v
func TestQQWryRealDatabase(t *testing.T) {
	path := os.Getenv("FTRW_IPDB_TEST_FILE")
	if path == "" {
		t.Skip("未设 FTRW_IPDB_TEST_FILE，跳过真库校验")
	}
	db, err := loadQQWry(path)
	if err != nil {
		t.Fatalf("载入真库失败: %v", err)
	}
	n := db.indexCount()
	t.Logf("真库 %s：%d 字节，索引 %d 条，字节序=%s，记录前缀=%v",
		filepath.Base(path), len(db.data), n, db.orderName(), db.endIPPfx)
	if n < 10000 {
		t.Fatalf("索引只有 %d 条，不是一份真库", n)
	}
	if !db.ipLE {
		t.Errorf("自检判成大端：纯真库按小端发布，说明 detectIPOrder 在真数据上失手")
	}

	// 整表扫一遍：起始 IP 必须严格递增。这一条比自检硬 —— 字节序读错的话必然在
	// 某个四字节的跳变处出倒序，而抽样计数可能刚好漏过那个位置。
	for i := uint32(1); i < n; i++ {
		if prev, cur := db.ipAt(db.entryOff(i-1)), db.ipAt(db.entryOff(i)); cur <= prev {
			t.Fatalf("索引不是递增的（第 %d 条 %#x ≤ 第 %d 条 %#x）", i, cur, i-1, prev)
		}
	}

	// 抽样扫一遍记录体：越界、成环、mode 越界全都会在这一步现形。只看「两个字段里至少有一个
	// 有内容」，不看具体内容：真库里有整段只写了地区没写国家的条目，那是数据本身就这样。
	step := max(1, int(n/200000))
	empty, checked := 0, 0
	for i := uint32(0); i < n; i += uint32(step) {
		country, city, found := db.resolve(db.ipAt(db.entryOff(i)))
		checked++
		if !found || (strings.TrimSpace(country)+strings.TrimSpace(city)) == "" {
			empty++
		}
	}
	if empty*20 > checked {
		t.Errorf("抽样 %d 条里有 %d 条解不出内容，记录体读法在真库上不成立", checked, empty)
	}
	t.Logf("抽样 %d 条，解不出内容 %d 条", checked, empty)

	// 几个常碰到的地址：只要求「能解出东西」，具体归属地随库版本变，不该写进断言。
	// 带 * 与 x 的是屏幕上常见的打码写法，补全之后必须仍能落在一个真归属地上。
	for _, q := range []string{"1.2.3.4", "8.8.8.8", "114.114.114.114", "202.38.64.1", "202.38.64.*", "114.242.x.x", "114.242.*.*", "58.30.0.1"} {
		u, ok := ipUint32(q)
		if !ok {
			t.Fatalf("用例地址不合法: %s", q)
		}
		country, city, found := db.resolve(u)
		t.Logf("%-16s found=%v %q %q", q, found, country, city)
		if !found {
			t.Errorf("%s 在真库里查不到", q)
		}
	}
}

/* --------------------------------------------------------------------------- */
/* 断点续传（库 20 多 MB，慢链路上不是一次就能跑完）                              */
/* --------------------------------------------------------------------------- */

// minimalQQWry 造一份结构合法的迷你库。它比真库小得多，但头部偏移是真的，所以
// 「索引尾落在文件内」那条收尾检查照样能挡住没下完的成品。
func minimalQQWry(t *testing.T) []byte {
	t.Helper()
	// 条数故意给多几条：单条只有几个字节，三十条就是几百字节，够把「截一半」当成
	// 两种不同长度的落盘结果区分开（太小会连头部 16 字节的自检都跑不出差异）。
	entries := make([]qqWryEntry, 0, 24)
	for i := 0; i < 24; i++ {
		base := uint32(i+1) << 24
		entries = append(entries, qqWryEntry{
			firstIP: base | 0x000001,
			lastIP:  base | 0x00ffff,
			country: fmt.Sprintf("国%02d", i),
			city:    fmt.Sprintf("市%02d", i),
		})
	}
	raw := buildQQWryBytes(t, qqWryOpts{entries: entries, bothAt: -1, chainAt: -1})
	if err := checkQQWryBytes(raw); err != nil {
		t.Fatalf("夹具库自身就不合法: %v", err)
	}
	if len(raw) < 128 {
		t.Fatalf("夹具库只有 %d 字节，截一半就没法验证续传", len(raw))
	}
	return raw
}

// rangeRecorder 记下服务器收到的 Range / If-Modified-Since。handler 在另一个 goroutine
// 里跑，测试自己断言，所以要加锁。
type rangeRecorder struct {
	mu   sync.Mutex
	seen []string
}

func (r *rangeRecorder) add(s string) {
	r.mu.Lock()
	r.seen = append(r.seen, s)
	r.mu.Unlock()
}

func (r *rangeRecorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

// rangeSrv 起一个支持 Range 的假库服务器。cutFirst>0 时第一条请求只发那么多个字节就
// 掉线（对应现实里的「连上了但断流」），之后的请求正常发全 —— 于是同一份库得跑
// 两轮才拿得全，正好用来验续传。
func rangeSrv(t *testing.T, body []byte, cutFirst int) (string, *rangeRecorder) {
	t.Helper()
	rec := &rangeRecorder{}
	var served atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add("range=[" + r.Header.Get("Range") + "] ims=[" + r.Header.Get("If-Modified-Since") + "]")
		start := int64(0)
		if rg := r.Header.Get("Range"); strings.HasPrefix(rg, "bytes=") {
			spec := strings.TrimPrefix(rg, "bytes=")
			dash := strings.IndexByte(spec, '-')
			v, err := strconv.ParseInt(spec[:dash], 10, 64)
			if dash < 0 || err != nil || v < 0 || v >= int64(len(body)) {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			start = v
		}
		end := len(body)
		cutting := cutFirst > 0 && served.Add(1) == 1
		if cutting {
			end = min(cutFirst, len(body))
		}
		// 进 WriteHeader 之前就得把 Content-Length 定下来，事后 Set 只会被丢掉。
		// 它必须写「完整长度」而不是「这一轮实际发多少」：承诺了却没发完，客户端才会
		// 拿到 unexpected EOF —— 只改写入字节数但把长度也改小，反而成了「一次完整传输」。
		w.Header().Set("Content-Length", strconv.Itoa(len(body)-int(start)))
		if start > 0 {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(body)-1, len(body)))
			w.WriteHeader(http.StatusPartialContent)
		} else {
			w.WriteHeader(http.StatusOK)
		}
		_, _ = w.Write(body[start:end])
		if cutting {
			// 先 Flush：已写出去的那些字节还躺在 bufio 里，直接 abort 会连着它们一起被丢，
			// 客户端就一个字节也拿不到，续传也就无从谈起。
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			panic(http.ErrAbortHandler) // 不补 chunked 结束位、直接断连，像一具断流的服务
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/qqwry.dat", rec
}

func TestFetchQQWryResumesAfterTruncatedTransfer(t *testing.T) {
	body := minimalQQWry(t)
	cut := len(body) / 2
	dlURL, rec := rangeSrv(t, body, cut)
	part := filepath.Join(t.TempDir(), "qqwry.dat.part")

	// 第一轮：断在半路。报错可以，但 .part 不能跟着一起被删 —— 删了就等于每轮从零开始。
	if _, _, err := fetchQQWryURL(dlURL, part, "", nil); err == nil {
		t.Fatal("第一轮本该因断流失败")
	}
	fi, err := os.Stat(part)
	if err != nil {
		t.Fatalf(".part 被删掉了，下一轮无从续起: %v", err)
	}
	if fi.Size() != int64(cut) {
		t.Fatalf("第一轮落盘 %d 字节，期望 %d", fi.Size(), cut)
	}

	// 第二轮：带 Range 接着下，拼出来必须和原库逐字节相同。
	got, skipped, err := fetchQQWryURL(dlURL, part, "", nil)
	if err != nil {
		t.Fatalf("第二轮续传失败: %v", err)
	}
	if skipped {
		t.Fatal("续传那一轮不该被当成「已是最新」跳过")
	}
	if got != int64(len(body)) {
		t.Errorf("got = %d，期望 %d（进度得把已有的半截算进去）", got, len(body))
	}
	data, err := os.ReadFile(part)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, body) {
		t.Errorf("续传拼出来的库和原库不一样（%d 字节对 %d 字节）", len(data), len(body))
	}
	if err := checkQQWryBytes(data); err != nil {
		t.Errorf("续传出来的库结构自检不过: %v", err)
	}

	seen := rec.all()
	if len(seen) != 2 {
		t.Fatalf("服务器收到 %d 次请求，期望 2 次", len(seen))
	}
	if want := fmt.Sprintf("range=[bytes=%d-]", cut); !strings.HasPrefix(seen[1], want) {
		t.Errorf("第二轮没按断点发 Range：%q，前缀应为 %q", seen[1], want)
	}
	// 续传时发 If-Modified-Since 只会换来一个 304，把这份马上要补全的半截判死。
	if !strings.HasSuffix(seen[1], "ims=[]") {
		t.Errorf("续传那一轮不该带 If-Modified-Since: %q", seen[1])
	}
	// 下完了就不用再留着「这份 .part 属于哪个地址」的记号。
	if _, err := os.Stat(part + ".src"); !os.IsNotExist(err) {
		t.Errorf("成功后 .src 记号没清掉（err=%v）", err)
	}
}

func TestFetchQQWryDiscardsPartFromAnotherSource(t *testing.T) {
	body := minimalQQWry(t)
	half := len(body) / 2
	dlURL, rec := rangeSrv(t, body, 0)
	part := filepath.Join(t.TempDir(), "qqwry.dat.part")

	// 伪造成「上次从另一个镜像下的半截」：字节本身是真的，所以单看头部会以为能续。
	if err := os.WriteFile(part, body[:half], 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(part+".src", []byte("https://other.example/qqwry.dat"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, _, err := fetchQQWryURL(dlURL, part, "", nil); err != nil {
		t.Fatalf("换源后整份重下却失败了: %v", err)
	}
	seen := rec.all()
	if len(seen) != 1 {
		t.Fatalf("收到 %d 次请求，期望 1 次", len(seen))
	}
	if !strings.HasPrefix(seen[0], "range=[]") {
		t.Errorf("换了地址还去续别人的半截: %q", seen[0])
	}
	data, err := os.ReadFile(part)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, body) {
		t.Errorf("换源后拼出来的库不对（%d 字节对 %d 字节）", len(data), len(body))
	}
}

func TestFetchQQWryRestartsWhenRangeIgnored(t *testing.T) {
	body := minimalQQWry(t)
	half := len(body) / 2
	// 一个故意不理 Range 的服务器：无论怎么问都回 200 全量。
	var gotRange atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			gotRange.Store(true)
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	dlURL := srv.URL + "/qqwry.dat"

	part := filepath.Join(t.TempDir(), "qqwry.dat.part")
	if err := os.WriteFile(part, body[:half], 0o644); err != nil {
		t.Fatal(err)
	}
	// 同一地址，所以这一轮会去发 Range；对方回 200 就得归零重写。
	if err := os.WriteFile(part+".src", []byte(dlURL), 0o644); err != nil {
		t.Fatal(err)
	}
	got, _, err := fetchQQWryURL(dlURL, part, "", nil)
	if err != nil {
		t.Fatalf("对方不理 Range 时本该整份重下并成功: %v", err)
	}
	if got != int64(len(body)) {
		t.Errorf("got = %d，期望 %d（归零重下就不能把半截也算进去）", got, len(body))
	}
	if !gotRange.Load() {
		t.Error("同地址的半截文件本该去试续传")
	}

	data, err := os.ReadFile(part)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, body) {
		t.Errorf("对方不理 Range 时拼出了 %d 字节的文件，期望 %d：把全量追加到半截后面了",
			len(data), len(body))
	}
	if err := checkQQWryBytes(data); err != nil {
		t.Errorf("重下后的库自检不过: %v", err)
	}
}

func TestRespHasRange(t *testing.T) {
	h := func(kv ...string) http.Header {
		out := http.Header{}
		for i := 0; i+1 < len(kv); i += 2 {
			out.Set(kv[i], kv[i+1])
		}
		return out
	}
	cases := []struct {
		name   string
		resp   *http.Response
		resume int64
		want   bool
	}{
		{name: "200 全量", resp: &http.Response{StatusCode: http.StatusOK, Header: h()}, resume: 100, want: false},
		{name: "206 且起点相符", resp: &http.Response{StatusCode: 206, Header: h("Content-Range", "bytes 100-999/1000")}, resume: 100, want: true},
		{name: "206 但从别处开始", resp: &http.Response{StatusCode: 206, Header: h("Content-Range", "bytes 0-999/1000")}, resume: 100, want: false},
		{name: "206 不给 Content-Range", resp: &http.Response{StatusCode: 206, Header: h()}, resume: 100, want: true},
		{name: "单位不是 bytes", resp: &http.Response{StatusCode: 206, Header: h("Content-Range", "items 100-999/1000")}, resume: 100, want: false},
		{name: "形状不对", resp: &http.Response{StatusCode: 206, Header: h("Content-Range", "bytes 100")}, resume: 100, want: false},
		{name: "BYTES 大写也算", resp: &http.Response{StatusCode: 206, Header: h("Content-Range", "BYTES 100-999/1000")}, resume: 100, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := respHasRange(tc.resp, tc.resume); got != tc.want {
				t.Errorf("respHasRange = %v，期望 %v", got, tc.want)
			}
		})
	}
}

func TestReadHeadOf(t *testing.T) {
	dir := t.TempDir()
	full := filepath.Join(dir, "full.bin")
	if err := os.WriteFile(full, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	short := filepath.Join(dir, "short.bin")
	if err := os.WriteFile(short, []byte("01"), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := readHeadOf(full, 4)
	if err != nil || string(b) != "0123" {
		t.Errorf("读开头 = %q, %v", b, err)
	}
	// 不够 n 字节不算错误，长度本身就说明了这件事（fetchQQWryURL 会自己看 len）。
	b, err = readHeadOf(short, 16)
	if err != nil || string(b) != "01" {
		t.Errorf("短文件 = %q, %v", b, err)
	}
	if _, err := readHeadOf(filepath.Join(dir, "不存在.bin"), 4); err == nil {
		t.Error("文件不存在该报错")
	}
}

// cutBody 把一个响应体包一层：读过 left 字节之后就开始报错。用它在本机模拟
// 「下了几 MB 然后断掉」，而不是靠运气碰上真断流。
type cutBody struct {
	io.ReadCloser
	left int64
}

func (c *cutBody) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, errors.New("夹具掐断传输")
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.ReadCloser.Read(p)
	c.left -= int64(n)
	return n, err
}

// rtFunc 把普通函数当 http.RoundTripper 用。
type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestQQWryDownloadResumesOverRealNetwork 是真网络用例：下一整个 20 多 MB 的库，
// 中途断一次，再验续传拼出来的库能真查。合成库只能证「代码按 Range 行事」，
// 证不了 GitHub 资产链负不负 Range、也证不了拼出来的 27MB 是不是完整的。
//
// 默认跳过（流量大、跑一分钟以上）。手工验证（配上代理才能连上的环境就把
// FTRW_PROXY_TEST 一并设上）：
//
//	$env:FTRW_PROXY_TEST = "http://127.0.0.1:9870"
//	$env:FTRW_IPDB_FETCH = "1"
//	go test -run TestQQWryDownloadResumesOverRealNetwork -v -timeout 10m
func TestQQWryDownloadResumesOverRealNetwork(t *testing.T) {
	if os.Getenv("FTRW_IPDB_FETCH") == "" {
		t.Skip("未设 FTRW_IPDB_FETCH，跳过真网络下载校验")
	}
	spec := strings.TrimSpace(os.Getenv("FTRW_PROXY_TEST"))
	if spec != "" {
		pc, err := parseProxySpec(spec)
		if err != nil {
			t.Fatalf("代理地址不对 %q: %v", spec, err)
		}
		restore := globalProxy()
		setGlobalProxy(pc)
		defer setGlobalProxy(restore)
		t.Logf("经 %s 下载", pc.label())
	} else {
		t.Log("未设 FTRW_PROXY_TEST，直连下载")
	}

	base := qqwryHTTP
	cutting := true
	const cutAt = 3 << 20
	qqwryHTTP = &http.Client{
		Timeout: qqwryDownloadTimeout,
		Transport: rtFunc(func(req *http.Request) (*http.Response, error) {
			resp, err := base.Transport.RoundTrip(req)
			if err != nil {
				return nil, err
			}
			// 只包真正带库的那一跳。releases/latest/download 是个 302，而 RoundTrip
			// 每个跳转都会走一次：不分清就会把那几百字节的跳转页当成文件去截，夹具
			// 永远碰不到、文件反而一次跑完。
			if cutting && resp.StatusCode == http.StatusOK {
				cutting = false
				resp.Body = &cutBody{ReadCloser: resp.Body, left: cutAt}
			}
			return resp, nil
		}),
	}
	defer func() { qqwryHTTP = base }()

	part := filepath.Join(t.TempDir(), qqWryFileName+".part")
	dlURL := qqwrySources[0]

	if _, _, err := fetchQQWryURL(dlURL, part, "", nil); err == nil {
		if cutting {
			t.Fatal("一个 200 都没见到，夹具根本没碰到传输（被重定向/代理拦了吗）")
		}
		t.Fatal("第一轮本该被夹具掐断，却报成功了")
	} else {
		t.Logf("第一轮如期失败: %v", err)
	}
	fi, err := os.Stat(part)
	if err != nil {
		t.Fatalf(".part 没留下来，下一轮无从续起: %v", err)
	}
	half := fi.Size()
	if half < 1<<20 {
		t.Fatalf("第一轮只落了 %d 字节，远不到夹具设定的 %d：流量没跑起来，续传验不出东西", half, cutAt)
	}

	start := time.Now()
	got, skipped, err := fetchQQWryURL(dlURL, part, "", nil)
	if err != nil {
		t.Fatalf("第二轮续传失败（已落 %d 字节）: %v", half, err)
	}
	if skipped {
		t.Fatal("续传那一轮不该被当成「已是最新」")
	}
	if fi, err := os.Stat(part); err != nil || fi.Size() != got {
		t.Errorf("got = %d，磁盘上是 %d", got, fi.Size())
	}
	t.Logf("续传完成：%d → %d 字节，耗时 %s", half, got, time.Since(start).Round(time.Second))

	// 拼出来的库必须能真用：这一条比逐字节对比更硬，因为它同时证了 Range 起点不多
	// 不少（差一个字节整张索引就会错位）、以及拼接处没有重叠。
	db, err := loadQQWry(part)
	if err != nil {
		t.Fatalf("续传拼出来的库载入失败: %v", err)
	}
	n := db.indexCount()
	if n < 10000 {
		t.Fatalf("只有 %d 条索引，不是一份完整的库", n)
	}
	t.Logf("索引 %d 条，字节序=%s，记录前缀=%v", n, db.orderName(), db.endIPPfx)
	for _, q := range []string{"1.2.3.4", "8.8.8.8", "114.114.114.114", "202.38.64.1"} {
		u, ok := ipUint32(q)
		if !ok {
			t.Fatalf("用例地址不合法: %s", q)
		}
		country, city, found := db.resolve(u)
		t.Logf("%-16s found=%v %q %q", q, found, country, city)
		if !found {
			t.Errorf("%s 在续传拼出来的库里查不到 —— 拼接处很可能错位了", q)
		}
	}
}
