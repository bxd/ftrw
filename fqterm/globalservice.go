package main

import (
	"log"
	"sync"
	"time"
)

// 跨应用热键引擎：把 globalkeys.json 编译进匹配器，用低级键盘钩子收键，
// 命中后把动作丢进队列，由独立 goroutine 执行。
//
// 三条来自钩子的硬约束，决定了这里的形状：
//  1. 回调跑在钩子线程上、受 LowLevelHooksTimeout 约束，所以 onKey 里只做匹配，
//     绝不调用窗口 API —— 那些调用内部是 InvokeSync，要等主线程，反过来还会拖死键盘链。
//  2. 保存热键表时不能持锁调用窗口 API，所以动作在队列消费者里执行，锁只保护匹配器本身。
//  3. 录制新热键时正在按的键会真的触发动作（呼出/隐藏窗口），因此界面打开期间要 paused，
//     连吞键也一并停掉，让按键原样走回系统。

// GlobalAction 是一条可绑定的动作，供界面下拉选择。
type GlobalAction struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

// globalKeyEngine 是跨应用热键的运行时。零值不可用，必须经 newGlobalKeyEngine。
type globalKeyEngine struct {
	mu      sync.Mutex
	match   *globalMatcher
	enabled bool
	paused  bool

	// 钩子只装一次：改绑定时重新编译匹配器即可，卸载再安装会让用户在改键的一瞬间丢键。
	hook      *kbdHook
	queue     chan string
	closeOnce sync.Once
	running   bool
}

func newGlobalKeyEngine() *globalKeyEngine {
	return &globalKeyEngine{queue: make(chan string, 8)}
}

// compile 重建匹配器。enabled 只影响匹配结果，钩子继续挂着，这样用户重新勾选启用时
// 不需要重装钩子（重装会有几百毫秒的输入空窗）。
func (e *globalKeyEngine) compile(gk GlobalKeymap) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.enabled = gk.Enabled
	e.match = newGlobalMatcher(gk)
	log.Printf("跨应用热键已装配：%d 条启用，双击窗口 %dms，总开关 %v", len(e.match.keys), gk.DoubleTapMs, gk.Enabled)
}

// onKey 是钩子回调入口（钩子线程）。返回值决定是否吞掉这次按键。
func (e *globalKeyEngine) onKey(ev gkEvent) bool {
	// 预览窗口的 ESC 排在总开关之前：用户关掉跨应用热键之后，ESC 关预览的能力不该跟着没。
	if hitEscWatch(ev) {
		return true
	}
	e.mu.Lock()
	if !e.enabled || e.match == nil {
		e.mu.Unlock()
		return false
	}
	if e.paused {
		// 暂停期间必须连内部按键表都不更新：否则「按下但没松」会被记成一直按着，
		// 恢复之后的第一次按下会被当成自动重复而丢掉。
		if !ev.Down {
			delete(e.match.down, ev.Vk) // 把按键表拉回与系统一致
			for _, b := range e.match.keys {
				b.phase = dtIdle
			}
		}
		e.mu.Unlock()
		return false
	}
	actions, swallow := e.match.feed(ev)
	e.mu.Unlock()
	for _, a := range actions {
		e.enqueue(a)
	}
	return swallow
}

func (e *globalKeyEngine) enqueue(action string) {
	select {
	case e.queue <- action:
	default:
		log.Printf("热键动作队列已满，丢弃 %s", action)
	}
}

// run 消费动作队列。actions 在 Start 之前装配好，运行期只读，因此不加锁。
func (e *globalKeyEngine) run(actions map[string]func()) {
	for action := range e.queue {
		fn, ok := actions[action]
		if !ok {
			log.Printf("跨应用热键动作 %q 不存在，可能被新版本改名", action)
			continue
		}
		// 单个动作 panic 不该让热键从此哑掉。
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("执行跨应用热键动作 %q panic: %v", action, r)
				}
			}()
			start := time.Now()
			fn()
			if d := time.Since(start); d > 200*time.Millisecond {
				log.Printf("热键动作 %s 耗时 %v，窗口操作过慢会拖住后续热键", action, d)
			}
		}()
	}
}

