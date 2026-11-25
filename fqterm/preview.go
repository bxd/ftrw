package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

// 超链接预览窗口。终端里点一条链接 → 这一个窗口负责把它呈现出来。
//
// 为什么不是一个嵌在界面里的浮层：需求要能显示水木那种发 X-Frame-Options /
// CSP frame-ancestors 拒绝被嵌套的网页，iframe 里只会是一片空白 → 只能做顶层导航，
// 也就是把整个 webview 指向那个 URL。于是同一个窗口有两种模式：
//
//	media —— 加载我们自己的 ?view=preview 页面，用 img / audio / video 放媒体，
//	          带「上一个 / 下一个」循环浏览（一次采集当前屏幕上的全部媒体链接）
//	page  —— SetURL 到外部网页，窗口整个交给对方的文档，绕开 frame 限制
//
// 两种模式共用的只有窗口本身：切回 media 时重新加载我们那一页，内容靠 pending
// 交付（见 TakePreviewRequest）。

// 链接类型。前端按扩展名判定后送进来，后端只用它分流，不再重复一套判断。
const (
	KindImage = "image"
	KindAudio = "audio"
	KindVideo = "video"
	KindPage  = "page"
	KindText  = "text"
	KindPDF   = "pdf"
	KindFile  = "file"
)

// 预览窗口的两种模式。
const (
	PreviewMedia = "media"
	PreviewPage  = "page"
)

const (
	// previewPageURL 是窗口里「我们自己那一页」的地址。写相对地址，由 Wails 补成
	// 资源服务器 origin（http://wails.localhost），换平台、换端口都不用改。
	previewPageURL = "/?view=preview"

	// previewWindowName 给窗口一个稳定的名字：Wails 按名字投递事件，
	// 也让挂 owner 之类的调试有得认。
	previewWindowName = "preview"

	// previewEventLoad 是「窗口已经开着，换内容」的通知。新建窗口时不发：
	// 那时文档还没挂上监听者，靠 pending 取。
	previewEventLoad = "preview:load"
)

// 预览窗弹出/收起的动画参数。滑动距离按物理像素算，随停泊位一起换算成 DIP；
// 节奏与主窗前端 .16s ease-out 一族相近，两个窗口的动效看起来是一套。
const (
	previewSlideInMS  = 220
	previewSlideOutMS = 170
	previewGlideMS    = 180
	previewSlideDist  = 64
	previewAnimTick   = 16 * time.Millisecond
)

// 预览窗关闭流程的三态。hook 拦下第一次关闭请求播淡出；淡出期间再来关闭请求
// （连按 ESC / Alt+F4）跳过动画立即关；pvClosing 是淡出播完后的正式关闭，hook 放行。
const (
	pvIdle      = 0
	pvFadingOut = 1
	pvClosing   = 2
)

// PreviewItem 一条预览内容。Name 是给人看的短名，Kind 见上面的常量。
// Path 仅本地文件有：前端用它调用系统默认程序打开，而不是把签名 URL 交给浏览器。
type PreviewItem struct {
	URL  string `json:"url"`
	Kind string `json:"kind"`
	Name string `json:"name"`
	Path string `json:"path,omitempty"`
}

// PreviewRequest 一次打开请求：整组媒体 + 当前序号。
//
// 为什么要带上整组：BBS 帖子里常常一排图，点开一张之后还得上/下一个翻着看，
// 而只有前端知道这一屏里都有些什么链接。
type PreviewRequest struct {
	Items []PreviewItem `json:"items"`
	Index int           `json:"index"`
}

// dialogGuarder 是 AppService 的一小面：预览窗口是独立 HWND，弹出必然让主窗口失焦，
// quake 模式「失焦即收起」不加这道护栏就会把自己的弹窗当成「用户切走了」。
// Mode 用来判断主窗口当下是不是 quake 下拉形态，决定预览窗要不要同步置顶。
type dialogGuarder interface {
	SetDialogGuard(on bool)
	Mode() string
	RestoreMainFocus()
}

type PreviewService struct {
	app  *application.App
	main *application.WebviewWindow
	ctl  dialogGuarder

	mu      sync.Mutex
	win     *application.WebviewWindow
	alive   bool
	hwnd    uintptr
	mode    string
	pending *PreviewRequest

	pvState   int              // pvIdle / pvFadingOut / pvClosing
	anim      *previewAnim     // 正在跑的窗口动画（淡入 / 淡出 / 停泊滑移）
	dock      application.Rect // 停泊位（DIP）：window 模式贴主窗底缘，quake 贴工作区顶缘
	dockStart application.Rect // 弹出起始位（DIP）：停泊位向主窗一侧偏一个滑动距离
}

func NewPreviewService() *PreviewService {
	return &PreviewService{}
}

// Bind 在 main 建好主窗口之后注入依赖。ctl 允许传 nil（测试里不需要护栏）。
//
//wails:ignore
func (s *PreviewService) Bind(app *application.App, main *application.WebviewWindow, ctl dialogGuarder) {
	s.app = app
	s.main = main
	s.ctl = ctl
}

/* --------------------------------------------------------------------------- */
/* 打开 / 关闭                                                                  */
/* --------------------------------------------------------------------------- */

// OpenPreview 是前端点到超链接的唯一入口。
//
// items 是按出现顺序采集的媒体链接（网页链接只有一条），index 指向被点的那一条。
func (s *PreviewService) OpenPreview(items []PreviewItem, index int) error {
	if len(items) == 0 {
		return errors.New("没有可预览的内容")
	}
	// 撞上正在淡出的预览：等它关完再走复用/新建的正常判断。
	s.waitFadeOut()
	index = clampIndex(index, len(items))
	item := items[index]
	var u *url.URL
	var err error
	if item.Kind == KindPage {
		// 网页模式会顶层导航，永远只接受 http/https；本地签名地址不能走这条路。
		u, err = checkPreviewURL(item.URL)
	} else {
		u, err = checkPreviewItemURL(item.URL)
	}
	if err != nil {
		log.Printf("预览链接被拒绝：%s（%v）", item.URL, err)
		return err
	}
	if item.Kind == KindPage {
		return s.openPage(u.String(), item)
	}
	return s.openMedia(&PreviewRequest{Items: items, Index: index}, item)
}

func (s *PreviewService) openMedia(req *PreviewRequest, item PreviewItem) error {
	s.mu.Lock()
	s.pending = req
	win, alive, mode := s.win, s.alive, s.mode
	s.mu.Unlock()

	title := previewTitle(item.Kind, item.Name, item.URL)
	// 上面「已打开」那行只记标题，而 BBS 的图标题就是 att.php / 一串附件号，看不出地址；
	// 类型判错、加载失败都只能靠这条完整 URL 复盘。
	log.Printf("预览媒体：[%s] %s", item.Kind, item.URL)
	switch {
	case !alive || win == nil:
		// 新窗口：页面挂载后会自己来取 pending，这里抢着发事件一定丢。
		return s.newWindow(previewPageURL, title, PreviewMedia)
	case mode != PreviewMedia:
		// 从网页模式切回来：整窗重新加载我们那一页，内容仍走 pending。
		win.SetTitle(title)
		win.SetURL(previewPageURL)
		s.setMode(PreviewMedia)
	default:
		win.SetTitle(title)
		win.EmitEvent(previewEventLoad, req)
	}
	s.frontPreview(win)
	s.redock(win)
	return nil
}

