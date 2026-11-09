// Command genicon 以确定性方式绘制 FTRW 应用图标，输出 build/appicon.png 所需的
// 1024x1024 PNG。
//
// 为什么不用生图模型：本项目的图标是纯几何扁平字形，AI 出图会带平台水印、圆角外的
// 白边（appicon.png 是无 alpha 的 24bpp，白角会原样进 .ico），而且笔画权重无法精确
// 控制，缩到 16px 后字形就读散了。这里改用 SDF 逐像素求值 + 超采样降采样，
// 边缘平滑、尺寸稳定、每次运行结果一致。
//
// 用法：
//
//	go run ./cmd/genicon -design f-cursor -out build/appicon.png
//	go run ./cmd/genicon -design prompt   -out %TEMP%/cand-prompt.png
//
// 改设计只需改下面的 shapes()，不需要动渲染管线。
package main

import (
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
)

// 画布统一用归一化坐标：x/y ∈ [0,1]，y 向下，原点在左上。
type vec struct{ x, y float64 }

// sub 归一化到矩形中心后的坐标：SDF 公式都以中心为原点书写。
func sub(p, c vec) vec { return vec{p.x - c.x, p.y - c.y} }

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
	pa := sub(p, a)
	ba := sub(b, a)
	d := ba.x*ba.x + ba.y*ba.y
	t := 0.0
	if d > 0 {
		t = math.Min(1, math.Max(0, (pa.x*ba.x+pa.y*ba.y)/d))
	}
	return math.Hypot(pa.x-t*ba.x, pa.y-t*ba.y)
}

// layer 一个着色层：后声明的层覆盖先声明的层。f 返回带符号距离，<=0 着色。
type layer struct {
	col color.RGBA
	f   func(p vec) float64
}

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

var (
	bgDeep  = color.RGBA{0x15, 0x18, 0x1D, 0xFF}
	bgSlate = color.RGBA{0x19, 0x1C, 0x22, 0xFF}
	teal    = color.RGBA{0x43, 0xC6, 0xA8, 0xFF}
	blue    = color.RGBA{0x5B, 0x9D, 0xF0, 0xFF}
	steel   = color.RGBA{0x3D, 0x7F, 0xBF, 0xFF}
)

// 所有方案的笔画厚度统一为 0.13，保证缩到 16px 时各笔权重一致、不会有的糊掉有的断掉。
const bar = 0.13

// shapes 返回（背景色，由底到顶的层列表）。
func shapes(design string) (color.RGBA, []layer) {
	switch design {
	case "prompt":
		// 纯终端提示符 "> _"，不含字母。16px 下轮廓最干净。
		return bgDeep, []layer{
			{blue, chevron(vec{0.20, 0.26}, vec{0.56, 0.50}, vec{0.20, 0.74}, bar)},
			{teal, box(vec{0.755, 0.735}, vec{0.100, bar / 2})},
		}

	case "f-cell":
		// 字母 F，光标块直接接在中横末端（比横笔更高，像被选中的终端单元格）。
		// 与 f-cursor 的区分就在这一点：光标悬空时会读成句号，连笔才读成光标。
		return bgDeep, []layer{
			{teal, box(vec{0.270, 0.500}, vec{bar / 2, 0.250})},
			{teal, box(vec{0.410, 0.315}, vec{0.205, bar / 2})},
			{teal, box(vec{0.3475, 0.503}, vec{0.1425, bar / 2})},
			{blue, box(vec{0.585, 0.503}, vec{0.095, 0.095})},
		}

	case "f-framed":
		// 字母 F + 块光标，外加一圈细圆角描边（更像“应用图标”而非孤立字形）。
		return bgSlate, []layer{
			{steel, func(p vec) float64 {
				return math.Abs(sdRoundBox(p, vec{0.5, 0.5}, vec{0.44, 0.44}, 0.10)) - 0.016
			}},
			{blue, box(vec{0.300, 0.500}, vec{bar / 2, 0.200})},   // 竖干
			{blue, box(vec{0.415, 0.330}, vec{0.170, bar / 2})},   // 上横
			{blue, box(vec{0.375, 0.500}, vec{0.130, bar / 2})},   // 中横
			{teal, box(vec{0.665, 0.645}, vec{bar / 2, bar / 2})}, // 光标
		}

	default: // "f-cursor"
		// 字母 F（竖干 + 上下两横，等粗）+ 右下块状光标。
		return bgDeep, []layer{
			{teal, box(vec{0.270, 0.500}, vec{bar / 2, 0.250})},   // 竖干 y .25-.75
			{teal, box(vec{0.410, 0.315}, vec{0.205, bar / 2})},   // 上横 y .25-.38
			{teal, box(vec{0.365, 0.503}, vec{0.160, bar / 2})},   // 中横
			{blue, box(vec{0.720, 0.685}, vec{bar / 2, bar / 2})}, // 光标，底边与竖干齐平
		}
	}
}

func main() {
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "用法: go run ./cmd/genicon -design f-cursor|f-cell|prompt|f-framed -out <png> [-size 1024] [-ss 4]")
	}
	design := flag.String("design", "f-cursor", "图标方案：f-cursor | f-cell | prompt | f-framed")
	out := flag.String("out", "", "输出 PNG 路径（必填）")
	size := flag.Int("size", 1024, "输出边长（正方形）")
	ss := flag.Int("ss", 4, "超采样倍数（抗锯齿质量）")
	flag.Parse()

	if *out == "" {
		fmt.Fprintln(os.Stderr, "必须指定 -out")
		os.Exit(2)
	}
	if *ss < 1 {
		fmt.Fprintln(os.Stderr, "-ss 至少为 1")
		os.Exit(2)
	}
	bg, layers := shapes(*design)

	// 先在 ss 倍大的画布上逐像素求值，再对 ss×ss 邻域做盒式平均降采样。
	// 形状是精确 SDF，降采样等于按覆盖面积加权，边缘既不锯齿也不会被滤糊。
	n := *size * *ss
	hires := image.NewRGBA(image.Rect(0, 0, n, n))
	for y := 0; y < n; y++ {
		py := (float64(y) + 0.5) / float64(n) // 采样点取子像素中心，避免边界整体偏移半像素
		for x := 0; x < n; x++ {
			p := vec{(float64(x) + 0.5) / float64(n), py}
			c := bg
			for _, l := range layers {
				if l.f(p) <= 0 {
					c = l.col
				}
			}
			hires.SetRGBA(x, y, c)
		}
	}

	outImg := image.NewRGBA(image.Rect(0, 0, *size, *size))
	f := *ss
	area := uint64(f * f)
	for y := 0; y < *size; y++ {
		for x := 0; x < *size; x++ {
			var r, g, b, a uint64
			for dy := 0; dy < f; dy++ {
				for dx := 0; dx < f; dx++ {
					c := hires.RGBAAt(x*f+dx, y*f+dy)
					r += uint64(c.R)
					g += uint64(c.G)
					b += uint64(c.B)
					a += uint64(c.A)
				}
			}
			// 先加半个面积再整除＝四舍五入。写成 r/area+1 会让满值 255 变成 256，
			// 转 uint8 后溢出回 0，整张不透明图直接变成全透明。
			outImg.SetRGBA(x, y, color.RGBA{
				R: uint8((r + area/2) / area),
				G: uint8((g + area/2) / area),
				B: uint8((b + area/2) / area),
				A: uint8((a + area/2) / area),
			})
		}
	}

	fh, err := os.Create(*out)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer fh.Close()
	if err := png.Encode(fh, outImg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("ok %s design=%s %dx%d\n", *out, *design, *size, *size)
}
