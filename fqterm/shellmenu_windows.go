//go:build windows

package main

import (
	"fmt"
	"log"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/wailsapp/wails/v3/pkg/application"
)

// shellmenu_windows.go：在常驻 STA 线程上用 Shell IContextMenu 弹 Explorer 原生右键菜单，
// 顶部拼一段 FTRW 插件动作。
//
// 线程模型（关键）：Wails 主循环在跑自身 Windows 消息泵，服务方法在独立 goroutine 上执行、
// 不占主线程；但原生菜单必须由「有消息队列且一直在泵消息」的线程承载，否则第三方外壳扩展
// （WinRAR 之类）往 owner 窗 SendMessage 会 deadlock。故起一个 runtime.LockOSThread 的常驻
// goroutine，CoInitializeEx(STA) 永不释放，建一个 message-only 窗做 owner，请求经 channel
// 串行进来。TrackPopupMenu 自身在模态期间会泵该线程消息；空闲等请求时也必须泵
// （MsgWaitForMultipleObjects + PeekMessage）：InvokeCommand 在本线程上留下的窗口
// （系统属性页等）靠宿主线程的消息循环才能显示/响应，裸等 channel 会把它们冻住。

var (
	procSHGetDesktopFolder = modShell32.NewProc("SHGetDesktopFolder")
	procILCreateFromPathW  = modShell32.NewProc("ILCreateFromPathW")
	procILFindLastID       = modShell32.NewProc("ILFindLastID")
	procILFree             = modShell32.NewProc("ILFree")

	procCreatePopupMenu           = user32Hook.NewProc("CreatePopupMenu")
	procAppendMenuW               = user32Hook.NewProc("AppendMenuW")
	procDestroyMenu               = user32Hook.NewProc("DestroyMenu")
	procTrackPopupMenu            = user32Hook.NewProc("TrackPopupMenu")
	procGetCursorPos              = user32Hook.NewProc("GetCursorPos")
	procDefWindowProcW            = user32Hook.NewProc("DefWindowProcW")
	procRegisterClassExW          = user32Hook.NewProc("RegisterClassExW")
	procCreateWindowExW           = user32Hook.NewProc("CreateWindowExW")
	procGetModuleHandleW          = modKernel32.NewProc("GetModuleHandleW")
	procPostMessageW              = user32Hook.NewProc("PostMessageW")
	procPeekMessageW              = user32Hook.NewProc("PeekMessageW")
	procMsgWaitForMultipleObjects = user32Hook.NewProc("MsgWaitForMultipleObjects")
	// procSetForegroundWindow 已在 quake_native_windows.go 声明，此处直接复用。
)

const (
	mfString    = 0x0000
	mfPopup     = 0x0010
	mfSeparator = 0x0800

	tpmLeftAlign   = 0x0000
	tpmRightButton = 0x0002
	tpmReturnCmd   = 0x0100

	cmfNormal  = 0x0000
	cmfExplore = 0x0004

	pmRemove   = 0x0001 // PeekMessage：取走消息
	qsAllInput = 0x04FF // MsgWaitForMultipleObjects：任何输入/消息都唤醒

	// IUnknown vtable 槽位。
	slotQueryInterface = 0
	slotRelease        = 2
	// IShellFolder vtable（已对照 Windows SDK ShObjIdl_core.h 核实，继 IUnknown 0/1/2 后）：
	// ParseDisplayName=3、EnumObjects=4、BindToObject=5、BindToStorage=6、CompareIDs=7、
	// CreateViewObject=8、GetAttributesOf=9、GetUIObjectOf=10、GetDisplayNameOf=11、SetNameOf=12。
	slotBindToObject  = 5
	slotGetUIObjectOf = 10
	// IContextMenu 槽位（继 IUnknown 后）：QueryContextMenu=3、InvokeCommand=4、GetCommandString=5。
	slotQueryContextMenu = 3
	slotInvokeCommand    = 4

	shellMenuTimeout = 15 * time.Second
)