func (s *PreviewService) openPage(rawURL string, item PreviewItem) error {
	title := previewTitle(KindPage, item.Name, rawURL)
	s.mu.Lock()
	// 网页模式下页面里没有我们的脚本，留着 pending 只会在下次挂载时塞错内容。
	s.pending = nil
	win, alive := s.win, s.alive
	s.mu.Unlock()

	if !alive || win == nil {
		return s.newWindow(rawURL, title, PreviewPage)
	}
	// 顶层导航：对方文档拿到整个 webview，X-Frame-Options 到此为止不再相关。
	win.SetURL(rawURL)
	win.SetTitle(title)
	s.setMode(PreviewPage)
	s.frontPreview(win)
	s.redock(win)
	log.Printf("预览窗口导航到网页：%s", rawURL)
	return nil
}

// newWindow 建预览窗口并挂好关闭钩子。startURL 可以是站内相对地址，也可以是外部网页地址。
func (s *PreviewService) newWindow(startURL, title, mode string) error {
	if s.app == nil || s.main == nil {
		return errors.New("预览服务还没接上主窗口（应用尚未启动）")
	}
	// 护栏必须赶在 frontPreview 的 Show/Focus 之前：抢焦点会同步
	// 触发主窗失焦钩子，quake 不加护栏会把下拉当「用户切走了」吞掉。
	if s.ctl != nil {
		s.ctl.SetDialogGuard(true)
	}
	dock, start := s.dockGeometry()
	// 最小尺寸跟着停泊位走：主窗本身很小时预览要能装进去，
	// 不让 Wails 的最小尺寸钳制把窗口又撑回主窗外面。
	minW, minH := 360, 260
	if dock.Width < minW {
		minW = dock.Width
	}
	if dock.Height < minH {
		minH = dock.Height
	}
	win := s.app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:            previewWindowName,
		Title:           title,
		Width:           dock.Width,
		Height:          dock.Height,
		MinWidth:        minW,
		MinHeight:       minH,
		InitialPosition: application.WindowXY,
		X:               start.X,
		Y:               start.Y,
		URL:             startURL,
		// 底色与预览页 CSS 根色 #1b1d22 一致（也与主窗口同源），开窗瞬间不闪色差。
		BackgroundColour: application.NewRGB(27, 29, 34),
		// Hidden 建窗：位置与透明度都由弹出动画接管，Wails 默认的「建窗即居中显示」
		// 没机会闪出来。
		Hidden: true,
		// 无边框：系统那条白标题栏跟整窗的深色完全不搭。媒体模式下预览页自己的工具条
		// 就是标题栏（能拖、能关）。代价在网页模式 —— 那时窗口整个交给对方文档，我们
		// 画不出任何东西，退路只剩 ESC（低级键盘钩子接住）与 Alt+F4；frameless 仍保留
		// WS_THICKFRAME/WS_SYSMENU，所以拖边框缩放和 Win+方向键贴边照旧能用。
		Frameless: true,
		Windows: application.WindowsWindow{
			HiddenOnTaskbar: true,
			// Wails 对 frameless 窗每次 WM_ACTIVATE 都重拉 DWM 边框，预览关闭、
			// 主窗被激活时会闪一下；关掉装饰，Win11 圆角用 setWindowRoundCorners 一次性补。
			DisableFramelessWindowDecorations: true,
		},
	})
	if win == nil {
		return errors.New("创建预览窗口失败")
	}

	// HWND 要在主线程上取（impl 里的字段是那边写的）。取不到不影响功能，
	// 只是 ESC 只能由页面自己的 keydown 兜着（网页模式就没有了），动画也退化为直显。
	var hwnd uintptr
	application.InvokeSync(func() {
		if p := win.NativeWindow(); p != nil {
			hwnd = uintptr(p)
		}
		// 全透明起步：frontPreview 的 SW_SHOW 露出的是隐形窗，fadeIn 从这里接手。
		previewSetLayered(hwnd, true)
		previewSetAlpha(hwnd, 0)
		// 只挂 owner 关系，不走 AttachModal：后者会 EnableWindow(main,false)，
		// 关窗时再启用 + 销毁子窗 + 系统重新激活主窗，主窗在关闭那一刻
		// 多一轮禁用/启用与激活切换，正是残闪的根源。
		if p := s.main.NativeWindow(); p != nil {
			previewSetOwner(hwnd, uintptr(p))
		}
	})

	// quake 下主窗是 WS_EX_TOPMOST：置顶要赶在 frontPreview 的 SW_SHOW/
	// SetForegroundWindow 之前，预览从露面那一刻就压在主窗上一层，
	// 而不是靠主窗被收起才显得在上面。
	if s.ctl != nil && s.ctl.Mode() == ModeQuake {
		win.SetAlwaysOnTop(true)
	}

	// 关闭拦截：第一次关闭请求播淡出，淡出中再请求立即关，淡出完毕的正式关闭放行。
	// 状态机见 pvIdle/pvFadingOut/pvClosing 注释。
	win.RegisterHook(events.Common.WindowClosing, func(ev *application.WindowEvent) {
		s.mu.Lock()
		if s.win != win || !s.alive {
			s.mu.Unlock()
			return // 窗口已被替换/已死：放行
		}
		switch s.pvState {
		case pvFadingOut:
			s.pvState = pvClosing // 连按 ESC/Alt+F4：跳过动画立即关
			s.mu.Unlock()
			return
		case pvClosing:
			s.mu.Unlock()
			return // 淡出播完的正式关闭：放行
		}
		s.pvState = pvFadingOut
		s.mu.Unlock()
		ev.Cancel()
		go s.fadeOut(win)
	})
	// 关闭回调里一个窗口 API 都不能碰：它跑在主线程上，而 Close/Hide 内部是
	// InvokeSync（要等主线程），在这儿等下去就是自锁。见 installCloseHook 的同类处理。
	win.OnWindowEvent(events.Common.WindowClosing, func(*application.WindowEvent) {
		s.onWindowClosed(win, hwnd)
	})
	// 应用内 ESC 绑定（走宿主窗口的按键分发，能命中就用上）+ 键盘钩子那条更硬的路。
	win.RegisterKeyBinding("escape", func(application.Window) { s.ClosePreview() })

	s.mu.Lock()
	s.win = win
	s.alive = true
	s.hwnd = hwnd
	s.mode = mode
	s.pvState = pvIdle
	s.dock, s.dockStart = dock, start
	s.mu.Unlock()

	// owner 关系已在取 HWND 时挂好（只设 GWLP_HWNDPARENT、不禁用主窗），
	// 预览始终压在主窗之上、随主窗最小化；关闭时 commitClose 主动交还前台，
	// 销毁那一刻系统不再「补」owner 激活。
	// 护栏已在建窗前设好，这里 frontPreview 抢焦点不会触发 quake 误收起。
	s.frontPreview(win)
	if hwnd != 0 {
		armEscWatch(hwnd, func() { s.ClosePreview() })
	} else {
		log.Printf("预览窗口未取得 HWND，ESC 只能靠页面内按键处理（网页模式下会失灵）")
	}
	go s.fadeIn(win, hwnd)
	log.Printf("预览窗口已打开（%s 模式，%dx%d @%d,%d，hwnd=%#x）：%s", mode, dock.Width, dock.Height, dock.X, dock.Y, hwnd, title)
	return nil
}

