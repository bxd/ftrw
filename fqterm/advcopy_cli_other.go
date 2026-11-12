//go:build !windows

package main

// 非 Windows 没有「GUI 子系统借控制台」的问题，标准流本来就接在终端上。
func attachParentConsole() {}
