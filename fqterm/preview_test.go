package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// 预览服务的窗口动作没法在测试里跑，能钉住的是那几个纯函数：链接准入、序号钳制、
// 标题拼装，以及另存为用的文件名清洗。第一个是安全边界（链接内容来自远端），
// 其余决定界面会不会出现空标题、越界序号，或存出一个双击打不开的文件。

func TestCheckPreviewURLAllowsOnlyHTTP(t *testing.T) {
	for _, ok := range []string{
		"http://bbs.mysmth.net/article/1.jpg",
		"https://img.smth.com/a/b.png?x=1#y",
		"  https://smth.net/  ", // 前后空白是终端里复制来的常客，得先裁掉
	} {
		if _, err := checkPreviewURL(ok); err != nil {
			t.Errorf("应放行 %q: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"",
		"不是链接",
		"javascript:alert(1)",                // 预览窗口里有后端绑定，放进 js: 等于交出去
		"data:text/html;base64,PHNjcmlwdD4=", // 同理
		"file:///C:/Windows/win.ini",         // 本地文件不该由远端链接决定
		"ftp://host/x",                       // 不是预览支持的协议
		"https:///no-host",                   // 缺主机名
	} {
		if u, err := checkPreviewURL(bad); err == nil {
			t.Errorf("应拒绝 %q，实际通过并得到 %v", bad, u)
		}
	}
}

func TestClampIndex(t *testing.T) {
	cases := [][3]int{
		{0, 5, 0},
		{4, 5, 4},
		{-1, 5, 0}, // 前端找不到被点的那条时传 -1，回到第一条
		{7, 5, 0},  // 越界同样回到第一条，而不是最后一条
		{3, 0, 0},  // 空列表
	}
	for _, c := range cases {
		if got := clampIndex(c[0], c[1]); got != c[2] {
			t.Errorf("clampIndex(%d,%d) = %d, want %d", c[0], c[1], got, c[2])
		}
	}
}

func TestPreviewTitle(t *testing.T) {
	if got := previewTitle(KindImage, "1.png", "http://h/a/1.png"); got != "图片预览 · 1.png" {
		t.Errorf("标题 = %q", got)
	}
	// 名字为空时退回主机名，标题栏不能只剩「预览」两个字
	if got := previewTitle(KindPage, "", "https://m.mysmth.net/nForum/board/AutoBoard"); got != "网页预览 · m.mysmth.net" {
		t.Errorf("缺名标题 = %q", got)
	}
	// 未知类型仍要有前缀
	if got := previewTitle("zzz", "x", "http://h/x"); got != "预览 · x" {
		t.Errorf("未知类型标题 = %q", got)
	}
	// 长名字按 rune 截：按字节切会把中文劈成半个，标题栏出现乱码
	long := ""
	for i := 0; i < 200; i++ {
		long += "汉"
	}
	got := previewTitle(KindVideo, long, "http://h/v.mp4")
	if n := len([]rune(got)); n > 70 {
		t.Errorf("长标题没截断：rune 数 %d", n)
	}
	if !isValidUTF8(got) {
		t.Errorf("截断位置切坏了 UTF-8: %q", got)
	}
}

func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == '\uFFFD' {
			return false
		}
	}
	return true
}

func TestEscWatchArmDisarm(t *testing.T) {
	// hwnd 取不到（非 Windows，或 NativeWindow 返回 nil）时不能留监听：
	// foregroundHWND 恒为 0，会让 ESC 在任意窗口上都被吃掉。
	fired := 0
	armEscWatch(0, func() { fired++ })
	if w := escWatchVal.Load(); w != nil {
		t.Fatalf("hwnd=0 不该建立监听：%+v", w)
	}

	armEscWatch(0x1234, func() { fired++ })
	if w := escWatchVal.Load(); w == nil || w.hwnd != 0x1234 {
		t.Fatalf("没建立监听：%+v", w)
	}
	// 别的窗口关窗时不能把当前监听误摘掉（hwnd 对不上）
	disarmEscWatch(0x999)
	if escWatchVal.Load() == nil {
		t.Fatalf("disarm 误摘了别人的监听")
	}
	// ESC 之外 / keyup 都不该命中
	if hitEscWatch(gkEvent{Vk: vkEscape, Down: false}) {
		t.Errorf("keyup 不该命中")
	}
	if hitEscWatch(gkEvent{Vk: vkEnter, Down: true}) {
		t.Errorf("非 ESC 不该命中")
	}
	disarmEscWatch(0x1234)
	if escWatchVal.Load() != nil {
		t.Fatalf("disarm 没摘掉监听")
	}
	if fired != 0 {
		t.Fatalf("没按 ESC 却触发了 %d 次", fired)
	}
}

