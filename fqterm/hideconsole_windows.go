//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// hideConsoleWindow 压制控制台子进程新建的窗口。本程序是 GUI 子系统（-H windowsgui）启动、
// 自身没有控制台，Windows 会给每个控制台子进程另开一个黑框。Python 探测、venv/pip 以及插件
// worker 都会 spawn 控制台进程，不压住启动时就会闪过 CMD 窗口。
func hideConsoleWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
}
