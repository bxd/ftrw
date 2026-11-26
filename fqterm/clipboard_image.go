package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/png"
	"math/bits"
)

const maxClipImagePixels = 64 << 20

func validPNG(data []byte) bool {
	if len(data) < 33 || !bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")) {
		return false
	}
	if !bytes.Contains(data[len(data)-12:], []byte("IEND")) {
		return false
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > maxClipImagePixels {
		return false
	}
	return true
}

// dibToPNG 接受剪贴板常见的 BITMAPINFOHEADER / V4 / V5 DIB，统一输出 RGBA PNG。
// Windows 剪贴板图片可能来自浏览器、截图工具或画图，不能只押注某一种位深。
func dibToPNG(data []byte) ([]byte, error) {
	if len(data) < 40 {
		return nil, errors.New("DIB 头不完整")
	}
	headerSize := int(binary.LittleEndian.Uint32(data[0:4]))
	if headerSize < 40 || headerSize > len(data) {
		return nil, errors.New("不支持的 DIB 头")
	}
	width64 := int64(int32(binary.LittleEndian.Uint32(data[4:8])))
	height64 := int64(int32(binary.LittleEndian.Uint32(data[8:12])))
	planes := binary.LittleEndian.Uint16(data[12:14])
	bpp := binary.LittleEndian.Uint16(data[14:16])
	compression := binary.LittleEndian.Uint32(data[16:20])
	colorsUsed := int(binary.LittleEndian.Uint32(data[32:36]))
	if width64 <= 0 || height64 == 0 || height64 == -1<<31 {
		return nil, errors.New("DIB 尺寸无效")
	}
	if planes != 1 || width64*absInt64(height64) > maxClipImagePixels {
		return nil, errors.New("DIB 尺寸过大或平面数无效")
	}
	width, height := int(width64), int(absInt64(height64))
	topDown := height64 < 0
	switch bpp {
	case 1, 4, 8, 16, 24, 32:
	default:
		return nil, errors.New("不支持该 DIB 位深")
	}
	if compression != 0 && compression != 3 && compression != 6 {
		return nil, errors.New("不支持压缩或 RLE 编码的 DIB")
	}
	if bpp <= 8 && compression != 0 {
		return nil, errors.New("调色板 DIB 不支持位域压缩")
	}
	pixelOffset := headerSize
	var redMask, greenMask, blueMask, alphaMask uint32
	if compression == 3 || compression == 6 {
		if headerSize >= 52 {
			redMask = binary.LittleEndian.Uint32(data[40:44])
			greenMask = binary.LittleEndian.Uint32(data[44:48])
			blueMask = binary.LittleEndian.Uint32(data[48:52])
			if compression == 6 && headerSize >= 56 {
				alphaMask = binary.LittleEndian.Uint32(data[52:56])
			}
		} else {
			maskBytes := 12
			if compression == 6 {
				maskBytes = 16
			}
			if headerSize+maskBytes > len(data) {
				return nil, errors.New("DIB 位域不完整")
			}
			redMask = binary.LittleEndian.Uint32(data[pixelOffset : pixelOffset+4])
			greenMask = binary.LittleEndian.Uint32(data[pixelOffset+4 : pixelOffset+8])
			blueMask = binary.LittleEndian.Uint32(data[pixelOffset+8 : pixelOffset+12])
			if compression == 6 {
				alphaMask = binary.LittleEndian.Uint32(data[pixelOffset+12 : pixelOffset+16])
			}
			pixelOffset += maskBytes
		}
	}
	if redMask == 0 {
		switch bpp {
		case 16:
			redMask, greenMask, blueMask = 0x7c00, 0x03e0, 0x001f
		case 32:
			redMask, greenMask, blueMask = 0x00ff0000, 0x0000ff00, 0x000000ff
		}
	}
	if (bpp == 16 || bpp == 32) && (redMask == 0 || greenMask == 0 || blueMask == 0) {
		return nil, errors.New("DIB 颜色位域无效")
	}

	paletteColors := 0
	if bpp <= 8 {
		paletteColors = colorsUsed
		if paletteColors == 0 {
			paletteColors = 1 << bpp
		}
		if paletteColors > 1<<bpp || pixelOffset+paletteColors*4 > len(data) {
			return nil, errors.New("DIB 调色板不完整")
		}
	}
	stride := ((int64(width)*int64(bpp) + 31) / 32) * 4
	needed := int64(pixelOffset) + stride*int64(height)
	if stride <= 0 || needed > int64(len(data)) {
		return nil, errors.New("DIB 像素数据不完整")
	}

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	anyAlpha := false
	for y := 0; y < height; y++ {
		sourceY := y
		if !topDown {
			sourceY = height - 1 - y
		}
		row := data[pixelOffset+int(stride)*sourceY : pixelOffset+int(stride)*(sourceY+1)]
		for x := 0; x < width; x++ {
			var c color.RGBA
			switch bpp {
			case 1, 4, 8:
				index := dibPaletteIndex(row[x*int(bpp):], bpp, x)
				if index >= paletteColors {
					return nil, errors.New("DIB 调色板索引越界")
				}
				entry := data[pixelOffset+index*4 : pixelOffset+index*4+4]
				c = color.RGBA{R: entry[2], G: entry[1], B: entry[0], A: 255}
			case 16:
				value := uint32(binary.LittleEndian.Uint16(row[x*2:]))
				c = color.RGBA{R: maskChannel(value, redMask), G: maskChannel(value, greenMask), B: maskChannel(value, blueMask), A: 255}
			case 24:
				p := row[x*3:]
				c = color.RGBA{R: p[2], G: p[1], B: p[0], A: 255}
			case 32:
				value := binary.LittleEndian.Uint32(row[x*4:])
				c = color.RGBA{R: maskChannel(value, redMask), G: maskChannel(value, greenMask), B: maskChannel(value, blueMask), A: 255}
				if alphaMask != 0 {
					c.A = maskChannel(value, alphaMask)
					anyAlpha = anyAlpha || c.A != 0
				} else if compression == 0 {
					c.A = uint8(value >> 24)
					anyAlpha = anyAlpha || c.A != 0
				}
			}
			img.SetRGBA(x, y, c)
		}
	}
	// 很多 32 位 DIB 虽保留第四字节，却把它恒置 0；只有任意像素出现非零 alpha
	// 才把它当透明通道，否则按不透明图片解释，避免整张截图变透明。
	if bpp == 32 && alphaMask == 0 && !anyAlpha {
		for i := 3; i < len(img.Pix); i += 4 {
			img.Pix[i] = 255
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func dibPaletteIndex(row []byte, bpp uint16, x int) int {
	switch bpp {
	case 8:
		return int(row[x])
	case 4:
		if x%2 == 0 {
			return int(row[x/2] >> 4)
		}
		return int(row[x/2] & 0x0f)
	default:
		return int((row[x/8] >> uint(7-x%8)) & 1)
	}
}

func maskChannel(value, mask uint32) uint8 {
	if mask == 0 {
		return 0
	}
	shift := bits.TrailingZeros32(mask)
	max := mask >> shift
	return uint8(uint64(value&mask>>shift) * 255 / uint64(max))
}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
