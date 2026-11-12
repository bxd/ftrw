package main

// filetask.go：文件管理器复制/移动/删除的异步任务引擎。
//
// StartTask 同步校验后立刻返回任务号，真正干活在 goroutine 里，进度经
// fm:task 事件推给前端（100ms 节流 + 末帧强制）。前端默认模态独占显示，
// 可最小化成发起栏左下角的浮动进度条（后台任务），主窗口照常操作。
// 取消是协作式的：文件之间与拷贝块之间查 cancel 旗标，已写入的部分留在原地。

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

const fileTaskEvent = "fm:task"

// FileTask 是一次文件操作的进度快照，事件 payload 与 StartTask 后的查询共用形状。
type FileTask struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`  // copy / move / delete / recycle
	State      string `json:"state"` // running / done / error / cancelled
	Items      int    `json:"items"` // 顶层条目数
	Dst        string `json:"dst"`   // 目标目录（删除类为空）
	TotalBytes int64  `json:"totalBytes"`
	DoneBytes  int64  `json:"doneBytes"`
	CurName    string `json:"curName"` // 正在处理的文件名（简要提示用）
	Error      string `json:"error"`
}

type fmTask struct {
	FileTask
	cancel   atomic.Bool
	lastEmit time.Time
}

// fmJob 是任务引擎的最小拷贝单元（一个文件），目录结构在扫描期已建好。
type fmJob struct {
	src  string
	dst  string
	size int64
	fi   os.FileInfo
}

// StartTask 发起一次异步文件操作，返回任务号。kind：copy/move/delete/recycle。
// 参数校验在这里同步做完，错误直接回传，不会起一个注定失败的任务。
func (s *FileService) StartTask(kind string, src []string, dst string) (string, error) {
	if len(src) == 0 {
		return "", fmt.Errorf("没有可操作的条目")
	}
	switch kind {
	case "copy":
		if isVirtualPath(dst) {
			return "", fmt.Errorf("不支持写入压缩包（目标在包内）：%s", dst)
		}
	case "move":
		if err := rejectVirtual(dst); err != nil {
			return "", err
		}
		if err := rejectVirtual(src...); err != nil {
			return "", err
		}
	case "delete", "recycle":
		if err := rejectVirtual(src...); err != nil {
			return "", err
		}
	default:
		return "", fmt.Errorf("未知操作类型：%s", kind)
	}
	t := &fmTask{FileTask: FileTask{
		ID:    fmt.Sprintf("fmt-%d", time.Now().UnixNano()),
		Kind:  kind,
		State: "running",
		Items: len(src),
		Dst:   dst,
	}}
	s.taskMu.Lock()
	if s.tasks == nil {
		s.tasks = make(map[string]*fmTask) // 零值服务（测试直构造）也能起任务
	}
	s.tasks[t.ID] = t
	s.taskMu.Unlock()
	go s.runFmTask(t, src)
	return t.ID, nil
}

// CancelTask 请求取消（协作式）：任务在下一个文件/块边界停下，状态落 cancelled。
func (s *FileService) CancelTask(id string) {
	s.taskMu.Lock()
	t := s.tasks[id]
	s.taskMu.Unlock()
	if t != nil {
		t.cancel.Store(true)
	}
}

// Conflicts 预检目标已存在的同名条目（顶层），供前端在复制/移动前弹覆盖确认。
// 目录同名也算冲突（内容会被逐文件覆盖合并）；源即目标的自拷贝不算。
func (s *FileService) Conflicts(src []string, dstDir string) []string {
	// 契约：永远回数组不回 null——nil slice 会被 JSON 序列化成 null，
	// 前端拿 null 渲染 .length 直接抛错、整个文件管理器子树被卸载。
	out := make([]string, 0)
	for _, from := range src {
		name := filepath.Base(from)
		if _, inner, ok := splitVirtualPath(from); ok {
			// 包内条目解出来的落点名是 inner 的末段（包根则用包名）。
			if b := filepath.Base(inner); b != "" && b != "." && b != string(filepath.Separator) {
				name = b
			}
		}
		to := filepath.Join(dstDir, name)
		if samePath(from, to) {
			continue
		}
		if _, err := os.Lstat(to); err == nil {
			out = append(out, name)
		}
	}
	return out
}

func (s *FileService) runFmTask(t *fmTask, src []string) {
	err := s.fmTaskWork(t, src)
	switch {
	case t.cancel.Load():
		t.State = "cancelled"
	case err != nil:
		t.State = "error"
		t.Error = err.Error()
	default:
		t.State = "done"
		t.DoneBytes = t.TotalBytes
	}
	s.emitTask(t, true)
	s.taskMu.Lock()
	delete(s.tasks, t.ID)
	s.taskMu.Unlock()
}

func (s *FileService) fmTaskWork(t *fmTask, src []string) error {
	switch t.Kind {
	case "copy":
		return s.fmCopyWork(t, src)
	case "move":
		return s.fmMoveWork(t, src)
	default: // delete / recycle
		return s.fmDeleteWork(t, src)
	}
}

// emitTask 推进度快照。force 为真不受节流（状态落定必须送达）。
func (s *FileService) emitTask(t *fmTask, force bool) {
	if s.taskEmit == nil {
		return
	}
	now := time.Now()
	if !force && now.Sub(t.lastEmit) < 100*time.Millisecond {
		return
	}
	t.lastEmit = now
	snap := t.FileTask
	s.taskEmit(&snap)
}

/* ---------- 扫描与进度 ---------- */

// fmScanSize 数一棵树的总字节（删除类任务的进度分母；目录本身不计）。
func fmScanSize(path string) int64 {
	var total int64
	_ = filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, e := d.Info(); e == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// fmBuildJobs 把源列表展开成逐文件任务，顺带建好目标目录结构、累计总字节。
func fmBuildJobs(src []string, dstDir string) ([]fmJob, int64, error) {
	var jobs []fmJob
	var total int64
	for _, from := range src {
		to := filepath.Join(dstDir, filepath.Base(from))
		if samePath(from, to) {
			return nil, 0, fmt.Errorf("源与目标为同一处，无法复制：%s", from)
		}
		fi, err := os.Lstat(from)
		if err != nil {
			return nil, 0, fmt.Errorf("源不存在：%s", from)
		}
		if fi.IsDir() {
			absFrom, _ := filepath.Abs(from)
			absTo, _ := filepath.Abs(to)
			if strings.EqualFold(absTo, absFrom) || strings.HasPrefix(strings.ToLower(absTo), strings.ToLower(absFrom)+string(filepath.Separator)) {
				return nil, 0, fmt.Errorf("不能把目录拷进它自己：%s", from)
			}
			err := filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				rel, err := filepath.Rel(from, p)
				if err != nil {
					return err
				}
				target := filepath.Join(to, rel)
				if d.IsDir() {
					return os.MkdirAll(target, 0o755)
				}
				if d.Type()&fs.ModeSymlink != 0 {
					return nil // 符号链接不跟不拷，与 ac 行为一致
				}
				info, err := d.Info()
				if err != nil {
					return err
				}
				jobs = append(jobs, fmJob{src: p, dst: target, size: info.Size(), fi: info})
				total += info.Size()
				return nil
			})
			if err != nil {
				return nil, 0, err
			}
		} else {
			jobs = append(jobs, fmJob{src: from, dst: to, size: fi.Size(), fi: fi})
			total += fi.Size()
		}
	}
	return jobs, total, nil
}

// fmCopyJobs 逐文件拷贝并按块报进度；块间查取消旗标。
func (s *FileService) fmCopyJobs(t *fmTask, jobs []fmJob) error {
	buf := make([]byte, 256*1024)
	for _, j := range jobs {
		if t.cancel.Load() {
			return nil
		}
		t.CurName = filepath.Base(j.src)
		if err := fmCopyOne(j, buf, func(n int64) {
			t.DoneBytes += n
			s.emitTask(t, false)
		}, &t.cancel); err != nil {
			return fmt.Errorf("复制失败（%s）：%v", j.src, err)
		}
		s.emitTask(t, false)
	}
	return nil
}

// fmCopyOne 拷一个文件：权限位与 mtime 保留，块回调报增量，cancel 命中即停。
func fmCopyOne(j fmJob, buf []byte, report func(int64), cancel *atomic.Bool) error {
	in, err := os.Open(j.src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(j.dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, j.fi.Mode().Perm())
	if err != nil {
		return err
	}
	defer out.Close()
	for {
		if cancel.Load() {
			return nil
		}
		n, rerr := in.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				return werr
			}
			report(int64(n))
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	_ = os.Chtimes(j.dst, j.fi.ModTime(), j.fi.ModTime())
	return nil
}

/* ---------- 三种操作 ---------- */

func (s *FileService) fmCopyWork(t *fmTask, src []string) error {
	// 目标目录不存在先补建（单文件拷贝时 fmBuildJobs 不会走到 MkdirAll 分支）。
	if err := os.MkdirAll(t.Dst, 0o755); err != nil {
		return fmt.Errorf("无法创建目标目录（%s）：%v", t.Dst, err)
	}
	// 压缩包内条目走解包通路（与同步 Copy 一致），按顶层条目报进度。
	var real []string
	for _, from := range src {
		if arc, inner, ok := splitVirtualPath(from); ok {
			if err := extractTo(arc, inner, t.Dst); err != nil {
				return fmt.Errorf("复制失败（%s）：%v", from, err)
			}
			t.DoneBytes += fmScanSize(filepath.Join(t.Dst, filepath.Base(inner)))
			s.emitTask(t, false)
			continue
		}
		real = append(real, from)
	}
	if len(real) == 0 {
		t.TotalBytes = t.DoneBytes
		return nil
	}
	jobs, total, err := fmBuildJobs(real, t.Dst)
	if err != nil {
		return err
	}
	t.TotalBytes = total + t.DoneBytes
	return s.fmCopyJobs(t, jobs)
}

func (s *FileService) fmMoveWork(t *fmTask, src []string) error {
	if err := os.MkdirAll(t.Dst, 0o755); err != nil {
		return fmt.Errorf("无法创建目标目录（%s）：%v", t.Dst, err)
	}
	// 先逐条试同卷改名（瞬时完成）；失败的攒起来走拷贝+删源。
	var slow []string
	for _, from := range src {
		to := filepath.Join(t.Dst, filepath.Base(from))
		if samePath(from, to) {
			continue
		}
		if err := os.Rename(from, to); err == nil {
			t.DoneBytes += fmScanSize(to)
			t.TotalBytes = t.DoneBytes // 分母随 instantaneous 部分水涨船高
			s.emitTask(t, false)
			continue
		}
		slow = append(slow, from)
	}
	if len(slow) == 0 {
		return nil
	}
	jobs, total, err := fmBuildJobs(slow, t.Dst)
	if err != nil {
		return err
	}
	t.TotalBytes += total
	if err := s.fmCopyJobs(t, jobs); err != nil {
		return err
	}
	for _, from := range slow {
		if t.cancel.Load() {
			return nil
		}
		if err := os.RemoveAll(from); err != nil {
			return fmt.Errorf("移动后删源失败（%s）：%v", from, err)
		}
	}
	return nil
}

func (s *FileService) fmDeleteWork(t *fmTask, src []string) error {
	// 先扫描各顶层条目的字节数当分母；删完一条计一条（删后再数就是 0 了）。
	sizes := make([]int64, len(src))
	for i, p := range src {
		sizes[i] = fmScanSize(p)
		t.TotalBytes += sizes[i]
	}
	for i, p := range src {
		if t.cancel.Load() {
			return nil
		}
		t.CurName = filepath.Base(p)
		var err error
		if t.Kind == "recycle" {
			err = recyclePath(p)
		} else {
			err = os.RemoveAll(p)
		}
		if err != nil {
			return fmt.Errorf("删除失败（%s）：%v", p, err)
		}
		t.DoneBytes += sizes[i]
		s.emitTask(t, false)
	}
	return nil
}
