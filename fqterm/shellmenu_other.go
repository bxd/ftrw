//go:build !windows

package main

// shellmenu_other.go：非 Windows 平台的原生右键菜单桩。系统 Explorer 菜单是 Windows 专属，
// 其它平台返回一句提示；前端仍保留「扩展 ▾」按钮作为插件动作入口。与 hideconsole_other.go
// 的同名对称约定一致。
func showShellMenu(_ uintptr, _ string, _ []string, _ []pluginMenuItem) shellMenuOutcome {
	return shellMenuOutcome{err: "原生右键菜单仅支持 Windows"}
}
