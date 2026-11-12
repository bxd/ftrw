//go:build windows

package main

// advcopy_cli_windows.go：控制台接线。
//
// 主通道是 shbin 里的控制台子系统副本 ftrw-cli.exe（见 shellbuiltins.go 的
// makeConsoleCopy）：shell 原生等待、Ctrl+C 原生送达、ConPTY 原生转发，什么都不用做。
// 这里的 AttachConsole 只兜底「直接跑 GUI 本体 ftrw.exe ac」的场景。
//
// 历史教训：曾改走 CreateFileW("CONOUT$") 直写，结果 ConPTY 下输出完全不可见、
// Ctrl+C 也收不到 —— 借来的控制台句柄必须开 VT（否则 \x1b[3A/\x1b[K 被吞，
// 原地重绘退化成新增行），且不要绕开标准句柄另开缓冲。
//
// 为什么不用 golang.org/x/sys/windows：v0.49.0 没导出 AttachConsole，
// 与 kbdhook_windows.go 的先例一致，自挂 kernel32。

import (
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32Adv        = syscall.NewLazyDLL("kernel32.dll")
	procAttachConsole  = kernel32Adv.NewProc("AttachConsole")
	procGetStdHandle   = kernel32Adv.NewProc("GetStdHandle")
	procGetConsoleMode = kernel32Adv.NewProc("GetConsoleMode")
	procSetConsoleMode = kernel32Adv.NewProc("SetConsoleMode")
	procGetFileType    = kernel32Adv.NewProc("GetFileType")
	procGetCSBI        = kernel32Adv.NewProc("GetConsoleScreenBufferInfo")
)

// advCSBI 是 CONSOLE_SCREEN_BUFFER_INFO 的最小布局，只取 dwSize.X 当终端列数。
type advCoord struct{ X, Y int16 }
type advSmallRect struct{ Left, Top, Right, Bottom int16 }
type advCSBI struct {
	Size   advCoord
	Cursor advCoord
	Attrs  uint16
	Window advSmallRect
	MaxWin advCoord
}

const (
	attachParentProcess = ^uint32(0) // ATTACH_PARENT_PROCESS = (DWORD)-1
	enableVTProcessing  = 0x0004     // ENABLE_VIRTUAL_TERMINAL_PROCESSING
)

// advKeepStdout 钉住被换下的旧 os.Stdout。os.File 带关闭句柄的 finalizer，
// 旧对象一旦失去引用被 GC，会把新旧共用的那个控制台句柄关掉——表现就是
// 首帧写成功后所有写入报 The handle is invalid（句柄值不变但已失效）。
var advKeepStdout *os.File

func attachParentConsole() {
	// 控制台副本本来就有控制台，这里会失败，无副作用；GUI 本体直跑时借父进程的。
	procAttachConsole.Call(uintptr(attachParentProcess))
	h := stdHandle(^uintptr(10)) // STD_OUTPUT_HANDLE = -11
	var mode uint32
	if ok, _, _ := procGetConsoleMode.Call(h, uintptr(unsafe.Pointer(&mode))); ok != 0 {
		procSetConsoleMode.Call(h, uintptr(mode|enableVTProcessing))
		var csbi advCSBI
		if ok2, _, _ := procGetCSBI.Call(h, uintptr(unsafe.Pointer(&csbi))); ok2 != 0 && csbi.Size.X > 0 {
			advWidth = int(csbi.Size.X)
		}
		// 启动时标准句柄就有效（控制台副本/ConPTY 继承）则原对象直接用，不重包；
		// 只有 GUI 本体 AttachConsole 借来的新句柄才需要换，且旧对象必须钉住。
		if os.Stdout.Fd() != h {
			advKeepStdout = os.Stdout
			os.Stdout = os.NewFile(h, "stdout")
		}
		return
	}
	// 不是控制台：管道（pwsh 重定向/WSL）照常写，ANSI 由对端终端解释；
	// 落盘文件或无效句柄（双击 GUI 本体）没有可画的地方，落 plain。
	if os.Getenv("WSL_DISTRO_NAME") == "" && !isPipeHandle(h) {
		advPlain = true
	}
}

// isPipeHandle 判句柄是不是管道：GetFileType 返回 FILE_TYPE_PIPE(3)。
// 控制台句柄已在上面的 GetConsoleMode 分支处理；磁盘文件不算，落 plain。
func isPipeHandle(h uintptr) bool {
	if h == 0 || h == ^uintptr(0) {
		return false
	}
	t, _, _ := procGetFileType.Call(h)
	return t == 3
}

func stdHandle(which uintptr) uintptr {
	h, _, _ := procGetStdHandle.Call(which)
	return h
}