func TestSanitizeFileName(t *testing.T) {
	cases := [][2]string{
		{"1.png", "1.png"},
		{"a/b:c?d*e", "a_b_c_d_e"}, // Windows 不允许的字符全部换掉，不能一个字符就把整条名字带坑
		{"  标题.  ", "标题"},          // 尾部的点与空格 NTFS 会默默吃掉
		{"带\t制表", "带制表"},           // 控制字符直接丢
		{"", ""},
		{"   ", ""},
	}
	for _, c := range cases {
		if got := sanitizeFileName(c[0]); got != c[1] {
			t.Errorf("sanitizeFileName(%q) = %q, want %q", c[0], got, c[1])
		}
	}
	// 按 rune 截：按字节切会把中文切成半个
	long := ""
	for i := 0; i < 300; i++ {
		long += "汉"
	}
	if got := sanitizeFileName(long); len([]rune(got)) != 120 {
		t.Errorf("长名字没截到 120 rune：%d", len([]rune(got)))
	}
}

func TestURLExt(t *testing.T) {
	cases := [][2]string{
		{"/a/b.JPG", "jpg"},
		{"/a/b", ""},
		{"/a/b/", ""},
		{"/a/x.tar.gz", "gz"},
		{"/a/b.mp4", "mp4"}, // 后缀里带数字是常态，不能只认字母
		{"/a/x.3gp", "3gp"},
		{"/a/x.123", ""}, // 全是数字不算后缀
		{"/a/toolongextension", ""},
		{"/a/b.", ""},
		{"/a/1.2 mb.png", "png"},
	}
	for _, c := range cases {
		if got := urlExt(c[0]); got != c[1] {
			t.Errorf("urlExt(%q) = %q, want %q", c[0], got, c[1])
		}
	}
}

func TestSaveSuggestName(t *testing.T) {
	mustParse := func(t *testing.T, s string) *url.URL {
		t.Helper()
		u, err := checkPreviewURL(s)
		if err != nil {
			t.Fatalf("构造地址失败 %q: %v", s, err)
		}
		return u
	}
	cases := []struct{ name, raw, want string }{
		{"1.png", "http://h/a/1.png", "1.png"},
		{"", "http://h/dir/photo.jpg?v=1", "photo.jpg"}, // 名字丢了从路径末段拿，query 不参与
		{"x", "http://h/a/b.mp4", "x.mp4"},              // 没后缀就补一个，否则存出来双击打不开
		{"标题. ", "https://h/a.png", "标题.png"},           // 先洗名字再补后缀
		{"", "https://h/", "download"},                  // 什么都拿不到也不能空着
		{"a/b:c", "http://h/1.jpg", "a_b_c.jpg"},
	}
	for _, c := range cases {
		if got := saveSuggestName(c.name, mustParse(t, c.raw)); got != c.want {
			t.Errorf("saveSuggestName(%q, %q) = %q, want %q", c.name, c.raw, got, c.want)
		}
	}
}

