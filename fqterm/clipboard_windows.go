//go:build windows

package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/wailsapp/wails/v3/pkg/application"
)

var (
	modUser32                = syscall.NewLazyDLL("user32.dll")
	procOpenClipboard        = modUser32.NewProc("OpenClipboard")
	procCloseClipboard       = modUser32.NewProc("CloseClipboard")
	procEmptyClipboard       = modUser32.NewProc("EmptyClipboard")
	procGetClipboardData     = modUser32.NewProc("GetClipboardData")
	procSetClipboardData     = modUser32.NewProc("SetClipboardData")
	procIsClipboardAvailable = modUser32.NewProc("IsClipboardFormatAvailable")
	procRegisterClipboardFmt = modUser32.NewProc("RegisterClipboardFormatW")
	procGlobalSize           = modKernel32.NewProc("GlobalSize")
	procMultiByteToWideChar  = modKernel32.NewProc("MultiByteToWideChar")
)

const (
	cfDIB       = 8
	cfDIBV5     = 17
	cpACP       = 0
	maxClipData = 64 << 20
)

// 剪贴板 API 必须串行访问；OpenClipboard 本身也会拒绝其它占用者。这里过桥到
// Wails 主线程，与出站 OLE 拖拽保持一致，避免服务调用 goroutine 的 COM 生命周期差异。
func copyFilesToClipboard(paths []string) error {
	clean, err := normalizeClipboardFilePaths(paths)
	if err != nil {
		return err
	}
	hdrop := buildHDrop(clean)
	application.InvokeSync(func() {
		err = writeClipboardHDrop(hdrop)
	})
	runtime.KeepAlive(hdrop)
	return err
}

func readEditorClipboard() (EditorClipboardData, error) {
	var out EditorClipboardData
	pngFormat, _, _ := procRegisterClipboardFmt.Call(uintptr(unsafe.Pointer(clipboardUTF16("PNG"))))
	if !clipboardAvailable(cfHDrop) &&
		!clipboardAvailable(cfDIB) && !clipboardAvailable(cfDIBV5) &&
		(pngFormat == 0 || !clipboardAvailable(pngFormat)) {
		return out, nil
	}

	var pngData []byte
	var invokeErr error
	application.InvokeSync(func() {
		if !openClipboard() {
			invokeErr = errors.New("系统剪贴板正被其它程序占用，请稍后重试")
			return
		}
		defer procCloseClipboard.Call()

		if clipboardAvailable(cfHDrop) {
			raw, err := clipboardGlobalBytes(cfHDrop)
			if err != nil {
				invokeErr = err
				return
			}
			out.Files = parseHDrop(raw)
			return
		}
		if pngFormat != 0 && clipboardAvailable(pngFormat) {
			data, err := clipboardGlobalBytes(pngFormat)
			if err != nil {
				invokeErr = err
				return
			}
			if !validPNG(data) {
				invokeErr = errors.New("剪贴板 PNG 数据无效")
				return
			}
			pngData = data
			return
		}
		format := uintptr(cfDIB)
		if clipboardAvailable(cfDIBV5) {
			format = cfDIBV5
		}
		if !clipboardAvailable(format) {
			return
		}
		data, err := clipboardGlobalBytes(format)
		if err != nil {
			invokeErr = err
			return
		}
		pngData, err = dibToPNG(data)
		if err != nil {
			invokeErr = fmt.Errorf("剪贴板图片转 PNG 失败：%w", err)
		}
	})
	if invokeErr != nil {
		return EditorClipboardData{}, invokeErr
	}
	if len(pngData) > 0 {
		out.ImageBase64 = base64.StdEncoding.EncodeToString(pngData)
	}
	return out, nil
}

func normalizeClipboardFilePaths(paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, errors.New("没有可复制的文件")
	}
	if len(paths) > 256 {
		return nil, errors.New("一次最多复制 256 个文件")
	}
	out := make([]string, 0, len(paths))
	seen := make(map[string]bool, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		if strings.Contains(path, "::") {
			return nil, errors.New("压缩包内的虚拟路径不能直接作为附件，请先解压")
		}
		if !filepath.IsAbs(path) {
			return nil, fmt.Errorf("只能复制绝对路径文件：%s", path)
		}
		path = filepath.Clean(path)
		key := strings.ToLower(path)
		if seen[key] {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("文件不存在或不可访问：%s", path)
		}
		if info.IsDir() {
			return nil, fmt.Errorf("只能复制文件，不能复制目录：%s", path)
		}
		seen[key] = true
		out = append(out, path)
	}
	if len(out) == 0 {
		return nil, errors.New("没有可复制的文件")
	}
	return out, nil
}

