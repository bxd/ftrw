//go:build windows

package main

import "runtime"

// 复用 kbdhook_windows.go 里的 user32.dll 句柄，这里只补几个前台切换 API。
// 它们都是一次直白的 user32 调用，不经 Wails、不等主线程，可以在任意 LockOSThread
// 的线程上安全调用。procGetForegroundWindow / procGetCurrentThreadId 已由钩子文件声明。
var (
	procSetForegroundWindow      = user32Hook.NewProc("SetForegroundWindow")
	procBringWindowToTop         = user32Hook.NewProc("BringWindowToTop")
	procGetWindowThreadProcessId = user32Hook.NewProc("GetWindowThreadProcessId")
	procAttachThreadInput        = user32Hook.NewProc("AttachThreadInput")
	procShowWindow               = user32Hook.NewProc("ShowWindow")
)

const swRestore = 9 // SW_RESTORE：窗口若被最小化，先还原才能进前台

// forceForeground 把 hwnd 强行提到前台，绕过 Windows 的「前台锁」。
//
// 为什么要自己来：全局热键（Alt+`）呼出 quake 时，FTRW 仍是后台进程，Wails 的
// Focus() 内部那句 SetForegroundWindow 会被系统的「焦点窃取防护」拦下——结果是
// 窗口显示出来了（SW_SHOW 不受限），键盘焦点却留在原来的前台应用，正是「呼出却没
// 拿到焦点」的现象。
//
// 经典解法：用 AttachThreadInput 把当前线程临时挂到前台窗口所属线程的输入队列上，
// 系统随即视二者为同一输入组，此时 SetForegroundWindow 被放行；用完再摘回去。
// attach / 调用 / detach 必须落在同一个 OS 线程，所以整段 LockOSThread 锁死。
func forceForeground(hwnd uintptr) {
	if hwnd == 0 {
		return
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	fg, _, _ := procGetForegroundWindow.Call()
	if fg == hwnd {
		return // 已经在前台，别多做动作触发无谓的失焦/聚焦抖动
	}
	curTid, _, _ := procGetCurrentThreadId.Call()
	var fgTid uintptr
	if fg != 0 {
		fgTid, _, _ = procGetWindowThreadProcessId.Call(fg, 0)
	}
	attached := false
	if fgTid != 0 && fgTid != curTid {
		r, _, _ := procAttachThreadInput.Call(curTid, fgTid, 1)
		attached = r != 0
	}

	procShowWindow.Call(hwnd, swRestore)
	procBringWindowToTop.Call(hwnd)
	procSetForegroundWindow.Call(hwnd)

	if attached {
		procAttachThreadInput.Call(curTid, fgTid, 0)
	}
}
