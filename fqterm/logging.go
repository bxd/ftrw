package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// 产物是用 -ldflags="-H windowsgui" 链接的 GUI 子系统程序：双击启动时既没有控制台，
// stderr 也无处可去，标准库里那些 log.Printf（含 Wails 内部的 "[WebView2] ..."）会
// 全部丢失。这里把它们接到一个滚动文件，同时保留 stderr，让 wails3 task run 在终端
// 里依旧能直接看到输出。
const (
	logBaseName   = "ftrw.log" // 当前日志固定名，方便外部工具 tail
	logHistPrefix = "ftrw-"    // 历史文件前缀，与 logBaseName 同stem
	logMaxBytes   = 4 << 20    // 单文件上限，超过即切出历史文件
	logKeepNum    = 4          // 历史文件保留份数（加上当前文件共 5 份 ≈ 20MB 上限）
	logHistLayout = "20060102-150405"
)

// runLog 是按大小滚动的日志写入器。零值与初始化失败的实例都可安全使用：Write
// 退化为丢弃，避免调用方到处判空。上限与保留份数做成字段而非直接用常量，是为了
// 让滚动能在测试里用几百字节触发。
type runLog struct {
	dir      string
	maxBytes int64
	keep     int

	mu sync.Mutex
	f  *os.File
	n  int64 // f 已写字节数，用于判断滚动时机
}

// logDir 是滚动日志所在目录（%APPDATA%\FTRW\logs），托盘菜单的「打开日志目录」也指这里。
func logDir() string { return filepath.Join(configRoot(), "logs") }

// SetupRunLog 打开当前日志文件。返回的实例总是可直接使用；err 非空表示初始化
// 失败、本次运行不落盘，调用方应把 err 报一次后继续跑。
//
// 注：当前文件名不带 PID，同时跑两个实例会共享 ftrw.log（Windows 下共享句柄允许，
// 但写入位置与滚动会互相干扰）。要彻底解决得开 Options.SingleInstance，不属于日志本身的话题。
func SetupRunLog() (*runLog, error) {
	return openRunLog(logDir(), logMaxBytes, logKeepNum)
}

func openRunLog(dir string, maxBytes int64, keep int) (*runLog, error) {
	l := &runLog{dir: dir, maxBytes: maxBytes, keep: keep}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return l, fmt.Errorf("mkdir logs: %w", err)
	}
	path := l.Path()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return l, fmt.Errorf("open %s: %w", path, err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return l, fmt.Errorf("stat %s: %w", path, err)
	}
	l.f, l.n = f, st.Size()
	return l, nil
}

func (l *runLog) Path() string { return filepath.Join(l.dir, logBaseName) }

func (l *runLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return len(p), nil
	}
	if l.n+int64(len(p)) > l.maxBytes {
		f, n := l.f, l.n
		l.f, l.n = nil, 0
		l.rotate(f, n)
		if l.f == nil {
			return len(p), nil // 滚动后重开失败：本次运行不再落盘，这条只能丢弃
		}
	}
	n, err := l.f.Write(p)
	l.n += int64(n)
	return n, err
}

func (l *runLog) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		l.f.Close()
		l.f = nil
		l.n = 0
	}
}

// rotate 把当前文件改名成 ftrw-<时间戳>.log，重开一个空的 ftrw.log，并把超出保留
// 份数的历史文件删掉。调用时 l.mu 已持有、l.f 已被摘成 nil，本函数负责回填 l.f/l.n。
//
// 这里的告警绝不能走 log.Printf：log 的输出最终会回到同一个 writer，而我们是持着
// l.mu 进来的，sync.Mutex 不可重入，那样会当场自锁把整个进程吊死。滚动失败本身就
// 多半是磁盘/句柄出了问题，再往同一个文件写也没意义，所以一律退到 stderr。
func (l *runLog) rotate(f *os.File, n int64) {
	f.Close()
	if n > 0 {
		if err := os.Rename(l.Path(), l.histPath()); err != nil {
			fmt.Fprintf(os.Stderr, "日志滚动改名失败: %v\n", err)
		} else {
			l.prune()
		}
	}
	nf, err := os.OpenFile(l.Path(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "日志文件重开失败，本次运行不再落盘: %v\n", err)
		return
	}
	l.f, l.n = nf, 0
}

// histPath 挑一个没被占用的历史文件名。同一秒内滚两次（日志洪水）就顺延编号。
func (l *runLog) histPath() string {
	base := filepath.Join(l.dir, logHistPrefix+time.Now().Format(logHistLayout)+".log")
	path := base
	for i := 1; fileExists(path); i++ {
		path = fmt.Sprintf("%s.%d", base, i)
	}
	return path
}

// prune 按文件名删旧。时间戳是定宽 YYYYMMDD-HHMMSS，字典序即时间序，无需读 mtime。
// 同样运行在 rotate 的锁内，失败只报 stderr。
func (l *runLog) prune() {
	ents, err := os.ReadDir(l.dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取日志目录失败: %v\n", err)
		return
	}
	var old []string
	for _, e := range ents {
		name := e.Name()
		if strings.HasPrefix(name, logHistPrefix) && strings.Contains(name, ".log") {
			old = append(old, name)
		}
	}
	sort.Strings(old)
	for _, name := range old[:max(0, len(old)-l.keep)] {
		if err := os.Remove(filepath.Join(l.dir, name)); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "清理旧日志 %s: %v\n", name, err)
		}
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// newSystemLogger 把 Wails 自己的 slog 也接到 out。必须显式传进 Options.Logger：
// production 构建里 Wails 默认把系统日志丢进 io.Discard，出问题时无从查起。
//
// 时间格式手动改成与标准库 log 前缀一致：slog 默认输出 RFC3339，两种格式混在一个文件里
// 就按时间 grep 不了。保留 INFO 级别：除了 Platform Info（WebView2 运行时版本、系统
// 版本），启动时会多出几条静态资源的 Handling request，一共不过五行，值得换取证价值。
func newSystemLogger(out io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{
		Level: slog.LevelInfo,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				if ts, ok := a.Value.Any().(time.Time); ok {
					a.Value = slog.StringValue(ts.Format("2006-01-02 15:04:05.000"))
				}
			}
			return a
		},
	}))
}

// logTargets 组合落盘与 stderr：前者是诊断的唯一去处，后者让终端里跑起来的程序
// 仍能实时看到输出。
func logTargets(l *runLog) io.Writer { return targetWriter(l, os.Stderr) }

// targetWriter 的两个写入者顺序很重要 —— io.MultiWriter 遇到第一个错误就返回，把
// 文件写在前面，双击启动时 stderr 失败才不会连累落盘。
func targetWriter(l *runLog, console io.Writer) io.Writer {
	return io.MultiWriter(l, console)
}