// frontPreview 把预览窗提到前面并聚焦。quake 下主窗是 WS_EX_TOPMOST，非 topmost
// 的预览窗哪怕有 owner 关系也永远压不过它，故同步置顶；复用路径也要先设护栏：
// 主窗（quake）持有焦点时，Show/Focus 抢焦点同样会触发失焦收起钩子。
func (s *PreviewService) frontPreview(win *application.WebviewWindow) {
	if s.ctl != nil {
		s.ctl.SetDialogGuard(true)
		if s.ctl.Mode() == ModeQuake {
			win.SetAlwaysOnTop(true)
		}
	}
	win.Show()
	win.Focus()
}

// ClosePreview 关掉预览窗口。✕、ESC、页面按钮都汇到这里，可重复调用。
func (s *PreviewService) ClosePreview() {
	s.mu.Lock()
	win, alive, hwnd := s.win, s.alive, s.hwnd
	s.mu.Unlock()
	if !alive || win == nil {
		return
	}
	if hwnd != 0 {
		disarmEscWatch(hwnd)
	}
	win.Close() // 收尾在 WindowClosing 里做
}

// onWindowClosed 清状态。带着窗口指针来比对，避免把「已经被新窗口取代」的
// 那份状态错清掉（关旧开新在同一瞬间可能发生）。
func (s *PreviewService) onWindowClosed(win *application.WebviewWindow, hwnd uintptr) {
	s.mu.Lock()
	if s.win != win {
		s.mu.Unlock()
		return
	}
	s.win = nil
	s.alive = false
	s.hwnd = 0
	s.mode = ""
	s.pending = nil
	s.pvState = pvIdle
	s.anim = nil
	s.dock, s.dockStart = application.Rect{}, application.Rect{}
	s.mu.Unlock()

	if hwnd != 0 {
		disarmEscWatch(hwnd)
	}
	if s.ctl != nil {
		s.ctl.SetDialogGuard(false)
		// 焦点还给主窗兜底：正常路径 commitClose 已在关前交还前台，
		// 这里补没走动画的路（如未取得 HWND）。本回调在 WindowClosing 里，
		// 窗口 API 又全是 InvokeSync，故起 goroutine 略等一下，免得和
		// 销毁流程抢焦点（同 installCloseHook 的线程考量）。
		go func() {
			time.Sleep(120 * time.Millisecond)
			s.ctl.RestoreMainFocus()
		}()
	}
	log.Printf("预览窗口已关闭")
}

// TakePreviewRequest 给刚挂载的预览页取走该显示的内容；取走即清，免得下次重新
// 挂载时把旧内容再放一遍。没有待显示内容时返回 nil（前端序列化成 null）。
func (s *PreviewService) TakePreviewRequest() *PreviewRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	req := s.pending
	s.pending = nil
	return req
}

// OpenExternal 交给系统默认浏览器。图片撞上防盗链、或网页在预览窗口里就是显示不出来时，
// 这是留给用户的出口。
func (s *PreviewService) OpenExternal(rawURL string) error {
	u, err := checkPreviewURL(rawURL)
	if err != nil {
		return err
	}
	if s.app == nil {
		return errors.New("应用尚未启动")
	}
	return s.app.Browser.OpenURL(u.String())
}

// GetPreviewState 供前端与测试确认窗口现状。
func (s *PreviewService) GetPreviewState() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.alive {
		return ""
	}
	return s.mode
}

// ToggleMaximise 最大化/还原预览窗口，返回切换后的状态给前端画图标。不能只靠前端
// 自己记：Win+方向键贴边、双击边框都能改状态，所以每次都以 IsMaximised 为准。
func (s *PreviewService) ToggleMaximise() (bool, error) {
	s.mu.Lock()
	win := s.win
	s.mu.Unlock()
	if win == nil {
		return false, errors.New("预览窗口未打开")
	}
	win.ToggleMaximise() // 内部 InvokeSync，回到这里时状态已经生效
	return win.IsMaximised(), nil
}

// SaveItem 把当前这条链接的原始字节存到本地：先弹系统「另存为」，再把流写进文件。
// 为什么不在页面里 fetch + <a download>：跨域图片的响应体 webview 读不到（CORS），
// 而带跨域的 download 属性会被直接忽略成「在新标签打开」。取消时返回空字符串。
func (s *PreviewService) SaveItem(rawURL string, name string) (string, error) {
	if s.app == nil {
		return "", errors.New("应用尚未启动")
	}
	u, err := checkPreviewURL(rawURL)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	win := s.win
	s.mu.Unlock()
	if win == nil {
		return "", errors.New("预览窗口未打开")
	}

	path, err := s.app.Dialog.SaveFileWithOptions(&application.SaveFileDialogOptions{
		Title:                "另存为",
		Filename:             saveSuggestName(suggestedSaveName(name, u), u),
		Directory:            saveStartDir(),
		CanCreateDirectories: true,
		Window:               win,
	}).PromptForSingleSelection()
	if err != nil {
		return "", err
	}
	if path == "" {
		return "", nil // 用户取消
	}
	if err := downloadToFile(u.String(), path); err != nil {
		return "", err
	}
	return path, nil
}

// suggestedSaveName 前端送来的名字可能就是网址末段（att.php、一串附件号），那样存出来
// 谁也不认得。这里再问服务器一次 Content-Disposition；点开时已经探过一次，走到另存为
// 说明要么当时探失败，要么这链接根本没进探测名单，重问一次的代价只是一个 HEAD。
func suggestedSaveName(name string, u *url.URL) string {
	if !looksGenericName(name) {
		return name
	}
	if n := probe(u.String()).Name; n != "" {
		return n
	}
	return name
}

/* --------------------------------------------------------------------------- */
/* 类型 / 文件名探测：扩展名认不出的链接问一次服务器                                 */
/* --------------------------------------------------------------------------- */

const probeTimeout = 5 * time.Second

// ProbeResult 一次探测带回来的两件事。空串一律表示「没探到」，调用方保留原判。
//
// 为什么文件名也要探：BBS 的图地址末段就是 att.php 或一串附件号，拿它当窗口标题、
// 当「另存为」的默认名，用户看到的都是同一个没有信息量的脚本名。真名字在服务器的
// Content-Disposition 里（或重定向后的真实地址里），只有探测时才顺路拿得到。
type ProbeResult struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// kindFromContentType 把响应头的 Content-Type 换成预览类型。只认 image/ audio/ video/
// 这三个大类，其余一律返回空（让调用方保留按扩展名的原判）：octet-stream 说明不了
// 任何事，text/html 则确实就是网页。
func kindFromContentType(ct string) string {
	media, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(ct)), ";")
	switch {
	case strings.HasPrefix(media, "image/"):
		return KindImage
	case strings.HasPrefix(media, "audio/"):
		return KindAudio
	case strings.HasPrefix(media, "video/"):
		return KindVideo
	}
	return ""
}

// mediaExtKind 与前端 linkscan.ts 的 IMAGE_EXT/AUDIO_EXT/VIDEO_EXT 同源。
// 服务器把媒体按 octet-stream 下发、却在 Content-Disposition 或存储名里给了
// 带媒体后缀的真名时，按名字后缀认类型——水木的 mp4 附件就是这一种。
var mediaExtKind = map[string]string{
	"jpg": KindImage, "jpeg": KindImage, "jfif": KindImage, "png": KindImage, "gif": KindImage,
	"webp": KindImage, "bmp": KindImage, "avif": KindImage, "svg": KindImage, "ico": KindImage,
	"tif": KindImage, "tiff": KindImage,
	"mp3": KindAudio, "wav": KindAudio, "ogg": KindAudio, "oga": KindAudio, "m4a": KindAudio,
	"aac": KindAudio, "flac": KindAudio, "opus": KindAudio, "wma": KindAudio, "amr": KindAudio,
	"mp4": KindVideo, "webm": KindVideo, "ogv": KindVideo, "mov": KindVideo, "m4v": KindVideo,
	"mkv": KindVideo, "avi": KindVideo, "wmv": KindVideo, "flv": KindVideo, "ts": KindVideo, "3gp": KindVideo,
}

