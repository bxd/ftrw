package main

import (
	"errors"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

// 窗口形态标识。两者共用同一个 WebviewWindow（同一个 HWND）。
const (
	ModeWindow = "window"
	ModeQuake  = "quake"
)

// WindowModeState 是广播给前端的窗口形态快照。
type WindowModeState struct {
	Mode    string `json:"mode"`
	Visible bool   `json:"visible"`
}

// AppService 承载与窗口形态相关的原生能力：系统托盘、开机自启动、
// quake/普通窗口切换，以及跨应用的呼出热键。
//
// 为什么 quake 不另开一个窗口：终端会话、ZMODEM 传输、标签页状态都挂在同一个
// webview 上，开第二个窗口就意味着两份进程内状态要同步，代价远大于收益。这里
// 切形态只做两件事——换窗口几何、换置顶策略——会话因此完全无感。
//
// 跨应用热键的引擎存在这里（gkMu / gkEngine），实现拆在
// globalservice.go：那套东西（编译、匹配、钩子线程、动作队列）有自己的一整套约束。
type AppService struct {
	store *Store

	app *application.App
	win *application.WebviewWindow

	trayIconLight []byte
	trayIconDark  []byte

	mu          sync.Mutex
	mode        string
	savedBounds application.Rect // 进入 quake 之前的普通窗口几何，用于还原
	tray        *application.SystemTray
	trayOn      bool // 当前是否已注册托盘，避免重复 Show/Destroy

	gkMu     sync.Mutex
	gkEngine *globalKeyEngine

	launcher *LauncherService // 启动器服务（main.go 注入），全局热键与托盘菜单共用

	quitting    atomic.Bool // 托盘「退出」置位：放行 WindowClosing 钩子
	dialogGuard atomic.Bool // 原生对话框占用焦点期间抑制 quake 收起
}

func NewAppService(store *Store) *AppService {
	return &AppService{store: store, mode: ModeWindow}
}

// Bind 在 main 创建完窗口后注入依赖。
func (s *AppService) Bind(app *application.App, win *application.WebviewWindow) {
	s.app = app
	s.win = win
}

// SetTrayIcons 注入明/暗两套托盘 .ico 字节。
//
// 必须是多尺寸未压缩 .ico 而不是 PNG：Wails 的 CreateSmallHIconFromImage 对 PNG
// 输入是原样透传、不按 SM_CXSMICON 缩放，一张 1024px 的 PNG 喂进去只会得到坏图标；
// 只有 .ico 才会走 selectICOImage 按通知区域实际尺寸挑帧。见 cmd/genicon。
func (s *AppService) SetTrayIcons(light, dark []byte) {
	s.trayIconLight = light
	s.trayIconDark = dark
}

// SetLauncher 注入启动器服务。动作表里 launcher.toggle 的实现要经由 AppService
// 这一扇门走（globalActionTable 的 run 签名是 func(*AppService)），窗口本体全在
// LauncherService 里，这里只做转发。参数类型会把 LauncherService 拖成 bindings
// 里的 model（与服务名撞车），所以整条对生成器隐去。
//
//wails:ignore
func (s *AppService) SetLauncher(l *LauncherService) { s.launcher = l }

// HotkeyToggleLauncher 是全局热键/托盘菜单的呼出入口（跑在热键动作 goroutine 上）。
func (s *AppService) HotkeyToggleLauncher() {
	if s.launcher == nil {
		log.Printf("启动器服务未接线，呼出被忽略")
		return
	}
	s.launcher.Toggle()
}

// prefs 取当前配置快照。Store 的锁只在取快照的这几行内持有，绝不在调用原生
// API（可能阻塞在 InvokeSync 上）时持有。
func (s *AppService) prefs() Preferences {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	return s.store.Prefs
}

/* --------------------------------------------------------------------------- */
/* 启动接线                                                                     */
/* --------------------------------------------------------------------------- */

// 启动接线只有一条：跑起来之后再装跨应用热键。
//
// 早先在这里调 Wails 的 GlobalShortcut.Register（底层 RegisterHotKey），必须赶在
// app.Run() 之前排队。现在换成自己装 WH_KEYBOARD_LL 钩子：钩子要一个专用线程去泵消息，
// 而动作又要能安全地往主线程 Invoke，所以统一放到 ApplicationStarted 之后（见
// globalservice.go 的 startGlobalKeys）。两条路径同时留着会让 Alt+` 双触发。

// Start 在应用真正跑起来之后装配窗口钩子，由 ApplicationStarted 事件触发。
//
// 托盘不在这里建：SystemTray.New() 在应用已运行时会同步 Run() 并把图标 NIM_ADD 进
// 通知区域，而那一刻 SetIcon 还没调，Wails 会回退到内置的 "W" 图标。见 SetupTray。
func (s *AppService) Start() {
	s.installCloseHook()
	s.installBlurHook()
	s.installQuakeOnTop()
	s.startGlobalKeys()
	s.publish()
}

// SetupTray 建托盘，必须在 app.Run() 之前调用（main.go）。
// 运行期由偏好开关再次调用 applyTray，那时 New() 会先闪一下回退图标再由
// SetIcon 的 NIM_MODIFY 覆盖，属于可接受的代价（只在用户改动这个开关时发生）。
func (s *AppService) SetupTray() {
	s.applyTray()
}

// installCloseHook 实现「✕ 收起进托盘而不退出」。
//
// 用 RegisterHook 而不是 OnWindowEvent：Wails 内部的 WindowClosing 监听器会直接
// CAS 置 closeStarted 并关闭窗口，监听器层面无法拦；而钩子跑在监听器之前，
// Cancel() 会短路整条监听链（见 WebviewWindow.HandleWindowEvent）。
func (s *AppService) installCloseHook() {
	s.win.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		if s.quitting.Load() {
			return // 放行：托盘菜单里的「退出」
		}
		if !s.prefs().CloseToTray || !s.trayOnNow() {
			return // 没开这个选项、或者根本没有托盘，正常退出
		}
		e.Cancel()
		// 钩子跑在事件循环线程上，Hide() 内部是 InvokeSync（要等主线程），
		// 而本钩子正是主线程处理 WM_CLOSE 时投递过来的，放 goroutine 里避免嵌套等待。
		go func() {
			s.win.Hide()
			s.publish()
		}()
	})
}