func (e *globalKeyEngine) setPaused(on bool) {
	e.mu.Lock()
	e.paused = on
	e.mu.Unlock()
}

// close 摘钩子、停队列。退出路径与「本平台不支持钩子」都走这里，可重复调用。
func (e *globalKeyEngine) close() {
	e.closeOnce.Do(func() {
		if h := e.hook; h != nil {
			h.stop()
			e.hook = nil
		}
		if e.running {
			close(e.queue)
		}
	})
}

// ListGlobalActions 给出界面可选的动作清单。与 defaultGlobalActionSpecs 共用同一份
// 描述，避免下拉里显示一套、引擎里执行另一套。
func (s *AppService) ListGlobalActions() []GlobalAction {
	return defaultGlobalActionSpecs()
}

// GetGlobalKeys 返回当前跨应用热键表（快照，锁只在取数据这几行内持有）。
func (s *AppService) GetGlobalKeys() GlobalKeymap {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	return normalizeGlobalKeymap(cloneGlobalKeymap(s.store.GlobalKey))
}

// DefaultGlobalKeys 供界面「恢复出厂」按钮取一份出厂表。
func (s *AppService) DefaultGlobalKeys() GlobalKeymap {
	return normalizeGlobalKeymap(DefaultGlobalKeymap())
}

// SaveGlobalKeys 落盘并立刻生效：重编译匹配器，必要时补装钩子。
//
// 钩子在应用跑起来之后才能装（它要一个自己的线程去泵消息，且必须能安全地往主线程
// Invoke），所以这里做惰性启动；启动失败不再静默 —— 用户在界面上改键时如果没生效，
// 日志会留下明确原因。
func (s *AppService) SaveGlobalKeys(gk GlobalKeymap) error {
	gk = normalizeGlobalKeymap(gk)
	s.store.mu.Lock()
	s.store.GlobalKey = gk
	s.store.mu.Unlock()
	if err := s.store.SaveGlobalKey(); err != nil {
		return err
	}
	s.applyGlobalKeys(gk)
	return nil
}

func (s *AppService) applyGlobalKeys(gk GlobalKeymap) {
	s.gkMu.Lock()
	defer s.gkMu.Unlock()
	if s.gkEngine == nil {
		return // 还没 Start（界面在启动完成前不可能打开，真发生就是顺序 bug）
	}
	s.gkEngine.compile(gk)
	if !s.gkEngine.running {
		s.startGlobalHookLocked()
	}
}

// SetGlobalKeysPaused 在热键编辑器打开期间静音跨应用热键。
// 由前端在对话框挂载/卸载时调用；不配对调用会一直静音，所以界面卸载一定要置 false。
func (s *AppService) SetGlobalKeysPaused(on bool) {
	s.gkMu.Lock()
	eng := s.gkEngine
	s.gkMu.Unlock()
	if eng != nil {
		eng.setPaused(on)
		log.Printf("跨应用热键%s", map[bool]string{true: "已暂停（热键编辑器打开中）", false: "已恢复"}[on])
	}
}

/* --------------------------------------------------------------------------- */
/* 装配                                                                         */
/* --------------------------------------------------------------------------- */

// globalActionSpec 是一条动作的元数据与实现。
type globalActionSpec struct {
	action GlobalAction
	run    func(*AppService)
}

// defaultGlobalActionSpecs 把动作表映成给界面看的清单。
func defaultGlobalActionSpecs() []GlobalAction {
	out := make([]GlobalAction, 0, len(globalActionTable))
	for _, a := range globalActionTable {
		out = append(out, a.action)
	}
	return out
}

