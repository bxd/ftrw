package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/text/encoding/simplifiedchinese"
)

// 压缩包 VFS 测试：zip/tar/tgz 用标准库 writer 现造夹具，都在 t.TempDir() 里真跑。
// 7z/rar 无免费写入器，reader 以真实文件手动验收为主；这里只测分派与护栏。

func TestSplitVirtualPath(t *testing.T) {
	cases := []struct {
		in    string
		arc   string
		inner string
		ok    bool
	}{
		{`F:\docs\pack.zip`, "", "", false},
		{`F:\docs\pack.zip::`, `F:\docs\pack.zip`, "", true},
		{`F:\docs\pack.zip::sub\dir`, `F:\docs\pack.zip`, `sub\dir`, true},
		{`F:\docs\pack.zip::sub/dir/`, `F:\docs\pack.zip`, `sub\dir`, true},
		{`F:\a::b::c`, `F:\a`, `b::c`, true}, // 按第一个 :: 拆
	}
	for _, c := range cases {
		arc, inner, ok := splitVirtualPath(c.in)
		if ok != c.ok || arc != c.arc || inner != c.inner {
			t.Errorf("splitVirtualPath(%q) = (%q,%q,%v), want (%q,%q,%v)", c.in, arc, inner, ok, c.arc, c.inner, c.ok)
		}
	}
}

func TestNormalizeInnerGuards(t *testing.T) {
	// zip-slip：.. 段与绝对路径一律归空，列举与解出都见不到。
	for _, bad := range []string{`../evil`, `a/../../evil`, `/abs/path`, `C:\evil`, `..`, `a/..`} {
		if got := normalizeInner(bad); got != "" {
			t.Errorf("normalizeInner(%q) = %q, want 空（护栏）", bad, got)
		}
	}
	if got := normalizeInner("a/b//c.txt"); got != `a\b\c.txt` {
		t.Errorf("斜杠归一失败：%q", got)
	}
}

func TestOpenArchiveDispatch(t *testing.T) {
	if _, err := openArchive(`F:\x.txt`); err == nil {
		t.Error("不认识的扩展名应报错")
	}
	if _, err := openArchive(`F:\x.gz`); err == nil {
		t.Error("独立 .gz（非 tar.gz）应报错")
	}
}

