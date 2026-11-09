// Command genicon 以确定性方式绘制 FTRW 应用图标，输出两类资产：
//
//   - build/appicon.png          1024x1024，供 wails3 generate icons 产出 .ico/exe 图标
//   - build/tray/tray*.ico       16/24/32/48 多尺寸，供系统托盘按 SM_CXSMICON 取最合适的一帧
//
// 为什么不用生图模型：本项目的图标是纯几何扁平字形，AI 出图会带平台水印、圆角外的
// 白边（appicon.png 是无 alpha 的 24bpp，白角会原样进 .ico），而且笔画权重无法精确
// 控制，缩到 16px 后字形就读散了。这里改用 SDF 逐像素求值 + 超采样降采样，
// 边缘平滑、尺寸稳定、每次运行结果一致。
//
// 用法：
//
//	go run ./cmd/genicon -design f-framed -out build/appicon.png
//	go run ./cmd/genicon -design tray -format ico -palette tray         -out build/tray/tray-lightbg.ico
//	go run ./cmd/genicon -design tray -format ico -palette tray-bright -out build/tray/tray-darkbg.ico
//
// 两个托盘文件名里的 lightbg/darkbg 指任务栏底色：Wails 的 SystemTray 把 SetIcon 用于
// 浅色模式、SetDarkModeIcon 用于深色模式（见 windowsSystemTray.resolveIcons），所以
// 深色字给浅底、浅色字给深底。改名后必须同步 main.go 的 //go:embed 路径。
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"strconv"
	"strings"
)

// 画布统一用归一化坐标：x/y ∈ [0,1]，y 向下，原点在左上。
type vec struct{ x, y float64 }

// sdBox 轴对齐矩形带符号距离场（Inigo Quilez 标准式）。q <= 0 即在矩形内。
func sdBox(p, c, h vec) float64 {
	q := vec{math.Abs(p.x-c.x) - h.x, math.Abs(p.y-c.y) - h.y}
	return math.Hypot(math.Max(q.x, 0), math.Max(q.y, 0)) + math.Min(math.Max(q.x, q.y), 0)
}

// sdRoundBox 圆角矩形距离场。b 半宽半高，r 圆角半径。
func sdRoundBox(p, c, b vec, r float64) float64 {
	q := vec{math.Abs(p.x-c.x) - b.x + r, math.Abs(p.y-c.y) - b.y + r}
	return math.Hypot(math.Max(q.x, 0), math.Max(q.y, 0)) + math.Min(math.Max(q.x, q.y), 0) - r
}

// sdSeg 点到线段的距离，用于画有厚度的斜笔（提示符的 ">"）。
func sdSeg(p, a, b vec) float64 {
	pax, pay := p.x-a.x, p.y-a.y
	bax, bay := b.x-a.x, b.y-a.y
	d := bax*bax + bay*bay
	t := 0.0
	if d > 0 {
		t = math.Min(1, math.Max(0, (pax*bax+pay*bay)/d))
	}
	return math.Hypot(pax-t*bax, pay-t*bay)
}

// layer 一个着色层：后声明的层覆盖先声明的层。f 返回带符号距离，<=0 着色。
type layer struct {
	col color.RGBA
	f   func(p vec) float64
}

// shape 是尚未取色的几何层：只描述“哪一笔、用什么墨”，颜色由配色方案决定，
// 这样同一套字形可以一次输出应用图标、浅色托盘、深色托盘三种配色。
type shape struct {
	ink ink
	f   func(p vec) float64
}

// ink 一层的具体颜色由配色方案决定，几何只描述字形本身。
type ink int

const (
	inkPrimary ink = iota // 主色（F 竖干/横笔、提示符的 >）
	inkAccent             // 强调色（光标块、下划线）
	inkFrame              // 描边
)

func box(c, h vec) func(vec) float64 {
	return func(p vec) float64 { return sdBox(p, c, h) }
}

// chevron 两条等粗斜笔拼成的 ">"，t 为笔画厚度。
func chevron(a, mid, b vec, t float64) func(vec) float64 {
	half := t / 2
	return func(p vec) float64 {
		return math.Min(sdSeg(p, a, mid)-half, sdSeg(p, mid, b)-half)
	}
}

type palette struct {
	name    string
	bg      color.RGBA
	primary color.RGBA
	accent  color.RGBA
	frame   color.RGBA
}

