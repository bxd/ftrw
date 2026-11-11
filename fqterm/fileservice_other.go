//go:build !windows

package main

import (
	"os"
	"strings"
)

// 文件管理器非 Windows 侧的专属件。驱动器枚举在这边没有对等形状：给一个根目录
// 桩，够前端把浏览跑起来；隐藏判定只认点名约定；无标准回收站，删到回收站退化为永久删除。

func listDrives() []DriveInfo {
	return []DriveInfo{{Root: "/", Label: "/", Type: 3, Ready: true}}
}

func isHiddenFile(fullPath, name string) bool {
	return strings.HasPrefix(name, ".")
}

// recyclePath 非 Windows 平台无统一回收站 API，退化为直接删除。
func recyclePath(p string) error {
	return os.RemoveAll(p)
}