// kindFromName 按文件名后缀认类型；认不出返回空，保留原判。
func kindFromName(name string) string {
	dot := strings.LastIndex(name, ".")
	if dot < 0 || dot == len(name)-1 {
		return ""
	}
	return mediaExtKind[strings.ToLower(name[dot+1:])]
}

// isOpaqueBinary 头里没说类型（或明说 octet-stream）：这时名字后缀才是唯一信号。
// text/html 一类明确非媒体的不在此列，保留网页原判。
func isOpaqueBinary(ct string) bool {
	media, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(ct)), ";")
	return media == "" || media == "application/octet-stream" || media == "binary/octet-stream"
}

// contentTypeExt 常见媒体的后缀对照。表外的一律不猜：把 image/vnd.microsoft.icon
// 当成 ".vnd" 只会让存出来的文件更难认。
var contentTypeExt = map[string]string{
	"image/jpeg": "jpg", "image/pjpeg": "jpg", "image/jpg": "jpg", "image/png": "png",
	"image/gif": "gif", "image/webp": "webp", "image/apng": "apng", "image/avif": "avif",
	"image/bmp": "bmp", "image/x-ms-bmp": "bmp", "image/tiff": "tif", "image/svg+xml": "svg",
	"video/mp4": "mp4", "video/x-m4v": "m4v", "video/quicktime": "mov", "video/webm": "webm",
	"video/x-matroska": "mkv", "video/x-flv": "flv", "video/mpeg": "mpeg", "video/3gpp": "3gp",
	"audio/mpeg": "mp3", "audio/mp3": "mp3", "audio/mp4": "m4a", "audio/x-m4a": "m4a",
	"audio/aac": "aac", "audio/ogg": "ogg", "audio/wav": "wav", "audio/x-wav": "wav",
	"audio/flac": "flac", "audio/x-flac": "flac", "audio/3gpp": "3gp", "audio/amr": "amr",
}

// extFromContentType 从 Content-Type 推一个能双击打开的后缀；推不出返回空。
func extFromContentType(ct string) string {
	media, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(ct)), ";")
	if ext, ok := contentTypeExt[media]; ok {
		return ext
	}
	main, sub, ok := strings.Cut(media, "/")
	if !ok || !isMediaMainType(main) {
		return ""
	}
	sub = strings.TrimPrefix(sub, "x-")
	if len(sub) == 0 || len(sub) > 5 {
		return ""
	}
	for i := 0; i < len(sub); i++ {
		c := sub[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			continue
		}
		return "" // 带 +xml、.foo 这种结构后缀的一律不猜
	}
	return sub
}

func isMediaMainType(main string) bool {
	return main == "image" || main == "audio" || main == "video"
}

// scriptExts 是「网址自己」的后缀，不是文件名：att.php、topic.jsp 这类地址的末段
// 拿去当标题，用户永远只能看到脚本名。
var scriptExts = map[string]bool{
	"php": true, "phtml": true, "asp": true, "aspx": true, "jsp": true, "jspx": true,
	"cgi": true, "pl": true, "py": true, "ashx": true, "cfm": true,
	"html": true, "htm": true, "shtml": true, "xhtml": true,
}

// looksGenericName 名字是否等于没名字：空、纯数字（附件号）、或者脚本/网页后缀。
// 命中就得再问服务器要真名。前端 linkscan.ts 里是同一套判断，改这里要一起改。
func looksGenericName(name string) bool {
	n := strings.TrimSpace(name)
	n = strings.TrimLeft(n, "/")
	if q := strings.IndexAny(n, "?#"); q >= 0 {
		n = n[:q]
	}
	if n == "" {
		return true
	}
	pureDigits := true
	for _, r := range n {
		if r < '0' || r > '9' {
			pureDigits = false
			break
		}
	}
	if pureDigits {
		return true // 47743632 这种附件号
	}
	dot := strings.LastIndex(n, ".")
	if dot < 0 || dot == len(n)-1 {
		return true // 没有后缀，看不出是什么文件
	}
	ext := strings.ToLower(n[dot+1:])
	return scriptExts[ext]
}

// probe 发一次 HEAD（服务器不认 HEAD 就退化成带 Range 的 GET），只为拿类型与文件名。
func probe(rawURL string) ProbeResult {
	u, err := checkPreviewURL(rawURL)
	if err != nil {
		return ProbeResult{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	if res, answered := probeOnce(ctx, u.String(), http.MethodHead); answered {
		return res
	}
	res, _ := probeOnce(ctx, u.String(), http.MethodGet)
	return res
}

// probeOnce 的第二个返回值是「服务器有没有好好答话」（2xx/3xx）。没答话时调用方
// 要换个方法再来一次，不能把 405 当成「这就是个网页」。
func probeOnce(ctx context.Context, rawURL, method string) (ProbeResult, bool) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return ProbeResult{}, false
	}
	req.Header.Set("User-Agent", previewUserAgent)
	if method == http.MethodGet {
		req.Header.Set("Range", "bytes=0-0") // 退化路径也不要下整张图
	}
	resp, err := previewHTTP.Do(req)
	if err != nil {
		return ProbeResult{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return ProbeResult{}, false
	}
	ct := resp.Header.Get("Content-Type")
	kind := kindFromContentType(ct)
	var finalURL *url.URL
	if resp.Request != nil {
		finalURL = resp.Request.URL // 被重定向时，真实地址末尾往往就是文件名
	}
	origURL, err := checkPreviewURL(rawURL)
	if err != nil {
		origURL = nil
	}
	name := probeName(resp.Header.Get("Content-Disposition"), ct, kind, finalURL, origURL)
	// 服务器把媒体当 octet-stream 下发、却用 Content-Disposition/存储名说了真名
	// （水木 mp4 附件即如此）：按名字后缀认类型，否则判成网页后顶层导航
	// 撞上 attachment 头就变成下载。
	if kind == "" && isOpaqueBinary(ct) {
		kind = kindFromName(name)
	}
	return ProbeResult{Kind: kind, Name: name}, true
}

// 查询串里的附件号参数：水木以外常见的写法，值本身就是这张图的身份。
// 顺序就是优先级，写在这里是为了同一个地址每次给同一个名字（遍历 map 做不到）。
var queryIDKeys = []string{"attid", "attachid", "attachment", "aid", "fid", "itemid", "fileid", "imgid", "picid", "att", "id"}

// probeName 给一次探测挑名字，按可信度从高到低：
//
//	Content-Disposition 里的原名    —— 服务器自己说的，最可信
//	地址末段（重定向后的优先）          —— 得「像个文件名」，att.php 不算
//	查询串里的文件名或附件号            —— att.php?id=47743632 这类，那个号就是这张图的身份
//	末段主干 + Content-Type 推的后缀 —— /att/47743632 变成 47743632.jpg：光一串号存下来打不开
//
// 全都拿不到就返回空，让调用方保留按扩展名的原判，不拿猜的结果去覆盖。
func probeName(disposition, contentType, kind string, finalURL, origURL *url.URL) string {
	if n := filenameFromDisposition(disposition); n != "" {
		return n
	}
	for _, u := range []*url.URL{finalURL, origURL} {
		if n := nameFromPath(u); n != "" {
			return n
		}
	}
	ext := ""
	if kind != "" { // 不是媒体就别硬凑后缀
		ext = extFromContentType(contentType)
	}
	for _, u := range []*url.URL{finalURL, origURL} {
		if n := nameFromQuery(u, ext); n != "" {
			return n
		}
	}
	if ext == "" {
		return ""
	}
	for _, u := range []*url.URL{finalURL, origURL} {
		if u == nil {
			continue
		}
		if stem := bestStemFromPath(u.Path); stem != "" {
			return stem + "." + ext
		}
	}
	return ""
}

// bestStemFromPath 从路径末尾往前挑一个能当文件名的主干。水木那类地址形如
// /nForum/att/AutoBoard/47743632/0：真正的身份是那个长附件号，末段只是个序号，
// 拿它凑出 0.jpg 比不给名字更让人糊涂。太短的数字串一律不当主干。
func bestStemFromPath(p string) string {
	segs := strings.Split(strings.Trim(p, "/"), "/")
	for i := len(segs) - 1; i >= 0; i-- {
		stem := stemOf(decodeSegment(segs[i]))
		if stem == "" || isShortDigits(stem) {
			continue
		}
		return stem
	}
	return ""
}

// isShortDigits 是不是小于 4 位的纯数字：附件号都是长串，短数字只会是序号、页数。
func isShortDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return len(s) < 4
}