// makeZip 造测试 zip：top.txt / sub/a.txt / sub/deep/b.txt / emptydir/（显式目录项）。
func makeZip(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w := zip.NewWriter(f)
	for name, content := range map[string]string{
		"top.txt":        "TOP",
		"sub/a.txt":      "AAA",
		"sub/deep/b.txt": "BBB",
	} {
		fw, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.Create("emptydir/"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestVfsZipListAndStat(t *testing.T) {
	dir := t.TempDir()
	arc := filepath.Join(dir, "pack.zip")
	makeZip(t, arc)

	root, err := listVirtualDir(arc, "")
	if err != nil {
		t.Fatal(err)
	}
	m := names(root)
	if len(root) != 3 {
		t.Fatalf("包根应 3 项（top.txt/sub/emptydir），得 %d：%+v", len(root), root)
	}
	if e, ok := m["top.txt"]; !ok || e.IsDir || e.Size != 3 {
		t.Errorf("top.txt 条目不对：%+v", m["top.txt"])
	}
	if e, ok := m["sub"]; !ok || !e.IsDir || e.Path != arc+"::sub" {
		t.Errorf("合成目录 sub 不对：%+v", m["sub"])
	}
	if e, ok := m["emptydir"]; !ok || !e.IsDir {
		t.Errorf("显式目录项 emptydir 丢了：%+v", m["emptydir"])
	}

	sub, err := listVirtualDir(arc, "sub")
	if err != nil {
		t.Fatal(err)
	}
	sm := names(sub)
	if len(sub) != 2 {
		t.Fatalf("sub 应 2 项（a.txt/deep），得 %d：%+v", len(sub), sub)
	}
	if e, ok := sm["a.txt"]; !ok || e.Path != arc+`::sub\a.txt` {
		t.Errorf("a.txt 虚拟路径不对：%+v", sm["a.txt"])
	}
	if e, ok := sm["deep"]; !ok || !e.IsDir {
		t.Errorf("深层合成目录 deep 不对：%+v", sm["deep"])
	}

	if st, err := statVirtual(arc, `sub\deep`); err != nil || !st.IsDir {
		t.Errorf("statVirtual 合成目录失败：%+v %v", st, err)
	}
	if st, err := statVirtual(arc, `sub\a.txt`); err != nil || st.IsDir || st.Size != 3 {
		t.Errorf("statVirtual 文件失败：%+v %v", st, err)
	}
	if _, err := statVirtual(arc, "不存在"); err == nil {
		t.Error("不存在的条目应报错")
	}
}

func TestVfsExtractAndCopy(t *testing.T) {
	dir := t.TempDir()
	arc := filepath.Join(dir, "pack.zip")
	makeZip(t, arc)
	out := filepath.Join(dir, "out")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}

	// 单文件解出
	if err := extractTo(arc, `sub\a.txt`, out); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(out, "a.txt")); err != nil || string(b) != "AAA" {
		t.Fatalf("单文件解出内容不对：%q %v", b, err)
	}

	// 子树解出保结构
	if err := extractTo(arc, "sub", out); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(out, "sub", "deep", "b.txt")); err != nil || string(b) != "BBB" {
		t.Fatalf("子树解出结构不对：%q %v", b, err)
	}

	// FileService.Copy 虚拟源
	dst := filepath.Join(dir, "dst")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	svc := &FileService{}
	if err := svc.Copy([]string{arc + "::sub"}, dst); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "sub", "a.txt")); err != nil || string(b) != "AAA" {
		t.Fatalf("Copy 虚拟源落地不对：%q %v", b, err)
	}

	// 复制进包应拒
	if err := svc.Copy([]string{filepath.Join(out, "a.txt")}, arc+"::"); err == nil {
		t.Fatal("复制进包应报错")
	}

	// PeekFile 虚拟路径
	s, err := svc.PeekFile(arc+"::top.txt", 0)
	if err != nil || s != "TOP" {
		t.Fatalf("PeekFile 虚拟路径失败：%q %v", s, err)
	}
}

