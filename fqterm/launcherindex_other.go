//go:build !windows

package main

import "errors"

// 启动器的索引与启动只有 Windows 实现：.lnk 扫描、WScript.Shell 解析、
// ShellExecuteW 都是 Windows 专属形状。其它平台要做的是另一套东西
// （.desktop 数据库 / LaunchServices），形状在这里先钉死，不让调用方分支。

func scanAppShortcuts(prefs Preferences) ([]LauncherEntry, error) {
	return nil, errors.New("本平台还没有启动器索引实现")
}

func launcherShellExecute(path string, verb string) error {
	return errors.New("本平台还没有启动器实现")
}

func launcherShellExecuteElevated(path string) error {
	return errors.New("本平台还没有启动器实现")
}
