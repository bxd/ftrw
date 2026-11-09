package main

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 日志滚动的两条硬约束：超限确实切文件且不越积越多，以及切文件时不自锁。
// 后者是真实会犯的错 —— rotate 运行在 Write 持有的锁内，里面若调用 log.Printf，
// 输出会绕回同一个 writer 再次 Lock，sync.Mutex 不可重入，进程当场吊死。

func countHist(dir string) int {
	n := 0
	for _, name := range listFilesSafe(dir) {
		if strings.HasPrefix(name, logHistPrefix) {
			n++
		}
	}
	return n
}

func listFilesSafe(dir string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

func TestRunLogRotatesAndPrunes(t *testing.T) {
	const maxBytes, keep = 200, 2
	dir := t.TempDir()
	l, err := openRunLog(dir, maxBytes, keep)
	if err != nil {
		t.Fatalf("openRunLog: %v", err)
	}
	defer l.Close()

	line := strings.Repeat("x", 40) + "\n" // 41 字节，约 5 行触顶
	for i := 0; i < 60; i++ {
		if _, err := l.Write([]byte(line)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	cur := l.Path()
	b, err := os.ReadFile(cur)
	if err != nil {
		t.Fatalf("read current log: %v", err)
	}
	if int64(len(b)) > maxBytes {
		t.Errorf("当前日志 %d 字节，超过上限 %d 却没滚动", len(b), maxBytes)
	}
	if !strings.HasSuffix(string(b), line) {
		t.Error("最后一次写入没落到当前文件")
	}
	if got := countHist(dir); got > keep {
		t.Errorf("历史文件 %d 份，超过保留数 %d", got, keep)
	}
	if got := countHist(dir); got < 1 {
		t.Error("发生了滚动却没有留下任何历史文件")
	}
}

// 续跑时旧文件不丢：新实例以 O_APPEND 打开并记住已有大小。
func TestRunLogReopensInAppendMode(t *testing.T) {
	dir := t.TempDir()
	l1, err := openRunLog(dir, 1<<20, 4)
	if err != nil {
		t.Fatalf("openRunLog: %v", err)
	}
	if _, err := l1.Write([]byte("first\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	l1.Close()

	l2, err := openRunLog(dir, 1<<20, 4)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer l2.Close()
	if l2.n != int64(len("first\n")) {
		t.Errorf("重开后已计大小 = %d，期望 %d", l2.n, len("first\n"))
	}
	if _, err := l2.Write([]byte("second\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	b, err := os.ReadFile(l2.Path())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(b) != "first\nsecond\n" {
		t.Errorf("内容 = %q，期望保留上一次运行的行", string(b))
	}
}

// 回归：走标准库 log 的滚动路径不能自锁。
func TestRunLogNoDeadlockThroughStdlibLog(t *testing.T) {
	l, err := openRunLog(t.TempDir(), 200, 2)
	if err != nil {
		t.Fatalf("openRunLog: %v", err)
	}
	oldOut, oldFlags := log.Writer(), log.Flags()
	t.Cleanup(func() {
		log.SetOutput(oldOut)
		log.SetFlags(oldFlags)
		l.Close()
	})
	log.SetOutput(l)
	log.SetFlags(0)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 300; i++ {
			log.Printf("flood %d %s", i, strings.Repeat("y", 40))
		}
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("log.Printf 在滚动时自锁：rotate/prune 里不得调用 log.Printf")
	}

	b, err := os.ReadFile(l.Path())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(b), "flood 299") {
		t.Error("最后一条日志没写进当前文件")
	}
}

// 初始化失败时（这里用指向文件的目录制造错误）实例仍可安全写入，不能 panic。
func TestRunLogUsableWhenInitFails(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "logs")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("setup blocker: %v", err)
	}
	l, err := openRunLog(blocker, 200, 2)
	if err == nil {
		t.Fatal("期望初始化失败，却成功了")
	}
	if _, werr := l.Write([]byte("ignored\n")); werr != nil {
		t.Errorf("失败实例的 Write 应静默丢弃，得到 %v", werr)
	}
	l.Close()
}

// logTargets 把文件排在 stderr 之前：stderr 失败（GUI 构建双击启动）不能连累落盘。
func TestLogTargetsWritesFileEvenIfStderrFails(t *testing.T) {
	l, err := openRunLog(t.TempDir(), 1<<20, 2)
	if err != nil {
		t.Fatalf("openRunLog: %v", err)
	}
	t.Cleanup(l.Close)

	out := targetWriter(l, failingWriter{})
	if _, err := out.Write([]byte("line\n")); err == nil {
		t.Fatal("期望 MultiWriter 因 stderr 侧失败而返回错误")
	}
	b, err := os.ReadFile(l.Path())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(b) != "line\n" {
		t.Errorf("落盘内容 = %q，期望 stderr 失败不影响文件侧", string(b))
	}
}

type failingWriter struct{}

func (failingWriter) Write(p []byte) (int, error) { return 0, os.ErrPermission }