func TestVfsWriteGates(t *testing.T) {
	svc := &FileService{}
	v := `F:\x.zip::a.txt`
	if err := svc.MakeDir(v); err == nil {
		t.Error("MakeDir 包内应拒")
	}
	if err := svc.Rename(v, `F:\x.zip::b.txt`); err == nil {
		t.Error("Rename 包内应拒")
	}
	if err := svc.Delete([]string{v}); err == nil {
		t.Error("Delete 包内应拒")
	}
	if err := svc.Recycle([]string{v}); err == nil {
		t.Error("Recycle 包内应拒")
	}
	if err := svc.Move([]string{v}, `F:\`); err == nil {
		t.Error("Move 包内源应拒")
	}
	if err := svc.Move([]string{`F:\real.txt`}, `F:\x.zip::`); err == nil {
		t.Error("Move 包内目标应拒")
	}
}

// makeTar 造测试 tar（可选 gzip 层）：故意不写显式目录项，验目录合成。
func makeTar(t *testing.T, path string, gz bool) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var w io.Writer = f
	var gzw *gzip.Writer
	if gz {
		gzw = gzip.NewWriter(f)
		w = gzw
	}
	tw := tar.NewWriter(w)
	for name, content := range map[string]string{"top.txt": "TOP", "sub/a.txt": "AAA"} {
		h := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), ModTime: time.Now()}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if gzw != nil {
		if err := gzw.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestVfsTarSynthDirs(t *testing.T) {
	for _, v := range []struct {
		name string
		gz   bool
	}{
		{"pack.tar", false},
		{"pack.tar.gz", true},
		{"pack.tgz", true},
	} {
		dir := t.TempDir()
		arc := filepath.Join(dir, v.name)
		makeTar(t, arc, v.gz)

		root, err := listVirtualDir(arc, "")
		if err != nil {
			t.Fatalf("%s: %v", v.name, err)
		}
		m := names(root)
		if e, ok := m["sub"]; !ok || !e.IsDir {
			t.Errorf("%s: 无显式目录项时应合成 sub：%+v", v.name, root)
		}
		sub, err := listVirtualDir(arc, "sub")
		if err != nil || len(sub) != 1 || sub[0].Name != "a.txt" {
			t.Errorf("%s: sub 列举不对：%+v %v", v.name, sub, err)
		}
		out := filepath.Join(dir, "out")
		if err := os.MkdirAll(out, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := extractTo(arc, "sub", out); err != nil {
			t.Fatalf("%s: 解出失败 %v", v.name, err)
		}
		if b, err := os.ReadFile(filepath.Join(out, "sub", "a.txt")); err != nil || string(b) != "AAA" {
			t.Errorf("%s: 解出内容不对：%q %v", v.name, b, err)
		}
	}
}

func TestVfsExtractTemp(t *testing.T) {
	dir := t.TempDir()
	arc := filepath.Join(dir, "pack.zip")
	makeZip(t, arc)
	tmp, err := extractTemp(arc, `sub\a.txt`)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(filepath.Dir(tmp))
	if filepath.Base(tmp) != "a.txt" {
		t.Errorf("临时文件应保留原名：%s", tmp)
	}
	if !strings.HasPrefix(filepath.Base(filepath.Dir(tmp)), "ftrw-arc-") {
		t.Errorf("临时目录应叫 ftrw-arc-*：%s", tmp)
	}
	if b, err := os.ReadFile(tmp); err != nil || string(b) != "AAA" {
		t.Errorf("临时文件内容不对：%q %v", b, err)
	}
}

// TestVfsGbkNames 中文 Windows 压缩包用 GBK 存文件名且不置 UTF-8 标志：
// 标准库按原始字节读，不修编码就是乱码。用 GBK 原始字节造夹具验列举/解出。
func TestVfsGbkNames(t *testing.T) {
	gbk := func(s string) string {
		t.Helper()
		b, err := simplifiedchinese.GBK.NewEncoder().String(s)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	dir := t.TempDir()

	// zip：writer 对非法 UTF-8 名不置 UTF-8 标志，恰好复现中文 Windows 包。
	arc := filepath.Join(dir, "gbk.zip")
	f, err := os.Create(arc)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(f)
	fw, err := w.Create(gbk("报表.xlsx"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte("X")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()

	zr, err := zip.OpenReader(arc)
	if err != nil {
		t.Fatal(err)
	}
	if !zr.File[0].NonUTF8 {
		t.Fatal("夹具应是未置 UTF-8 标志的 GBK 包")
	}
	zr.Close()

	root, err := listVirtualDir(arc, "")
	if err != nil {
		t.Fatal(err)
	}
	m := names(root)
	if e, ok := m["报表.xlsx"]; !ok || e.Path != arc+"::报表.xlsx" {
		t.Fatalf("GBK 文件名未修好：%+v", root)
	}
	tmp, err := extractTemp(arc, "报表.xlsx")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(filepath.Dir(tmp))
	if filepath.Base(tmp) != "报表.xlsx" {
		t.Errorf("解出应保留中文原名：%s", tmp)
	}

	// tar：无编码标志，走「非法 UTF-8 → GBK」启发，目录名同样要修。
	tarc := filepath.Join(dir, "gbk.tar")
	tf, err := os.Create(tarc)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(tf)
	h := &tar.Header{Name: gbk("中文/笔记.txt"), Mode: 0o644, Size: 1, ModTime: time.Now()}
	if err := tw.WriteHeader(h); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("Y")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	tf.Close()

	troot, err := listVirtualDir(tarc, "")
	if err != nil {
		t.Fatal(err)
	}
	tm := names(troot)
	if e, ok := tm["中文"]; !ok || !e.IsDir {
		t.Fatalf("tar GBK 目录名未修好：%+v", troot)
	}
	sub, err := listVirtualDir(tarc, "中文")
	if err != nil || len(sub) != 1 || sub[0].Name != "笔记.txt" {
		t.Fatalf("tar GBK 子级列举不对：%+v %v", sub, err)
	}
}