// installBlurHook 实现 quake 失焦自动收起（Guake/ yakuake 的默认手感）。
func (s *AppService) installBlurHook() {
	s.win.OnWindowEvent(events.Common.WindowLostFocus, func(*application.WindowEvent) {
		if s.Mode() != ModeQuake {
			return
		}
		if !s.prefs().QuakeHideOnBlur {
			return
		}
		// 原生文件对话框/确认框是独立 HWND，弹出时必然抢走主窗口焦点；不加这道
		// 护栏，上传文件选完路径窗口就直接消失了。
		if s.dialogGuard.Load() {
			return
		}
		s.win.Hide()
		s.publish()
	})
}

// installQuakeOnTop 给 quake 模式再加一道保险：窗口每次获得焦点都重申置顶，
// 覆盖「被其它置顶应用短暂压住后点回来」等场景，让下拉始终浮在最上层。
func (s *AppService) installQuakeOnTop() {
	s.win.OnWindowEvent(events.Common.WindowFocus, func(*application.WindowEvent) {
		if s.Mode() == ModeQuake {
			s.win.SetAlwaysOnTop(true)
		}
	})
}

/* --------------------------------------------------------------------------- */
/* 托盘                                                                         */
/* --------------------------------------------------------------------------- */

// applyTray 按配置创建或销毁托盘图标。可重复调用（保存偏好后前端会再调一次）。
func (s *AppService) applyTray() {
	want := s.prefs().TrayEnabled
	if !want {
		s.destroyTray()
		return
	}
	if s.tray != nil {
		// 已存在：只刷新图标/菜单，不重建——Windows 重建 NOTIFYICONDATA 会闪。
		s.refreshTray()
		return
	}
	tray := s.app.SystemTray.New()
	tray.SetIcon(s.trayIconLight)
	if len(s.trayIconDark) > 0 {
		tray.SetDarkModeIcon(s.trayIconDark)
	}
	tray.SetTooltip("FTRW - BBS 终端")
	tray.SetMenu(s.buildTrayMenu())
	// 左键：quake 模式走「呼出/收起」，普通模式走「显示/隐藏」。右键菜单由
	// Windows 托盘实现在设了 SetMenu 后自动处理，不需要 OnRightClick。
	tray.OnClick(s.TrayClick)
	tray.Show()
	s.mu.Lock()
	s.tray = tray
	s.trayOn = true
	s.mu.Unlock()
}

