//go:build !windows

package main

import "os/exec"

// hideConsoleWindow 在非 Windows 上是空操作：POSIX 下 GUI 程序 spawn 子进程不会闪控制台窗口。
func hideConsoleWindow(cmd *exec.Cmd) {}
