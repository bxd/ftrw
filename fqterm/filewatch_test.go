package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// filewatch 测试：真建目录真写文件，验事件推送与引用计数。
// 防抖窗口 300ms，断言超时给足余量。

func TestDirWatcherEventAndRefs(t *testing.T) {
	ch := make(chan string, 8)
	dw := newDirWatcher(func(p string) { ch <- p })
	if dw == nil {
		t.Skip("本平台 fsnotify 不可用")
	}
	dir := t.TempDir()

	dw.watch(dir)
	dw.watch(dir) // 两栏同目录：引用计数 2

	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("A"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-ch:
		if got != dir {
			t.Fatalf("事件应原样回传 key：%q != %q", got, dir)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("写文件后未收到事件")
	}

	// 退一次（计数 1）：仍应监听。
	dw.unwatch(dir)
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("B"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("引用计数 1 时仍应监听")
	}

	// 再退（计数 0）：不再推。
	dw.unwatch(dir)
	if err := os.WriteFile(filepath.Join(dir, "c.txt"), []byte("C"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-ch:
		t.Fatalf("计数归零后不应再推：%q", got)
	case <-time.After(800 * time.Millisecond):
	}
}

func TestDirWatcherVirtualIgnored(t *testing.T) {
	ch := make(chan string, 1)
	dw := newDirWatcher(func(p string) { ch <- p })
	if dw == nil {
		t.Skip("本平台 fsnotify 不可用")
	}
	dw.watch(`F:\x.zip::sub`)
	dw.unwatch(`F:\x.zip::sub`)
	dw.mu.Lock()
	n := len(dw.refs)
	dw.mu.Unlock()
	if n != 0 {
		t.Fatalf("虚拟路径不应进监听表：%d", n)
	}

	// 未 Bind（dw=nil）时 API 空转不炸。
	svc := &FileService{}
	svc.WatchDir(`F:\real`)
	svc.UnwatchDir(`F:\real`)
}
