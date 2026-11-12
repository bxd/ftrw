package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/mozillazg/go-pinyin"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

// Alfred 式启动器：一个隐藏常驻的搜索窗，全局热键（默认双击 Ctrl+Alt）呼出，
// 输入即模糊搜索「应用程序快捷方式」，回车启动，ESC / 失焦收起。
//
// 为什么预建窗口而不是用的时候再建：webview 冷启动要几百毫秒，而启动器的全部
// 手感就在「按下即现」。窗口建在 Hidden 状态，页面脚本早早挂好，呼出只是
// Show + 抢前台（forceForeground 那套前台锁处理与 quake/预览窗完全同源）。
//
// 首版只做应用搜索：索引扫开始菜单（用户+全体）与桌面（用户+公用）的 .lnk，
// 文件搜索、命令、书签留待后续。启动一律 ShellExecuteW 打 .lnk 本体，系统
// 负责参数与工作目录，所以 TargetPath 解析只服务于匹配，解析失败不影响启动。

const (
	// launcherPageURL 走 main.ts 的 ?view= 分流，与预览窗同一套机制：共用前端
	// 产物，不给启动器单开构建入口。
	launcherPageURL = "/?view=launcher"

	launcherWindowName = "launcher"

	// launcherEventShow 通知页面「又被呼出了」：清空上次的输入并重新聚焦。
	launcherEventShow = "launcher:show"

	// launcherEventReveal 通知主窗口「把这条路径在文件管理器里显示出来」：
	// Shift+Enter 只在左栏定位选中，Alt+Enter 额外在右栏预览。接收方见
	// App.vue（复用/新建文件管理器标签）与 FileManagerView.vue（实际定位）。
	launcherEventReveal = "fm:reveal"

	// launcherMaxResults 一屏给键盘够选的宽度，再多也没人滚。
	launcherMaxResults = 20

	// launcherWidth/Height 是窗口 DIP 尺寸。Alfred 式：宽条居中、悬在工作区上方。
	launcherWidth  = 640
	launcherHeight = 420
)

// LauncherEntry 是一条索引项。Path 是 .lnk 全路径或 shell:AppsFolder 动词（invoke 用它），
// Target 是解析出的 exe 路径（可能为空），Alias 是额外的可搜英文名（UWP 从包名
// 拆词而来：显示名叫「计算器」时，calc 也能命中）。
// PinyinFull/PinyinShort 只在名称含汉字时生成：「记事本」同时能被 jishiben（全拼）
// 和 jsb（首字母）搜到。
type LauncherEntry struct {
	Name        string
	Path        string
	Target      string
	Alias       string
	PinyinFull  string
	PinyinShort string
}

// LauncherResult 交给前端的一条命中。
type LauncherResult struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Target string `json:"target"`
	Score  int    `json:"score"`
}

// LauncherInfo 是索引状态快照，页面用它显示占位提示。
type LauncherInfo struct {
	Ready    bool `json:"ready"`
	Count    int  `json:"count"`
	Indexing bool `json:"indexing"`
}

// LauncherRevealPayload 是 fm:reveal 事件的载荷。Path 是要在文件管理器里定位的
// 文件全路径（一定来自索引项）；Preview 为真时额外在右栏就地预览。
type LauncherRevealPayload struct {
	Path    string `json:"path"`
	Preview bool   `json:"preview"`
}

// LauncherService 管启动器窗口与索引。零值不可用，须经 NewLauncherService + Bind。
type LauncherService struct {
	app  *application.App
	main *application.WebviewWindow
	ctl  dialogGuarder

	mu       sync.Mutex
	win      *application.WebviewWindow
	hwnd     uintptr
	visible  bool
	entries  []LauncherEntry
	ready    bool
	indexing bool
	prefs    Preferences
}

func NewLauncherService() *LauncherService {
	return &LauncherService{prefs: defaultPreferences()}
}

// Bind 在 main 建好主窗口之后注入依赖。ctl 允许传 nil（测试形态）。
//
//wails:ignore
func (s *LauncherService) Bind(app *application.App, main *application.WebviewWindow, ctl dialogGuarder) {
	s.app = app
	s.main = main
	s.ctl = ctl
}

