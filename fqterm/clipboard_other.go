//go:build !windows

package main

import "errors"

// 非 Windows 没有统一的 CF_HDROP / DIB 剪贴板表示；保留空读取结果，让编辑器
// 继续使用浏览器默认文字粘贴和 HTML5 文件拖放，不把平台差异变成前端分支。
func copyFilesToClipboard(paths []string) error {
	return errors.New("当前系统不支持把文件列表写入系统剪贴板")
}

func readEditorClipboard() (EditorClipboardData, error) {
	return EditorClipboardData{Files: []string{}}, nil
}