func (s *AppService) refreshTray() {
	s.mu.Lock()
	tray := s.tray
	s.mu.Unlock()
	if tray == nil {
		return
	}
	tray.SetIcon(s.trayIconLight)
	if len(s.trayIconDark) > 0 {
		tray.SetDarkModeIcon(s.trayIconDark)
	}
	tray.SetMenu(s.buildTrayMenu())
}

func (s *AppService) destroyTray() {
	s.mu.Lock()
	tray := s.tray
	s.tray = nil
	s.trayOn = false
	s.mu.Unlock()
	if tray != nil {
		tray.Destroy()
	}
}

func (s *AppService) trayOnNow() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.trayOn
}

func (s *AppService) buildTrayMenu() *application.Menu {
	menu := application.NewMenu()
	menu.Add("显示 / 隐藏 FTRW").OnClick(func(*application.Context) { s.TrayClick() })
	menu.Add("启动器").OnClick(func(*application.Context) { s.HotkeyToggleLauncher() })
	menu.AddSeparator()
	menu.Add("切换 Quake 下拉模式").OnClick(func(*application.Context) { s.ToggleWindowMode() })
	menu.AddSeparator()
	menu.Add("打开日志目录").OnClick(func(*application.Context) { s.OpenLogDir() })
	menu.AddSeparator()
	menu.Add("退出 FTRW").OnClick(func(*application.Context) { s.Quit() })
	return menu
}

// OpenLogDir 在文件管理器里打开日志目录。GUI 子系统构建下没有控制台，这是用户
// 能直接拿到 ftrw.log 的唯一入口。
func (s *AppService) OpenLogDir() {
	dir := logDir()
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("explorer", dir)
	case "darwin":
		cmd = exec.Command("open", dir)
	default:
		cmd = exec.Command("xdg-open", dir)
	}
	// 目录不存在时先建一个，免得用户点了半天没反应。
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("建日志目录 %s: %v", dir, err)
	}
	// explorer 即使成功也会以非 0 退出，所以只 Start、不等它结束。
	if err := cmd.Start(); err != nil {
		log.Printf("打开日志目录 %s: %v", dir, err)
		return
	}
	go func() { _ = cmd.Wait() }()
	log.Printf("打开日志目录: %s", dir)
}

// TrayClick 是托盘左键行为。
//
// 不用 SystemTray.AttachWindow/ToggleWindow：那套内置切换会把窗口 Show 到它自己
// 记录的偏移位置，和 quake 的「按工作区百分比重新算几何」互相打架。
func (s *AppService) TrayClick() {
	if s.win.IsVisible() {
		s.win.Hide()
	} else {
		s.win.Show()
		s.raiseQuake()
		s.forceShowFront()
		s.win.Focus()
	}
	s.publish()
}

// Quit 走正常退出：先置 quitting，让 WindowClosing 钩子放行内部的关闭监听器。
// 钩子要在真正退出之前摘掉：否则关窗过程中用户再按一下热键，会把一个正在拆的窗口又拉出来。
func (s *AppService) Quit() {
	s.quitting.Store(true)
	s.stopGlobalKeys()
	s.app.Quit()
}

/* --------------------------------------------------------------------------- */
/* 模式切换                                                                     */
/* --------------------------------------------------------------------------- */

func (s *AppService) Mode() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mode
}

// GetWindowMode 供前端初始化时拉一次状态。
func (s *AppService) GetWindowMode() WindowModeState {
	return WindowModeState{Mode: s.Mode(), Visible: s.win.IsVisible()}
}

// ToggleWindowMode 在 quake 与普通窗口之间互切，对应出厂绑定 M-x M-q。
// 无论切到哪一侧都保证窗口最终可见并聚焦——不然「切换」看起来就像窗口丢了。
func (s *AppService) ToggleWindowMode() WindowModeState {
	target := ModeWindow
	if s.Mode() == ModeWindow {
		target = ModeQuake
	}
	s.SetMode(target)
	if !s.win.IsVisible() {
		s.win.Show()
	}
	s.raiseQuake()
	s.forceShowFront()
	s.win.Focus()
	s.publish()
	return s.GetWindowMode()
}

// HotkeyToggleQuake 是全局热键回调（跑在独立 goroutine）：已显示的 quake 收起，
// 其余情况一律切到 quake 并呼出。
func (s *AppService) HotkeyToggleQuake() {
	if s.Mode() == ModeQuake && s.win.IsVisible() {
		s.win.Hide()
		s.publish()
		return
	}
	s.SetMode(ModeQuake)
	s.win.Show()
	s.raiseQuake()
	s.forceShowFront()
	s.win.Focus()
	s.publish()
}