var palettes = map[string]palette{
	// 应用图标：深色满幅底 + 亮色字形。
	"app": {
		name: "app", bg: color.RGBA{0x19, 0x1C, 0x22, 0xFF},
		primary: color.RGBA{0x5B, 0x9D, 0xF0, 0xFF}, accent: color.RGBA{0x43, 0xC6, 0xA8, 0xFF},
		frame: color.RGBA{0x3D, 0x7F, 0xBF, 0xFF},
	},
	// 托盘（浅色任务栏）：透明底 + 深中调，保证在浅灰底上有足够对比。
	"tray": {
		name: "tray", bg: color.RGBA{0, 0, 0, 0},
		primary: color.RGBA{0x2F, 0x7F, 0xD1, 0xFF}, accent: color.RGBA{0x1E, 0x9E, 0x85, 0xFF},
		frame: color.RGBA{0x2F, 0x7F, 0xD1, 0xFF},
	},
	// 托盘（深色任务栏）：透明底 + 亮调。
	"tray-bright": {
		name: "tray-bright", bg: color.RGBA{0, 0, 0, 0},
		primary: color.RGBA{0x6A, 0xB0, 0xF3, 0xFF}, accent: color.RGBA{0x4E, 0xC9, 0xB0, 0xFF},
		frame: color.RGBA{0x6A, 0xB0, 0xF3, 0xFF},
	},
}

// shapes 返回几何层列表（按声明顺序覆盖）。
//
// 所有方案的笔画厚度统一，保证缩到 16px 时各笔权重一致、不会有的糊掉有的断掉；
// tray 方案用更粗的 barT，因为细描边在 16px 下会整圈消失。
func shapes(design string) ([]shape, error) {
	switch design {
	case "prompt":
		return []shape{
			{inkPrimary, chevron(vec{0.20, 0.26}, vec{0.56, 0.50}, vec{0.20, 0.74}, bar)},
			{inkAccent, box(vec{0.755, 0.735}, vec{0.100, bar / 2})},
		}, nil

	case "f-cell":
		// 字母 F，光标块直接接在中横末端（比横笔更高，像被选中的终端单元格）。
		// 光标悬空时会读成句号，连笔才读成光标。
		return []shape{
			{inkPrimary, box(vec{0.270, 0.500}, vec{bar / 2, 0.250})},
			{inkPrimary, box(vec{0.410, 0.315}, vec{0.205, bar / 2})},
			{inkPrimary, box(vec{0.3475, 0.503}, vec{0.1425, bar / 2})},
			{inkAccent, box(vec{0.585, 0.503}, vec{0.095, 0.095})},
		}, nil

	case "f-framed":
		// 字母 F + 块光标，外加一圈细圆角描边（更像“应用图标”而非孤立字形）。
		return []shape{
			{inkFrame, func(p vec) float64 {
				return math.Abs(sdRoundBox(p, vec{0.5, 0.5}, vec{0.44, 0.44}, 0.10)) - 0.016
			}},
			{inkPrimary, box(vec{0.300, 0.500}, vec{bar / 2, 0.200})},
			{inkPrimary, box(vec{0.415, 0.330}, vec{0.170, bar / 2})},
			{inkPrimary, box(vec{0.375, 0.500}, vec{0.130, bar / 2})},
			{inkAccent, box(vec{0.665, 0.645}, vec{bar / 2, bar / 2})},
		}, nil

	case "tray":
		// 托盘专用：去掉会在 16px 下消失的细描边，笔画加粗、字形放大占满画布。
		return []shape{
			{inkPrimary, box(vec{0.260, 0.500}, vec{barT / 2, 0.300})},
			{inkPrimary, box(vec{0.440, 0.280}, vec{0.260, barT / 2})},
			{inkPrimary, box(vec{0.380, 0.500}, vec{0.200, barT / 2})},
			{inkAccent, box(vec{0.660, 0.720}, vec{barT / 2, barT / 2})},
		}, nil

	case "f-cursor":
		return []shape{
			{inkPrimary, box(vec{0.270, 0.500}, vec{bar / 2, 0.250})},
			{inkPrimary, box(vec{0.410, 0.315}, vec{0.205, bar / 2})},
			{inkPrimary, box(vec{0.365, 0.503}, vec{0.160, bar / 2})},
			{inkAccent, box(vec{0.720, 0.685}, vec{bar / 2, bar / 2})},
		}, nil

	default:
		return nil, fmt.Errorf("未知字形方案 %q", design)
	}
}

const (
	bar  = 0.13  // 应用图标笔画厚度
	barT = 0.155 // 托盘笔画厚度（小尺寸下更耐缩）
)

// colorize 把几何层按配色方案落成具体颜色。
func colorize(sh []shape, pal palette) []layer {
	out := make([]layer, len(sh))
	for i, s := range sh {
		var c color.RGBA
		switch s.ink {
		case inkPrimary:
			c = pal.primary
		case inkAccent:
			c = pal.accent
		case inkFrame:
			c = pal.frame
		}
		out[i] = layer{c, s.f}
	}
	return out
}