// nameFromPath 地址末段能直接当文件名就用它。
func nameFromPath(u *url.URL) string {
	if u == nil {
		return ""
	}
	seg := pathLastSegment(u.Path)
	if seg == "" || looksGenericName(seg) {
		return ""
	}
	return seg
}

// nameFromQuery 从查询串里挑名字。两种写法都见过：
//
//	att.php?ZM3HKEGY…AAABkm0CB.png   水木 —— 整串查询就是存储名，根本没有键
//	?fn=cat.png                        一般 —— 某个参数的值是文件名
//
// 按地址自己的顺序取第一个「像个文件名」的片段（值优先于键），不用 map 遍历；
// 都没有时，才退到常见附件号参数（?id=47743632）配上推出来的后缀。
func nameFromQuery(u *url.URL, ext string) string {
	if u == nil {
		return ""
	}
	for _, pair := range strings.Split(strings.TrimPrefix(u.RawQuery, "?"), "&") {
		key, val, _ := strings.Cut(pair, "=")
		for _, cand := range []string{val, key} {
			if cand == "" || len(cand) > 80 {
				continue
			}
			if n := cleanDispositionName(cand); n != "" { // 顺路去目录、还原转义、验后输
				return n
			}
		}
	}
	if ext == "" {
		return ""
	}
	q := u.Query()
	for _, k := range queryIDKeys {
		if stem := sanitizeStem(q.Get(k)); stem != "" {
			return stem + "." + ext
		}
	}
	return ""
}

// scriptStems 是所有链接共用的脚本主干：att、img、download……拿它当文件名，
// 一整排图都会叫 att.jpg，看着就是 bug。这些地址得靠附件号或 Content-Disposition。
var scriptStems = map[string]bool{
	"att": true, "attachment": true, "attach": true, "img": true, "image": true, "pic": true,
	"file": true, "files": true, "download": true, "dl": true, "show": true, "get": true,
	"view": true, "load": true, "up": true, "upload": true, "index": true, "main": true,
}

// stemOf 去掉后缀留下主干，并且只收「能当名字用」的主干。
func stemOf(seg string) string {
	if dot := strings.LastIndex(seg, "."); dot > 0 {
		seg = seg[:dot]
	}
	stem := sanitizeStem(seg)
	if stem != "" && scriptStems[strings.ToLower(stem)] {
		return ""
	}
	return stem
}

// sanitizeStem 只收字母数字下划线连字符，并且不能太长：查询串里塞整段路径、塞 URL
// 的写法真实存在（?url=../../x），那类值宁可不给名字。
func sanitizeStem(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" || len(s) > 32 {
		return ""
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return ""
		}
	}
	return s
}

func pathLastSegment(p string) string {
	p = strings.TrimRight(p, "/")
	if p == "" {
		return ""
	}
	return decodeSegment(p[strings.LastIndex(p, "/")+1:])
}

// decodeSegment 还原地址里的 %xx 转义。解不动（残缺转义、解出来不是合法 UTF-8）
// 就原样给回 —— 地址本身就是给人看的，猜一个怪名字不如不做。
func decodeSegment(seg string) string {
	if decoded, err := url.PathUnescape(seg); err == nil && utf8.ValidString(decoded) {
		return decoded
	}
	return seg
}

// filenameFromDisposition 从 Content-Disposition 取文件名。认两种写法：
//
//	filename="photo.jpg"            最常见
//	filename*=UTF-8''%E5%BE%BD.png  RFC 5987/6266，中文站常用，charset 可能是 gbk
//
// 两个都给时取 filename*：服务器是拿带 * 的那份放非 ASCII 名字的。
func filenameFromDisposition(header string) string {
	if strings.TrimSpace(header) == "" {
		return ""
	}
	plain, ext := dispositionParams(header)
	if n := percentDecodeExt(ext); n != "" {
		return n
	}
	return cleanDispositionName(plain)
}

// dispositionParams 拆出 filename 与 filename*。先走 mime.ParseMediaType（它能正确处理
// 引号里的分号），但它对 filename*=gbk”%CE%D2… 这种不合 token 规矩的写法会「成功但
// 什么都不给」（实测 params 是空 map），所以没拿到名字时必须退回粗切分。
func dispositionParams(header string) (plain, ext string) {
	if _, params, err := mime.ParseMediaType(header); err == nil {
		if p, e := params["filename"], params["filename*"]; p != "" || e != "" {
			return p, e
		}
	}
	for _, part := range strings.Split(header, ";") {
		key, val, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "filename*":
			ext = strings.TrimSpace(val)
		case "filename":
			if plain == "" {
				plain = strings.TrimSpace(val)
			}
		}
	}
	return plain, ext
}

// percentDecodeExt 解 RFC 5987 的 "charset'lang'名字"。名字按字节做百分号解码再按
// charset 转 UTF-8 —— 直接用 url.PathUnescape 拿到的 string 会把非 UTF-8 字节原样保留，
// 正好能当字节序列交给 GBK 解码器。
func percentDecodeExt(spec string) string {
	spec = strings.Trim(strings.TrimSpace(spec), `"`)
	charset, rest, ok := strings.Cut(spec, "'")
	if !ok {
		return ""
	}
	_, rest, _ = strings.Cut(rest, "'") // 语言段可有可无
	if raw, err := url.PathUnescape(rest); err == nil {
		rest = raw
	}
	return cleanDispositionName(decodeCharset(rest, charset))
}

// cleanDispositionName 去掉目录部分与引号：名字是远端给的，"../../../Windows/x" /
// "C:\evil.exe" 这种写法不能直接进「另存为」的默认名。
func cleanDispositionName(raw string) string {
	s := strings.Trim(strings.TrimSpace(raw), `"`)
	if s == "" {
		return ""
	}
	// 不少 PHP 站点拿 rawurlencode 的结果直接填进 filename="…"，不先还原就存不出中文。
	// 真有字面 % 的名字（100%discount.jpg）PathUnescape 会报错，原样保留。
	if decoded, err := url.PathUnescape(s); err == nil && utf8.ValidString(decoded) {
		s = decoded
	}
	s = strings.ReplaceAll(s, "\\", "/")
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	if strings.Contains(s, "/") {
		return ""
	}
	if !looksLikeFileName(s) {
		return "" // 一串没后缀的乱码不如不给，让调用方继续往下找
	}
	return s
}

