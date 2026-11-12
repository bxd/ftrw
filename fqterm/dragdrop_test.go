package main

import (
	"os"
	"path/filepath"
	"testing"
)

// 拖拽源物化：虚拟源解到临时目录保原名/保结构，真实源验存在，包根拒。
// 原生 DoDragDrop 需交互鼠标，构建后手动验收。

func TestMaterializeDragPaths(t *testing.T) {
	dir := t.TempDir()
	arc := filepath.Join(dir, "pack.zip")
	makeZip(t, arc)
	real := filepath.Join(dir, "real.txt")
	writeFile(t, real, "R")

	out, err := materializeDragPaths([]string{arc + `::sub\a.txt`, arc + "::sub", real, filepath.Join(dir, "missing.txt")})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 {
		t.Fatalf("应 3 条（单文件/子树/真实），得 %d：%v", len(out), out)
	}
	if filepath.Base(out[0]) != "a.txt" || filepath.Base(out[1]) != "sub" || out[2] != real {
		t.Fatalf("物化路径不对：%v", out)
	}
	if b, err := os.ReadFile(out[0]); err != nil || string(b) != "AAA" {
		t.Fatalf("解出内容不对：%q %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(out[1], "deep", "b.txt")); err != nil || string(b) != "BBB" {
		t.Fatalf("子树结构不对：%q %v", b, err)
	}
	defer os.RemoveAll(filepath.Dir(out[0]))

	if _, err := materializeDragPaths([]string{arc + "::"}); err == nil {
		t.Fatal("包根应拒")
	}
}