// ApplyPrefs 收下搜索范围那份偏好（由 main.go 在启动与每次偏好保存后调用）。
// 调用方要不要重建由自己定：启动路径上 Init 紧跟着就建，不必重复一轮。
func (s *LauncherService) ApplyPrefs(p Preferences) {
	s.mu.Lock()
	s.prefs = p
	s.mu.Unlock()
}

/* --------------------------------------------------------------------------- */
/* 生命周期                                                                     */
/* --------------------------------------------------------------------------- */

// Init 预建隐藏窗口并后台建索引。由 ApplicationStarted 触发：窗口要能安全地
// 往主线程 Invoke，得等应用跑起来。
func (s *LauncherService) Init() {
	if err := s.ensureWindow(); err != nil {
		log.Printf("启动器窗口创建失败（热键将无窗口可用）：%v", err)
		return
	}
	log.Printf("启动器窗口已就绪（隐藏常驻，%dx%d）", launcherWidth, launcherHeight)
	go func() {
		if err := s.rebuildIndex(); err != nil {
			log.Printf("启动器索引构建失败：%v", err)
		}
	}()
}

// ensureWindow 建窗（幂等）。形状抄 preview.go 的 newWindow，区别是不模态：
// 启动器是一个随叫随到的浮层，靠自己的置顶与失焦收起，不碰 AttachModal。
func (s *LauncherService) ensureWindow() error {
	if s.app == nil || s.main == nil {
		return errors.New("启动器服务还没接上主窗口（应用尚未启动）")
	}
	s.mu.Lock()
	if s.win != nil {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	win := s.app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:             launcherWindowName,
		Title:            "启动器",
		Width:            launcherWidth,
		Height:           launcherHeight,
		DisableResize:    true,
		Frameless:        true,
		Hidden:           true,
		URL:              launcherPageURL,
		BackgroundColour: application.NewRGB(24, 26, 30),
		Windows:          application.WindowsWindow{HiddenOnTaskbar: true},
	})
	if win == nil {
		return errors.New("创建启动器窗口失败")
	}

	var hwnd uintptr
	application.InvokeSync(func() {
		if p := win.NativeWindow(); p != nil {
			hwnd = uintptr(p)
		}
	})

	// 失焦即收起：与 quake 的 blur hook 同形状。护栏置位期间（本窗可见期间）主窗
	// 的 quake 收起被 ctl 按住，反过来本窗不需要护栏 —— 它就是被抢焦点才该收。
	win.OnWindowEvent(events.Common.WindowLostFocus, func(*application.WindowEvent) {
		s.hide()
	})
	// 页面内 ESC 是主路（见 LauncherApp.vue），这条窗口级绑定是 webview 按键
	// 分发能命中时的保险。
	win.RegisterKeyBinding("escape", func(application.Window) { s.hide() })

	x, y, w, h := s.geometry()
	win.SetBounds(application.Rect{X: x, Y: y, Width: w, Height: h})

	s.mu.Lock()
	s.win = win
	s.hwnd = hwnd
	s.mu.Unlock()
	return nil
}

// geometry 取当前主窗所在屏的工作区，水平居中、悬在上方约 18% 处。
// 用工作区而不是整屏：任务栏顶部的屏不该被启动器盖出边界。
func (s *LauncherService) geometry() (x, y, w, h int) {
	w, h = launcherWidth, launcherHeight
	screen, err := s.main.GetScreen()
	if err != nil || screen == nil {
		return 100, 100, w, h // 兜底：宁可贴角也不要建不出窗
	}
	wa := screen.WorkArea
	x = wa.X + (wa.Width-w)/2
	y = wa.Y + wa.Height*18/100
	if y+h > wa.Y+wa.Height {
		y = wa.Y + wa.Height - h
	}
	return x, y, w, h
}

/* --------------------------------------------------------------------------- */
/* 呼出 / 收起                                                                  */
/* --------------------------------------------------------------------------- */

// Toggle 是全局热键与托盘菜单的公共入口（跑在热键动作 goroutine 上）：
// 已显示则收起，否则呼出并聚焦。
func (s *LauncherService) Toggle() {
	s.mu.Lock()
	visible := s.visible
	win := s.win
	s.mu.Unlock()
	if win == nil {
		log.Printf("启动器窗口不可用（创建失败过），呼出被忽略")
		return
	}
	if visible {
		s.hide()
		return
	}
	s.show(win)
}

