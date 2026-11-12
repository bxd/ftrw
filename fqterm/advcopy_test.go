package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeRenderer 收最终进度快照与 note，供引擎断言用。
type fakeRenderer struct {
	last  advProgress
	notes []string
}

func (f *fakeRenderer) render(p *advProgress) { f.last = *p }
func (f *fakeRenderer) note(line string)      { f.notes = append(f.notes, line) }
func (f *fakeRenderer) finish()               {}

func TestParseAdvArgs(t *testing.T) {
	o, err := parseAdvArgs([]string{"-r", "-g", "a", "b", "dst"})
	if err != nil {
		t.Fatal(err)
	}
	if !o.recursive || len(o.sources) != 2 || o.sources[0] != "a" || o.dest != "dst" {
		t.Errorf("解析形状不对：%+v", o)
	}
	// -- 之后全是位置参数，哪怕长得像选项
	o, err = parseAdvArgs([]string{"--", "-weird", "dst"})
	if err != nil || o.sources[0] != "-weird" {
		t.Errorf("-- 后的位置参数被误判：%+v err=%v", o, err)
	}
	if _, err := parseAdvArgs([]string{"-h"}); err != errAdvHelp {
		t.Errorf("-h 应返回 errAdvHelp，got %v", err)
	}
	if _, err := parseAdvArgs([]string{"-x", "a", "b"}); err == nil {
		t.Error("未知选项应报错")
	}
	if _, err := parseAdvArgs([]string{"only"}); err == nil {
		t.Error("只有一个位置参数应报错")
	}
}

func writeFileT(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAdvCopyFileKeepsContentAndMtime(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	dst := filepath.Join(dir, "dst.bin")
	writeFileT(t, src, "hello advanced copy")
	mtime := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(src, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	r := &fakeRenderer{}
	if err := runAdvanced(&advOptions{sources: []string{src}, dest: dst}, r); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(dst)
	if err != nil || string(b) != "hello advanced copy" {
		t.Fatalf("目标内容不对：%q err=%v", b, err)
	}
	fi, err := os.Stat(dst)
	if err != nil || !fi.ModTime().Equal(mtime) {
		t.Errorf("mtime 未保留：got %v want %v", fi.ModTime(), mtime)
	}
	if r.last.doneBytes != int64(len("hello advanced copy")) || r.last.filesDone != 1 || r.last.filesTotal != 1 {
		t.Errorf("进度总量不对：%+v", r.last)
	}
}

func TestAdvCopyRecursive(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "tree")
	writeFileT(t, filepath.Join(src, "a.txt"), "aaa")
	writeFileT(t, filepath.Join(src, "sub", "b.txt"), "bbbb")
	dst := filepath.Join(dir, "out") // 不存在 → 目录另名落点
	r := &fakeRenderer{}
	if err := runAdvanced(&advOptions{recursive: true, sources: []string{src}, dest: dst}, r); err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]string{"a.txt": "aaa", filepath.Join("sub", "b.txt"): "bbbb"} {
		b, err := os.ReadFile(filepath.Join(dst, rel))
		if err != nil || string(b) != want {
			t.Errorf("%s 内容不对：%q err=%v", rel, b, err)
		}
	}
	if r.last.filesTotal != 2 || r.last.filesDone != 2 || r.last.doneBytes != 7 {
		t.Errorf("递归进度不对：%+v", r.last)
	}
	// 源还在（这是拷贝不是移动）
	if _, err := os.Stat(filepath.Join(src, "a.txt")); err != nil {
		t.Error("拷贝后源不应消失")
	}
}

func TestAdvCopyDirIntoExistingDir(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "tree")
	writeFileT(t, filepath.Join(src, "a.txt"), "x")
	dstDir := filepath.Join(dir, "dest")
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 目标是已存在目录 → 挂 basename 进去
	if err := runAdvanced(&advOptions{recursive: true, sources: []string{src}, dest: dstDir}, &fakeRenderer{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dstDir, "tree", "a.txt")); err != nil {
		t.Errorf("应落到 dest/tree/a.txt：%v", err)
	}
}

func TestAdvCopyErrors(t *testing.T) {
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "d")
	writeFileT(t, filepath.Join(srcDir, "f"), "x")
	f1 := filepath.Join(dir, "f1")
	f2 := filepath.Join(dir, "f2")
	writeFileT(t, f1, "1")
	writeFileT(t, f2, "2")

	// 目录不带 -r
	if err := runAdvanced(&advOptions{sources: []string{srcDir}, dest: filepath.Join(dir, "o")}, &fakeRenderer{}); err == nil || !strings.Contains(err.Error(), "-r") {
		t.Errorf("目录无 -r 应提示递归：%v", err)
	}
	// 多源目标不是目录
	if err := runAdvanced(&advOptions{sources: []string{f1, f2}, dest: filepath.Join(dir, "nope")}, &fakeRenderer{}); err == nil {
		t.Error("多源目标非目录应报错")
	}
	// 拷进自己
	if err := runAdvanced(&advOptions{recursive: true, sources: []string{srcDir}, dest: filepath.Join(srcDir, "inner")}, &fakeRenderer{}); err == nil {
		t.Error("目录拷进自己应报错")
	}
	// 源不存在
	if err := runAdvanced(&advOptions{sources: []string{filepath.Join(dir, "ghost")}, dest: dir}, &fakeRenderer{}); err == nil {
		t.Error("源不存在应报错")
	}
}

func TestAdvMoveFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "m.txt")
	dst := filepath.Join(dir, "moved.txt")
	writeFileT(t, src, "move me")
	r := &fakeRenderer{}
	if err := runAdvanced(&advOptions{move: true, sources: []string{src}, dest: dst}, r); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("移动后源应消失")
	}
	b, _ := os.ReadFile(dst)
	if string(b) != "move me" {
		t.Errorf("移动后内容不对：%q", b)
	}
	if len(r.notes) != 1 || !strings.HasPrefix(r.notes[0], "moved ") {
		t.Errorf("同盘改名应走 note 快速通道：%v", r.notes)
	}
}

func TestAdvMoveDirRecursive(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "tree")
	writeFileT(t, filepath.Join(src, "sub", "f"), "data")
	dst := filepath.Join(dir, "renamed")
	if err := runAdvanced(&advOptions{move: true, recursive: true, sources: []string{src}, dest: dst}, &fakeRenderer{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("移动后源目录应消失")
	}
	b, err := os.ReadFile(filepath.Join(dst, "sub", "f"))
	if err != nil || string(b) != "data" {
		t.Errorf("移动后内容不对：%q err=%v", b, err)
	}
}

func TestWSLToWindowsPath(t *testing.T) {
	cases := map[string]string{
		"/mnt/c/Users/x":  `C:\Users\x`,
		"/mnt/d":          `D:\`,
		"/home/u/file":    `\\wsl$\Ubuntu\home\u\file`,
		"/mnt/1/notdrive": `\\wsl$\Ubuntu\mnt\1\notdrive`,
		"relative/path":   "relative/path",
		".":               ".",
	}
	for in, want := range cases {
		if got := wslToWindowsPath(in, "Ubuntu"); got != want {
			t.Errorf("wslToWindowsPath(%q) = %q，want %q", in, got, want)
		}
	}
}

func TestWSLShimPath(t *testing.T) {
	if got := wslShimPath(`C:\Users\x\ftrw.exe`); got != "/mnt/c/Users/x/ftrw.exe" {
		t.Errorf("wslShimPath = %q", got)
	}
}

func TestTermRendererInPlaceRedraw(t *testing.T) {
	var buf strings.Builder
	r := &termRenderer{w: &buf}
	p := &advProgress{totalBytes: 100, filesTotal: 1, curTotal: 100, start: time.Now().Add(-time.Second)}
	r.render(p)
	first := buf.String()
	if strings.Contains(first, "\x1b[3A") {
		t.Errorf("首帧不应上移（还没画过块）：%q", first)
	}
	if n := len(strings.Split(first, "\n")); n != 4 { // 三行 + 末尾换行
		t.Errorf("首帧应为三行块：%q", first)
	}
	// 节流窗口内的中间帧被丢弃
	p.doneBytes = 30
	p.curDone = 30
	r.render(p)
	if buf.String() != first {
		t.Error("100ms 内的中间帧应被节流")
	}
	// 越过节流窗口的重绘：上移三行回到块顶原地刷新，不新增行
	r.last = time.Now().Add(-200 * time.Millisecond)
	r.render(p)
	second := buf.String()[len(first):]
	if !strings.HasPrefix(second, "\x1b[3A") {
		t.Errorf("重绘帧应先上移三行：%q", second)
	}
	if n := len(strings.Split(second, "\n")); n != 4 {
		t.Errorf("重绘帧应为三行块：%q", second)
	}
	// 末帧不受节流：刚画完立刻到 100% 也要画出来
	p.doneBytes = 100
	p.curDone = 100
	p.filesDone = 1
	r.render(p)
	last := buf.String()[len(first)+len(second):]
	if !strings.Contains(last, "100.0 %") {
		t.Errorf("末帧未被强制渲染：%q", last)
	}
}

// 长文件名 + 窄终端：每行截断到宽度内，三行块高度恒定，\x1b[3A 上移才能原地重绘。
func TestTermRendererTruncatesToWidth(t *testing.T) {
	var buf strings.Builder
	r := &termRenderer{w: &buf, width: 60}
	p := &advProgress{
		totalBytes: 100, doneBytes: 50, filesTotal: 1,
		curTotal: 100, curDone: 50,
		curName: strings.Repeat("very-long-filename-", 10),
		start:   time.Now().Add(-time.Second),
	}
	r.render(p)
	lines := strings.Split(buf.String(), "\n")
	if len(lines) != 4 { // 三行 + 末尾换行产生的空段
		t.Fatalf("应为三行块：%q", buf.String())
	}
	for i, line := range lines[:3] {
		clean := strings.ReplaceAll(strings.ReplaceAll(line, "\r", ""), "\x1b[K", "")
		if n := len([]rune(clean)); n > 59 {
			t.Errorf("第 %d 行超宽 %d>59：%q", i+1, n, clean)
		}
	}
	if !strings.Contains(lines[2], "%") {
		t.Errorf("截断后应保住百分比：%q", lines[2])
	}
}

func TestPlainRendererOnlyFinal(t *testing.T) {
	p := &advProgress{totalBytes: 10, filesTotal: 1}
	r := &plainRenderer{}
	r.render(p) // 未完成：不输出（无法直接断言 stdout，只验证不 panic 且幂等）
	p.doneBytes = 10
	p.filesDone = 1
	r.render(p)
	r.render(p)
	if !r.done {
		t.Error("完成后应置 done")
	}
}

func TestHumanBytesAndBar(t *testing.T) {
	if got := humanBytes(1536); got != "1.5 KiB" {
		t.Errorf("humanBytes(1536) = %q", got)
	}
	if got := humanBytes(5); got != "5.0 B" {
		t.Errorf("humanBytes(5) = %q", got)
	}
	full := advBar(10, 10, 8)
	if full != "[========]" {
		t.Errorf("满条形状不对：%q", full)
	}
	zero := advBar(0, 10, 8)
	if !strings.HasPrefix(zero, "[>") || len(zero) != 10 {
		t.Errorf("零进度条形状不对：%q", zero)
	}
}
