package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 启动器的可测内核：打分、排序、路径白名单。窗口与 COM 那两头不碰。

func TestFuzzyScore(t *testing.T) {
	cases := []struct {
		query, name string
		want0       bool // 只验「不该匹配」
		minimum     int  // 只验「分数下限」
	}{
		{query: "chrome", name: "Chrome", minimum: 1000},       // 全等吃最高档
		{query: "chr", name: "Google Chrome", minimum: 300},    // 词首子序列
		{query: "chrome", name: "Chrome Canary", minimum: 690}, // 前缀（-len 差）
		{query: "cmd", name: "Command Prompt", minimum: 300},   // c-m-d 分散但词首
		{query: "gitch", name: "Git Bash", want0: true},        // 吃不完就不算匹配
		{query: "xyz", name: "Google Chrome", want0: true},
		{query: "", name: "Chrome", want0: true},
		{query: "CHROME", name: "chrome", minimum: 1000}, // 大小写不敏感
		{query: "记事", name: "记事本", minimum: 690},         // 中文前缀
		{query: "本", name: "记事本", minimum: 300},          // 中文子序列
	}
	for _, c := range cases {
		got := fuzzyScore(c.query, c.name)
		if c.want0 && got != 0 {
			t.Errorf("fuzzyScore(%q,%q)=%d，应为 0", c.query, c.name, got)
		}
		if !c.want0 && got < c.minimum {
			t.Errorf("fuzzyScore(%q,%q)=%d，应 ≥%d", c.query, c.name, got, c.minimum)
		}
	}
}

// TestFuzzyScoreRanking 校相对档位：全等 > 前缀 > 词首连续 > 分散子序列。
func TestFuzzyScoreRanking(t *testing.T) {
	scores := map[string]int{
		"exact":   fuzzyScore("Code", "Code"),
		"prefix1": fuzzyScore("chrome", "Chrome Browser"),
		"prefix2": fuzzyScore("chrome", "Chronograph Studio"),
		"bound1":  fuzzyScore("cmd", "Command Prompt"),
		"bound2":  fuzzyScore("cmd", "Acme Disk Manager"),
	}
	if scores["exact"] <= scores["prefix1"] {
		t.Errorf("全等应压过前缀：%d vs %d", scores["exact"], scores["prefix1"])
	}
	if scores["prefix1"] <= scores["prefix2"] {
		t.Errorf("同为前缀，目标应越短越优先：%d vs %d", scores["prefix1"], scores["prefix2"])
	}
	if scores["bound1"] <= scores["bound2"] {
		t.Errorf("词首连续命中应压过散落在词中的子序列：%d vs %d", scores["bound1"], scores["bound2"])
	}
}

func TestSearchEntries(t *testing.T) {
	entries := []LauncherEntry{
		{Name: "记事本", Path: `C:\ProgramData\Microsoft\Windows\Start Menu\Programs\记事本.lnk`, Target: `C:\Windows\System32\notepad.exe`, PinyinFull: "jishiben", PinyinShort: "jsb"},
		{Name: "Google Chrome", Path: `C:\Users\a\AppData\Roaming\Microsoft\Windows\Start Menu\Programs\Google Chrome.lnk`, Target: `C:\Program Files\Google\Chrome\Application\chrome.exe`},
		{Name: "Git Bash", Path: `C:\Users\a\AppData\Roaming\Microsoft\Windows\Start Menu\Programs\Git\Git Bash.lnk`, Target: `C:\Program Files\Git\git-bash.exe`},
		{Name: "计算器", Path: uwpLauncherPath + "Microsoft.WindowsCalculator_8wekyb3d8bbwe!App", Alias: "Windows Calculator", PinyinFull: "jisuanqi", PinyinShort: "jsq"},
	}

	// UWP 中文显示名靠包名别名吃到拉丁查询：calc 要能命中「计算器」。
	if got := searchEntries(entries, "calc", 20); len(got) != 1 || got[0].Name != "计算器" {
		t.Errorf("calc 应经 Alias 命中计算器：%+v", got)
	}
	// 拼音两路：首字母 jsb 与全拼 jishiben 都要能命中「记事本」。
	if got := searchEntries(entries, "jsb", 20); len(got) != 1 || got[0].Name != "记事本" {
		t.Errorf("首字母 jsb 应命中记事本：%+v", got)
	}
	if got := searchEntries(entries, "jishiben", 20); len(got) != 1 || got[0].Name != "记事本" {
		t.Errorf("全拼 jishiben 应命中记事本：%+v", got)
	}
	// 全拼面不吃其它中文名的子序列：jisuanqi 与 jishiben 只差几个字母，
	// 拼音面不能把两个应用都捣成一团（jsq 只该命中计算器）。
	if got := searchEntries(entries, "jsq", 20); len(got) != 1 || got[0].Name != "计算器" {
		t.Errorf("首字母 jsq 应只命中计算器：%+v", got)
	}
	// 中文显示名直命中；英文查询经 target exe 名 boost 也能命中「记事本」。
	if got := searchEntries(entries, "记事", 20); len(got) != 1 || got[0].Name != "记事本" {
		t.Errorf("中文查询失败：%+v", got)
	}
	if got := searchEntries(entries, "notepad", 20); len(got) != 1 || got[0].Name != "记事本" {
		t.Errorf("exe 名 boost 失败：%+v", got)
	}
	if got := searchEntries(entries, "chr", 20); len(got) != 1 || got[0].Name != "Google Chrome" {
		t.Errorf("子序列命中失败：%+v", got)
	}
	// 空查询按名称排序列全量，limit 截断。
	if got := searchEntries(entries, "", 2); len(got) != 2 || got[0].Name != "Git Bash" {
		t.Errorf("空查询应排序截断：%+v", got)
	}
	// 无命中返回空列表而不是 nil 崩溃。
	if got := searchEntries(entries, "zzzz", 20); len(got) != 0 {
		t.Errorf("不该有命中：%+v", got)
	}
}