// render 在 size×size 上精确求值：先在 ss 倍画布上逐像素算 SDF，再对 ss×ss 邻域
// 做盒式平均。image.RGBA 是预乘 alpha，透明背景像素贡献 (0,0,0,0)，平均结果仍是
// 正确的预乘值（等价于按覆盖率加权），所以降采样不需要额外处理 alpha。
func render(layers []layer, bg color.RGBA, size, ss int) *image.RGBA {
	n := size * ss
	hires := image.NewRGBA(image.Rect(0, 0, n, n))
	inv := 1.0 / float64(n)
	for y := 0; y < n; y++ {
		py := (float64(y) + 0.5) * inv // 采样点取子像素中心，避免边界整体偏移半像素
		for x := 0; x < n; x++ {
			p := vec{(float64(x) + 0.5) * inv, py}
			c := bg
			for _, l := range layers {
				if l.f(p) <= 0 {
					c = l.col
				}
			}
			hires.SetRGBA(x, y, c)
		}
	}

	out := image.NewRGBA(image.Rect(0, 0, size, size))
	area := uint64(ss * ss)
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			var r, g, b, a uint64
			for dy := 0; dy < ss; dy++ {
				for dx := 0; dx < ss; dx++ {
					c := hires.RGBAAt(x*ss+dx, y*ss+dy)
					r += uint64(c.R)
					g += uint64(c.G)
					b += uint64(c.B)
					a += uint64(c.A)
				}
			}
			// 先加半个面积再整除＝四舍五入。写成 r/area+1 会让满值 255 变成 256，
			// 转 uint8 后溢出回 0，整张不透明图直接变成全透明。
			out.SetRGBA(x, y, color.RGBA{
				R: uint8((r + area/2) / area),
				G: uint8((g + area/2) / area),
				B: uint8((b + area/2) / area),
				A: uint8((a + area/2) / area),
			})
		}
	}
	return out
}

// writePNG 编码为 PNG（image.RGBA 已是预乘，png.Encode 会自行反预乘）。
func writePNG(path string, img *image.RGBA) error {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

// writeICO 写多帧 .ico，每帧用「32bpp 未压缩 BGRA + 1bpp AND 掩码」。
//
// 不用 PNG 压缩帧：Wails 的 CreateSmallHIconFromImage 走 CreateIconFromResourceEx，
// 对 PNG 帧的支持不可靠；未压缩 DIB 帧是这个 API 确定支持的格式。
// 约定：biHeight = 2×实际高（XOR 位图 + AND 掩码各占一半），两者都自底向上存放。
// XOR 存的是「非预乘」BGRA，所以要先把 image.RGBA 的预乘值反预乘回去。
func writeICO(path string, imgs []*image.RGBA) error {
	var dir, data bytes.Buffer
	binary.Write(&dir, binary.LittleEndian, uint16(0)) // reserved
	binary.Write(&dir, binary.LittleEndian, uint16(1)) // type: icon
	binary.Write(&dir, binary.LittleEndian, uint16(len(imgs)))

	offset := 6 + 16*len(imgs)
	for _, img := range imgs {
		w, h := img.Bounds().Dx(), img.Bounds().Dy()
		frame, err := icoFrame(img)
		if err != nil {
			return err
		}
		// 目录项里的宽高：0 表示 256（1 字节字段放不下）
		dw, dh := byte(w), byte(h)
		if w >= 256 {
			dw = 0
		}
		if h >= 256 {
			dh = 0
		}
		binary.Write(&dir, binary.LittleEndian, dw)
		binary.Write(&dir, binary.LittleEndian, dh)
		dir.WriteByte(0)                                    // 颜色数
		dir.WriteByte(0)                                    // reserved
		binary.Write(&dir, binary.LittleEndian, uint16(1))  // planes
		binary.Write(&dir, binary.LittleEndian, uint16(32)) // bitCount
		binary.Write(&dir, binary.LittleEndian, uint32(len(frame)))
		binary.Write(&dir, binary.LittleEndian, uint32(offset))
		data.Write(frame)
		offset += len(frame)
	}
	return os.WriteFile(path, append(dir.Bytes(), data.Bytes()...), 0o644)
}

func icoFrame(img *image.RGBA) ([]byte, error) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	var buf bytes.Buffer
	// BITMAPINFOHEADER
	binary.Write(&buf, binary.LittleEndian, uint32(40))
	binary.Write(&buf, binary.LittleEndian, int32(w))
	binary.Write(&buf, binary.LittleEndian, int32(h*2)) // XOR + AND
	binary.Write(&buf, binary.LittleEndian, uint16(1))  // planes
	binary.Write(&buf, binary.LittleEndian, uint16(32)) // bitCount
	binary.Write(&buf, binary.LittleEndian, uint32(0))  // BI_RGB
	binary.Write(&buf, binary.LittleEndian, uint32(0))  // sizeImage（可为 0）
	binary.Write(&buf, binary.LittleEndian, int32(0))   // xPPM
	binary.Write(&buf, binary.LittleEndian, int32(0))   // yPPM
	binary.Write(&buf, binary.LittleEndian, uint32(0))  // clrUsed
	binary.Write(&buf, binary.LittleEndian, uint32(0))  // clrImportant

	// XOR：自底向上，非预乘 BGRA
	for y := h - 1; y >= 0; y-- {
		for x := 0; x < w; x++ {
			c := img.RGBAAt(b.Min.X+x, b.Min.Y+y)
			buf.WriteByte(unpremultiply(c.B, c.A))
			buf.WriteByte(unpremultiply(c.G, c.A))
			buf.WriteByte(unpremultiply(c.R, c.A))
			buf.WriteByte(c.A)
		}
	}

	// AND：自底向上，1bpp，行补齐到 4 字节；alpha==0 处置 1
	rowBytes := (w + 31) / 32 * 4
	mask := make([]byte, rowBytes)
	for y := h - 1; y >= 0; y-- {
		for i := range mask {
			mask[i] = 0
		}
		for x := 0; x < w; x++ {
			if img.RGBAAt(b.Min.X+x, b.Min.Y+y).A == 0 {
				mask[x/8] |= 1 << (7 - x%8)
			}
		}
		buf.Write(mask)
	}
	return buf.Bytes(), nil
}

