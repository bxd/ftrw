//go:build !windows

package main

// foregroundHWND 在非 Windows 上没有实现：那里根本没装低级键盘钩子（见 kbdHookSupported），
// ESC 完全由预览页自己的 keydown 处理。返回 0 让监听永远对不上号，
// 顺带使 armEscWatch 变成空操作。
func foregroundHWND() uintptr { return 0 }
