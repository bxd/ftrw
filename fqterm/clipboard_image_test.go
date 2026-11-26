package main

import (
	"bytes"
	"encoding/binary"
	"image/png"
	"testing"

	"github.com/wailsapp/wails/v3/pkg/application"
)

func TestDIB24ToPNGBottomUp(t *testing.T) {
	width, height := 2, 2
	stride := 8
	data := make([]byte, 40+stride*height)
	binary.LittleEndian.PutUint32(data[0:4], 40)
	binary.LittleEndian.PutUint32(data[4:8], uint32(width))
	binary.LittleEndian.PutUint32(data[8:12], uint32(height))
	binary.LittleEndian.PutUint16(data[12:14], 1)
	binary.LittleEndian.PutUint16(data[14:16], 24)
	// 底行红、绿；顶行蓝、白。DIB 像素按 BGR(A) 存储。
	copy(data[40:48], []byte{0, 0, 255, 0, 255, 0, 0, 0})
	copy(data[48:56], []byte{255, 0, 0, 255, 255, 255, 0, 0})

	encoded, err := dibToPNG(data)
	if err != nil {
		t.Fatal(err)
	}
	if !validPNG(encoded) {
		t.Fatal("转换结果不是有效 PNG")
	}
	img, err := png.Decode(bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	want := [][4]uint8{
		{0, 0, 255, 255}, {255, 255, 255, 255},
		{255, 0, 0, 255}, {0, 255, 0, 255},
	}
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			r, g, b, a := img.At(x, y).RGBA()
			got := [4]uint8{uint8(r >> 8), uint8(g >> 8), uint8(b >> 8), uint8(a >> 8)}
			if got != want[y*width+x] {
				t.Fatalf("像素 (%d,%d) = %v, want %v", x, y, got, want[y*width+x])
			}
		}
	}
}

func TestDIB32TopDownZeroAlphaMeansOpaque(t *testing.T) {
	data := make([]byte, 52+16)
	binary.LittleEndian.PutUint32(data[0:4], 52)
	binary.LittleEndian.PutUint32(data[4:8], 2)
	binary.LittleEndian.PutUint32(data[8:12], uint32(^uint32(1))) // -1，顶行优先
	binary.LittleEndian.PutUint16(data[12:14], 1)
	binary.LittleEndian.PutUint16(data[14:16], 32)
	binary.LittleEndian.PutUint32(data[16:20], 3) // BI_BITFIELDS
	binary.LittleEndian.PutUint32(data[40:44], 0x00ff0000)
	binary.LittleEndian.PutUint32(data[44:48], 0x0000ff00)
	binary.LittleEndian.PutUint32(data[48:52], 0x000000ff)
	binary.LittleEndian.PutUint32(data[52:56], 0x0000ff00)
	binary.LittleEndian.PutUint32(data[60:64], 0x00ff0000)

	encoded, err := dibToPNG(data)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, a := img.At(0, 0).RGBA()
	if a != 0xffff {
		t.Fatalf("零 alpha 的旧式 32 位 DIB 应按不透明处理，得到 %04x", a)
	}
}

func TestDIBRejectsTruncatedAndUnsupportedData(t *testing.T) {
	if _, err := dibToPNG(make([]byte, 39)); err == nil {
		t.Fatal("截断 DIB 头应报错")
	}
	data := make([]byte, 40)
	binary.LittleEndian.PutUint32(data[0:4], 40)
	binary.LittleEndian.PutUint32(data[4:8], 100)
	binary.LittleEndian.PutUint32(data[8:12], 100)
	binary.LittleEndian.PutUint16(data[12:14], 1)
	binary.LittleEndian.PutUint16(data[14:16], 24)
	if _, err := dibToPNG(data); err == nil {
		t.Fatal("像素数据不足时应报错")
	}
}

func TestEditorDropTargetFilter(t *testing.T) {
	details := application.DropTargetDetails{Attributes: map[string]string{"data-file-drop-target": editorDropTarget}}
	if got := editorDropFiles(details, []string{`C:\a.txt`}); len(got) != 1 {
		t.Fatalf("编辑区应接收文件，得到 %v", got)
	}
	details.Attributes["data-file-drop-target"] = "terminal"
	if got := editorDropFiles(details, []string{`C:\a.txt`}); got != nil {
		t.Fatalf("非编辑区不应接收文件，得到 %v", got)
	}
	if got := editorDropFiles(application.DropTargetDetails{}, nil); got != nil {
		t.Fatalf("空文件列表应忽略，得到 %v", got)
	}
}