// unpremultiply 把 image.RGBA 的预乘通道还原成 .ico 要求的非预乘值。
// 直接整除会系统性偏暗（边缘像素尤其明显），所以先加 a/2 再除做四舍五入，
// 并钉在 255：a 很小时 255*255/a 会超出 uint8。
func unpremultiply(v, a uint8) uint8 {
	if a == 0 {
		return 0 // 完全透明的像素 RGB 无意义，置 0 让掩码/alpha 说话
	}
	if a == 255 {
		return v
	}
	r := (uint32(v)*255 + uint32(a)/2) / uint32(a)
	if r > 255 {
		return 255
	}
	return uint8(r)
}

func parseSizes(s string) ([]int, error) {
	var out []int
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 256 {
			return nil, fmt.Errorf("非法尺寸 %q（须在 1..256）", p)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("-sizes 不能为空")
	}
	return out, nil
}

func main() {
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "用法: go run ./cmd/genicon -design <方案> -out <文件> [-format png|ico] [-palette ...] [-sizes 16,24,32,48]")
	}
	design := flag.String("design", "f-cursor", "字形方案：f-cursor | f-cell | prompt | f-framed | tray")
	out := flag.String("out", "", "输出文件路径（必填）")
	format := flag.String("format", "png", "png（单尺寸）| ico（多尺寸）")
	paletteName := flag.String("palette", "app", "配色：app | tray | tray-bright")
	size := flag.Int("size", 1024, "png 输出边长")
	sizes := flag.String("sizes", "16,24,32,48", "ico 帧尺寸列表")
	ss := flag.Int("ss", 4, "超采样倍数（抗锯齿质量）；ico 模式按每帧自动放大到约 1024 求值")
	flag.Parse()

	if *out == "" {
		flag.Usage()
		os.Exit(2)
	}
	pal, ok := palettes[*paletteName]
	if !ok {
		fmt.Fprintf(os.Stderr, "未知配色 %q，可选：%s\n", *paletteName, strings.Join(paletteNames(), " | "))
		os.Exit(2)
	}
	sh, err := shapes(*design)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	layers := colorize(sh, pal)

	switch *format {
	case "png":
		if *ss < 1 {
			fmt.Fprintln(os.Stderr, "-ss 至少为 1")
			os.Exit(2)
		}
		img := render(layers, pal.bg, *size, *ss)
		if err := writePNG(*out, img); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("ok %s design=%s palette=%s %dx%d\n", *out, *design, *paletteName, *size, *size)

	case "ico":
		list, err := parseSizes(*sizes)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		var imgs []*image.RGBA
		for _, sz := range list {
			// 每帧都求值到约 1024 的等效精度，小尺寸才不会因为超采样不足而丢笔画
			f := 1024 / sz
			if f < 1 {
				f = 1
			}
			imgs = append(imgs, render(layers, pal.bg, sz, f))
		}
		if err := writeICO(*out, imgs); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("ok %s design=%s palette=%s frames=%s\n", *out, *design, *paletteName, *sizes)

	default:
		fmt.Fprintf(os.Stderr, "未知格式 %q\n", *format)
		os.Exit(2)
	}
}

func paletteNames() []string {
	var v []string
	for k := range palettes {
		v = append(v, k)
	}
	return v
}
