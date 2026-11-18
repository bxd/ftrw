//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

// foregroundHWND 取当前前台窗口的 HWND。
//
// 只能在键盘钩子线程上调：它是一次直白的 user32 调用，不经过 Wails，因此不会去等
// 主线程 —— 钩子回调里只允许这种档次的操作。
func foregroundHWND() uintptr {
	r, _, _ := procGetForegroundWindow.Call()
	return uintptr(r)
}

/* --------------------------------------------------------------------------- */
/* 预览窗淡入淡出：WS_EX_LAYERED + SetLayeredWindowAttributes                   */
/* --------------------------------------------------------------------------- */

// 复用 kbdhook_windows.go 的 user32Hook。GWL_EXSTYLE 用 LongW 一族就够：
// 扩展风格整个落在 32 位里，x64 下 GetWindowLongW 同样合法，还避开
// GetWindowLongPtrW 在 32 位系统上不存在的导出问题。
var (
	procGetWindowLongW             = user32Hook.NewProc("GetWindowLongW")
	procSetWindowLongW             = user32Hook.NewProc("SetWindowLongW")
	procSetWindowLongPtrW          = user32Hook.NewProc("SetWindowLongPtrW")
	procSetLayeredWindowAttributes = user32Hook.NewProc("SetLayeredWindowAttributes")
	procGetLayeredWindowAttributes = user32Hook.NewProc("GetLayeredWindowAttributes")
	procIsIconic                   = user32Hook.NewProc("IsIconic")

	dwmapiDLL                 = syscall.NewLazyDLL("dwmapi.dll")
	procDwmSetWindowAttribute = dwmapiDLL.NewProc("DwmSetWindowAttribute")
)

const (
	gwlExStyle     = -20
	gwlpHwndParent = -8
	wsExLayered    = 0x00080000
	lwaAlpha       = 0x02

	// DWMWA_WINDOW_CORNER_PREFERENCE / DWMWCP_ROUND：Win11 圆角。
	// 关掉 Wails 的 frameless 装饰后默认变方角，用这个一次性补回来。
	dwmwaCornerPreference = 33
	dwmwcpRound           = 2
)

// previewSetLayered 开关分层扩展风格。淡入淡出期间开，结束后摘掉：
// 分层窗走 DWM 重定向合成，长开着白耗性能，也没必要。
func previewSetLayered(hwnd uintptr, on bool) {
	if hwnd == 0 {
		return
	}
	// GWL_EXSTYLE 是负数索引：先落进变量再转 uintptr，避开常量转换的溢出检查。
	idx := int(gwlExStyle)
	r, _, _ := procGetWindowLongW.Call(hwnd, uintptr(idx))
	style := int32(r)
	if on {
		style |= wsExLayered
	} else {
		style &^= wsExLayered
	}
	procSetWindowLongW.Call(hwnd, uintptr(idx), uintptr(style))
}

// previewSetAlpha 设整窗不透明度（0 全透 → 255 不透）。Win8+ 的 LWA_ALPHA
// 连同子窗口（WebView2 的渲染 HWND）一起合成，网页模式的外部文档同样能淡。
func previewSetAlpha(hwnd uintptr, alpha byte) {
	if hwnd == 0 {
		return
	}
	procSetLayeredWindowAttributes.Call(hwnd, 0, uintptr(alpha), lwaAlpha)
}

// previewAlpha 读当前不透明度；没开分层或读失败一律当 255（完全不透明）。
func previewAlpha(hwnd uintptr) byte {
	if hwnd == 0 {
		return 255
	}
	var alpha byte
	r, _, _ := procGetLayeredWindowAttributes.Call(hwnd, 0, uintptr(unsafe.Pointer(&alpha)), 0)
	if r == 0 {
		return 255
	}
	return alpha
}

// previewSetOwner 只挂 owner 关系（GWLP_HWNDPARENT），不做 AttachModal 的
// EnableWindow(main,false)：禁用/恢复主窗会让关窗那一刻多一轮启用+激活切换，
// 主窗因此闪一下。仅 owner 关系已保证预览压在主窗之上、随主窗最小化。
// HWND 在 x64 下是 64 位，必须走 PtrW；32 位系统没有该导出，回退 LongW。
func previewSetOwner(child, parent uintptr) {
	if child == 0 || parent == 0 {
		return
	}
	idx := int(gwlpHwndParent)
	if procSetWindowLongPtrW.Find() == nil {
		procSetWindowLongPtrW.Call(child, uintptr(idx), parent)
	} else {
		procSetWindowLongW.Call(child, uintptr(idx), parent)
	}
}

// previewFocusMain 预览正式关闭前把前台交还主窗：抢在销毁之前完成激活，
// 销毁子窗时系统就不会再「补」一次 owner 激活，主窗看不到激活切换。
// 主窗最小化时不抢，免得关预览顺手把主窗还原出来。
func previewFocusMain(hwnd uintptr) {
	if hwnd == 0 {
		return
	}
	if r, _, _ := procIsIconic.Call(hwnd); r != 0 {
		return
	}
	procSetForegroundWindow.Call(hwnd)
}

// setWindowRoundCorners 给无边框窗口补 Win11 圆角。Win10 及以下这个属性不存在，
// 调用失败静默忽略 —— 那里本来就是方角，不算回归。
func setWindowRoundCorners(hwnd uintptr) {
	if hwnd == 0 {
		return
	}
	pref := int32(dwmwcpRound)
	procDwmSetWindowAttribute.Call(hwnd, dwmwaCornerPreference, uintptr(unsafe.Pointer(&pref)), unsafe.Sizeof(pref))
}
