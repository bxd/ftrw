package main

// advcopy.go：内置 ac/am（Advanced Copy 式）拷贝/移动引擎。
// 参照 beatex.org Advanced Copy（coreutils 补丁）：整体 + 当前文件双层进度条、
// 速度、ETA。ac/am 默认就带进度（-g/--progress-bar 兼容接受，无副作用）。
// 引擎跨平台纯 Go；CLI 外壳与 shell 注入分别见 advcopy_cli*.go / shellbuiltins.go。

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var errAdvHelp = errors.New("adv help")

const advUsage = `用法: ac [-r] 源... 目标    带进度条的拷贝（目录需 -r）
      am [-r] 源... 目标    带进度条的移动（同盘直接改名，跨盘拷贝后删源）
选项: -r/-R 递归  -g/--progress-bar 兼容 advcpmv（进度默认开）  -h/--help`

type advOptions struct {
	recursive bool
	move      bool
	sources   []string
	dest      string
}

func parseAdvArgs(args []string) (*advOptions, error) {
	o := &advOptions{}
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-r" || a == "-R" || a == "--recursive":
			o.recursive = true
		case a == "-g" || a == "--progress-bar":
			// advcpmv 的开关；这里进度常开，接受只为肌肉记忆兼容。
		case a == "-h" || a == "--help":
			return nil, errAdvHelp
		case a == "--":
			pos = append(pos, args[i+1:]...)
			i = len(args)
		case len(a) > 1 && a[0] == '-':
			return nil, fmt.Errorf("不认识的选项 %s（ac/am 支持 -r 递归、-g 进度兼容、--help）", a)
		default:
			pos = append(pos, a)
		}
	}
	if len(pos) < 2 {
		return nil, errors.New("至少一个源和一个目标。" + advUsage)
	}
	o.sources = pos[:len(pos)-1]
	o.dest = pos[len(pos)-1]
	return o, nil
}

// ---- 进度模型与渲染 ----

type advProgress struct {
	totalBytes int64
	doneBytes  int64
	filesTotal int
	filesDone  int
	curName    string
	curTotal   int64
	curDone    int64
	start      time.Time
}

type advRenderer interface {
	render(p *advProgress)
	note(line string)
	finish()
}

// termRenderer 三行块原地重绘：整体条 / 速度+ETA / 当前文件条（advcpmv 版式）。
// 重绘用 \x1b[3A 上移三行 + \r\x1b[K 清行。真实 ConPTY 环回验证：只要每行不超宽
// 换行（块高恒=3），ConPTY 会转成绝对定位重印，原地更新稳定；一旦长行换行块高>3
// 就会逐帧残留/冻住。故 width（终端列数）内截断每行是原地重绘正确的前提。
type termRenderer struct {
	w     io.Writer
	last  time.Time
	drawn bool
	width int
}

func humanBytes(n int64) string {
	f := float64(n)
	for _, u := range []string{"B", "KiB", "MiB", "GiB", "TiB"} {
		if f < 1024 || u == "TiB" {
			return fmt.Sprintf("%.1f %s", f, u)
		}
		f /= 1024
	}
	return fmt.Sprintf("%d B", n)
}

func advBar(done, total int64, width int) string {
	if total <= 0 {
		return "[" + strings.Repeat("=", width) + "]"
	}
	fill := int(float64(done) / float64(total) * float64(width))
	if fill > width {
		fill = width
	}
	head := ""
	if fill < width {
		head = ">"
	}
	return "[" + strings.Repeat("=", fill) + head + strings.Repeat(" ", width-fill-len(head)) + "]"
}

func advETA(p *advProgress, now time.Time) string {
	sec := now.Sub(p.start).Seconds()
	if sec <= 0 || p.doneBytes <= 0 {
		return "about ? remaining"
	}
	speed := float64(p.doneBytes) / sec
	left := float64(p.totalBytes-p.doneBytes) / speed
	if left < 0 {
		left = 0
	}
	h := int(left) / 3600
	m := (int(left) % 3600) / 60
	s := int(left) % 60
	return fmt.Sprintf("about %dh %dm %ds remaining", h, m, s)
}

func advSpeed(p *advProgress, now time.Time) string {
	sec := now.Sub(p.start).Seconds()
	if sec <= 0 {
		return "0.0 B/s"
	}
	return humanBytes(int64(float64(p.doneBytes)/sec)) + "/s"
}

func (t *termRenderer) render(p *advProgress) {
	now := time.Now()
	// 末帧不受节流限制：否则最后一笔可能停在 97% 之类的中间态。
	done := p.filesDone >= p.filesTotal && p.doneBytes >= p.totalBytes
	if !done && now.Sub(t.last) < 100*time.Millisecond {
		return
	}
	t.last = now
	pct := func(d, tot int64) float64 {
		if tot <= 0 {
			return 100
		}
		return float64(d) / float64(tot) * 100
	}
	var b strings.Builder
	if t.drawn {
		b.WriteString("\x1b[3A") // 回到三行块顶
	}
	t.drawn = true
	l1 := fmt.Sprintf("%d/%d files  %s / %s %s %.1f %%",
		p.filesDone, p.filesTotal, humanBytes(p.doneBytes), humanBytes(p.totalBytes),
		advBar(p.doneBytes, p.totalBytes, 40), pct(p.doneBytes, p.totalBytes))
	l2 := fmt.Sprintf("Copying at %s (%s)", advSpeed(p, now), advETA(p, now))
	// 当前文件行：先算固定后缀，再把文件名截到剩余宽度，保住百分比不被切掉。
	suffix := fmt.Sprintf("  %s / %s %s %.1f %%",
		humanBytes(p.curDone), humanBytes(p.curTotal),
		advBar(p.curDone, p.curTotal, 30), pct(p.curDone, p.curTotal))
	name := p.curName
	if t.width > 0 {
		l1 = advTrunc(l1, t.width-1)
		l2 = advTrunc(l2, t.width-1)
		budget := t.width - 1 - len([]rune(suffix))
		if budget < 1 {
			budget = 1
		}
		name = advTrunc(name, budget)
	}
	l3 := name + suffix
	if t.width > 0 {
		l3 = advTrunc(l3, t.width-1) // 极窄终端连后缀都放不下时的最后保险
	}
	fmt.Fprintf(&b, "\r\x1b[K%s\n", l1)
	fmt.Fprintf(&b, "\r\x1b[K%s\n", l2)
	fmt.Fprintf(&b, "\r\x1b[K%s\n", l3)
	io.WriteString(t.w, b.String())
}