// globalActionTable 是动作 id → 实现的唯一事实来源，界面下拉就从这里生成。三个动作
// 都已经在 appservice.go 里实现好了：这里只把「系统级触发」接到同一扇门上，不新增窗口逻辑。
var globalActionTable = []globalActionSpec{
	{
		action: GlobalAction{
			ID:          "quake.toggle",
			Label:       "呼出 / 收起 Quake 下拉窗口",
			Description: "已显示的 quake 收起，其余情况一律切到 quake 并呼出",
		},
		run: (*AppService).HotkeyToggleQuake,
	},
	{
		action: GlobalAction{
			ID:          "window.toggle",
			Label:       "显示 / 隐藏主窗口（保持当前形态）",
			Description: "等同托盘左键，quake 与普通窗口都不改",
		},
		run: (*AppService).TrayClick,
	},
	{
		action: GlobalAction{
			ID:          "mode.toggle",
			Label:       "普通窗口 / quake 模式互切",
			Description: "等同应用内 M-x M-q，切换后窗口一定可见并聚焦",
		},
		run: (*AppService).ToggleWindowModeSafe,
	},
}

func globalActionHandlers(s *AppService) map[string]func() {
	out := make(map[string]func(), len(globalActionTable))
	for _, a := range globalActionTable {
		fn := a.run
		out[a.action.ID] = func() { fn(s) }
	}
	return out
}

// startGlobalKeys 在 ApplicationStarted 之后装配引擎并挂钩子。
func (s *AppService) startGlobalKeys() {
	s.gkMu.Lock()
	defer s.gkMu.Unlock()
	if s.gkEngine != nil {
		return
	}
	eng := newGlobalKeyEngine()
	eng.running = true // 先定好再发布，免得 close() 与它抢读写
	eng.compile(s.globalKeysSnapshot())
	s.gkEngine = eng
	go eng.run(globalActionHandlers(s)) // 串行消费：一次只做一个窗口动作，不互相插队
	s.startGlobalHookLocked()
}

// globalKeysSnapshot 取配置里的跨应用热键表（store 的锁只在拿拷贝这几行里持有）。
func (s *AppService) globalKeysSnapshot() GlobalKeymap {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	return cloneGlobalKeymap(s.store.GlobalKey)
}

// startGlobalHookLocked 装钩子。调用方必须持有 gkMu。
func (s *AppService) startGlobalHookLocked() {
	eng := s.gkEngine
	if eng == nil || eng.hook != nil {
		return
	}
	if !kbdHookSupported {
		log.Printf("本平台没有低级键盘钩子实现，跨应用热键不会生效（应用内键序列不受影响）")
		return
	}
	h, err := startKbdHook(eng.onKey)
	if err != nil {
		log.Printf("跨应用热键未能启动：%v", err)
		return
	}
	eng.hook = h
	log.Printf("跨应用热键已启用（WH_KEYBOARD_LL）")
}

// stopGlobalKeys 摘钩子。退出前调用，免得关窗过程中还有按键触发窗口操作。
func (s *AppService) stopGlobalKeys() {
	s.gkMu.Lock()
	defer s.gkMu.Unlock()
	if s.gkEngine != nil {
		s.gkEngine.close()
	}
}

// cloneGlobalKeymap 复制一份表，避免把 store 里的切片交给 normalizeGlobalKeymap ——
// 它会原地改写 Bindings 切片，那等于在未持锁的情况下动配置。
func cloneGlobalKeymap(gk GlobalKeymap) GlobalKeymap {
	out := gk
	out.Bindings = append([]GlobalKey(nil), gk.Bindings...)
	return out
}

// ToggleWindowModeSafe 是给热键动作用的形式：ToggleWindowMode 带返回值，
// 放进 map[string]func() 里要显式丢掉，不能让返回值把调用形状带歪。
func (s *AppService) ToggleWindowModeSafe() { _ = s.ToggleWindowMode() }