var (
	// IID_IContextMenu {000214E4-0000-0000-C000-000000000046}
	iidIContextMenu = ole.GUID{Data1: 0x000214E4, Data2: 0x0000, Data3: 0x0000, Data4: [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
	// IID_IShellFolder {000214E6-0000-0000-C000-000000000046}（Windows SDK ShObjIdl_core.h）
	iidIShellFolder = ole.GUID{Data1: 0x000214E6, Data2: 0x0000, Data3: 0x0000, Data4: [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
)

type shellMenuJob struct {
	hwnd  uintptr
	dir   string
	paths []string
	items []pluginMenuItem
	reply chan shellMenuOutcome
}

var (
	shellMenuOnce   sync.Once
	shellMenuJobs   = make(chan *shellMenuJob)
	menuOwnerOnce   sync.Once
	menuOwnerHwnd   uintptr
	menuOwnerFailed bool
)

// showShellMenu 把一次请求投递到 STA 菜单线程并同步等回执，带两级超时（送不进 / 收不回）。
func showShellMenu(hwnd uintptr, dir string, paths []string, items []pluginMenuItem) shellMenuOutcome {
	shellMenuOnce.Do(func() { go shellMenuThread() })
	job := &shellMenuJob{hwnd: hwnd, dir: dir, paths: paths, items: items, reply: make(chan shellMenuOutcome, 1)}
	select {
	case shellMenuJobs <- job:
	case <-time.After(shellMenuTimeout):
		return shellMenuOutcome{err: "原生菜单线程忙，请稍后再试"}
	}
	select {
	case oc := <-job.reply:
		return oc
	case <-time.After(shellMenuTimeout):
		return shellMenuOutcome{err: "原生菜单响应超时（可能被某个扩展卡住）"}
	}
}

func shellMenuThread() {
	runtime.LockOSThread()
	if err := ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED); err != nil {
		log.Printf("右键菜单线程 COM 初始化失败：%v", err)
	}
	owner := ensureMenuOwner()
	for {
		select {
		case job := <-shellMenuJobs:
			job.reply <- runShellMenuSafe(owner, job)
		default:
			// 空闲必须泵消息：InvokeCommand 在本线程上创建的窗口（系统属性页等）
			// 靠宿主线程的消息循环显示/绘制/响应。旧版裸等 channel，属性页建好后
			// 被冻住，要等下一次右键的 COM 调用顺带泵消息才冒出来——看上去就是
			// 「再点一次右键才出来」。MsgWait 阻塞等消息或 10ms 超时，不空转。
			if r, _, _ := procMsgWaitForMultipleObjects.Call(0, 0, 0, 10, qsAllInput); r == 0 {
				pumpShellMessages()
			}
		}
	}
}

// pumpShellMessages 一轮取空本线程消息队列。msg 布局与 Translate/Dispatch 复用
// kbdhook_windows.go（同包）；只泵消息不读字段，与那边消息循环同一套用法。
func pumpShellMessages() {
	var m msg
	for {
		r, _, _ := procPeekMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0, pmRemove)
		if r == 0 {
			return
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

// runShellMenuSafe 兜住 Go 层 panic（坏 manifest/空指针等），让菜单线程活着；原生崩溃无法
// recover，但那属于扩展自身问题，超时闸门已把 UI 从「永久转圈」里救出来。
func runShellMenuSafe(owner uintptr, job *shellMenuJob) (oc shellMenuOutcome) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("右键菜单处理异常：%v", r)
			oc = shellMenuOutcome{err: "弹出原生菜单时出错"}
		}
	}()
	return runShellMenu(owner, job)
}

func runShellMenu(owner uintptr, job *shellMenuJob) shellMenuOutcome {
	if len(job.paths) == 0 {
		return shellMenuOutcome{err: "没有有效的目标路径"}
	}
	// menuOwner：优先用主窗 HWND（其所属主线程一直在泵消息），退回 message-only owner。
	// 关键：外壳扩展在 QueryContextMenu/GetUIObjectOf 期间会向 owner SendMessage；若 owner
	// 线程（我们的 STA 线程）此刻正阻塞在同一 COM 调用里不泵消息，就自锁。挂到主窗上，
	// 由主线程的消息泵把扩展的消息接走，菜单线程才能顺利跑完 COM 序列。
	menuOwner := job.hwnd
	if menuOwner == 0 {
		menuOwner = owner
	}
	desktop, err := shGetDesktopFolder()
	if err != nil {
		return shellMenuOutcome{err: err.Error()}
	}
	defer comRelease(desktop)

	dir := job.dir
	if dir == "" {
		dir = filepath.Dir(job.paths[0])
	}
	dirPIDL, err := ilCreateFromPath(dir)
	if err != nil {
		return shellMenuOutcome{err: err.Error()}
	}
	defer procILFree.Call(dirPIDL)

	folder, err := shellBindToObject(desktop, dirPIDL)
	if err != nil {
		return shellMenuOutcome{err: err.Error()}
	}
	defer comRelease(folder)

	// 子项相对 PIDL：ILFindLastID 返回的指针指向完整 PIDL 内部，随 full 一起释放，不能单 free。
	var fulls, rels []uintptr
	defer func() {
		for _, f := range fulls {
			procILFree.Call(f)
		}
	}()
	for _, p := range job.paths {
		full, err := ilCreateFromPath(p)
		if err != nil {
			continue
		}
		rel, _, _ := procILFindLastID.Call(full)
		if rel == 0 {
			procILFree.Call(full)
			continue
		}
		fulls = append(fulls, full)
		rels = append(rels, rel)
	}
	if len(rels) == 0 {
		return shellMenuOutcome{err: "目标项目前不存在"}
	}

	icm, err := getUIObjectOfFolder(folder, menuOwner, rels, iidIContextMenu)
	if err != nil {
		log.Printf("GetUIObjectOf 失败：%v", err)
		return shellMenuOutcome{err: err.Error()}
	}
	defer comRelease(icm)

	menu, _, _ := procCreatePopupMenu.Call()
	if menu == 0 {
		return shellMenuOutcome{err: "创建菜单失败"}
	}
	defer procDestroyMenu.Call(menu)

	// FTRW 段：所有插件动作收进一个子菜单，顶部以单项挂载；子菜单与系统原生菜单之间
	// 用两条横线隔开。子菜单 HMENU 挂到父菜单后由父菜单 DestroyMenu 一并销毁，不单 free。
	var indexMenu uintptr
	if len(job.items) > 0 {
		sub, _, _ := procCreatePopupMenu.Call()
		if sub != 0 {
			for _, it := range job.items {
				wp, err := syscall.UTF16PtrFromString(it.title)
				if err != nil {
					continue
				}
				procAppendMenuW.Call(sub, mfString, uintptr(it.cmd), uintptr(unsafe.Pointer(wp)))
			}
			title, _ := syscall.UTF16PtrFromString("FTRW")
			procAppendMenuW.Call(menu, mfPopup, sub, uintptr(unsafe.Pointer(title)))
			indexMenu++
		}
		procAppendMenuW.Call(menu, mfSeparator, 0, 0)
		procAppendMenuW.Call(menu, mfSeparator, 0, 0)
		indexMenu += 2
	}

	if _, err := contextMenuQuery(icm, menu, indexMenu, 0, shellPluginCmdBase-1, cmfNormal|cmfExplore); err != nil {
		log.Printf("QueryContextMenu 失败：%v", err)
		return shellMenuOutcome{err: err.Error()}
	}

	var pt struct{ x, y int32 }
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	// TrackPopupMenu 的 owner 必须与调用线程同线程（否则 ERROR_INVALID_PARAMETER），且要能
	// 真正持前台，否则模态循环刚起来就被去激活 →「闪现即消失」。独立 STA 线程 + 隐形 owner
	// 始终在与 WebView 抢前台，治不彻底。根治：把纯 UI 的 TrackPopupMenu 过桥到「持有主窗的
	// Wails 主线程」执行，owner 用主窗 job.hwnd——它是用户刚右键的真实前台窗，同线程+天然前台。
	// COM 造菜单（icm / QueryContextMenu）仍留在本 STA 线程；InvokeCommand 因 icm 套间亲和也回本线程。
	mainHwnd := job.hwnd
	var cmd uintptr
	var winErr error
	if mainHwnd != 0 {
		application.InvokeSync(func() {
			procSetForegroundWindow.Call(mainHwnd)
			cmd, _, winErr = procTrackPopupMenu.Call(
				menu,
				tpmLeftAlign|tpmRightButton|tpmReturnCmd,
				uintptr(int32(pt.x)), uintptr(int32(pt.y)),
				0, mainHwnd, 0,
			)
			procPostMessageW.Call(mainHwnd, 0, 0, 0) // WM_NULL 清前台锁
		})
	} else {
		// 取不到主窗时退化：留在本 STA 线程，用同线程 owner 窗 + AttachThreadInput 绕前台锁。
		fg, _, _ := procGetForegroundWindow.Call()
		fgTid, _, _ := procGetWindowThreadProcessId.Call(fg, 0)
		curTid, _, _ := procGetCurrentThreadId.Call()
		attached := fgTid != 0 && fgTid != curTid
		if attached {
			r, _, _ := procAttachThreadInput.Call(curTid, fgTid, 1)
			attached = r != 0
		}
		procSetForegroundWindow.Call(owner)
		cmd, _, winErr = procTrackPopupMenu.Call(
			menu,
			tpmLeftAlign|tpmRightButton|tpmReturnCmd,
			uintptr(int32(pt.x)), uintptr(int32(pt.y)),
			0, owner, 0,
		)
		procPostMessageW.Call(owner, 0, 0, 0) // WM_NULL
		if attached {
			procAttachThreadInput.Call(curTid, fgTid, 0)
		}
	}
	if int(cmd) == -1 || cmd == 0 {
		// 0 = 点了空白/Esc；-1 = TrackPopupMenu 出错。都算「没选」。
		if int(cmd) == -1 {
			log.Printf("TrackPopupMenu 失败 mainHwnd=0x%X owner=0x%X：%v", mainHwnd, owner, winErr)
			return shellMenuOutcome{err: "弹出菜单失败"}
		}
		return shellMenuOutcome{}
	}

	if uint32(cmd) >= shellPluginCmdBase {
		if it, ok := lookupPluginMenuItem(job.items, uint32(cmd)); ok {
			return shellMenuOutcome{picked: true, isPlugin: true, pluginID: it.pluginID, actionID: it.actionID}
		}
		return shellMenuOutcome{}
	}

	// 原生动词：父窗优先用主窗 HWND，取不到退回菜单 owner。
	invokeHwnd := job.hwnd
	if invokeHwnd == 0 {
		invokeHwnd = owner
	}
	if err := invokeCommand(icm, invokeHwnd, uint32(cmd)); err != nil {
		return shellMenuOutcome{picked: true, err: err.Error()}
	}
	return shellMenuOutcome{picked: true}
}

// ---- COM vtable 手搬（go-ole v1.3.0 无 comexp 外壳封装，主包也没导出这些接口）----

// comCall 就是 SyscallN 的薄封装（统一返回 (uintptr, error)）。
//
// 必须在菜单线程（STA）上就地同步调用，不能换 goroutine：COM 调用有线程套间亲和，
// 换线程会破坏 STA 语义。避免卡死的正确做法是给 Shell 传一个「一直在泵消息」的
// owner 窗（主窗），见 runShellMenu 里的 menuOwner 选取。
func comCall(fn uintptr, args ...uintptr) (uintptr, error) {
	r, _, err := syscall.SyscallN(fn, args...)
	return r, err
}

func comMethod(obj unsafe.Pointer, slot int) uintptr {
	vtbl := *(*unsafe.Pointer)(obj)
	entry := unsafe.Add(vtbl, slot*int(unsafe.Sizeof(vtbl)))
	return *(*uintptr)(entry)
}

func comRelease(obj unsafe.Pointer) {
	if obj == nil {
		return
	}
	syscall.SyscallN(comMethod(obj, slotRelease), uintptr(obj))
}

func shGetDesktopFolder() (unsafe.Pointer, error) {
	var p unsafe.Pointer
	r, _, _ := procSHGetDesktopFolder.Call(uintptr(unsafe.Pointer(&p)))
	if int32(r) != 0 || p == nil {
		return nil, hrErr(r, "SHGetDesktopFolder")
	}
	return p, nil
}

// shellBindToObject：desktop(IShellFolder)->BindToObject(pidlChild, NULL, IID_IShellFolder, &out)。
// 根目录的「相对」即完整 PIDL，故传 desktop 与目录的完整 PIDL 正好绑定到该文件夹。
func shellBindToObject(desktop unsafe.Pointer, pidl uintptr) (unsafe.Pointer, error) {
	var out unsafe.Pointer
	r, _ := comCall(
		comMethod(desktop, slotBindToObject),
		uintptr(desktop), pidl, 0,
		uintptr(unsafe.Pointer(&iidIShellFolder)),
		uintptr(unsafe.Pointer(&out)),
	)
	if int32(r) != 0 || out == nil {
		return nil, hrErr(r, "BindToObject")
	}
	return out, nil
}

func getUIObjectOfFolder(folder unsafe.Pointer, owner uintptr, rels []uintptr, iid ole.GUID) (unsafe.Pointer, error) {
	var out unsafe.Pointer
	// GetUIObjectOf 原型：(hwndOwner, cbid, apidl[], riid, rgfInOut*, ppv**)。rgfInOut 通例传
	// NULL（标准 MSJ 范例即如此）；之前误把 &out 放在 rgfInOut 位、把 NULL 放在 ppv 位，
	// 使 COM 拿到空 ppv 而失败。现在顺序摆正：riid → rgfInOut(NULL) → ppv(&out)。
	r, _ := comCall(
		comMethod(folder, slotGetUIObjectOf),
		uintptr(folder), owner, uintptr(len(rels)),
		uintptr(unsafe.Pointer(&rels[0])),
		uintptr(unsafe.Pointer(&iid)),
		0,
		uintptr(unsafe.Pointer(&out)),
	)
	if int32(r) != 0 || out == nil {
		return nil, hrErr(r, "GetUIObjectOf")
	}
	return out, nil
}

func contextMenuQuery(icm unsafe.Pointer, menu, indexMenu, idFirst, idLast, flags uintptr) (uintptr, error) {
	// 只信 HRESULT（第 1 返回值）；COM 方法不设 last-error，SyscallN 的第 3 返回值是残留
	// 垃圾（常为“参数不正确”），绝不能当失败。成功时返回 MAKE_HRESULT(SUCCESS,0,nVerbs)，
	// 即非负的小整数（新增项数）。
	r, _ := comCall(comMethod(icm, slotQueryContextMenu), uintptr(icm), menu, indexMenu, idFirst, idLast, flags)
	if int32(r) < 0 {
		return 0, hrErr(r, "QueryContextMenu")
	}
	return r, nil
}

// cminvoke 是 CMINVOKECOMMANDINFO（ShObjIdl_core.h 原文 9 字段，非 EX）；verb 走 MAKEINTRESOURCE
// 偏移形式（QueryContextMenu 的 idFirst=0，菜单 id 即偏移），lpDirectory 传 NULL
// （项已由 PIDL 定位，省掉宽/ANSI 目录串转换）。
// 曾误写成缺 fMask/dwHotKey/hIcon 的「6 字段版」，cbSize=48；Shell 按
// sizeof(CMINVOKECOMMANDINFO)=56（x64）校验 cbSize，不等直接 E_INVALIDARG
// （0x80070057），属性等所有原生动词全失败。字段序与尺寸已对照 SDK 头文件核实，
// 并由 TestCMInvokeCommandInfoLayout 锁死防再犯。
type cminvoke struct {
	cbSize       uint32
	fMask        uint32
	hwnd         uintptr
	lpVerb       uintptr
	lpParameters uintptr
	lpDirectory  uintptr
	nShow        int32
	dwHotKey     uint32
	hIcon        uintptr
}

func invokeCommand(icm unsafe.Pointer, hwnd uintptr, verb uint32) error {
	c := cminvoke{
		cbSize: uint32(unsafe.Sizeof(cminvoke{})),
		hwnd:   hwnd,
		lpVerb: uintptr(verb),
		nShow:  1, // SW_SHOWNORMAL
	}
	r, _ := comCall(comMethod(icm, slotInvokeCommand), uintptr(icm), uintptr(unsafe.Pointer(&c)))
	if int32(r) != 0 {
		return hrErr(r, "InvokeCommand")
	}
	return nil
}

// ilCreateFromPath 用 shell32 的 ILCreateFromPathW 造完整 PIDL（失败返回 0）。
func ilCreateFromPath(p string) (uintptr, error) {
	wp, err := syscall.UTF16PtrFromString(p)
	if err != nil {
		return 0, err
	}
	r, _, _ := procILCreateFromPathW.Call(uintptr(unsafe.Pointer(wp)))
	if r == 0 {
		return 0, fmt.Errorf("无法解析路径：%s", p)
	}
	return r, nil
}

func hrErr(hr uintptr, what string) error {
	return fmt.Errorf("%s 失败 (0x%08X)", what, uint32(hr))
}

// ---- message-only owner 窗 ----

// wndclassEx 严格对齐 Windows WNDCLASSEXW：lpfnWndProc 之后、hInstance 之前必须有
// cbClsExtra/cbWndExtra（各 int32，x64 下正好占一个指针宽度）。漏掉会让后续字段整体
// 错位 8 字节，RegisterClassExW 从错误偏移读到 lpszClassName=NULL，窗类注册失败。
type wndclassEx struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     uintptr
	hIcon         uintptr
	hCursor       uintptr
	hbrBackground uintptr
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       uintptr
}

var shellMenuWndProc = syscall.NewCallback(func(h, m, w, l uintptr) uintptr {
	r, _, _ := procDefWindowProcW.Call(h, m, w, l)
	return r
})

// ensureMenuOwner 在菜单线程上惰性建一个 message-only 窗做 TrackPopupMenu 的 owner。
// 建不成则记 failed，owner 传 0（菜单仍能弹，只是没有父窗收扩展消息，退化但不致命）。
func ensureMenuOwner() uintptr {
	menuOwnerOnce.Do(func() {
		hinst, _, _ := procGetModuleHandleW.Call(0)
		cls, err := syscall.UTF16PtrFromString("FTRWShellMenuWnd")
		if err != nil {
			menuOwnerFailed = true
			return
		}
		wc := wndclassEx{
			cbSize:        uint32(unsafe.Sizeof(wndclassEx{})),
			lpfnWndProc:   shellMenuWndProc,
			hInstance:     hinst,
			lpszClassName: cls,
		}
		atom, _, _ := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
		if atom == 0 {
			log.Printf("右键菜单 owner 窗类注册失败")
			menuOwnerFailed = true
			return
		}
		// owner 必须是菜单线程上的「真正顶层窗」而非 message-only 窗：message-only 窗
		// 永不被激活，SetForegroundWindow 对其无效；而不可见（无 WS_VISIBLE）的普通窗同样
		// 抢不到前台。两者都会使 TrackPopupMenu 的模态循环因非前台而闪现即消失。这里建一个
		// 可见（WS_VISIBLE）但 0 尺寸、移出屏幕（-32000）的 WS_POPUP 工具窗：它技术上可见、
		// 可被激活持前台（菜单得以维持），但尺寸 0 且在屏幕外，用户看不见、也不占任务栏/Alt-Tab。
		const (
			wsPopup        = 0x80000000
			wsVisible      = 0x10000000
			wsExToolwindow = 0x00000080
			cloffscreenTop = ^uintptr(32000 - 1) // -32000（uintptr 回绕）
		)
		hwnd, _, _ := procCreateWindowExW.Call(
			wsExToolwindow,
			uintptr(unsafe.Pointer(cls)),
			0,
			wsPopup|wsVisible,
			cloffscreenTop, cloffscreenTop, 0, 0,
			0, // 顶级窗父柄=NULL（关键：不再是 HWND_MESSAGE）
			0,
			hinst,
			0,
		)
		if hwnd == 0 {
			log.Printf("右键菜单 owner 窗创建失败")
			menuOwnerFailed = true
			return
		}
		menuOwnerHwnd = hwnd
	})
	if menuOwnerFailed {
		return 0
	}
	return menuOwnerHwnd
}
