//go:build windows

package main

import (
	"errors"
	"os"
	"syscall"
	"unsafe"
)

// 版本号的唯一事实来源是 exe 自己的版本资源：build/windows/info.json 经
// wails3 generate syso 编进可执行文件，这里读回来，不再另立一份会漂移的常量。

var (
	modVersion           = syscall.NewLazyDLL("version.dll")
	procGetFileVersionSz = modVersion.NewProc("GetFileVersionInfoSizeW")
	procGetFileVersion   = modVersion.NewProc("GetFileVersionInfoW")
	procVerQueryValue    = modVersion.NewProc("VerQueryValueW")
)

// exeFileVersion 读本进程的 FileVersion 字符串（如 "0.5.9"）。
func exeFileVersion() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	p, err := syscall.UTF16PtrFromString(exe)
	if err != nil {
		return "", err
	}
	sz, _, _ := procGetFileVersionSz.Call(uintptr(unsafe.Pointer(p)), 0)
	if sz == 0 {
		return "", errors.New("exe 没有版本资源")
	}
	buf := make([]byte, sz)
	r, _, e := procGetFileVersion.Call(uintptr(unsafe.Pointer(p)), 0, sz, uintptr(unsafe.Pointer(&buf[0])))
	if r == 0 {
		return "", e
	}
	// 构建固定喂 0409（info.json 的语言块），翻译表路径即 040904B0。
	q, err := syscall.UTF16PtrFromString(`\StringFileInfo\040904B0\FileVersion`)
	if err != nil {
		return "", err
	}
	var vp unsafe.Pointer
	var vl uint32
	r, _, e = procVerQueryValue.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(q)), uintptr(unsafe.Pointer(&vp)), uintptr(unsafe.Pointer(&vl)))
	if r == 0 || vl == 0 {
		return "", e
	}
	us := unsafe.Slice((*uint16)(vp), vl)
	n := 0
	for n < len(us) && us[n] != 0 {
		n++
	}
	return syscall.UTF16ToString(us[:n]), nil
}