func (s *LauncherService) show(win *application.WebviewWindow) {
	// 护栏赶在抢焦点之前：本窗 Show 必然让主窗失焦，quake 的收起钩子不该因此
	// 把终端下拉吞掉（与 preview 的时序教训同一条）。
	if s.ctl != nil {
		s.ctl.SetDialogGuard(true)
	}
	// 主窗若是 quake，它是 WS_EX_TOPMOST；启动器不跟着置顶会被自家终端压住。
	win.SetAlwaysOnTop(true)
	win.Show()
	s.forceFront()
	win.Focus()
	// 通知页面重置输入并聚焦搜索框（webview 早就算好了 DOM，事件一来即清）。
	win.EmitEvent(launcherEventShow, nil)

	s.mu.Lock()
	s.visible = true
	s.mu.Unlock()
}

// hide 收起窗口。可重复调用；失焦钩子与 ESC 两条路都汇到这儿。
func (s *LauncherService) hide() {
	s.mu.Lock()
	win, visible := s.win, s.visible
	s.visible = false
	s.mu.Unlock()
	if win == nil || !visible {
		return
	}
	win.Hide()
	if s.ctl != nil {
		s.ctl.SetDialogGuard(false)
	}
}

// forceFront 把启动器窗提到 OS 前台（热键触发时本进程是后台应用，裸 Focus()
// 会被前台锁拦下 —— 机制与 AppService.forceShowFront 完全相同，这里操作的是
// 自己的 hwnd，就不把 AppService 的那扇门再开一个口子了）。
func (s *LauncherService) forceFront() {
	s.mu.Lock()
	hwnd := s.hwnd
	s.mu.Unlock()
	forceForeground(hwnd)
}

// LauncherHide 给页面内 ESC 用（webview 里按键的直连路径）。
func (s *LauncherService) LauncherHide() { s.hide() }

/* --------------------------------------------------------------------------- */
/* 索引                                                                         */
/* --------------------------------------------------------------------------- */

// rebuildIndex 重扫快捷方式与用户指定目录。平台实现在 launcherindex_windows.go
// （非 Windows 是空桩）。
func (s *LauncherService) rebuildIndex() error {
	s.mu.Lock()
	if s.indexing {
		s.mu.Unlock()
		return nil
	}
	s.indexing = true
	prefs := s.prefs
	s.mu.Unlock()

	entries, err := scanAppShortcuts(prefs)
	removed := 0
	if err == nil {
		deduped := dedupByTarget(entries)
		removed = len(entries) - len(deduped)
		entries = deduped
	}

	s.mu.Lock()
	s.indexing = false
	if err == nil {
		s.entries = entries
		s.ready = true
	}
	n := len(s.entries)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	log.Printf("启动器索引完成：%d 个快捷方式（同目标合并 %d 条）", n, removed)
	return nil
}

// dedupByTarget 把指向同一目标的条目收敛成一条（大小写不敏感）：同一应用常在
// 开始菜单与桌面各躺一份 .lnk，指定目录扫描也可能撞上同一个 exe。只留先扫到的
// 那条（扫描顺序：用户开始菜单 → 全体开始菜单 → 桌面 → 公用桌面 → 指定目录）。
// Target 为空的（.lnk 解析失败 / UWP）不参与判重：没有可比的目标，宁可多列不误并。
// 注意与「同名不同目标」是两回事：那种故意保留两条让用户自己选（见索引处注释）。
func dedupByTarget(entries []LauncherEntry) []LauncherEntry {
	seen := make(map[string]bool, len(entries))
	out := make([]LauncherEntry, 0, len(entries))
	for _, e := range entries {
		t := strings.ToLower(strings.TrimSpace(e.Target))
		if t != "" {
			if seen[t] {
				continue
			}
			seen[t] = true
		}
		out = append(out, e)
	}
	return out
}

// LauncherRebuild 供前端或调试手动重建。
func (s *LauncherService) LauncherRebuild() error { return s.rebuildIndex() }