// looksLikeFileName 只要一眼能看出「这是个文件」就放行：有个短后缀，而且后缀是 2
// 到 5 个字母。远端的名字千奇百怪（?a=1.2 这种也带个点），宁可不给也别给一串乱码。
func looksLikeFileName(s string) bool {
	dot := strings.LastIndex(s, ".")
	if dot <= 0 || dot == len(s)-1 {
		return false
	}
	ext := strings.ToLower(s[dot+1:])
	if len(ext) < 2 || len(ext) > 5 {
		return false
	}
	for i := 0; i < len(ext); i++ {
		c := ext[i]
		if c < 'a' || c > 'z' {
			return false
		}
	}
	return true
}

// Probe 单条探测：前端点开的这一条要先定性，才知道走媒体还是走网页，名字得问服务器才拿得到。
func (s *PreviewService) Probe(rawURL string) ProbeResult { return probe(rawURL) }

// ProbeMany 并行探测一组链接，返回值与入参一一对应（探不出来是空字段）。
// 下标写各自的格子，不共享写。限并发是为了不把一次点击拖成几十并发。
func (s *PreviewService) ProbeMany(urls []string) []ProbeResult {
	out := make([]ProbeResult, len(urls))
	if len(urls) == 0 {
		return out
	}
	lim := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, u := range urls {
		wg.Add(1)
		go func(i int, u string) {
			defer wg.Done()
			lim <- struct{}{}
			defer func() { <-lim }()
			out[i] = probe(u)
		}(i, u)
	}
	wg.Wait()
	return out
}

/* --------------------------------------------------------------------------- */
/* 另存为：下载与文件名                                                          */
/* --------------------------------------------------------------------------- */

// previewUserAgent 用一个普通浏览器 UA：不少站点对空 UA 也会拦。
const previewUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36"

// saveTimeout 给大文件留足时间：只限住单次下载，不能当成全局 client.Timeout 用
// （那样会把一个慢慢但一直在动的传输提前掉）。
const saveTimeout = 10 * time.Minute

var previewHTTP = &http.Client{
	// 与 IP 库下载同一个 transport：设置里那一个代理同时管住这两条出站路，
	// 不需要在预览里再配一次。
	Transport: proxyAwareTransport(4),
}

// downloadToFile 先写 .part 再改名覆盖：os.Create 会把目标截断，万一下载到一半
// 失败，用户原来那个文件就空了。
func downloadToFile(rawURL, path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), saveTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", previewUserAgent)
	// 不带 Referer：跟预览里 <img referrerpolicy="no-referrer"> 一致，
	// 多站点的图正是「无来源」才放行。

	resp, err := previewHTTP.Do(req)
	if err != nil {
		return fmt.Errorf("下载失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("服务器返回 %s", resp.Status)
	}

	tmp := path + ".part"
	if err := writeStreamToFile(resp.Body, tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	if _, err := os.Stat(path); err == nil {
		// Windows 的 rename 不覆盖已存在文件；能走到这一步说明用户已在对话框里确认过覆盖
		if err := os.Remove(path); err != nil {
			os.Remove(tmp)
			return fmt.Errorf("无法覆盖已存在的文件：%w", err)
		}
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("保存失败：%w", err)
	}
	log.Printf("预览另存为：%s → %s", rawURL, path)
	return nil
}

func writeStreamToFile(r io.Reader, path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	n, cpErr := io.Copy(f, r)
	clErr := f.Close()
	if cpErr != nil {
		return fmt.Errorf("下载中断：%w", cpErr)
	}
	if clErr != nil {
		return fmt.Errorf("写入失败：%w", clErr)
	}
	log.Printf("另存为已写入 %d 字节：%s", n, path)
	return nil
}

// saveSuggestName 拼一个安全的默认文件名：名字丢了就从路径末段拿，
// 没扩展名就从 URL 补一个（否则存出来双击打不开）。
//
// att.php / index.html 这类「网站自己的脚本名」直接丢掉：一整帖图的默认名全叫
// att.php 等于没名字，不如落在 download 上。
func saveSuggestName(name string, u *url.URL) string {
	base := sanitizeFileName(name)
	if isScriptName(base) {
		base = ""
	}
	if base == "" {
		segs := strings.Split(strings.Trim(u.Path, "/"), "/")
		base = sanitizeFileName(segs[len(segs)-1])
		if isScriptName(base) {
			base = ""
		}
	}
	if base == "" {
		base = "download"
	}
	if filepath.Ext(base) == "" {
		if ext := urlExt(u.Path); ext != "" && !scriptExts[ext] {
			base += "." + ext
		}
	}
	return base
}

// isScriptName 名字的后缀是不是 php/html 这种「网站自己」的后缀。
// 比 looksGenericName 窄：那一套用「要不要去探测」，纯数字、没后缀的名字仍值得保留，
// 但脚本名是确定无效的，不能当文件名给用户。
func isScriptName(name string) bool {
	ext := strings.ToLower(filepath.Ext(strings.TrimSpace(name)))
	if ext == "" {
		return false
	}
	return scriptExts[ext[1:]]
}

// urlExt 取路径末段的后缀，只收字母数字且不太长的（避开 «.html?x» 这类垃圾）。
// 不能只认字母：mp4、3gp、m4a 这些常见媒体后缀里就有数字。
func urlExt(p string) string {
	last := p[strings.LastIndex(p, "/")+1:]
	dot := strings.LastIndex(last, ".")
	if dot < 0 || dot == len(last)-1 {
		return ""
	}
	ext := strings.ToLower(last[dot+1:])
	if len(ext) > 5 {
		return ""
	}
	letters := 0
	for _, r := range ext {
		switch {
		case r >= 'a' && r <= 'z':
			letters++
		case r >= '0' && r <= '9':
		default:
			return ""
		}
	}
	if letters == 0 {
		return "" // 全是数字（.123）不是后缀
	}
	return ext
}

// sanitizeFileName 去掉 Windows 不允许的字符与控制字符，并裁掉尾部的点与空格
// （NTFS 会把它们默默吃掉，结枛得到一个看上去有名字的文件）。按 rune 截，
// 不能把中文剔成半个。
func sanitizeFileName(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '<', '>', ':', '"', '/', '\\', '|', '?', '*':
			b.WriteRune('_')
		default:
			if r < 0x20 {
				continue
			}
			b.WriteRune(r)
		}
	}
	s = strings.TrimRight(b.String(), " .")
	if rs := []rune(s); len(rs) > 120 {
		s = string(rs[:120])
	}
	return s
}

// saveStartDir 默认落在用户目录下的 Downloads，拿不到就退回用户目录。
func saveStartDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "."
	}
	dl := filepath.Join(home, "Downloads")
	if st, err := os.Stat(dl); err == nil && st.IsDir() {
		return dl
	}
	return home
}

func (s *PreviewService) setMode(mode string) {
	s.mu.Lock()
	s.mode = mode
	s.mu.Unlock()
}

