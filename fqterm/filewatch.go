package main

import (
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/wailsapp/wails/v3/pkg/application"
)

// filewatch.go：监听文件管理器两栏当前目录，外部改动（explorer 里复制/删除/
// 改名等）时推 fm:dir-changed，前端据此自动刷新对应栏。
//
// 引用计数：两栏可能同目录，0→1 才真 Add、1→0 才 Remove。防抖：批量操作事件
// 成风暴，按目录静默 300ms 合并成一次推送。watch 的 key 用前端原样路径串
// （事件 payload 原样回传，前端按 pane.path 精确比对），匹配事件时才 Clean。
// 虚拟路径（压缩包内）不监听。NewWatcher 失败整体降级为无自动刷新，不拖累其它功能。

const dirChangedEvent = "fm:dir-changed"

// dirWatchDebounce 是事件风暴的静默合并窗口。
const dirWatchDebounce = 300 * time.Millisecond

type dirWatcher struct {
	mu     sync.Mutex
	w      *fsnotify.Watcher
	refs   map[string]int
	timers map[string]*time.Timer
	emit   func(path string)
}

func newDirWatcher(emit func(string)) *dirWatcher {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil
	}
	dw := &dirWatcher{w: w, refs: make(map[string]int), timers: make(map[string]*time.Timer), emit: emit}
	go dw.loop()
	return dw
}

func (dw *dirWatcher) loop() {
	for {
		select {
		case ev, ok := <-dw.w.Events:
			if !ok {
				return
			}
			dw.touch(ev.Name)
		case _, ok := <-dw.w.Errors:
			if !ok {
				return
			}
			// 单条错误（权限等）不处理；目录真没了前端重载自然会报错。
		}
	}
}

// touch 把「受影响条目」映射回被监听的目录 key：事件名是子级全路径，
// 目录自身被改名/删除时事件名就是 key 本身。最多两栏，线性扫足够。
func (dw *dirWatcher) touch(child string) {
	dir := filepath.Dir(child)
	cc := filepath.Clean(child)
	dw.mu.Lock()
	defer dw.mu.Unlock()
	for key := range dw.refs {
		ck := filepath.Clean(key)
		if ck == dir || ck == cc {
			dw.schedule(key)
		}
	}
}

// schedule 重置该目录的防抖定时器；调用方持锁。
func (dw *dirWatcher) schedule(key string) {
	if t := dw.timers[key]; t != nil {
		t.Reset(dirWatchDebounce)
		return
	}
	dw.timers[key] = time.AfterFunc(dirWatchDebounce, func() {
		dw.mu.Lock()
		delete(dw.timers, key)
		dw.mu.Unlock()
		dw.emit(key)
	})
}

func (dw *dirWatcher) watch(path string) {
	if path == "" || isVirtualPath(path) {
		return
	}
	dw.mu.Lock()
	defer dw.mu.Unlock()
	dw.refs[path]++
	if dw.refs[path] == 1 {
		_ = dw.w.Add(path) // 目录不存在等失败静默降级
	}
}

func (dw *dirWatcher) unwatch(path string) {
	if path == "" || isVirtualPath(path) {
		return
	}
	dw.mu.Lock()
	defer dw.mu.Unlock()
	if dw.refs[path] == 0 {
		return
	}
	dw.refs[path]--
	if dw.refs[path] > 0 {
		return
	}
	delete(dw.refs, path)
	_ = dw.w.Remove(path)
	if t := dw.timers[path]; t != nil {
		t.Stop()
		delete(dw.timers, path)
	}
}

// bindWatch 在主窗建好后挂事件出口（Bind 调用，见 shellmenu.go）。
//
//wails:ignore
func (s *FileService) bindWatch(win *application.WebviewWindow) {
	s.dw = newDirWatcher(func(p string) { win.EmitEvent(dirChangedEvent, p) })
}

// WatchDir 登记一栏当前目录的兴趣（前端加载成功后调）。引用计数，同目录两栏只监听一份。
func (s *FileService) WatchDir(path string) {
	if s.dw != nil {
		s.dw.watch(path)
	}
}

// UnwatchDir 取消兴趣（栏离开目录 / 组件卸载时调）。
func (s *FileService) UnwatchDir(path string) {
	if s.dw != nil {
		s.dw.unwatch(path)
	}
}