func clipboardUTF16(value string) *uint16 {
	p, _ := syscall.UTF16PtrFromString(value)
	return p
}

func clipboardAvailable(format uintptr) bool {
	r, _, _ := procIsClipboardAvailable.Call(format)
	return r != 0
}

func openClipboard() bool {
	for i := 0; i < 8; i++ {
		if r, _, _ := procOpenClipboard.Call(0); r != 0 {
			return true
		}
		time.Sleep(15 * time.Millisecond)
	}
	return false
}

func writeClipboardHDrop(data []byte) error {
	if !openClipboard() {
		return errors.New("系统剪贴板正被其它程序占用，请稍后重试")
	}
	defer procCloseClipboard.Call()
	h, _, _ := procGlobalAlloc.Call(gmemMoveable|gmemZeroInit, uintptr(len(data)))
	if h == 0 {
		return errors.New("分配剪贴板文件列表失败")
	}
	locked, _, _ := procGlobalLock.Call(h)
	if locked == 0 {
		procGlobalFree.Call(h)
		return errors.New("锁定剪贴板文件列表失败")
	}
	p := *(*unsafe.Pointer)(unsafe.Pointer(&locked)) // 与出站 OLE 拖拽相同的 vet 安全重解释
	copy(unsafe.Slice((*byte)(p), len(data)), data)
	procGlobalUnlock.Call(h)
	procEmptyClipboard.Call()
	if r, _, callErr := procSetClipboardData.Call(cfHDrop, h); r == 0 {
		procGlobalFree.Call(h)
		if callErr != syscall.Errno(0) {
			return fmt.Errorf("写入系统剪贴板失败：%w", callErr)
		}
		return errors.New("写入系统剪贴板失败")
	}
	return nil
}

func clipboardGlobalBytes(format uintptr) ([]byte, error) {
	h, _, _ := procGetClipboardData.Call(format)
	if h == 0 {
		return nil, errors.New("读取系统剪贴板数据失败")
	}
	size, _, _ := procGlobalSize.Call(h)
	if size == 0 || size > maxClipData {
		return nil, errors.New("系统剪贴板数据为空或过大")
	}
	locked, _, _ := procGlobalLock.Call(h)
	if locked == 0 {
		return nil, errors.New("锁定系统剪贴板数据失败")
	}
	defer procGlobalUnlock.Call(h)
	p := *(*unsafe.Pointer)(unsafe.Pointer(&locked))
	return append([]byte(nil), unsafe.Slice((*byte)(p), int(size))...), nil
}

func parseHDrop(raw []byte) []string {
	if len(raw) < 20 {
		return nil
	}
	offset := int(uint32(raw[0]) | uint32(raw[1])<<8 | uint32(raw[2])<<16 | uint32(raw[3])<<24)
	wide := uint32(raw[16])|uint32(raw[17])<<8|uint32(raw[18])<<16|uint32(raw[19])<<24 != 0
	if offset < 20 || offset >= len(raw) {
		return nil
	}
	out := []string{}
	list := raw[offset:]
	for len(list) > 0 && len(out) < 256 {
		step := 1
		if wide {
			step = 2
			if len(list) < step || (list[0] == 0 && list[1] == 0) {
				break
			}
		} else if list[0] == 0 {
			break
		}

		// CF_HDROP 是“每路径一个 NUL，全部路径后再多一个 NUL”，不是用
		// 双 NUL 分隔每个路径。逐串读取才能正确处理文件管理器复制的多文件。
		terminator := -1
		if wide {
			for i := 0; i+1 < len(list); i += 2 {
				if list[i] == 0 && list[i+1] == 0 {
					terminator = i
					break
				}
			}
		} else {
			for i, value := range list {
				if value == 0 {
					terminator = i
					break
				}
			}
		}
		if terminator <= 0 {
			break
		}
		path := ""
		if wide {
			units := make([]uint16, terminator/2)
			for i := range units {
				units[i] = uint16(list[i*2]) | uint16(list[i*2+1])<<8
			}
			path = syscall.UTF16ToString(units)
		} else {
			path = ansiToUTF16(list[:terminator])
		}
		if path != "" {
			out = append(out, path)
		}
		list = list[terminator+step:]
	}
	return out
}

func ansiToUTF16(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	n, _, _ := procMultiByteToWideChar.Call(cpACP, 0, uintptr(unsafe.Pointer(&raw[0])), uintptr(len(raw)), 0, 0)
	if n == 0 {
		return string(raw)
	}
	buf := make([]uint16, n+1)
	procMultiByteToWideChar.Call(cpACP, 0, uintptr(unsafe.Pointer(&raw[0])), uintptr(len(raw)), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return syscall.UTF16ToString(buf)
}
