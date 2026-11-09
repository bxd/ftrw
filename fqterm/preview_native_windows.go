//go:build windows

package main

// foregroundHWND 取当前前台窗口的 HWND。
//
// 只能在键盘钩子线程上调：它是一次直白的 user32 调用，不经过 Wails，因此不会去等
// 主线程 —— 钩子回调里只允许这种档次的操作。
func foregroundHWND() uintptr {
	r, _, _ := procGetForegroundWindow.Call()
	return uintptr(r)
}