// TestParseStartAppsCSV 验两列 CSV 的解析：表头跳过、引号内逗号与 "" 转义、
// 引号外的散输出（powershell 报错混进 stdout 的形状）不能成行。
func TestParseStartAppsCSV(t *testing.T) {
	out := "\"Name\",\"AppID\"\n" +
		"\"计算器\",\"Microsoft.WindowsCalculator_8wekyb3d8bbwe!App\"\n" +
		"\"记事本, 专业版\",\"Fake.Note!App\"\n" +
		"\"带\"\"引号\"\"的名字\",\"Fake.Quote!App\"\n" +
		"\" 单击以执行\",\"MicrosoftWindows.Client.CoreAI_cw5n1h2txyewy!ClickToDoApp\"\n" +
		"ConvertTo-Csv : 某些乱入的报错文本\n" +
		"\"空AppID\",\"C:\\path\\to.lnk\"\n"
	apps := parseStartAppsCSV(out)
	if len(apps) != 5 {
		t.Fatalf("应解析出 5 行：%+v", apps)
	}
	if apps[0].Name != "计算器" {
		t.Errorf("中文名失败：%q", apps[0].Name)
	}
	if apps[1].Name != "记事本, 专业版" {
		t.Errorf("引号内逗号被切碎：%q", apps[1].Name)
	}
	if apps[2].Name != `带"引号"的名字` {
		t.Errorf("双引号转义失败：%q", apps[2].Name)
	}
	if apps[3].Name != " 单击以执行" {
		t.Errorf("引号内前导空格应原样交给 merge 层修剪：%q", apps[3].Name)
	}
}

// TestMergeUWPApps 验 UWP 补种的取舍：只收带「!」的 AppID；与已有快捷方式
// 同名（不分大小写）跳过；路径拼成 shell:AppsFolder 动词。
func TestMergeUWPApps(t *testing.T) {
	base := []LauncherEntry{{Name: "记事本", Path: `C:\Start Menu\记事本.lnk`}}
	apps := []startApp{
		{Name: "计算器", AppID: "Microsoft.WindowsCalculator_x!App"},
		{Name: " 记事本 ", AppID: "Microsoft.WindowsNotepad_x!App"}, // 同名，让位给 .lnk
		{Name: "Chrome", AppID: `C:\Programs\Google Chrome.lnk`}, // 桌面应用，不收
		{Name: "", AppID: "Fake!App"},                            // 空名，不收
	}
	got := mergeUWPApps(base, apps)
	if len(got) != 2 {
		t.Fatalf("应只补一条计算器：%+v", got)
	}
	if got[1].Name != "计算器" || got[1].Path != uwpLauncherPath+"Microsoft.WindowsCalculator_x!App" {
		t.Errorf("UWP 条目形状不对：%+v", got[1])
	}
	if got[1].Alias != "Windows Calculator" {
		t.Errorf("UWP 别名应拆自包名：%q", got[1].Alias)
	}
}

// TestUWPAlias 验包名到可搜英文名的提取：连续大写词、哈希包名、无点分段的形状。
func TestUWPAlias(t *testing.T) {
	cases := []struct{ appID, want string }{
		{"Microsoft.WindowsCalculator_8wekyb3d8bbwe!App", "Windows Calculator"},
		{"Microsoft.MicrosoftPCManager_8wekyb3d8bbwe!App", "Microsoft PC Manager"},
		{"Microsoft.ZuneVideo_8wekyb3d8bbwe!Microsoft.ZuneVideo", "Zune Video"},
		{"3AD8A913.49937325EFC59_j4r74wbqry4cc!App", ""}, // 哈希包名没有可搜面
		{"AppWithoutDots_Sub!X", "App Without Dots"},     // 无点分段但含小写，照拆
	}
	for _, c := range cases {
		if got := uwpAlias(c.appID); got != c.want {
			t.Errorf("uwpAlias(%q)=%q，应为 %q", c.appID, got, c.want)
		}
	}
}