// SetMode 落地形态切换：只动几何与置顶，会话状态一概不碰。
func (s *AppService) SetMode(target string) {
	if target != ModeWindow && target != ModeQuake {
		return
	}

	s.mu.Lock()
	if target == s.mode {
		s.mu.Unlock()
		return
	}
	if target == ModeQuake {
		// 先取消最大化再记几何：Windows 上最大化状态的 GetWindowRect 拿到的是
		// 整个工作区，直接存下来会让「回到普通窗口」变成满屏。
		maximised := s.win.IsMaximised()
		s.mu.Unlock()
		if maximised {
			s.win.UnMaximise()
		}
		s.mu.Lock()
		s.savedBounds = s.win.Bounds()
	}
	s.mode = target
	saved := s.savedBounds
	s.mu.Unlock()

	if target == ModeQuake {
		qb, err := s.quakeBounds()
		if err != nil {
			log.Printf("计算 quake 几何失败，回退到普通窗口模式: %v", err)
			s.mu.Lock()
			s.mode = ModeWindow
			s.mu.Unlock()
			s.publish()
			return
		}
		s.win.SetBounds(qb)
		// quake 必须置顶，否则「呼出」出来的下拉会被当前活动应用盖住。
		s.win.SetAlwaysOnTop(true)
		s.publish()
		return
	}

	s.win.SetAlwaysOnTop(false)
	if saved.Width > 0 && saved.Height > 0 {
		s.win.SetBounds(saved)
	}
	s.publish()
}

// raiseQuake 在 quake 模式下把窗口重新钉到最上层。
// WS_EX_TOPMOST 只在 SetMode「切换到 quake」那一次设置；「失焦收起 → 再呼出」时模式
// 没变、SetMode 提前 return 不重申，就可能被收起期间冒出来的其它置顶窗口压住。各呼出
// 路径统一补一次，保证 quake 下拉永远浮在最上层。
func (s *AppService) raiseQuake() {
	if s.Mode() != ModeQuake {
		return
	}
	s.win.SetAlwaysOnTop(true)
}

// forceShowFront 把主窗口强行提到 OS 前台，抢在 Wails 的 Focus() 之前破掉「前台锁」。
// 全局热键/托盘从别的 app 手里呼出 quake 时，单靠 win.Focus() 内部的 SetForegroundWindow
// 会被焦点窃取防护拦下（窗口显示但键盘进不来）；forceForeground 用 AttachThreadInput
// 绕开后，紧随的 win.Focus() 才能真正把焦点送进 webview，前端再 term.focus() 落到终端。
func (s *AppService) forceShowFront() {
	var hwnd uintptr
	application.InvokeSync(func() {
		if p := s.win.NativeWindow(); p != nil {
			hwnd = uintptr(p)
		}
	})
	forceForeground(hwnd)
}

// RestoreMainFocus 在预览这类独立弹窗关闭后把前台收回主窗口：Wails 的模态清理
// 只做 EnableWindow(main, true)，不还前台——弹窗销毁那一刻系统按 z 序把前台交给
// 别的应用，主窗口就这么丢了焦点。不可见时不抢：焦点还回来不该顺手把窗口呼出。
func (s *AppService) RestoreMainFocus() {
	if s.win == nil || !s.win.IsVisible() {
		return
	}
	s.raiseQuake()
	s.forceShowFront()
	s.win.Focus()
}

// quakeBounds 按当前屏幕工作区与配置百分比算下拉几何。
// 用 WorkArea（逻辑像素）而不是 Bounds：后者含任务栏，会把窗口挤到任务栏底下。
// SetBounds 也工作在 DIP，和 WorkArea 同一坐标系。
//
// 宽/高都是百分比：换显示器、改系统缩放比不需要重配。不足全幅时水平居中，
// 固定靠一边会让下拉在超宽屏上歪到角落，不像从顶上展开一条横幅。
func (s *AppService) quakeBounds() (application.Rect, error) {
	screen, err := s.win.GetScreen()
	if err != nil {
		return application.Rect{}, err
	}
	if screen == nil {
		return application.Rect{}, errors.New("窗口所在屏幕不可用")
	}
	wa := screen.WorkArea
	p := s.prefs()
	def := defaultPreferences()
	w := wa.Width * quakePct(p.QuakeWidthPct, def.QuakeWidthPct) / 100
	h := wa.Height * quakePct(p.QuakeHeightPct, def.QuakeHeightPct) / 100
	// 留两条下限：高度至少标题栏 + 一到两行终端，宽度至少四十列，再小就没法用了。
	const floorH, floorW = 80, 320
	if h < floorH {
		h = floorH
	}
	if w < floorW {
		w = floorW
	}
	x := wa.X
	if w < wa.Width {
		x += (wa.Width - w) / 2
	}
	return application.Rect{X: x, Y: wa.Y, Width: w, Height: h}, nil
}

