package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// 本机 shell 这套逻辑的回归重点有三处：命令行拼引号（拼错就直接起不来进程）、
// 站点字段映射（保活开着会往 shell 里灌 NUL）、以及同步时的补种/清理判断（把
// 「这次没探到」当成「已卸载」删掉用户条目是最伤人的）。

func TestWindowsQuoteArg(t *testing.T) {
	cases := []struct {
		in, want string
		reason   string
	}{
		{"-d", `-d`, "无空格不加引号"},
		{"", `""`, "空参数必须留一对引号，否则它会被吞掉、参数错位"},
		{`C:\Program Files\Git\bin\bash.exe`, `"C:\Program Files\Git\bin\bash.exe"`, "路径含空格"},
		{`Ubuntu 24.04 LTS`, `"Ubuntu 24.04 LTS"`, "发行版名含空格"},
		// 结尾反斜杠必须翻倍：单个反斜杠会转义掉闭合引号，引号配对就此崩掉。
		{`C:\my tools\`, `"C:\my tools\\"`, `结尾反斜杠必须翻倍，否则会吃掉闭合引号`},
		{`a"b`, `"a\"b"`, "内嵌引号要转义"},
		{`C:\"x"\`, `"C:\\\"x\"\\"`, "反斜杠紧邻引号才翻倍"},
	}
	for _, c := range cases {
		if got := windowsQuoteArg(c.in); got != c.want {
			t.Errorf("%s: windowsQuoteArg(%q) = %q, want %q", c.reason, c.in, got, c.want)
		}
	}
}

func TestWindowsCommandLine(t *testing.T) {
	got := windowsCommandLine([]string{`C:\Windows\System32\wsl.exe`, "-d", "Ubuntu 24.04 LTS"})
	want := `C:\Windows\System32\wsl.exe -d "Ubuntu 24.04 LTS"`
	if got != want {
		t.Errorf("命令行 = %q, want %q", got, want)
	}
}

func TestShellArgv(t *testing.T) {
	got := shellArgv(Site{Host: `C:\msys64\usr\bin\bash.exe`, ShellArgs: []string{"--login", "", "-i"}})
	if strings.Join(got, "|") != `C:\msys64\usr\bin\bash.exe|--login|-i` {
		t.Errorf("argv = %v，空参数应被剔除", got)
	}

	// 老配置（Local 协议但不填路径）退回系统默认 shell，等价于改名前的行为。
	fallback := shellArgv(Site{})
	if len(fallback) != 1 || fallback[0] == "" {
		t.Errorf("空路径未回退默认 shell：%v", fallback)
	}
}

func TestSiteFromShellTurnsOffServerStuff(t *testing.T) {
	args := []string{"-d", "Ubuntu"}
	s := siteFromShell(ShellSpec{ID: "wsl:Ubuntu", Name: "WSL · Ubuntu", Path: `/usr/bin/wsl.exe`, Args: args})

	if s.ProtocolType != 3 || s.Group != ShellSiteGroup {
		t.Fatalf("协议/分组不对：protocol=%d group=%q", s.ProtocolType, s.Group)
	}
	// 保活一旦开着，每隔 maxIdleSeconds 就往 shell 里写一个 NUL。
	if s.AntiIdle || s.AutoReconnect || s.EnableZmodem || s.MaxIdleSeconds != 0 {
		t.Errorf("本地 shell 不该带服务器侧开关：antiIdle=%v reconnect=%v zmodem=%v idle=%d",
			s.AntiIdle, s.AutoReconnect, s.EnableZmodem, s.MaxIdleSeconds)
	}
	// ConPTY 已经把控制台输出统一成 UTF-8，与系统代码页无关。
	if s.ServerEncodingID != 2 {
		t.Errorf("编码 = %d, want 2 (UTF-8)", s.ServerEncodingID)
	}
	if s.ShellID != "wsl:Ubuntu" || strings.Join(s.ShellArgs, " ") != "-d Ubuntu" || !s.Auto {
		t.Errorf("shell 字段丢失：id=%q args=%v auto=%v", s.ShellID, s.ShellArgs, s.Auto)
	}
	// 参数切片必须是副本：站点表里的改动不能反噬到探测结果上。
	s.ShellArgs[0] = "MUTATED"
	if args[0] != "-d" {
		t.Errorf("siteFromShell 没有复制参数切片")
	}
}

func TestNormalizeSiteLeavesLocalAlone(t *testing.T) {
	// 旧站点靠 maxIdleSeconds==0 判定并补默认值，而本地 shell 的 0 是「故意关掉」。
	// 这条是这轮最要紧的断言：一旦被补成 120 + antiIdle=true，shell 就会定时收到 NUL。
	s := normalizeSite(siteFromShell(ShellSpec{ID: "cmd", Name: "CMD", Path: `C:\Windows\System32\cmd.exe`}))
	if s.AntiIdle || s.MaxIdleSeconds != 0 || s.AntiIdleMessage != "" {
		t.Errorf("本地站点被补了保活：antiIdle=%v idle=%d msg=%q", s.AntiIdle, s.MaxIdleSeconds, s.AntiIdleMessage)
	}

	// 远程旧站点仍然要补。
	old := normalizeSite(Site{ProtocolType: 0})
	if !old.AntiIdle || old.MaxIdleSeconds != 120 || old.AntiIdleMessage != "^@" {
		t.Errorf("远程旧站点没补默认保活：antiIdle=%v idle=%d msg=%q", old.AntiIdle, old.MaxIdleSeconds, old.AntiIdleMessage)
	}
	// 同样在「旧站点」分支里补的两个显示开关：useAnsiColor 出厂为开，缺键拿到 false 会让
	// 整个版面默默变黑白；alwaysHighlight 出厂为关，零值正好，不应当被补成开。
	if !old.UseANSIColor || old.AlwaysHighlight {
		t.Errorf("旧站点显示开关默认值错：useAnsiColor=%v alwaysHighlight=%v", old.UseANSIColor, old.AlwaysHighlight)
	}
}

func TestShellSource(t *testing.T) {
	cases := map[string]string{"wsl:Ubuntu 24.04": "wsl", "cmd": "cmd", ":x": ":x", "pwsh:": "pwsh"}
	for id, want := range cases {
		if got := shellSource(id); got != want {
			t.Errorf("shellSource(%q) = %q, want %q", id, got, want)
		}
	}
}

func TestDecodeWslOutput(t *testing.T) {
	// 老 wsl.exe 往管道里写 UTF-16LE，新版写 UTF-8，两者都得认。
	utf16 := []byte{'U', 0, 'b', 0, 'u', 0, 'n', 0, 't', 0, 'u', 0, '\n', 0}
	if got := decodeWslOutput(utf16); got != "Ubuntu\n" {
		t.Errorf("UTF-16LE 解码 = %q", got)
	}
	if got := decodeWslOutput([]byte("Debian\r\n")); got != "Debian\r\n" {
		t.Errorf("UTF-8 原样透出 = %q", got)
	}
	// 奇数长度不能当 UTF-16 处理，否则会把内容截断一位。
	if got := decodeWslOutput([]byte{0x41, 0x00, 0x42}); got != "A\x00B" {
		t.Errorf("奇数长度 = %q", got)
	}
}

func TestDistroName(t *testing.T) {
	cases := map[string]string{
		"  Ubuntu 24.04 LTS  ": "Ubuntu 24.04 LTS", // 名字里的空格必须留住
		"\ufeff\"Alpine\"":     "Alpine",
		"   ":                  "",
	}
	for in, want := range cases {
		if got := distroName(in); got != want {
			t.Errorf("distroName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeShellPath(t *testing.T) {
	a := normalizeShellPath(`C:\Program Files\Git\bin\bash.exe`)
	b := normalizeShellPath(`c:\program files\git\bin\bash.exe`)
	if a != b {
		t.Errorf("同一路径大小写归一化后不等：%q vs %q", a, b)
	}
	if normalizeShellPath(`  "C:\x\pwsh.exe"  `) != normalizeShellPath(`C:\x\pwsh.exe`) {
		t.Errorf("外层引号/空白没被清掉")
	}
	if normalizeShellPath("   ") != "" {
		t.Errorf("空白应归一成空串")
	}
}

func TestInteractiveArgs(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Windows 侧参数完全由站点配置决定，这里塞东西只会让 cmd.exe 认不出来。
		if got := interactiveArgs("cmd.exe"); got != nil {
			t.Errorf("Windows 上 interactiveArgs 应为空：%v", got)
		}
		return
	}
	if got := interactiveArgs("bash"); len(got) != 1 || got[0] != "-i" {
		t.Errorf("bash 应补 -i：%v", got)
	}
	if got := interactiveArgs("fish"); got != nil {
		t.Errorf("fish 默认交互，不该加参数：%v", got)
	}
}

// --- 站点表同步：喂人造探测结果，断言补种/认领/修路径/清理四种走向 ---

func newTestConfigService(t *testing.T, sites ...Site) *ConfigService {
	t.Helper()
	st := &Store{dir: t.TempDir(), Sites: sites}
	// 先落一次盘：sites.json 存在，“没变化就不重写”那条断言才有时间戳可比。
	if err := st.saveJSON("sites.json", sites); err != nil {
		t.Fatalf("初始化 sites.json: %v", err)
	}
	return NewConfigService(st)
}

// touchShell 造一个存在的假可执行文件，让 progExists 的判断能跑起来。
func touchShell(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte("fake"), 0o755); err != nil {
		t.Fatalf("造测试文件: %v", err)
	}
	return p
}

func indexOfShell(t *testing.T, sites []Site, id string) int {
	t.Helper()
	for i, s := range sites {
		if s.ShellID == id {
			return i
		}
	}
	t.Fatalf("站点表里没有 ShellID=%q：%+v", id, sites)
	return -1
}

func TestMergeShellSitesSeedsOnce(t *testing.T) {
	ub := touchShell(t, "bash.exe")
	specs := []ShellSpec{{ID: "gitbash", Name: "Git Bash", Path: ub, Args: []string{"--login", "-i"}}}
	cs := newTestConfigService(t, Site{Name: "老站", Host: "bbs.example.com", ProtocolType: 0})

	res, err := cs.mergeShellSites(specs, map[string]bool{"gitbash": true})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if res.Added != 1 {
		t.Fatalf("added = %d, want 1", res.Added)
	}
	sites := cs.store.Sites
	if len(sites) != 2 {
		t.Fatalf("站点数 = %d, want 2", len(sites))
	}
	s := sites[indexOfShell(t, sites, "gitbash")]
	if s.Name != "Git Bash" || s.Host != ub || s.Group != ShellSiteGroup || !s.Auto {
		t.Errorf("补种条目不对： %+v", s)
	}

	// 第二次扫描必须完全没动静 —— 否则用户每启动一次就多一列重复 shell。
	res2, err := cs.mergeShellSites(specs, map[string]bool{"gitbash": true})
	if err != nil {
		t.Fatalf("merge2: %v", err)
	}
	if res2.Added+res2.Updated+res2.Removed != 0 || len(cs.store.Sites) != 2 {
		t.Fatalf("重复补种： %+v, 站点数 %d", res2, len(cs.store.Sites))
	}
}

func TestMergeShellSitesAdoptsLegacyByPath(t *testing.T) {
	// 用户早就手建过一条指向同一个程序的 Local 站点：只认领 ID，不再补一条，
	// 也不把它变成可自动删除的 Auto 条目。
	ub := touchShell(t, "bash.exe")
	legacy := Site{Name: "我的 bash", Host: strings.ToUpper(ub), ProtocolType: 3}
	cs := newTestConfigService(t, legacy)

	res, err := cs.mergeShellSites([]ShellSpec{{ID: "gitbash", Name: "Git Bash", Path: ub}}, map[string]bool{"gitbash": true})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if res.Added != 0 {
		t.Errorf("认领失败，又补了一条：added=%d", res.Added)
	}
	if len(cs.store.Sites) != 1 {
		t.Fatalf("站点数 = %d, want 1", len(cs.store.Sites))
	}
	s := cs.store.Sites[0]
	if s.ShellID != "gitbash" || s.Auto || s.Name != "我的 bash" {
		t.Errorf("认领后不该改掉的字段：id=%q auto=%v name=%q", s.ShellID, s.Auto, s.Name)
	}
}

func TestMergeShellSitesRepointsBrokenPath(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "pwsh.exe") // 7.4 目录已被升级清掉
	fresh := touchShell(t, "pwsh.exe")
	cs := newTestConfigService(t, Site{
		Name: "PowerShell 7", Host: gone, ProtocolType: 3,
		ShellID: "pwsh", Auto: true, Group: ShellSiteGroup,
	})

	res, err := cs.mergeShellSites([]ShellSpec{{ID: "pwsh", Name: "PowerShell 7", Path: fresh, Args: []string{"-NoLogo"}}},
		map[string]bool{"pwsh": true})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if res.Updated != 1 || cs.store.Sites[0].Host != fresh {
		t.Fatalf("失效路径没被指回新位置：updated=%d host=%q", res.Updated, cs.store.Sites[0].Host)
	}
	if strings.Join(cs.store.Sites[0].ShellArgs, " ") != "-NoLogo" {
		t.Errorf("参数没跟着刷新：%v", cs.store.Sites[0].ShellArgs)
	}
}

func TestMergeShellSitesPrunesOnlyProbed(t *testing.T) {
	// 卸载了 Git：探不到 gitbash 且这次探测确实可信 → 清掉自动条目。
	cs := newTestConfigService(t,
		Site{Name: "Git Bash", Host: `C:\Git\bin\bash.exe`, ProtocolType: 3, ShellID: "gitbash", Auto: true, Group: ShellSiteGroup},
		Site{Name: "WSL · Ubuntu", Host: `C:\Windows\System32\wsl.exe`, ProtocolType: 3, ShellID: "wsl:Ubuntu", Auto: true, Group: ShellSiteGroup},
		Site{Name: "我的 CMD", Host: `C:\Windows\System32\cmd.exe`, ProtocolType: 3, ShellID: "cmd", Auto: false, Group: "工具"},
	)
	probed := map[string]bool{"gitbash": true, "cmd": true} // 注意：没有 wsl

	res, err := cs.mergeShellSites([]ShellSpec{{ID: "cmd", Name: "CMD", Path: `C:\Windows\System32\cmd.exe`}}, probed)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if res.Removed != 1 {
		t.Fatalf("removed = %d, want 1（只该删掉 gitbash）", res.Removed)
	}
	var names []string
	for _, s := range cs.store.Sites {
		names = append(names, s.Name)
	}
	if !strings.Contains(strings.Join(names, "|"), "WSL · Ubuntu") {
		t.Errorf("wsl 探针不可信时不该删条目：%v", names)
	}
	// Auto=false 的手建条目哪怕 ShellID 撞上也不动。
	if !strings.Contains(strings.Join(names, "|"), "我的 CMD") {
		t.Errorf("手建条目被误删：%v", names)
	}
	if len(cs.store.Sites) != 2 {
		t.Errorf("站点数 = %d, want 2：%v", len(cs.store.Sites), names)
	}
}

func TestMergeShellSitesWritesDisk(t *testing.T) {
	ub := touchShell(t, "bash.exe")
	cs := newTestConfigService(t)
	if _, err := cs.mergeShellSites([]ShellSpec{{ID: "gitbash", Name: "Git Bash", Path: ub}}, map[string]bool{"gitbash": true}); err != nil {
		t.Fatalf("merge: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(cs.store.dir, "sites.json"))
	if err != nil {
		t.Fatalf("读落盘文件: %v", err)
	}
	if !strings.Contains(string(b), `"shellID"`) || !strings.Contains(string(b), `"auto": true`) {
		t.Errorf("sites.json 里没带上 shell 字段：%s", b)
	}
}

func TestMergeShellSitesNoChangeDoesNotWrite(t *testing.T) {
	cs := newTestConfigService(t, Site{Name: "老站", Host: "bbs.example.com", ProtocolType: 0})
	before := siteFileMtime(t, cs)
	if _, err := cs.mergeShellSites(nil, map[string]bool{}); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if got := siteFileMtime(t, cs); !got.Equal(before) {
		t.Errorf("没有任何变化却重写了 sites.json（%v -> %v），会白刷用户配置时间戳", before, got)
	}
}

func siteFileMtime(t *testing.T, cs *ConfigService) time.Time {
	t.Helper()
	fi, err := os.Stat(filepath.Join(cs.store.dir, "sites.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return time.Time{}
		}
		t.Fatalf("stat sites.json: %v", err)
	}
	return fi.ModTime()
}