// dockGeometry 算停泊位与弹出起始位（都是 DIP）。
//
//	window 模式：弹进主窗口显示区域里面——与主窗同宽、底缘对齐主窗底缘，
//	             像从主窗底部推上来的抽屉；高度先按屏幕比例取，再夹到主窗
//	             高度的 3/4，留住顶部工具条和几行终端。
//	quake 模式：主窗本就挂在屏幕顶，预览从工作区顶缘弹出，盖住主窗上半截。
//
// 高度按工作区高度的 62% 取，再夹进 [260, 1200] 与可用空间。中间量一律用物理
// 像素（Bounds 是 DIP、WorkArea 是物理，两者不能混算），最后统一换算。
func (s *PreviewService) dockGeometry() (dock, start application.Rect) {
	const defW, defH = 1000, 700
	fallback := application.Rect{Width: defW, Height: defH}
	if s.main == nil || s.app == nil {
		return fallback, fallback
	}
	mb := application.DipToPhysicalRect(s.main.Bounds())
	screen, err := s.main.GetScreen()
	if err != nil || screen == nil {
		return fallback, fallback
	}
	wa := screen.PhysicalWorkArea
	w, h := mb.Width, wa.Height*62/100
	if h < 260 {
		h = 260
	}
	if h > 1200 {
		h = 1200
	}
	if h > wa.Height {
		h = wa.Height
	}
	if w > wa.Width {
		w = wa.Width
	}
	x := mb.X
	if x < wa.X {
		x = wa.X
	}
	if x+w > wa.X+wa.Width {
		x = wa.X + wa.Width - w
	}
	quake := s.ctl != nil && s.ctl.Mode() == ModeQuake
	var y int
	if quake {
		y = wa.Y
	} else {
		// window 模式：泊在主窗里面、底缘对齐。主窗很小时高度跟着它走，
		// 最小尺寸下限已在 newWindow 按停泊位放宽，不会被钳制撑回去。
		if capH := mb.Height * 3 / 4; h > capH {
			h = capH
		}
		if h > mb.Height {
			h = mb.Height
		}
		y = mb.Y + mb.Height - h
	}
	dockPhys := application.Rect{X: x, Y: y, Width: w, Height: h}
	startPhys := dockPhys
	// 起始位允许越出屏幕：quake 从顶缘上方滑入，window 从主窗底缘下方推入，不夹。
	if quake {
		startPhys.Y -= previewSlideDist
	} else {
		startPhys.Y += previewSlideDist
	}
	return application.PhysicalToDipRect(dockPhys), application.PhysicalToDipRect(startPhys)
}

/* --------------------------------------------------------------------------- */
/* 弹出 / 收起动画                                                                */
/* --------------------------------------------------------------------------- */

// previewAnim 一次窗口动画的令牌：cancel 通知中途放弃，done 等它真正退出。
// 新动画总是先取消上一个并等它退出，保证不会有两个动画同时写一个窗口。
type previewAnim struct {
	cancel chan struct{}
	done   chan struct{}
}

// beginAnim 接管动画槽。调用方不能在主播线程：等上一个动画退出可能需要它
// 跑完手里那一帧 InvokeSync，而 InvokeSync 要等主线程空出来。
func (s *PreviewService) beginAnim() *previewAnim {
	a := &previewAnim{cancel: make(chan struct{}), done: make(chan struct{})}
	s.mu.Lock()
	prev := s.anim
	s.anim = a
	s.mu.Unlock()
	if prev != nil {
		close(prev.cancel)
		<-prev.done
	}
	return a
}

// sameWindow 窗口还是服务手里那个且活着：动画半途窗口被关/被换就必须立刻放弃。
func (s *PreviewService) sameWindow(win *application.WebviewWindow) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.alive && s.win == win
}

func (s *PreviewService) rects() (dock, start application.Rect) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dock, s.dockStart
}