// PickLauncherDir 弹原生选目录框，供设置页「添加目录」用。取消返空串而不是
// 错误（与 PickKeyFile 同一套约定）；起始目录落在用户主目录。
func (s *LauncherService) PickLauncherDir() (string, error) {
	if s.app == nil {
		return "", errors.New("应用尚未就绪")
	}
	if s.ctl != nil {
		s.ctl.SetDialogGuard(true)
		defer s.ctl.SetDialogGuard(false)
	}
	opts := &application.OpenFileDialogOptions{
		Title:                "选择启动器搜索目录",
		Message:              "选一个应用安装目录，启动器会递归搜索里面的可执行文件",
		CanChooseFiles:       false,
		CanChooseDirectories: true,
	}
	if home, err := os.UserHomeDir(); err == nil {
		opts.Directory = home
	}
	if s.main != nil {
		opts.Window = s.main
	}
	path, err := s.app.Dialog.OpenFileWithOptions(opts).PromptForSingleSelection()
	if err != nil {
		// Windows 取消对话框把 ERROR_CANCELLED 当错误丢回来：那不是故障，就是用户没选。
		if strings.Contains(strings.ToLower(err.Error()), "cancel") {
			return "", nil
		}
		return "", err
	}
	return path, nil
}

// LauncherInfo 返回索引状态，页面用来决定占位文案。
func (s *LauncherService) LauncherInfo() LauncherInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return LauncherInfo{Ready: s.ready, Count: len(s.entries), Indexing: s.indexing}
}

// LauncherSearch 按查询模糊搜索引，返回按分排序的前 20 条。
func (s *LauncherService) LauncherSearch(query string) []LauncherResult {
	s.mu.Lock()
	entries := append([]LauncherEntry(nil), s.entries...)
	s.mu.Unlock()
	return searchEntries(entries, query, launcherMaxResults)
}

/* --------------------------------------------------------------------------- */
/* 启动                                                                         */
/* --------------------------------------------------------------------------- */

// LauncherInvoke 启动一条命中；elevate 为 true 时用 runas 动词（UAC 提权，
// Ctrl+Enter 的路）。path 必须来自索引 —— 这是能从全局热键触发的
// 代码路径，不接受前端随手递来的任意路径，否则搜索框就成了 Run 对话框的旁路。
func (s *LauncherService) LauncherInvoke(path string, elevate bool) error {
	s.mu.Lock()
	ok, isUWP := false, false
	for _, e := range s.entries {
		if strings.EqualFold(e.Path, path) {
			ok, isUWP = true, strings.HasPrefix(e.Path, uwpLauncherPath)
			break
		}
	}
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("启动器不认识这条路径：%s", path)
	}
	if elevate && isUWP {
		// UWP 没法以管理员身份启动（商店应用的权限模型不允许）；shell:AppsFolder
		// 动词也不认 runas，送过去只会拿到一个费解的 shell 错误码，不如直说。
		return fmt.Errorf("UWP 应用无法以管理员身份启动：%s", path)
	}
	// 先收起再启动：目标窗口出现的同一瞬间搜索条已经不在屏幕上，手感才对。
	// 启动本身不等退出（GUI 程序本来就不该等），错误照样回传。
	s.hide()
	// 提权走 launcherShellExecuteElevated：它先直接 runas，对只注册了 open、没注册
	// runas 的文档会 NOASSOC，再由它解析处理程序 exe 回退提权。非提权一律默认动词。
	var err error
	if elevate {
		err = launcherShellExecuteElevated(path)
	} else {
		err = launcherShellExecute(path, "")
	}
	if err != nil {
		if elevate {
			// UAC 弹窗被取消、或目标本就拒绝提权，都走到这儿。
			return fmt.Errorf("提权启动失败（UAC 被取消，或该文件的打开方式不支持提权）：%v", err)
		}
		return err
	}
	log.Printf("启动器拉起：%s%s", verbNote(elevate), path)
	return nil
}

func verbNote(elevate bool) string {
	if elevate {
		return "（管理员）"
	}
	return ""
}

/* --------------------------------------------------------------------------- */
/* 在文件管理器中显示                                                           */
/* --------------------------------------------------------------------------- */