// advTrunc 按 rune 数截断到 n，避免超宽换行破坏三行块的原地重绘。
func advTrunc(s string, n int) string {
	if n <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func (t *termRenderer) note(line string) {
	if t.drawn {
		io.WriteString(t.w, "\x1b[3A")
		t.drawn = false
	}
	fmt.Fprintf(t.w, "\r\x1b[K%s\n", line)
}

func (t *termRenderer) finish() {
	// 三行块光标已在块下方一行，无需动作。
}

// ---- 引擎 ----

type advJob struct {
	src, dst string
	size     int64
	mode     fs.FileMode
	mtime    time.Time
}

func isDirPath(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// advTarget 按 cp 语义算单个源的落点：目标是已存在目录 → 进去挂 basename；
// 否则目标本身就是落点（单源改名/目录另名）。
func advTarget(src, dest string, destIsDir bool) string {
	if destIsDir {
		return filepath.Join(dest, filepath.Base(src))
	}
	return dest
}

func buildAdvJobs(o *advOptions) ([]advJob, error) {
	destIsDir := isDirPath(o.dest)
	if len(o.sources) > 1 && !destIsDir {
		return nil, fmt.Errorf("多个源时目标必须是目录：%s", o.dest)
	}
	var jobs []advJob
	for _, src := range o.sources {
		fi, err := os.Stat(src)
		if err != nil {
			return nil, fmt.Errorf("源不存在：%s", src)
		}
		root := advTarget(src, o.dest, destIsDir)
		if fi.IsDir() {
			if !o.recursive {
				return nil, fmt.Errorf("略过目录 %s（递归请加 -r）", src)
			}
			absSrc, _ := filepath.Abs(src)
			absRoot, _ := filepath.Abs(root)
			if absRoot == absSrc || strings.HasPrefix(absRoot, absSrc+string(filepath.Separator)) {
				return nil, fmt.Errorf("不能把目录拷进它自己：%s -> %s", src, root)
			}
			err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				rel, err := filepath.Rel(src, p)
				if err != nil {
					return err
				}
				dst := filepath.Join(root, rel)
				if d.IsDir() {
					return os.MkdirAll(dst, 0o755)
				}
				if d.Type()&fs.ModeSymlink != 0 {
					return nil // 符号链接不跟不拷，与 cp 默认行为相近
				}
				info, err := d.Info()
				if err != nil {
					return err
				}
				jobs = append(jobs, advJob{src: p, dst: dst, size: info.Size(), mode: info.Mode(), mtime: info.ModTime()})
				return nil
			})
			if err != nil {
				return nil, err
			}
		} else {
			jobs = append(jobs, advJob{src: src, dst: root, size: fi.Size(), mode: fi.Mode(), mtime: fi.ModTime()})
		}
	}
	return jobs, nil
}

type countWriter struct {
	w io.Writer
	p *advProgress
	r advRenderer
	n int64
}

func (c *countWriter) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	c.n += int64(n)
	c.p.curDone = c.n
	c.p.doneBytes += int64(n)
	c.r.render(c.p)
	return n, err
}

func copyAdvJob(j advJob, p *advProgress, r advRenderer) error {
	if err := os.MkdirAll(filepath.Dir(j.dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(j.src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(j.dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, j.mode.Perm())
	if err != nil {
		return err
	}
	p.curName = filepath.Base(j.src)
	p.curTotal = j.size
	p.curDone = 0
	cw := &countWriter{w: out, p: p, r: r}
	_, err = io.CopyBuffer(cw, in, make([]byte, 256*1024))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("拷贝 %s：%w", j.src, err)
	}
	_ = os.Chmod(j.dst, j.mode.Perm())
	_ = os.Chtimes(j.dst, time.Now(), j.mtime)
	p.filesDone++
	r.render(p)
	return nil
}

// runAdvanced 执行拷贝/移动。移动先试同盘改名（瞬间），失败再走拷贝+删源。
func runAdvanced(o *advOptions, r advRenderer) error {
	if o.move && len(o.sources) == 1 {
		target := advTarget(o.sources[0], o.dest, isDirPath(o.dest))
		if err := os.Rename(o.sources[0], target); err == nil {
			r.note(fmt.Sprintf("moved %s -> %s", o.sources[0], target))
			return nil
		}
	}
	jobs, err := buildAdvJobs(o)
	if err != nil {
		return err
	}
	p := &advProgress{filesTotal: len(jobs), start: time.Now()}
	for _, j := range jobs {
		p.totalBytes += j.size
	}
	for _, j := range jobs {
		if err := copyAdvJob(j, p, r); err != nil {
			return err
		}
	}
	r.render(p)
	if o.move {
		for _, src := range o.sources {
			if err := os.RemoveAll(src); err != nil {
				return fmt.Errorf("移动后删源 %s：%w", src, err)
			}
		}
	}
	return nil
}