// 探测只看 Content-Type 的大类：认不出来的必须返回空串，让前端保留按扩展名的原判，
// 不能拿 text/html 以外的东西乱猜成图片。
func TestKindFromContentType(t *testing.T) {
	cases := [][2]string{
		{"image/jpeg", KindImage},
		{"image/png;charset=x", KindImage},
		{" Image/GIF ", KindImage},
		{"image/svg+xml", KindImage},
		{"audio/mpeg", KindAudio},
		{"video/mp4", KindVideo},
		{"text/html", ""},
		{"text/html; charset=utf-8", ""},
		{"application/octet-stream", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := kindFromContentType(c[0]); got != c[1] {
			t.Errorf("kindFromContentType(%q) = %q, want %q", c[0], got, c[1])
		}
	}
}

// 探测是这两轮修复的根：水木的图走 att.php?... 与纯附件号，扩展名判不出来，只能问服务器。
// 这里钉住四件事：HEAD 能拿到类型、服务器不认 HEAD 时要退化成 GET、其余情况一律当没探到、
// Content-Disposition 里的真名字要带回来。
func TestProbeKindAndName(t *testing.T) {
	headOK := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
	}))
	defer headOK.Close()
	// /att 这种主干是脚本名（att.php 同一类），凑出 att.jpg 只会让整帖图都叫同一个名，
	// 所以宁可不给名字，只报类型。
	if got := probe(headOK.URL + "/att"); got.Kind != KindImage || got.Name != "" {
		t.Errorf("HEAD 应探到 image 且不乱凑名字，实际 %+v", got)
	}
	if got := probe(headOK.URL + "/att/47743632"); got.Kind != KindImage || got.Name != "47743632.jpg" {
		t.Errorf("附件号地址应凑成 47743632.jpg，实际 %+v", got)
	}

	noHead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			http.Error(w, "nope", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
	}))
	defer noHead.Close()
	if got := probe(noHead.URL + "/clip"); got.Kind != KindVideo {
		t.Errorf("不认 HEAD 时应退化成 GET 探到 video，实际 %+v", got)
	}

	// 名字只能从 Content-Disposition 拿：地址末是 att.php，存下来必须叫徽章.png。
	named := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/att.php" {
			w.Header().Set("Content-Type", "image/png")
			w.Header().Set("Content-Disposition", "inline; filename*=UTF-8''%E5%BE%BD%E7%AB%A0.png")
		}
	}))
	defer named.Close()
	if got := probe(named.URL + "/att.php"); got.Kind != KindImage || got.Name != "徽章.png" {
		t.Errorf("应从 Content-Disposition 拿到 徽章.png，实际 %+v", got)
	}

	htmlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	}))
	defer htmlSrv.Close()
	if got := probe(htmlSrv.URL + "/board"); got != (ProbeResult{}) {
		t.Errorf("网页应全部返回空（保留原判），实际 %+v", got)
	}

	notFound := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer notFound.Close()
	if got := probe(notFound.URL + "/x"); got != (ProbeResult{}) {
		t.Errorf("404 应返回空，实际 %+v", got)
	}
	if got := probe("http://127.0.0.1:1/nope"); got != (ProbeResult{}) {
		t.Errorf("连不上应返回空，实际 %+v", got)
	}
	if got := probe("javascript:alert(1)"); got != (ProbeResult{}) {
		t.Errorf("非 http 地址不该去探，实际 %+v", got)
	}
}

func TestProbeManyKeepsOrder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/a":
			w.Header().Set("Content-Type", "image/png")
		case "/b":
			w.Header().Set("Content-Type", "audio/mpeg")
		default:
			w.Header().Set("Content-Type", "text/html")
		}
	}))
	defer srv.Close()
	s := &PreviewService{}
	got := s.ProbeMany([]string{srv.URL + "/a", srv.URL + "/x", srv.URL + "/b", "javascript:1"})
	want := []ProbeResult{{KindImage, "a.png"}, {}, {KindAudio, "b.mp3"}, {}}
	if len(got) != len(want) {
		t.Fatalf("长度变了：%v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 条 = %+v, want %+v（并行不能打乱对应关系）", i, got[i], want[i])
		}
	}
	if empty := s.ProbeMany(nil); len(empty) != 0 {
		t.Errorf("空入参应给空出参：%v", empty)
	}
}

