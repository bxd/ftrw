//go:build windows

package main

import (
	"fmt"
	"syscall"
	"unsafe"
)

// 文件管理器的 Windows 侧专属件：逻辑驱动器枚举、隐藏属性判定、以及删到回收站。
// 驱动器那部分是 kernel32 的三个小口子；回收站走 shell32 的 SHFileOperationW。

var (
	modKernel32          = syscall.NewLazyDLL("kernel32.dll")
	procGetLogicalDrives = modKernel32.NewProc("GetLogicalDrives")
	procGetDriveTypeW    = modKernel32.NewProc("GetDriveTypeW")
	procGetVolumeInfoW   = modKernel32.NewProc("GetVolumeInformationW")
	procGetFileAttrW     = modKernel32.NewProc("GetFileAttributesW")

	// modShell32 已在 launcherindex_windows.go 声明（同包同构建标签），这里只挂 SHFileOperationW。
	procSHFileOperationW = modShell32.NewProc("SHFileOperationW")
)

// listDrives 用 GetLogicalDrives 的位掩码定有哪些盘符，再逐个 GetDriveTypeW 分类、
// GetVolumeInformationW 取卷标与就绪态。盘全空（异常机器）时返回 nil，前端会退回
// 只显示主目录。
func listDrives() []DriveInfo {
	mask, _, _ := procGetLogicalDrives.Call()
	var out []DriveInfo
	for i := 0; i < 26; i++ {
		if mask&(1<<uint(i)) == 0 {
			continue
		}
		root := string(rune('A'+i)) + `:\`
		rootPtr, err := syscall.UTF16PtrFromString(root)
		if err != nil {
			continue
		}
		typ, _, _ := procGetDriveTypeW.Call(uintptr(unsafe.Pointer(rootPtr)))

		var labelBuf [261]uint16
		var fsName [261]uint16
		var serial, maxComp, fsFlags uint32
		rv, _, _ := procGetVolumeInfoW.Call(
			uintptr(unsafe.Pointer(rootPtr)),
			uintptr(unsafe.Pointer(&labelBuf[0])),
			uintptr(len(labelBuf)),
			uintptr(unsafe.Pointer(&serial)),
			uintptr(unsafe.Pointer(&maxComp)),
			uintptr(unsafe.Pointer(&fsFlags)),
			uintptr(unsafe.Pointer(&fsName[0])),
			uintptr(len(fsName)),
		)
		out = append(out, DriveInfo{
			Root:  root,
			Label: syscall.UTF16ToString(labelBuf[:]),
			Type:  int(typ),
			Ready: rv != 0,
		})
	}
	return out
}

// isHiddenFile 先认点名（.git 一类点开头），再查 FILE_ATTRIBUTE_HIDDEN。
// 两条都算隐藏，交给前端按开关过滤。
func isHiddenFile(fullPath, name string) bool {
	if len(name) > 0 && name[0] == '.' {
		return true
	}
	p, err := syscall.UTF16PtrFromString(fullPath)
	if err != nil {
		return false
	}
	attrs, _, _ := procGetFileAttrW.Call(uintptr(unsafe.Pointer(p)))
	if attrs == uintptr(syscall.INVALID_FILE_ATTRIBUTES) {
		return false
	}
	return attrs&syscall.FILE_ATTRIBUTE_HIDDEN != 0
}

// SHFILEOPSTRUCTW 的字段顺序/对齐按 MSVC x64 布局；Go 自动插入的填充刚好一致
// （hwnd 8 / wFunc 4 + 对齐 / pFrom 8 / pTo 8 / fFlags 2 + 对齐 / abandoned 4 /
// hNameMappings 8 / title 8）。
type shellFileOpW struct {
	hwnd          uintptr
	wFunc         uint32
	pFrom         *uint16
	pTo           *uint16
	fFlags        uint16
	abandoned     uint32
	hNameMappings uintptr
	title         *uint16
}

const (
	foDelete     = 3
	fofSilent    = 0x0004
	fofNoConfirm = 0x0010
	fofAllowUndo = 0x0040
	fofNoErrorUI = 0x0400
)

// recyclePath 用 SHFileOperationW + FOF_ALLOWUNDO 把一条路径送进回收站。
// pFrom 要求「路径\0\0」双零结尾；前端已弹过确认框，故加 NOCONFIRMATION/NOERRORUI。
func recyclePath(p string) error {
	from, err := syscall.UTF16FromString(p) // 已含结尾 \0
	if err != nil {
		return err
	}
	buf := append(from, 0) // 再补一个 \0 凑成双零结尾
	op := shellFileOpW{
		wFunc:  foDelete,
		pFrom:  &buf[0],
		fFlags: fofAllowUndo | fofNoConfirm | fofSilent | fofNoErrorUI,
	}
	rv, _, _ := procSHFileOperationW.Call(uintptr(unsafe.Pointer(&op)))
	if rv != 0 {
		return fmt.Errorf("SHFileOperationW 返回 %d", rv)
	}
	return nil
}
