package main

import (
	"os"
	"path/filepath"
	"testing"
)

// 文件管理器的跨平台服务层测试：都在 t.TempDir() 里真跑，不碰系统目录。
// OpenPath / ListDrives 是平台相关且需要真实 shell/盘，这里不测（构建后手动验收）。

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func names(entries []FileEntry) map[string]FileEntry {
	m := make(map[string]FileEntry, len(entries))
	for _, e := range entries {
		m[e.Name] = e
	}
	return m
}

func TestFileListDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "hello")
	writeFile(t, filepath.Join(dir, "sub", "b.bin"), "x")
	if err := os.MkdirAll(filepath.Join(dir, "emptydir"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := (&FileService{}).ListDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := names(got)
	if len(got) != 3 {
		t.Fatalf("期望 3 条，实得 %d：%v", len(got), got)
	}
	a, ok := m["a.txt"]
	if !ok {
		t.Fatal("缺 a.txt")
	}
	if a.IsDir || a.Size != 5 {
		t.Errorf("a.txt 判错：isDir=%v size=%d", a.IsDir, a.Size)
	}
	if sub := m["sub"]; !sub.IsDir {
		t.Errorf("sub 应是目录")
	}
	if ed := m["emptydir"]; !ed.IsDir {
		t.Errorf("emptydir 应是目录")
	}
}

func TestFileListDirEmptyGoesHome(t *testing.T) {
	// 空路径不报错，兜到主目录（前端首帧可能传空）。
	got, err := (&FileService{}).ListDir("")
	if err != nil {
		t.Fatalf("空路径不该报错：%v", err)
	}
	_ = got // 主目录内容因机而异，只验不炸
}

func TestFileMakeDirRenameDelete(t *testing.T) {
	dir := t.TempDir()
	svc := &FileService{}

	sub := filepath.Join(dir, "newdir")
	if err := svc.MakeDir(sub); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(sub); err != nil || !fi.IsDir() {
		t.Fatalf("新目录没建出来：%v", err)
	}

	writeFile(t, filepath.Join(dir, "old.txt"), "data")
	if err := svc.Rename(filepath.Join(dir, "old.txt"), filepath.Join(dir, "renamed.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "old.txt")); !os.IsNotExist(err) {
		t.Error("重命名后旧名应不存在")
	}
	if _, err := os.Stat(filepath.Join(dir, "renamed.txt")); err != nil {
		t.Error("重命名后新名应存在")
	}

	if err := svc.Delete([]string{sub, filepath.Join(dir, "renamed.txt")}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sub); !os.IsNotExist(err) {
		t.Error("删除后目录应不存在")
	}
}

func TestFileCopyTree(t *testing.T) {
	root := t.TempDir()
	srcDir := filepath.Join(root, "src")
	writeFile(t, filepath.Join(srcDir, "top.txt"), "AAA")
	writeFile(t, filepath.Join(srcDir, "nested", "deep.txt"), "BBBB")
	dstDir := filepath.Join(root, "dst")
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := (&FileService{}).Copy([]string{srcDir}, dstDir); err != nil {
		t.Fatal(err)
	}
	// 结构应原样落在 dst/src 下。
	b, err := os.ReadFile(filepath.Join(dstDir, "src", "nested", "deep.txt"))
	if err != nil {
		t.Fatalf("目录树没完整复制：%v", err)
	}
	if string(b) != "BBBB" {
		t.Errorf("内容不符：%q", b)
	}
	// 源仍在。
	if _, err := os.Stat(filepath.Join(srcDir, "top.txt")); err != nil {
		t.Error("复制不该动源")
	}
}

func TestFileCopyRefusesSelfTarget(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "f.txt"), "x")
	// 目标目录就是源所在目录 → Base 拼接后同路径，应被拒绝而不是自己套自己。
	if err := (&FileService{}).Copy([]string{filepath.Join(dir, "f.txt")}, dir); err == nil {
		t.Fatal("复制到同一处应报错")
	}
}

func TestFileMove(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a")
	b := filepath.Join(root, "b")
	writeFile(t, filepath.Join(a, "move.txt"), "payload")
	if err := os.MkdirAll(b, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := (&FileService{}).Move([]string{filepath.Join(a, "move.txt")}, b); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(b, "move.txt")); err != nil {
		t.Error("目标缺文件")
	}
	if _, err := os.Stat(filepath.Join(a, "move.txt")); !os.IsNotExist(err) {
		t.Error("源应已移走")
	}
}

func TestFilePeekTrimsTail(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "t.txt")
	writeFile(t, p, "中文abc")
	s, err := (&FileService{}).PeekFile(p, 4) // 只给 4 字节，会切在「中」的多字节里
	if err != nil {
		t.Fatal(err)
	}
	if s != "" && len(s) > 4 {
		t.Errorf("peek 越界：%q", s)
	}
	// 完整读一遍应原样。
	full, _ := (&FileService{}).PeekFile(p, 0)
	if full != "中文abc" {
		t.Errorf("完整读不符：%q", full)
	}
}