// LauncherReveal 把一条索引项的路径送进主窗口的文件管理器标签：
// preview=false（Shift+Enter）只在左栏定位并选中该文件；preview=true（Alt+Enter）
// 额外在右栏就地预览。主窗口里已有文件管理器标签则复用（App.vue 取最后一个），
// 没有则新建；两种情况都要先收起启动器、把主窗口提到前台。
//
// path 必须来自索引 —— 与 LauncherInvoke 同一道闸：这是能从全局热键触达的窗口，
// 不接受前端随手递来的任意路径。
func (s *LauncherService) LauncherReveal(path string, preview bool) error {
	s.mu.Lock()
	ok := false
	for _, e := range s.entries {
		if strings.EqualFold(e.Path, path) {
			ok = true
			break
		}
	}
	main := s.main
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("启动器不认识这条路径：%s", path)
	}
	if main == nil {
		return errors.New("主窗口不可用，无法显示文件路径")
	}
	// 先收启动器（顺带撤掉它压着的 quake 护栏），再把主窗提到前台并投递定位事件。
	s.hide()
	s.revealToFront(main, path, preview)
	return nil
}

// revealToFront 把主窗口 Show + 抢前台 + 聚焦，然后通知前端去定位。提窗动作与
// AppService.forceShowFront 同源：先取原生 hwnd，Show 后用 forceForeground 绕开
// 前台锁，裸 Focus() 才能真正把键盘焦点送进 webview。事件必须在窗口可见之后再发，
// 因为 FileManagerView 靠 offsetParent 判定「自己是可见的活动栏」才响应。
func (s *LauncherService) revealToFront(main *application.WebviewWindow, path string, preview bool) {
	var hwnd uintptr
	application.InvokeSync(func() {
		if p := main.NativeWindow(); p != nil {
			hwnd = uintptr(p)
		}
	})
	main.Show()
	forceForeground(hwnd)
	main.Focus()
	main.EmitEvent(launcherEventReveal, &LauncherRevealPayload{Path: path, Preview: preview})
}

/* --------------------------------------------------------------------------- */
/* 匹配打分（纯函数，可脱离窗口单测）                                            */
/* --------------------------------------------------------------------------- */

