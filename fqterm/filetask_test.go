package main

// filetask_test.go：异步文件任务引擎——完成态、进度单调、取消、删除计数。

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// taskCollector 收事件快照，供断言终态与进度单调。
type taskCollector struct {
	mu   sync.Mutex
	evs  []FileTask
	done chan struct{}
}

func newTaskCollector() *taskCollector {
	return &taskCollector{done: make(chan struct{})}
}

func (c *taskCollector) emit(t *FileTask) {
	c.mu.Lock()
	c.evs = append(c.evs, *t)
	last := t.State != "running"
	c.mu.Unlock()
	if last {
		close(c.done)
	}
}

func (c *taskCollector) wait(t *testing.T) []FileTask {
	t.Helper()
	select {
	case <-c.done:
	case <-time.After(10 * time.Second):
		t.Fatal("任务 10s 未落定")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]FileTask(nil), c.evs...)
}

func taskService(c *taskCollector) *FileService {
	s := &FileService{tasks: make(map[string]*fmTask), taskEmit: c.emit}
	return s
}

func TestFileTaskCopyProgress(t *testing.T) {
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "src")
	dstDir := filepath.Join(dir, "dst")
	writeFile(t, filepath.Join(srcDir, "a.bin"), "aaaa")
	writeFile(t, filepath.Join(srcDir, "sub", "b.bin"), "bbbbbb")
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		t.Fatal(err)
	}
	c := newTaskCollector()
	s := taskService(c)
	id, err := s.StartTask("copy", []string{srcDir}, dstDir)
	if err != nil {
		t.Fatal(err)
	}
	evs := c.wait(t)
	last := evs[len(evs)-1]
	if last.ID != id || last.State != "done" {
		t.Fatalf("终态不对：%+v", last)
	}
	if last.TotalBytes != 10 || last.DoneBytes != 10 {
		t.Errorf("字节数不对：%+v", last)
	}
	// 进度单调不回退
	var prev int64
	for _, e := range evs {
		if e.DoneBytes < prev {
			t.Errorf("进度回退：%d → %d", prev, e.DoneBytes)
		}
		prev = e.DoneBytes
	}
	if b, err := os.ReadFile(filepath.Join(dstDir, "src", "sub", "b.bin")); err != nil || string(b) != "bbbbbb" {
		t.Errorf("目标内容不对：%q err=%v", b, err)
	}
	if _, err := os.Stat(filepath.Join(srcDir, "a.bin")); err != nil {
		t.Error("拷贝后源应还在")
	}
}

func TestFileTaskMoveCrossTree(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	writeFile(t, filepath.Join(a, "m.bin"), "payload")
	if err := os.MkdirAll(b, 0o755); err != nil {
		t.Fatal(err)
	}
	c := newTaskCollector()
	s := taskService(c)
	if _, err := s.StartTask("move", []string{filepath.Join(a, "m.bin")}, b); err != nil {
		t.Fatal(err)
	}
	evs := c.wait(t)
	if last := evs[len(evs)-1]; last.State != "done" {
		t.Fatalf("终态不对：%+v", last)
	}
	if _, err := os.Stat(filepath.Join(b, "m.bin")); err != nil {
		t.Error("目标缺文件")
	}
	if _, err := os.Stat(filepath.Join(a, "m.bin")); !os.IsNotExist(err) {
		t.Error("源应已移走")
	}
}

func TestFileTaskDeleteCounts(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "x.bin"), "12345")
	writeFile(t, filepath.Join(dir, "y.bin"), "678")
	c := newTaskCollector()
	s := taskService(c)
	if _, err := s.StartTask("delete", []string{filepath.Join(dir, "x.bin"), filepath.Join(dir, "y.bin")}, ""); err != nil {
		t.Fatal(err)
	}
	evs := c.wait(t)
	last := evs[len(evs)-1]
	if last.State != "done" || last.TotalBytes != 8 || last.DoneBytes != 8 {
		t.Fatalf("删除进度不对：%+v", last)
	}
	if _, err := os.Stat(filepath.Join(dir, "x.bin")); !os.IsNotExist(err) {
		t.Error("x.bin 应已删除")
	}
}

func TestFileTaskCancel(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "big.bin")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	// 256MB 稀疏文件：够取消在块边界命中，又不真占盘。
	if err := f.Truncate(256 * 1024 * 1024); err != nil {
		t.Fatal(err)
	}
	f.Close()
	c := newTaskCollector()
	s := taskService(c)
	id, err := s.StartTask("copy", []string{src}, filepath.Join(dir, "out"))
	if err != nil {
		t.Fatal(err)
	}
	// 等首个进度事件再取消，确保任务真跑起来了。
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		n := len(c.evs)
		c.mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	s.CancelTask(id)
	evs := c.wait(t)
	if last := evs[len(evs)-1]; last.State != "cancelled" {
		t.Fatalf("应为 cancelled：%+v", last)
	}
}

func TestFileTaskRejectsBadKind(t *testing.T) {
	s := taskService(newTaskCollector())
	if _, err := s.StartTask("bogus", []string{"x"}, ""); err == nil {
		t.Error("未知类型应报错")
	}
	if _, err := s.StartTask("copy", nil, ""); err == nil {
		t.Error("空源应报错")
	}
}

func TestFileConflicts(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	writeFile(t, filepath.Join(src, "hit.txt"), "a")
	writeFile(t, filepath.Join(src, "miss.txt"), "b")
	if err := os.MkdirAll(filepath.Join(dst, "hit.txt"), 0o755); err != nil { // 目录同名也算冲突
		t.Fatal(err)
	}
	s := &FileService{}
	got := s.Conflicts([]string{filepath.Join(src, "hit.txt"), filepath.Join(src, "miss.txt")}, dst)
	if len(got) != 1 || got[0] != "hit.txt" {
		t.Errorf("冲突名单不对：%v", got)
	}
	// 源即目标不算冲突
	if got := s.Conflicts([]string{filepath.Join(dst, "hit.txt")}, dst); len(got) != 0 {
		t.Errorf("自拷贝不应算冲突：%v", got)
	}
	// 无冲突必须回非 nil 空数组：nil slice 序列化成 JSON null 会让前端渲染崩溃
	if got := s.Conflicts([]string{filepath.Join(src, "miss.txt")}, dst); got == nil {
		t.Error("无冲突时返回 nil slice（JSON 会变 null），应返回空数组")
	}
}
