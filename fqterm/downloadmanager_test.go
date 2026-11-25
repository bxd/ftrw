package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func downloadTestService(root string) *DownloadManagerService {
	store := &Store{dir: root, Prefs: Preferences{ZmodemDir: root}}
	return NewDownloadManagerService(store, NewFileService())
}

func TestDownloadManagerListOnlyDirectories(t *testing.T) {
	root := filepath.Join(t.TempDir(), "download")
	svc := downloadTestService(root)
	oldDir := filepath.Join(root, "旧文章")
	newDir := filepath.Join(root, "新文章")
	for _, dir := range []string{oldDir, newDir, filepath.Join(oldDir, "nested")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(oldDir, "post.txt"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newDir, "a.txt"), []byte("1234"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newDir, "b.jpg"), []byte("123"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "散文件.txt"), []byte("ignore"), 0o644); err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-time.Hour)
	_ = os.Chtimes(oldDir, base, base)
	_ = os.Chtimes(newDir, base.Add(time.Minute), base.Add(time.Minute))

	got, err := svc.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "新文章" || got[1].Name != "旧文章" {
		t.Fatalf("列表应只含两个直接子目录并按时间倒序：%+v", got)
	}
	if got[0].FileCount != 2 || got[0].Size != 7 {
		t.Fatalf("新文章统计错误：%+v", got[0])
	}
	if got[1].FileCount != 1 || got[1].Size != 3 {
		t.Fatalf("旧文章统计错误：%+v", got[1])
	}
}

func TestDownloadManagerRootDirCreatesAndRejectsFile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing", "download")
	svc := downloadTestService(root)
	got, err := svc.RootDir()
	if err != nil || got != root {
		t.Fatalf("根目录 = %q, err=%v", got, err)
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		t.Fatalf("根目录未建立：%v", err)
	}

	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := downloadTestService(file).RootDir(); err == nil {
		t.Fatal("下载根目录指向文件时应报错")
	}
}

func TestDownloadManagerPreviewSplitArticle(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "post")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(dir, "post.TXT")
	payload := append([]byte("正文\n"), articleRecord([]byte("post.jpg"), []byte("image"))...)
	if err := os.WriteFile(original, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, split, err := splitDownloadedArticle(original, 0); err != nil || !split {
		t.Fatalf("准备拆分夹具失败：split=%v err=%v", split, err)
	}

	items, err := downloadTestService(root).Preview(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("应预览正文和附件而不重复原始容器：%+v", items)
	}
	if items[0].Name != "正文" || items[0].Kind != "text" || items[0].Path != filepath.Join(dir, "post_正文.txt") {
		t.Fatalf("正文预览项错误：%+v", items[0])
	}
	if items[1].Name != "post.jpg" || items[1].Kind != "image" {
		t.Fatalf("同主名附件不应被当作原始容器排除：%+v", items[1])
	}
	for _, item := range items {
		if item.URL == "" || item.Path == original {
			t.Fatalf("预览项签名或容器过滤错误：%+v", item)
		}
	}
}

func TestDownloadManagerPreviewOrdinaryFile(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "普通下载")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "readme.md")
	if err := os.WriteFile(path, []byte("# 普通文件"), 0o644); err != nil {
		t.Fatal(err)
	}
	items, err := downloadTestService(root).Preview(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Path != path || items[0].Name != "readme.md" || items[0].Kind != "text" {
		t.Fatalf("普通下载预览错误：%+v", items)
	}
}

func TestDownloadManagerDeleteValidatesBoundary(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "download")
	dir := filepath.Join(root, "文章")
	nested := filepath.Join(dir, "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "post.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}

	svc := downloadTestService(root)
	var recycled []string
	svc.recycle = func(paths []string) error {
		recycled = append(recycled, paths...)
		return nil
	}
	invalid := []string{"", root, nested, file, outside, filepath.Join(root, "missing"), filepath.Join(root, "文章", "..", "..", "outside")}
	for _, path := range invalid {
		if err := svc.Delete(path); err == nil {
			t.Errorf("越界路径不应删除：%q", path)
		}
	}
	if len(recycled) != 0 {
		t.Fatalf("越界请求已经到达回收站：%v", recycled)
	}
	if err := svc.Delete(dir); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(recycled, []string{dir}) {
		t.Fatalf("回收站收到 %v, want [%s]", recycled, dir)
	}
}

func TestDownloadManagerDeleteManyValidatesAllBeforeRecycle(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "download")
	first := filepath.Join(root, "第一篇")
	second := filepath.Join(root, "第二篇")
	outside := filepath.Join(base, "outside")
	for _, dir := range []string{first, second, outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	svc := downloadTestService(root)
	var recycled []string
	svc.recycle = func(paths []string) error {
		recycled = append(recycled, paths...)
		return nil
	}
	if err := svc.DeleteMany([]string{first, outside, second}); err == nil {
		t.Fatal("批量中包含越界路径时应拒绝删除")
	}
	if len(recycled) != 0 {
		t.Fatalf("批量校验失败后不应删除任何目录：%v", recycled)
	}
	if err := svc.DeleteMany([]string{first, second, first}); err != nil {
		t.Fatal(err)
	}
	if want := []string{first, second}; !reflect.DeepEqual(recycled, want) {
		t.Fatalf("回收站收到 %v, want %v", recycled, want)
	}
	recycled = nil
	if err := svc.DeleteMany(nil); err != nil {
		t.Fatal(err)
	}
	if len(recycled) != 0 {
		t.Fatalf("空批量不应调用回收站：%v", recycled)
	}
}

func TestDownloadManagerPreviewRejectsEscapeAndEmptyDirectory(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "download")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(root, "empty")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	svc := downloadTestService(root)
	for _, path := range []string{root, outside, filepath.Join(root, "..", "outside")} {
		if _, err := svc.Preview(path); err == nil || strings.Contains(err.Error(), "secret.txt") {
			t.Fatalf("越界预览应被拒绝且不读文件：path=%q err=%v", path, err)
		}
	}
	if _, err := svc.Preview(empty); err == nil {
		t.Fatal("空下载项应返回无可预览文件")
	}
}

func TestStoreDownloadDirUsesConfiguredAndDefault(t *testing.T) {
	configured := filepath.Join(t.TempDir(), "custom")
	store := &Store{dir: t.TempDir(), Prefs: Preferences{ZmodemDir: configured}}
	if got := store.DownloadDir(); got != configured {
		t.Fatalf("自定义下载目录 = %q, want %q", got, configured)
	}
	store.Prefs.ZmodemDir = ""
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := store.DownloadDir(), filepath.Join(filepath.Dir(exe), "download"); got != want {
		t.Fatalf("默认下载目录 = %q, want %q", got, want)
	}
}