// quakePct 钳百分比：非法值（旧 prefs.json 里根本不存在的字段被解码成 0、负数、超 100）
// 一律回到出厂值，否则下拉会被压成看不见的 0 尺寸条。
func quakePct(v, def int) int {
	if v < 20 || v > 100 {
		return def
	}
	return v
}

/* --------------------------------------------------------------------------- */
/* 开机自启动                                                                   */
/* --------------------------------------------------------------------------- */

// 自启动的唯一事实来源是注册表（HKCU\...\Run），不再往 Preferences 里存一份镜像：
// 用户在「启动」里手工删掉注册表后，如果应用还按 prefs.json 显示「已开启」，
// 下次保存就会把它偷偷写回来。

// GetAutostart 报告是否已注册随系统启动。
func (s *AppService) GetAutostart() bool {
	on, err := s.app.Autostart.IsEnabled()
	if err != nil {
		log.Printf("读取自启动状态失败: %v", err)
		return false
	}
	return on
}

// SetAutostart 开/关随系统启动。注册在下次登录生效，不影响当前会话。
func (s *AppService) SetAutostart(enabled bool) error {
	if enabled {
		return s.app.Autostart.Enable()
	}
	return s.app.Autostart.Disable()
}

/* --------------------------------------------------------------------------- */
/* 前端辅助                                                                     */
/* --------------------------------------------------------------------------- */

// SetDialogGuard 由前端在打开原生对话框前置 true、对话框关闭后置 false，
// 防止 quake 模式被自己的对话框抢焦点后误收起。
func (s *AppService) SetDialogGuard(on bool) {
	s.dialogGuard.Store(on)
}

// PickKeyFile 弹原生选文件框挑 SSH 私钥，起始目录给 ~/.ssh（多数人的钥匙都在那儿）。
// 取消返空串而不是错误；guard 在后端自己兜住，免得前端前后各调一次还忘了后一半。
func (s *AppService) PickKeyFile() (string, error) {
	if s.app == nil {
		return "", errors.New("应用尚未就绪")
	}
	s.dialogGuard.Store(true)
	defer s.dialogGuard.Store(false)

	opts := &application.OpenFileDialogOptions{
		Title:                   "选择 SSH 私钥",
		Message:                 "选择私钥文件（id_ed25519 / id_rsa / *.pem；PuTTY 的 .ppk 不认）",
		CanChooseFiles:          true,
		CanChooseDirectories:    false,
		AllowsMultipleSelection: false,
		AllowsOtherFileTypes:    true,
		ShowHiddenFiles:         true,
	}
	if home, err := os.UserHomeDir(); err == nil {
		opts.Directory = home
		if fi, err := os.Stat(filepath.Join(home, ".ssh")); err == nil && fi.IsDir() {
			opts.Directory = filepath.Join(home, ".ssh")
		}
	}
	if s.win != nil {
		opts.Window = s.win
	}
	path, err := s.app.Dialog.OpenFileWithOptions(opts).PromptForSingleSelection()
	if err != nil {
		// Windows 取消对话框会把 ERROR_CANCELLED 当错误丢回来：那不是故障，就是用户没选。
		if strings.Contains(strings.ToLower(err.Error()), "cancel") {
			return "", nil
		}
		return "", err
	}
	return path, nil
}

// ApplyPreferences 在偏好保存后重放与原生侧相关的设置（目前是托盘显隐）。
// 模式几何不在此处重算：quake 宽高只在下次进入 quake 时生效，避免正在用的下拉
// 因为改了个数字就突然跳高跳宽。
func (s *AppService) ApplyPreferences() {
	s.applyTray()
	s.publish()
}

func (s *AppService) publish() {
	if s.app == nil {
		return
	}
	s.app.Event.Emit("app:mode", &WindowModeState{
		Mode:    s.Mode(),
		Visible: s.win.IsVisible(),
	})
}
