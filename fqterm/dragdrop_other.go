//go:build !windows

package main

import "fmt"

// 非 Windows 无 OLE 拖放对等件，报不支持（浏览/拷出等其余功能不受影响）。
func dragPathsNative(paths []string) error {
	return fmt.Errorf("拖拽到外部应用仅支持 Windows")
}