// TestPinyinFaces 验汉字名的全拼/首字母两面，与不含汉字时的静默跳过。
func TestPinyinFaces(t *testing.T) {
	cases := []struct {
		name      string
		wantFull  string
		wantShort string
	}{
		{name: "记事本", wantFull: "jishiben", wantShort: "jsb"},
		{name: "计算器", wantFull: "jisuanqi", wantShort: "jsq"},
		{name: "微信 WeChat", wantFull: "weixinwechat", wantShort: "wx"}, // 字母只进全拼面
		{name: "Google Chrome", wantFull: "", wantShort: ""},           // 纯拉丁名不生成
		{name: "记事本 (2)", wantFull: "jishiben2", wantShort: "jsb"},     // 括号空格丢、数字只留全拼面
	}
	for _, c := range cases {
		full, short := pinyinFaces(c.name)
		if full != c.wantFull || short != c.wantShort {
			t.Errorf("pinyinFaces(%q)=(%q,%q)，应为 (%q,%q)", c.name, full, short, c.wantFull, c.wantShort)
		}
	}
}

// TestNormalizePreferencesLauncherFields 验搜索范围的入库归一：目录剪空白去重，
// 后缀小写去重，整列被剪空（或旧 prefs.json 没这个键）退回出厂值 ——
// 「一个后缀都不搜」不是能成立的状态。
func TestNormalizePreferencesLauncherFields(t *testing.T) {
	p := Preferences{
		LauncherDirs: []string{"  C:\\Apps  ", "C:\\Tools", "", "C:\\Apps"},
		LauncherExts: []string{" EXE ", "bat", "", "exe"},
	}
	got := normalizePreferences(p)
	if len(got.LauncherDirs) != 2 || got.LauncherDirs[0] != `C:\Apps` || got.LauncherDirs[1] != `C:\Tools` {
		t.Errorf("目录归一失败：%q", got.LauncherDirs)
	}
	if len(got.LauncherExts) != 2 || got.LauncherExts[0] != "exe" || got.LauncherExts[1] != "bat" {
		t.Errorf("后缀归一失败：%q", got.LauncherExts)
	}
	// 两个空形状：nil（旧配置没键）与全空白列，都要拿回出厂后缀。
	for _, bad := range [][]string{nil, {" ", ","}} {
		g := normalizePreferences(Preferences{LauncherExts: bad})
		if strings.Join(g.LauncherExts, ",") != strings.Join(defaultLauncherExts, ",") {
			t.Errorf("空后缀列应回出厂：%q", g.LauncherExts)
		}
	}
}

// TestScanDirFiles 用临时目录验真实扫描：命中后缀、递归子目录、跳过 $ 开头目录、
// 不认的后缀不收、seen 命中的路径不列两遍、目录不存在静默跳过。
func TestScanDirFiles(t *testing.T) {
	root := t.TempDir()
	mkdir := func(s string) string { d := filepath.Join(root, s); os.MkdirAll(d, 0o755); return d }
	touch := func(s string) string { p := filepath.Join(root, s); os.WriteFile(p, []byte("x"), 0o644); return p }
	mkdir("tools")
	touch("app.exe")
	touch("tools\\nested.bat")
	touch("readme.txt") // 不认的后缀
	touch("noext")      // 没后缀
	mkdir("$RECYCLE.BIN")
	touch("$RECYCLE.BIN\\ghost.exe")

	seen := map[string]bool{strings.ToLower(touch("dup.exe")): true}
	got := scanDirFiles([]string{root, filepath.Join(root, "gone")}, []string{"exe", "bat"}, seen)

	want := map[string]bool{
		strings.ToLower(filepath.Join(root, "app.exe")):             true,
		strings.ToLower(filepath.Join(root, "tools", "nested.bat")): true,
	}
	if len(got) != len(want) {
		t.Fatalf("应只收 app.exe 与 nested.bat：%+v", got)
	}
	for _, e := range got {
		if !want[strings.ToLower(e.Path)] {
			t.Errorf("意外条目：%+v", e)
		}
		// 目录文件的 Target 就是自身：exe 名那一面不靠解析。
		if e.Target != e.Path {
			t.Errorf("Target 应等于 Path：%+v", e)
		}
	}
	// 没目录可搜 / 没后缀都不该报错，只该空手。
	if got := scanDirFiles(nil, []string{"exe"}, map[string]bool{}); got != nil {
		t.Errorf("空目录列表应空手：%+v", got)
	}
	if got := scanDirFiles([]string{root}, nil, map[string]bool{}); got != nil {
		t.Errorf("空后缀列表应空手：%+v", got)
	}
}

