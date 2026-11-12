//go:build !windows

package main

import "errors"

// exeFileVersion 在非 Windows 平台没有版本资源可读（syso 只编进 Windows 构建），
// 回退报错，由调用方落到开发版标识。
func exeFileVersion() (string, error) {
	return "", errors.New("版本资源仅 Windows 构建可用")
}