// searchEntries 是 LauncherSearch 的无锁内核。空查询按名称排序给前 N 条
// （首版没有使用频率数据，「全部列出」比「什么都不显示」友好）。
func searchEntries(entries []LauncherEntry, query string, limit int) []LauncherResult {
	q := strings.TrimSpace(query)
	out := make([]LauncherResult, 0, len(entries))
	if q == "" {
		for _, e := range entries {
			out = append(out, LauncherResult{Name: e.Name, Path: e.Path, Target: e.Target, Score: 0})
		}
		sort.SliceStable(out, func(i, j int) bool {
			return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
		})
		if limit > 0 && len(out) > limit {
			out = out[:limit]
		}
		return out
	}
	for _, e := range entries {
		score := fuzzyScore(q, e.Name)
		if base := filepath.Base(e.Target); base != "" {
			// exe 名再算一次：中文系统里「记事本.lnk」也能被 notepad 搜到。
			if s2 := fuzzyScore(q, strings.TrimSuffix(base, ".exe")); s2 > score {
				score = s2
			}
		}
		if e.Alias != "" {
			// UWP 的英文包名：显示名是中文时唯一能命中拉丁查询的面。
			if s3 := fuzzyScore(q, e.Alias); s3 > score {
				score = s3
			}
		}
		// .lnk 文件名本身：TargetPath 解析失败时（脚本报错、目标被移走），
		// 英文名往往还写在这条路径里，不该跟着一起丢。
		if base := strings.TrimSuffix(filepath.Base(e.Path), filepath.Ext(e.Path)); base != "" && !strings.EqualFold(base, e.Name) {
			if s4 := fuzzyScore(q, base); s4 > score {
				score = s4
			}
		}
		// 拼音两面包中文名的拉丁查询：jsb / jishiben 都能命中「记事本」。
		if e.PinyinFull != "" {
			if s5 := fuzzyScore(q, e.PinyinFull); s5 > score {
				score = s5
			}
		}
		if e.PinyinShort != "" {
			if s6 := fuzzyScore(q, e.PinyinShort); s6 > score {
				score = s6
			}
		}
		if score <= 0 {
			continue
		}
		out = append(out, LauncherResult{Name: e.Name, Path: e.Path, Target: e.Target, Score: score})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return len(out[i].Name) < len(out[j].Name)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// fuzzyScore 给单个名字打分：0 = 不匹配。规则刻意做小 ——
// 全等 > 前缀 > 顺序子序列（连续段、词首、首字母各有加分，跨度惩罚）。
// ASCII 大小写不敏感；中文按 rune 直接比（GBK 时代过来的用户会输汉字，
// 但快捷方式名也是汉字，不存在转写问题）。
func fuzzyScore(query, name string) int {
	if query == "" || name == "" {
		return 0
	}
	q := []rune(strings.ToLower(query))
	n := []rune(strings.ToLower(name))
	if string(q) == string(n) {
		return 1000
	}
	if len(n) >= len(q) && string(n[:len(q)]) == string(q) {
		return 700 - (len(n) - len(q)) // 越短越优先
	}
	// 子序列匹配：贪心左到右，全程 rune 下标（中文名不能拿字节偏移算跨度）。
	var qi, first, last, runs, boundaries int
	first, last = -1, -1
	for i := 0; i < len(n) && qi < len(q); i++ {
		if n[i] != q[qi] {
			continue
		}
		if first < 0 {
			first = i
		}
		if last == i-1 {
			runs++
		}
		if i == 0 || isLauncherSep(n[i-1]) {
			boundaries++
		}
		last = i
		qi++
	}
	if qi < len(q) {
		return 0 // 没吃完 query，不算匹配
	}
	score := 300 + runs*20 + boundaries*15
	if first == 0 {
		score += 30
	}
	score -= (last - first + 1 - len(q)) * 3 // 命中越分散越靠后
	if score < 1 {
		score = 1
	}
	return score
}

func isLauncherSep(r rune) bool {
	switch r {
	case ' ', '-', '_', '(', '[', '.', '&':
		return true
	}
	return false
}

// pinyinFaces 为名称生成两个拉丁可搜面：全拼（音节连写）与首字母（各音节首字符）。
//
//	"记事本"        → ("jishiben", "jsb")
//	"微信 WeChat"   → ("weixinwechat", "wx")
//	"Google Chrome" → ("", "") —— 不含汉字就不生成：纯拉丁名在 Name 面上已全覆盖
//
// 汉字取 go-pinyin 最常见读音（多音字不穷举：「行」只给 xing 不给 hang ——
// 全量同音字带来的假命中比漏一个生僻读法更伤手感，首版认一条最常见路）。
// 字母/数字只进全拼面：首字母面是「音节首字符」的缩写，混进整段英文会让它
// 长得跟全拼一样，既抢分数又造出英文名缩写式的假命中（如 wxwechat 被 wdwp
// 一类查询乱搜）；字母留在全拼面已足够覆盖「百度网盘 plus」这种混合名。
// 空格、括号等装饰字符两面都跳过：只稀释子序列打分的跨度。
func pinyinFaces(name string) (full, short string) {
	if !strings.ContainsFunc(name, func(r rune) bool { return unicode.Is(unicode.Han, r) }) {
		return "", ""
	}
	// 零值 Args：无声调、不穷举多音字；非汉字走默认 Fallback（忽略），
	// SinglePinyin 返空切片 —— 正好是这里要的形状。
	pyArgs := pinyin.NewArgs()
	var fb, sb strings.Builder
	for _, r := range name {
		if py := pinyin.SinglePinyin(r, pyArgs); len(py) > 0 && py[0] != "" {
			fb.WriteString(py[0])
			sb.WriteByte(py[0][0])
			continue
		}
		if c := unicode.ToLower(r); unicode.Is(unicode.Latin, c) || unicode.IsDigit(c) {
			fb.WriteRune(c)
		}
	}
	return fb.String(), sb.String()
}

/* --------------------------------------------------------------------------- */
/* UWP 补种（「所有应用」里没有物理 .lnk 的那一批）                        */
/* --------------------------------------------------------------------------- */

// uwpLauncherPath 前缀：ShellExecuteW 认这个动词，由 shell 解析到 appx 注册项，
// 等价于开始菜单里点一下那个磁贴。
const uwpLauncherPath = `shell:AppsFolder\`

// startApp 是 Get-StartApps 输出的一行。AppID 含「!」是 UWP（包名!应用 id），
// 否则是桌面应用的 .lnk 路径 —— 扫描已经收过，这里用不上。
type startApp struct {
	Name  string
	AppID string
}

// parseStartAppsCSV 解析 ConvertTo-Csv 的两列输出（含表头行）。
// 引号状态机 + "" 转义：应用名里带逗号真实存在，拿 split 切会被切碎。
// 字节循环对 UTF-8 安全：多字节序列原样透进列缓冲。
func parseStartAppsCSV(out string) []startApp {
	var apps []startApp
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		cols := splitCSVRow(line)
		if len(cols) != 2 || cols[0] == "Name" || cols[0] == "" {
			continue // 表头、坏行、没有引号包住的散输出
		}
		apps = append(apps, startApp{Name: cols[0], AppID: cols[1]})
	}
	return apps
}

// splitCSVRow 拆一行两列：支持双引号包裹与 "" 转义，引号外的空白丢弃。
func splitCSVRow(line string) []string {
	var cols []string
	var cur strings.Builder
	inQ := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case inQ:
			if c == '"' {
				if i+1 < len(line) && line[i+1] == '"' {
					cur.WriteByte('"')
					i++
				} else {
					inQ = false
				}
			} else {
				cur.WriteByte(c)
			}
		case c == '"':
			inQ = true
		case c == ',':
			cols = append(cols, cur.String())
			cur.Reset()
		default:
			if c != ' ' && c != '\r' && c != '\t' {
				cur.WriteByte(c)
			}
		}
	}
	cols = append(cols, cur.String())
	return cols
}

// mergeUWPApps 把 .lnk 索引里没有的 UWP 应用补进 entries。名称已被快捷方式
// 占用的跳过：同名时 .lnk 那份信息更全（target/工作目录），而且两种入口启的
// 是同一个应用，列两条只会让人怀疑搜索出了 bug。
func mergeUWPApps(entries []LauncherEntry, apps []startApp) []LauncherEntry {
	taken := make(map[string]bool, len(entries))
	for _, e := range entries {
		taken[strings.ToLower(e.Name)] = true
	}
	for _, a := range apps {
		// 「 单击以执行」那种前导空白是 Get-StartApps 的真输出，不剪会在
		// 排序与展示里露出一个缝。
		name := strings.TrimSpace(a.Name)
		if name == "" || !strings.Contains(a.AppID, "!") {
			continue // 桌面应用在 AppID 里给的还是 lnk 路径，扫描已收
		}
		if taken[strings.ToLower(name)] {
			continue
		}
		taken[strings.ToLower(name)] = true
		entries = append(entries, LauncherEntry{Name: name, Path: uwpLauncherPath + a.AppID, Alias: uwpAlias(a.AppID)})
	}
	return entries
}

// uwpAlias 从 UWP AppID 拆出可搜的英文名片段：
//
//	Microsoft.WindowsCalculator_8wekyb3d8bbwe!App → "Windows Calculator"
//	3AD8A913.49937325EFC59_j4r74wbqry4cc!App     → ""（包名是哈希，没有可搜面）
//
// 取最后一段点分名再按驼峰拆词；不含小写的段（发行商哈希、纯大写编号）直接丢。
func uwpAlias(appID string) string {
	pkg := appID
	if i := strings.IndexByte(pkg, '!'); i >= 0 {
		pkg = pkg[:i]
	}
	if i := strings.LastIndexByte(pkg, '_'); i >= 0 {
		pkg = pkg[:i] // 发行商后缀（_8wekyb3d8bbwe 一类）
	}
	if i := strings.LastIndexByte(pkg, '.'); i >= 0 {
		pkg = pkg[i+1:]
	}
	hasLower := strings.IndexFunc(pkg, unicode.IsLower) >= 0
	if !hasLower {
		return ""
	}
	return splitCamelCase(pkg)
}

// splitCamelCase 在大小写边界插空格："MicrosoftPCManager" → "Microsoft PC Manager"。
func splitCamelCase(s string) string {
	r := []rune(s)
	var b strings.Builder
	for i, c := range r {
		if i > 0 && unicode.IsUpper(c) {
			prev := r[i-1]
			if unicode.IsLower(prev) {
				b.WriteByte(' ')
			} else if unicode.IsUpper(prev) && i+1 < len(r) && unicode.IsLower(r[i+1]) {
				b.WriteByte(' ')
			}
		}
		b.WriteRune(c)
	}
	return b.String()
}