// waitFadeOut 新的打开请求撞上正在淡出的预览：等它关完再走正常流程（新建窗口）。
// 淡出最多 170ms，10ms 一轮 poll 40 轮足够宽裕；真卡住了也不会死等。
func (s *PreviewService) waitFadeOut() {
	for i := 0; i < 40; i++ {
		s.mu.Lock()
		fading := s.alive && s.pvState == pvFadingOut
		s.mu.Unlock()
		if !fading {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func easeOutCubic(t float64) float64  { u := 1 - t; return 1 - u*u*u }
func easeInQuad(t float64) float64    { return t * t }
func lerpInt(a, b int, t float64) int { return a + int(math.Round(float64(b-a)*t)) }

// fadeIn 把预览窗从起始位滑进停泊位并淡入，落定后摘掉分层风格。
func (s *PreviewService) fadeIn(win *application.WebviewWindow, hwnd uintptr) {
	a := s.beginAnim()
	defer close(a.done)
	dock, start := s.rects()
	t0 := time.Now()
	tick := time.NewTicker(previewAnimTick)
	defer tick.Stop()
	for {
		select {
		case <-a.cancel:
			return
		case <-tick.C:
		}
		if !s.sameWindow(win) {
			return
		}
		t := float64(time.Since(t0)) / float64(previewSlideInMS*time.Millisecond)
		if t >= 1 {
			break
		}
		e := easeOutCubic(t)
		x, y := lerpInt(start.X, dock.X, e), lerpInt(start.Y, dock.Y, e)
		// 淡入比滑动稍快：弹出早期就看得见内容，不是滑到位才突然冒出来。
		alpha := byte(math.Min(255, 255*e*1.5))
		application.InvokeSync(func() {
			// SetPosition 内部会把分层窗的 alpha 重置回 255（Wails setPhysicalBounds），
			// 所以移动之后必须补回目标透明度；两句同批落在主线程，外面看不到中间帧。
			win.SetPosition(x, y)
			previewSetAlpha(hwnd, alpha)
		})
	}
	application.InvokeSync(func() {
		win.SetPosition(dock.X, dock.Y)
		previewSetAlpha(hwnd, 255)
		previewSetLayered(hwnd, false)
		setWindowRoundCorners(hwnd)
	})
}

// fadeOut 把预览窗沿弹出路径收起并淡出，然后正式关闭。起点取窗口当前的实际
// 位置/透明度：淡入半途被打断、或用户拖过窗口，都从当下状态顺順当当地收。
func (s *PreviewService) fadeOut(win *application.WebviewWindow) {
	a := s.beginAnim()
	defer close(a.done)
	s.mu.Lock()
	hwnd, dock, start := s.hwnd, s.dock, s.dockStart
	s.mu.Unlock()
	if hwnd == 0 {
		s.commitClose(win)
		return
	}
	var curX, curY int
	var fromAlpha byte
	application.InvokeSync(func() {
		curX, curY = win.Position()
		fromAlpha = previewAlpha(hwnd)
		previewSetLayered(hwnd, true)
		previewSetAlpha(hwnd, fromAlpha)
	})
	// 沿原弹出路径收回：用户挪过窗口就把整条路径平移同样的偏移，保持「从哪来回哪去」。
	dx, dy := start.X-dock.X, start.Y-dock.Y
	toX, toY := curX+dx, curY+dy
	t0 := time.Now()
	tick := time.NewTicker(previewAnimTick)
	defer tick.Stop()
	for {
		select {
		case <-a.cancel:
			return
		case <-tick.C:
		}
		if !s.sameWindow(win) {
			return
		}
		t := float64(time.Since(t0)) / float64(previewSlideOutMS*time.Millisecond)
		if t >= 1 {
			break
		}
		e := easeInQuad(t)
		x, y := lerpInt(curX, toX, e), lerpInt(curY, toY, e)
		alpha := byte(float64(fromAlpha) * (1 - e))
		application.InvokeSync(func() {
			win.SetPosition(x, y)
			previewSetAlpha(hwnd, alpha)
		})
	}
	application.InvokeSync(func() { previewSetAlpha(hwnd, 0) })
	s.commitClose(win)
}

// commitClose 淡出的收尾：动画没被接管、窗口没被替换才正式关。先置 pvClosing
// 让 hook 放行这一次 Close，否则会被再拦下来没完没了。
func (s *PreviewService) commitClose(win *application.WebviewWindow) {
	s.mu.Lock()
	if s.pvState != pvFadingOut || s.win != win || !s.alive {
		s.mu.Unlock()
		return
	}
	s.pvState = pvClosing
	hwnd := s.hwnd
	s.mu.Unlock()
	// 预览还在前台时主动把前台交还主窗，销毁子窗时系统就不会再「补」一次
	// owner 激活——主窗在关闭那一刻看不到激活切换。用户已 Alt+Tab 去别的
	// 应用时不抢前台。
	if hwnd != 0 && foregroundHWND() == hwnd && s.main != nil {
		application.InvokeSync(func() {
			if p := s.main.NativeWindow(); p != nil {
				previewFocusMain(uintptr(p))
			}
		})
	}
	win.Close()
}

// redock 复用已开着的预览窗时（主窗可能挪过/改过大小/切过形态）滑回停泊位。
// 顺带收拾淡入被打断的残局：先恢复不透明、摘掉分层，再滑。
func (s *PreviewService) redock(win *application.WebviewWindow) {
	dock, start := s.dockGeometry()
	s.mu.Lock()
	s.dock, s.dockStart = dock, start
	hwnd := s.hwnd
	s.mu.Unlock()
	a := s.beginAnim()
	defer close(a.done)
	var cur application.Rect
	application.InvokeSync(func() {
		previewSetAlpha(hwnd, 255)
		previewSetLayered(hwnd, false)
		cur = win.Bounds()
	})
	if cur == dock {
		return
	}
	t0 := time.Now()
	tick := time.NewTicker(previewAnimTick)
	defer tick.Stop()
	for {
		select {
		case <-a.cancel:
			return
		case <-tick.C:
		}
		if !s.sameWindow(win) {
			return
		}
		t := float64(time.Since(t0)) / float64(previewGlideMS*time.Millisecond)
		if t >= 1 {
			break
		}
		e := easeOutCubic(t)
		r := application.Rect{
			X:      lerpInt(cur.X, dock.X, e),
			Y:      lerpInt(cur.Y, dock.Y, e),
			Width:  lerpInt(cur.Width, dock.Width, e),
			Height: lerpInt(cur.Height, dock.Height, e),
		}
		application.InvokeSync(func() { win.SetBounds(r) })
	}
	application.InvokeSync(func() { win.SetBounds(dock) })
}

/* --------------------------------------------------------------------------- */
/* 纯函数：链接准入、序号钳制、标题                                               */
/* --------------------------------------------------------------------------- */

// checkPreviewItemURL 在链接准入之外，只额外放行 FileService 签过名的本地文件路由。
// 真正的 HMAC 仍由 fmassets.go 的资产处理器校验；这里不收任意相对路径、file: 或跨域地址。
func checkPreviewItemURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err == nil && u.Scheme == "" && u.Host == "" && u.Path == fmFileRoute && u.Fragment == "" {
		q := u.Query()
		if q.Get("p") != "" && q.Get("k") != "" {
			return u, nil
		}
	}
	return checkPreviewURL(raw)
}

// checkPreviewURL 只放行 http/https。链接内容来自远端，javascript: / data: / file:
// 一律不许导航 —— 预览窗口跑在应用自己的 webview 里，放进去等于把后端绑定交出去。
func checkPreviewURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("链接无法解析：%w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("不支持的链接类型 %q（只允许 http/https）", u.Scheme)
	}
	if u.Host == "" {
		return nil, errors.New("链接缺少主机名")
	}
	return u, nil
}

func clampIndex(i, n int) int {
	if n <= 0 || i < 0 || i >= n {
		return 0
	}
	return i
}

// previewTitle 组窗口标题：类型 + 一个人类认得出的短名。短名取不到就退回主机名，
// 标题栏不能是空的。截断按 rune，切半个中文出来会在 UTF-8 里留下乱码。
func previewTitle(kind, name, rawURL string) string {
	label := map[string]string{
		KindImage: "图片", KindAudio: "音频", KindVideo: "视频", KindPage: "网页",
		KindText: "文本", KindPDF: "PDF", KindFile: "文件",
	}[kind]
	n := strings.TrimSpace(name)
	if n == "" {
		n = urlHost(rawURL)
	}
	if n == "" {
		n = rawURL
	}
	const maxRunes = 60
	if rs := []rune(n); len(rs) > maxRunes {
		n = string(rs[:maxRunes]) + "…"
	}
	// 认不出的类型只留「预览」两字：拼成「预览预览 · x」是 bug 不是空值回退。
	prefix := "预览"
	if label != "" {
		prefix = label + "预览"
	}
	return prefix + " · " + n
}

func urlHost(rawURL string) string {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return ""
	}
	return u.Host
}

/* --------------------------------------------------------------------------- */
/* ESC 监听：给键盘钩子用                                                        */
/* --------------------------------------------------------------------------- */

// escWatch 是「现在按 ESC 该关掉哪个窗口」。
//
// 为什么要伸到系统键盘钩子里去接：预览窗口切到网页模式之后，整个 webview 都是对方的
// 文档 —— 脚本注入不进去，keydown 收不到，ESC 只能在更下面一层截住。
//
// 钩子线程只做一个 user32 调用比对前台 HWND（关窗动作绝对不能在钩子线程上等：
// Close() 内部 InvokeSync 要等主线程，那会把整条键盘链拖死），命中后把动作丢进
// goroutine。全局只可能有一个预览窗口，所以一个原子指针就够了。
type escWatch struct {
	hwnd uintptr
	fire func()
}

var escWatchVal atomic.Pointer[escWatch]

func armEscWatch(hwnd uintptr, fire func()) {
	if hwnd == 0 || fire == nil {
		disarmEscWatch(0)
		return
	}
	escWatchVal.Store(&escWatch{hwnd: hwnd, fire: fire})
}

// disarmEscWatch 摘掉监听。hwnd 传 0 表示「不管是谁都摘」，用于关窗收尾时手里
// 已经没有 HWND 的情况；传具体值时只摘对得上的，避免误摘后来者的监听。
func disarmEscWatch(hwnd uintptr) {
	w := escWatchVal.Load()
	if w == nil || (hwnd != 0 && w.hwnd != hwnd) {
		return
	}
	escWatchVal.CompareAndSwap(w, nil)
}

// hitEscWatch 由键盘钩子线程调用，返回 true 表示这次按键已被吃掉。
//
// 它挂在 globalKeyEngine.onKey 的最前面，比「跨应用热键总开关」还早：用户把全局
// 热键整个关掉之后，ESC 关预览窗口的能力不该跟着消失。
func hitEscWatch(ev gkEvent) bool {
	if ev.Vk != vkEscape || !ev.Down {
		return false
	}
	w := escWatchVal.Load()
	if w == nil || foregroundHWND() != w.hwnd {
		return false
	}
	if !escWatchVal.CompareAndSwap(w, nil) {
		return true // 另一次按键已经接管关窗，这次也不该把 ESC 漏给网页
	}
	go w.fire()
	return true
}
