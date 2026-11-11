//go:build !windows

package main

// forceForeground 在非 Windows 上是空操作：前台锁是 Windows 特有的焦点窃取防护，
// 其它平台没有对应的 Win32 API；且跨应用热键本就依赖低级键盘钩子（见
// kbdHookSupported），非 Windows 上根本不会走到呼出这条路径。
func forceForeground(hwnd uintptr) {}
