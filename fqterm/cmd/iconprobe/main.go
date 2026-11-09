//go:build windows

// 一次性验证工具：用与 Wails 托盘完全相同的 API 链路解析自写的 .ico ——
// w32.CreateSmallHIconFromImage → selectICOImage(按 SM_CXSMICON 挑帧) →
// CreateIconFromResourceEx，然后用 w32.SaveHIconAsPNG（GetIconInfo + GetDIBits）
// 把解码后的图标位图直接读回来存 PNG 供肉眼核对。
//
// 之所以要探针而不是直接跑应用：托盘图标只在通知区域显示 16px，一旦看不到东西根本
// 分不清是「ICO 没被解析」还是「解析了但画糊了」。这里把中间产物显式落地。
//
//	go run ./cmd/iconprobe -in build/tray/tray-lightbg.ico -out %TEMP%\probe.png
package main

import (
	"flag"
	"fmt"
	"image"
	"os"
	"syscall"
	"unsafe"

	"github.com/wailsapp/wails/v3/pkg/w32"
)

var (
	user32         = syscall.NewLazyDLL("user32.dll")
	gdi32          = syscall.NewLazyDLL("gdi32.dll")
	procGetIconInf = user32.NewProc("GetIconInfo")
	procGetObjectW = gdi32.NewProc("GetObjectW")
	procDeleteObj  = gdi32.NewProc("DeleteObject")
	procGetLastErr = syscall.NewLazyDLL("kernel32.dll").NewProc("GetLastError")
)

func lastErr() uintptr {
	r, _, _ := procGetLastErr.Call()
	return r
}

func main() {
	in := flag.String("in", "", "输入 .ico/.png 路径")
	out := flag.String("out", "", "输出 PNG 路径")
	flag.Parse()
	if *in == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "-in 与 -out 必填")
		os.Exit(2)
	}

	smallW := w32.GetSystemMetrics(w32.SM_CXSMICON)
	smallH := w32.GetSystemMetrics(w32.SM_CYSMICON)

	fileData, err := os.ReadFile(*in)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读取失败:", err)
		os.Exit(1)
	}

	procGetLastErr.Call()
	hicon, err := w32.CreateSmallHIconFromImage(fileData)
	fmt.Printf("%s (%d 字节)  SM_CXSMICON=%dx%d\n", *in, len(fileData), smallW, smallH)
	fmt.Printf("  CreateSmallHIconFromImage → HICON=%d err=%v lastErr=%d\n",
		uintptr(hicon), err, lastErr())
	if hicon == 0 {
		os.Exit(1)
	}

	// 解码结果的实际尺寸与位深：能确认 selectICOImage 挑中的是哪一帧
	var info struct {
		FIcon    int32
		XHotspot uint32
		YHotspot uint32
		HbmMask  syscall.Handle
		HbmColor syscall.Handle
	}
	ret, _, e := procGetIconInf.Call(uintptr(hicon), uintptr(unsafe.Pointer(&info)))
	if ret != 0 {
		for _, bm := range []struct {
			name string
			h    syscall.Handle
		}{{"color", info.HbmColor}, {"mask", info.HbmMask}} {
			if bm.h == 0 {
				continue
			}
			var b w32.BITMAP
			if n, _, _ := procGetObjectW.Call(uintptr(bm.h), unsafe.Sizeof(b), uintptr(unsafe.Pointer(&b))); n != 0 {
				fmt.Printf("  %-5s bitmap: %dx%d  bitCount=%d  planes=%d\n",
					bm.name, b.BmWidth, b.BmHeight, b.BmBitsPixel, b.BmPlanes)
			}
			defer procDeleteObj.Call(uintptr(bm.h))
		}
	} else {
		fmt.Println("  GetIconInfo 失败:", e, "lastErr=", lastErr())
	}

	if err := w32.SaveHIconAsPNG(hicon, *out); err != nil {
		fmt.Fprintln(os.Stderr, "  SaveHIconAsPNG 失败:", err)
		os.Exit(1)
	}
	img, err := os.Open(*out)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer img.Close()
	decoded, format, err := image.Decode(img)
	if err != nil {
		fmt.Fprintln(os.Stderr, "解码探针输出失败:", err)
		os.Exit(1)
	}
	b := decoded.Bounds()
	lit, opaque := 0, 0
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, a := decoded.At(x, y).RGBA()
			if r != 0 || g != 0 || bl != 0 {
				lit++
			}
			if a > 0xE000 {
				opaque++
			}
		}
	}
	fmt.Printf("  ok %s  format=%s  %dx%d  非黑像素=%d  实心像素=%d\n",
		*out, format, b.Dx(), b.Dy(), lit, opaque)
	if lit == 0 {
		fmt.Println("  !! 图标解码为空：托盘上会看不见东西")
		os.Exit(1)
	}
	w32.DestroyIcon(hicon)
}