// TestLauncherInvokeRejectsUnknownPath 验白名单：这是全局热键可达的代码路径，
// 搜索框不能变成 Run 对话框。成功分支会真的拉起应用，不在测试里跑。
func TestLauncherInvokeRejectsUnknownPath(t *testing.T) {
	s := &LauncherService{entries: []LauncherEntry{{Name: "x", Path: `C:\Start Menu\x.lnk`}}}
	err := s.LauncherInvoke(`C:\Windows\System32\calc.exe`, false)
	if err == nil || !strings.Contains(err.Error(), "启动器不认识") {
		t.Fatalf("任意路径应被拒绝，实际 err=%v", err)
	}
	// 提权路同样先过白名单：不在索引里的路径，换个动词也旁路不了。
	err = s.LauncherInvoke(`C:\Windows\System32\calc.exe`, true)
	if err == nil || !strings.Contains(err.Error(), "启动器不认识") {
		t.Fatalf("elevate 也应被白名单拒绝，实际 err=%v", err)
	}
	// 索引内的 UWP 路径：提权要在真的去跑 shell 动词之前被拦下（错误文案直说原因）。
	u := &LauncherService{entries: []LauncherEntry{{Name: "计算器", Path: uwpLauncherPath + "Microsoft.WindowsCalculator_x!App"}}}
	if err := u.LauncherInvoke(uwpLauncherPath+"Microsoft.WindowsCalculator_x!App", true); err == nil ||
		!strings.Contains(err.Error(), "UWP 应用无法以管理员") {
		t.Fatalf("UWP 提权应被明确拒绝，实际 err=%v", err)
	}
	// 数据文件不再拿扩展名白名单武断拦（用户主动把 png 加进可搜后缀就是要试）：
	// 提权路不在白名单/UWP 两道前置里报错，会走到真的 launcherShellExecute。
	// 测试环境不真拉起，只验它不再返回“不是程序或脚本”那种误拦文案。
	d := &LauncherService{entries: []LauncherEntry{{Name: "图", Path: `C:\pics\a.png`, Target: `C:\pics\a.png`}}}
	if err := d.LauncherInvoke(`C:\pics\a.png`, true); err != nil && strings.Contains(err.Error(), "不是程序或脚本") {
		t.Fatalf("png 不该再被扩展名白名单误拦：%v", err)
	}
	// 空索引同样拒绝（索引没建好之前按回车不该有副作用）。
	e := &LauncherService{}
	if err := e.LauncherInvoke(`C:\Start Menu\x.lnk`, false); err == nil {
		t.Fatal("空索引应拒绝")
	}
}

// TestSeedFactoryGlobalBindingsLauncherKey 验老用户拿到出厂双击 Ctrl+Alt 的那一跳：
// 已有表里没有 launcher.toggle 时补一次；重复加载幂等；用户改过键的不强塞。
func TestSeedFactoryGlobalBindingsLauncherKey(t *testing.T) {
	cur := GlobalKeymap{
		Enabled:     true,
		DoubleTapMs: 400,
		Bindings:    []GlobalKey{{Keys: "M-`", Action: "quake.toggle", Enabled: true}},
	}
	seedFactoryGlobalBindings(&cur, DefaultGlobalKeymap())
	var hit *GlobalKey
	for i := range cur.Bindings {
		if cur.Bindings[i].Action == "launcher.toggle" {
			hit = &cur.Bindings[i]
		}
	}
	if hit == nil || !hit.Enabled || hit.Keys != "<C-M> <C-M>" {
		t.Fatalf("应补种双击 Ctrl+Alt → launcher.toggle：%+v", cur.Bindings)
	}
	// 幂等：台账已记，再补一次不应变胖。
	n := len(cur.Bindings)
	seedFactoryGlobalBindings(&cur, DefaultGlobalKeymap())
	if len(cur.Bindings) != n {
		t.Fatalf("重复补种：%d → %d", n, len(cur.Bindings))
	}
	// 用户把启动器改到了别的键上：动作 id 还在，不该再多出一条出厂行。
	custom := GlobalKeymap{Bindings: []GlobalKey{{Keys: "M-p", Action: "launcher.toggle", Enabled: true}}}
	seedFactoryGlobalBindings(&custom, DefaultGlobalKeymap())
	hits := 0
	for _, b := range custom.Bindings {
		if b.Action == "launcher.toggle" {
			hits++
		}
	}
	if hits != 1 {
		t.Fatalf("用户改键后不该强塞出厂行：%+v", custom.Bindings)
	}
}
