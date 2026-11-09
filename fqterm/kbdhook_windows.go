//go:build windows

package main

import (
	"fmt"
	"log"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// 用系统级低级键盘钩子（WH_KEYBOARD_LL）实现跨应用热键。
//
// 为什么不用 Wails 的 GlobalShortcut（底层是 RegisterHotKey）：那条路一次只能绑一个
// 「修饰键 + 单键」的组合，既表达不了「双击 Ctrl+Alt」，也不允许用户自己改键序 ——
// 而低级钩子拿到的是原始 VK 上升/下降流，可以在我们自己的状态机里判手势。
//
// 为什么必须自己起一个线程：低级钩子的回调在安装它的那个线程上执行，而系统要求该线程
// 一直在泵消息，否则超过 LowLevelHooksTimeout 就会被判超时（钩子被静默跳过甚至卸载）。
// 所以这里 LockOSThread + GetMessage 循环，退出用 PostThreadMessage(WM_QUIT)。
//
// 为什么不用 golang.org/x/sys/windows：v0.49.0 的 windows 包没有导出 SetWindowsHookEx /
// CallNextHookEx / GetMessage 这一族 API（grep 无命中），只能走 stdlib 的 LazyDLL。
var (
	user32Hook   = syscall.NewLazyDLL("user32.dll")
	kernel32Hook = syscall.NewLazyDLL("kernel32.dll")

	procSetWindowsHookEx    = user32Hook.NewProc("SetWindowsHookExW")
	procCallNextHookEx      = user32Hook.NewProc("CallNextHookEx")
	procUnhookWindowsHookEx = user32Hook.NewProc("UnhookWindowsHookEx")
	procGetMessageW         = user32Hook.NewProc("GetMessageW")
	procTranslateMessage    = user32Hook.NewProc("TranslateMessage")
	procDispatchMessageW    = user32Hook.NewProc("DispatchMessageW")
	procPostThreadMessageW  = user32Hook.NewProc("PostThreadMessageW")
	procGetAsyncKeyState    = user32Hook.NewProc("GetAsyncKeyState")
	procGetForegroundWindow = user32Hook.NewProc("GetForegroundWindow")
	procGetCurrentThreadId  = kernel32Hook.NewProc("GetCurrentThreadId")
)

const (
	whKeyboardLL = 13
	wmQuit       = 0x0012
	llkhfUp      = 0x80 // KBDLLHOOKSTRUCT.flags 的 LLKHF_UP
)

// kbdLLHookStruct 对应 KBDLLHOOKSTRUCT。
type kbdLLHookStruct struct {
	VkCode      uint32
	ScanCode    uint32
	Flags       uint32
	Time        uint32
	DwExtraInfo uintptr
}

// msg 对应 Win32 MSG。UINT/DWORD 在 x64 上后面都跟着 4 字节填充，用 uintptr 铺平正好
// 复现同一套偏移（48 字节），32 位下也正好是 28 字节；我们只关心消息循环有没有被唤醒，
// 字段本身一个都不读。
type msg struct {
	Hwnd    uintptr
	Message uintptr
	WParam  uintptr
	LParam  uintptr
	Time    uintptr
	PtX     int32
	PtY     int32
}

const kbdHookSupported = true

// kbdHook 是跑在专用线程上的一个低级键盘钩子实例。本进程只装一个。
type kbdHook struct {
	onEvent func(gkEvent) bool // 返回 true 表示吞掉这次按键

	// 以下字段只在钩子线程上读写（回调与消息循环同线程），以及 start 建立前的初始化。
	hh        uintptr
	swallowUp map[uint16]bool // 被吞掉 keydown 的键，keyup 也要一起吞

	ready    chan error
	done     chan struct{}
	tid      uint32 // 由 loop 写入；startKbdHook 收到 ready 之后才允许 stop 读
	stopOnce sync.Once
}

var (
	kbdHookMu      sync.Mutex
	kbdHookCurrent *kbdHook
)

// cbKbdHook 必须是包级单例：syscall.NewCallback 每次调用都会占用一个不可回收的回调槽位。
var cbKbdHook = syscall.NewCallback(kbdHookProc)

func currentKbdHook() *kbdHook {
	kbdHookMu.Lock()
	h := kbdHookCurrent
	kbdHookMu.Unlock()
	return h
}

// kbdHookProc 是钩子回调，跑在钩子线程上，受 LowLevelHooksTimeout 约束 —— 所以只做
// 「读 VK + 取修饰键快照 + 交给匹配器」，真正的动作由引擎派发到别的 goroutine。
//
// 参数用 unsafe.Pointer 而不是 uintptr：syscall.NewCallback 两种都支持，而 uintptr →
// Pointer 的还原会被 go vet 的 unsafeptr 判为「possible misuse」（它无法证明这个整数还
// 指向一个活对象），直接拿 Pointer 既能解引用又不需那次转换。
func kbdHookProc(nCode int, wParam, lParam unsafe.Pointer) uintptr {
	if nCode >= 0 && lParam != nil {
		if h := currentKbdHook(); h != nil {
			data := *(*kbdLLHookStruct)(lParam)
			ev := gkEvent{
				Vk:   uint16(data.VkCode),
				Down: data.Flags&llkhfUp == 0,
				Mods: currentModMask(),
				At:   time.Now(),
			}
			swallow := h.onEvent != nil && h.onEvent(ev)
			if !ev.Down && h.swallowUp[ev.Vk] {
				delete(h.swallowUp, ev.Vk)
				swallow = true // 按下被吞了，抬起也得吞，否则目标应用会收到半个键
			} else if ev.Down && swallow {
				h.swallowUp[ev.Vk] = true
			}
			if swallow {
				return 1
			}
		}
	}
	r, _, _ := procCallNextHookEx.Call(kbdHandleOfCurrent(), uintptr(wParam), uintptr(lParam))
	return r
}

func kbdHandleOfCurrent() uintptr {
	if h := currentKbdHook(); h != nil {
		return h.hh
	}
	return 0
}

// currentModMask 以系统快照为准取修饰键状态。用 GetAsyncKeyState 而不是自己数钩子事件，
// 是为了让「漏掉一次 keyup」（切换桌面、UAC 提权窗口抢占输入）能自愈：残留的「一直按着」
// 状态会让所有绑定都判不出精确修饰匹配，等于热键从此失灵。
func currentModMask() modMask {
	var m modMask
	if asyncDown(vkControl, vkLCtrl, vkRCtrl) {
		m |= modCtrl
	}
	if asyncDown(vkMenu, vkLAlt, vkRAlt) {
		m |= modAlt
	}
	if asyncDown(vkShift, vkLShift, vkRShift) {
		m |= modShift
	}
	if asyncDown(vkLWin, vkRWin) {
		m |= modMeta
	}
	return m
}

func asyncDown(vks ...uint16) bool {
	for _, vk := range vks {
		r, _, _ := procGetAsyncKeyState.Call(uintptr(vk))
		if int16(r) < 0 { // 最高位：当前是否按下
			return true
		}
	}
	return false
}

// startKbdHook 装钩子并等它就绪（失败要能立刻报给调用方，否则会静默失灵）。
// 就绪前 h.tid 写完、并通过 ready 的收发建立 happens-before，所以 stop 读它是安全的。
func startKbdHook(onEvent func(gkEvent) bool) (*kbdHook, error) {
	h := &kbdHook{
		onEvent:   onEvent,
		swallowUp: make(map[uint16]bool),
		ready:     make(chan error, 1),
		done:      make(chan struct{}),
	}
	go h.loop()
	// 等安装结果。没有这道超时的话，钩子线程一旦卡在 API 上就会让启动流程无限等下去。
	select {
	case err := <-h.ready:
		if err != nil {
			return nil, err
		}
		return h, nil
	case <-time.After(3 * time.Second):
		return nil, fmt.Errorf("安装低级键盘钩子超时（3 秒），钩子线程未就绪")
	}
}

func (h *kbdHook) loop() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	tid, _, _ := procGetCurrentThreadId.Call()
	h.tid = uint32(tid)
	// hMod 传 0：WH_KEYBOARD_LL 是全局钩子，文档允许 NULL；dwThreadId 传 0 表示所有线程。
	hh, _, callErr := procSetWindowsHookEx.Call(whKeyboardLL, cbKbdHook, 0, 0)
	if hh == 0 {
		h.ready <- fmt.Errorf("SetWindowsHookExW(WH_KEYBOARD_LL) 失败: %v（本进程权限低于前台应用时会被 UIPI 拒绝，例如以管理员身份运行的窗口）", callErr)
		return
	}
	h.hh = hh
	kbdHookMu.Lock()
	kbdHookCurrent = h
	kbdHookMu.Unlock()
	h.ready <- nil

	// 消息泵。低级钩子必须有它，否则回调一次都不会被调用。
	var m msg
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if b := uint32(r); b == 0 || b == 0xFFFFFFFF { // 0 = WM_QUIT，-1 = 出错
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}

	procUnhookWindowsHookEx.Call(h.hh)
	kbdHookMu.Lock()
	if kbdHookCurrent == h {
		kbdHookCurrent = nil
	}
	kbdHookMu.Unlock()
	close(h.done)
}

// stop 摘钩子。向钩子线程投 WM_QUIT 让消息循环返回，卸载必须发生在安装它的那个线程上。
func (h *kbdHook) stop() {
	h.stopOnce.Do(func() {
		if h.tid != 0 {
			procPostThreadMessageW.Call(uintptr(h.tid), wmQuit, 0, 0)
		}
		select {
		case <-h.done:
		case <-time.After(2 * time.Second):
			log.Printf("低级键盘钩子线程 2 秒内没退出，键盘链可能仍被占用")
		}
	})
}