// 「名字等不等于没名字」判错了就不会去探测，也就不会重命名。这里钉住 BBS 的几种写法。
func TestLooksGenericName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"", true},
		{"att.php", true},
		{"topic.PHP?id=3", true},
		{"index.html", true},
		{"47743632", true},
		{"/", true},
		{"topic.PHP?id=3", true},
		{"a_b_c", true},
		{"pic.jpg", false},
		{"徽章.png", false},
		{"photo.JPEG", false},
		{"no_ext", true},
		{"trailing.", true},
	}
	for _, c := range cases {
		if got := looksGenericName(c.name); got != c.want {
			t.Errorf("looksGenericName(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestExtFromContentType(t *testing.T) {
	cases := [][2]string{
		{"image/jpeg", "jpg"},
		{"image/pjpeg", "jpg"},
		{"Image/PNG; charset=x", "png"},
		{"video/x-matroska", "mkv"},
		{"audio/x-m4a", "m4a"},
		{"image/svg+xml", "svg"},
		{"image/x-icon", "icon"},
		{"image/vnd.microsoft.icon", ""}, // 带结构的名一律不猜
		{"image/x-foo-bar", ""},          // 带连字符的也不猜
		{"text/html", ""},
		{"application/octet-stream", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := extFromContentType(c[0]); got != c[1] {
			t.Errorf("extFromContentType(%q) = %q, want %q", c[0], got, c[1])
		}
	}
}

func TestFilenameFromDisposition(t *testing.T) {
	cases := [][2]string{
		{`attachment; filename="photo.jpg"`, "photo.jpg"},
		{`inline; filename=simple.png`, "simple.png"},
		{`attachment; filename="徽章.png"`, "徽章.png"},
		{`attachment; filename*=UTF-8''%E5%BE%BD%E7%AB%A0.png`, "徽章.png"},
		{`attachment; filename="a.png"; filename*=UTF-8''b.png`, "b.png"}, // 带 * 的优先
		{`attachment; filename="..%2F..%2FWindows.exe"`, "Windows.exe"},   // 目录部分必须丢掉
		{`attachment; filename="C:\\tmp\\x.jpg"`, "x.jpg"},                // 反斜杠同理
		{`attachment; filename*=gbk''%CE%D2%C3%C7.png`, "我们.png"},         // 老站给 GBK 字节
		{`attachment; filename*=us-ascii''%E4%B8%AD.png`, "中.png"},        // charset 写错了，字节其实是 UTF-8
		{`attachment`, ""},
		{`attachment; filename=""`, ""},
		{`attachment; filename=nodot`, ""}, // 看不出是文件的不算名字
		{"", ""},
	}
	for _, c := range cases {
		if got := filenameFromDisposition(c[0]); got != c[1] {
			t.Errorf("filenameFromDisposition(%q) = %q, want %q", c[0], got, c[1])
		}
	}
}

// 名字来源的可信度顺序：服务器自己给的名字 > 重定向后的真实地址 > 原地址 >
// 查询串里的文件名/附件号 > 主干 + 推的后缀。
func TestProbeName(t *testing.T) {
	parse := func(t *testing.T, s string) *url.URL {
		t.Helper()
		u, err := checkPreviewURL(s)
		if err != nil {
			t.Fatalf("构造地址失败 %q: %v", s, err)
		}
		return u
	}
	jpgURL := parse(t, "http://h/a.jpg")
	attPHP := parse(t, "http://h/att.php")

	cases := []struct {
		desc            string
		disposition, ct string
		final, orig     *url.URL
		want            string
	}{
		{"Content-Disposition 最可信", `attachment; filename="real.png"`, "image/jpeg", jpgURL, attPHP, "real.png"},
		{"重定向后的地址末段带名字", "", "image/jpeg", parse(t, "http://h/upload/day_240501/猫.jpg"), attPHP, "猫.jpg"},
		{"原地址末段可用", "", "image/gif", nil, parse(t, "http://h/pic.gif"), "pic.gif"},
		{"查询串本身就是文件名", "", "image/png", nil, parse(t, "http://h/att.php?fn=cat.png"), "cat.png"},
		// 日志里抓到的真实地址：水木的查询串根本没有键，整串就是带后缀的存储名。
		{"水木那种无键查询串", "", "image/png", nil, parse(t, "https://www.newsmth.net/att.php?ZM3HKEGY0g5nxakQFkILYAgAAUjYEAOUAAABkm0CB.png"), "ZM3HKEGY0g5nxakQFkILYAgAAUjYEAOUAAABkm0CB.png"},
		{"查询串里的附件号补后缀", "", "image/jpeg", nil, parse(t, "http://h/att.php?board=Photo&id=47743632"), "47743632.jpg"},
		{"附件号优先取 attid", "", "video/mp4", nil, parse(t, "http://h/att.php?id=1&attid=9988"), "9988.mp4"},
		{"附件号里塞路径不给名字", "", "image/png", nil, parse(t, "http://h/att.php?id=../../x"), ""},
		{"主干是脚本名时不乱凑", "", "image/png", nil, attPHP, ""},
		{"长附件号优先于尾下标", "", "image/jpeg", nil, parse(t, "http://www.mysmth.net/nForum/att/AutoBoard/47743632/0"), "47743632.jpg"},
		{"不是媒体不硬凑后缀", "", "text/html", nil, parse(t, "http://h/topic"), ""},
		{"推不出后缀宁可不给", "", "application/octet-stream", nil, parse(t, "http://h/att/47743632"), ""},
	}
	for _, c := range cases {
		kind := kindFromContentType(c.ct)
		if got := probeName(c.disposition, c.ct, kind, c.final, c.orig); got != c.want {
			t.Errorf("%s：probeName = %q, want %q", c.desc, got, c.want)
		}
	}
}

// 查询串里什么算名字、什么不算：?a=1.2 这种也带个点，不能当成文件名。
func TestNameFromQuery(t *testing.T) {
	parse := func(t *testing.T, s string) *url.URL {
		t.Helper()
		u, err := checkPreviewURL(s)
		if err != nil {
			t.Fatalf("构造地址失败 %q: %v", s, err)
		}
		return u
	}
	cases := [][3]string{
		{"http://h/att.php?board=Photo&style=3", "jpg", ""},
		{"http://h/dl?a=1.2&b=3", "jpg", ""},                   // 带点的数值不是名字
		{"http://h/dl?f=pic.tar.gz", "jpg", "pic.tar.gz"},      // 真实后缀放行
		{"http://h/dl?name=%E7%8C%AB.png", "png", "猫.png"},     // 转义要还原
		{"http://h/dl?path=..%2F..%2Fxk.xml", "xml", "xk.xml"}, // 目录部分不能跟着进来
		{"http://h/att.php?id=47743632", "jpg", "47743632.jpg"},
		{"http://h/att.php?id=47743632", "", ""}, // 推不出后缀就不凑
	}
	for _, c := range cases {
		got := nameFromQuery(parse(t, c[0]), c[1])
		if got != c[2] {
			t.Errorf("nameFromQuery(%q, ext=%q) = %q, want %q", c[0], c[1], got, c[2])
		}
	}
}

// 另存为的默认名：前端送来的名字不可信时再探一次，探到了就用它。
func TestSuggestedSaveName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Disposition", `attachment; filename="photo.png"`)
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL + "/att.php")
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	if got := suggestedSaveName("att.php", u); got != "photo.png" {
		t.Errorf("没名字时应重探一次，实际 %q", got)
	}
	if got := suggestedSaveName("已知名字.jpg", u); got != "已知名字.jpg" {
		t.Errorf("名字可用时不该再去问服务器，实际 %q", got)
	}
	dead, _ := url.Parse("http://127.0.0.1:1/att.php")
	if got := suggestedSaveName("att.php", dead); got != "att.php" {
		t.Errorf("探不到就保留原判，实际 %q", got)
	}
}
