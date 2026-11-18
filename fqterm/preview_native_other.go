//go:build !windows

package main

// foregroundHWND 在非 Windows 上没有实现：那里根本没装低级键盘钩子（见 kbdHookSupported），
// ESC 完全由预览页自己的 keydown 处理。返回 0 让监听永远对不上号，
// 顺带使 armEscWatch 变成空操作。
func foregroundHWND() uintptr { return 0 }

// 非 Windows 没有分层窗口动画：预览窗直接显示/关闭，功能不受影响。
func previewSetLayered(hwnd uintptr, on bool)  {}
func previewSetAlpha(hwnd uintptr, alpha byte) {}
func previewAlpha(hwnd uintptr) byte           { return 255 }
func setWindowRoundCorners(hwnd uintptr)       {}
func previewSetOwner(child, parent uintptr)    {}
func previewFocusMain(hwnd uintptr)            {}
