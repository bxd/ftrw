package main

import (
	"strings"
	"testing"
	"time"
)

// ── 语法 ────────────────────────────────────────────────────────────────

func TestParseGKTokenCanonical(t *testing.T) {
	ok := map[string]string{
		"M-`":        "M-`",   // 出厂那条：Alt+反引号
		"C-M-t":      "C-M-t", // 修饰顺序归一
		"M-C-t":      "C-M-t",
		"C-X":        "C-x", // 字母统一小写（匹配本就不分大小写）
		"S-a":        "S-a", // 显式写了 S- 就保留：那是「按下 Shift」的硬要求
		"<F1>":       "<F1>",
		"S-<F1>":     "S-<F1>", // 前缀 + 尖括号混写
		"C-<Esc>":    "C-<ESC>",
		"esc":        "<ESC>",
		"PageUp":     "<PGUP>",
		"<C-M>":      "<C-M>", // 纯修饰键
		"<ALT-CTRL>": "<C-M>", // 别名 + 乱序
		"<W>":        "<W>",
		"!":          "S-!", // 上档符号拆成 1 + Shift
		"C-!":        "C-S-!",
		"'":          "'",
		"<SPC>":      "<SPC>",
	}
	for in, want := range ok {
		got, err := parseGKToken(in)
		if err != nil {
			t.Errorf("parseGKToken(%q) 报错：%v", in, err)
			continue
		}
		if got.String() != want {
			t.Errorf("parseGKToken(%q) = %q, 期望 %q", in, got.String(), want)
		}
	}
	// 别名写法与简写必须是同一个 token
	for _, pair := range [][2]string{{"<CTRL-ALT>", "<C-M>"}, {"<Alt>", "<M>"}, {"<enter>", "<RET>"}} {
		a, err1 := parseGKToken(pair[0])
		b, err2 := parseGKToken(pair[1])
		if err1 != nil || err2 != nil {
			t.Errorf("%q / %q 解析失败: %v / %v", pair[0], pair[1], err1, err2)
			continue
		}
		if a.String() != b.String() {
			t.Errorf("%q 与 %q 规范化后不同：%q vs %q", pair[0], pair[1], a, b)
		}
	}
}

func TestParseGKTokenRejects(t *testing.T) {
	for _, bad := range []string{"", "  ", "C-M-S-x-y", "<>", "foo", "<F25>", "M-<F0>", "AB", "M-x p"} {
		if _, err := parseGKToken(bad); err == nil {
			t.Errorf("parseGKToken(%q) 本该报错却通过了", bad)
		}
	}
}

// ── 形状 ────────────────────────────────────────────────────────────────

func TestCompileGlobalKeyShapes(t *testing.T) {
	must := func(keys string) *compiledGK {
		c, err := compileGlobalKey(GlobalKey{Keys: keys, Action: "quake.toggle", Enabled: true})
		if err != nil {
			t.Fatalf("compileGlobalKey(%q) 失败: %v", keys, err)
		}
		return c
	}
	if got := must("M-`"); got.kind != kindChord {
		t.Errorf("M-` 应编译成组合键，得到 kind=%d", got.kind)
	}
	if got := must("<C-M> <C-M>"); got.kind != kindDoubleTap {
		t.Errorf("<C-M> <C-M> 应编译成双击，得到 kind=%d", got.kind)
	}
	if got := must("<F9> <F9>"); got.kind != kindDoubleTap {
		t.Errorf("<F9> <F9> 该允许（功能键不会打字）")
	}
	for _, bad := range []string{
		"<C-M>",         // 纯修饰键不能单独成键
		"C-x <SPC>",     // 两段不同
		"M-x p",         // 多段序列跨应用做不到
		"t",             // 不带修饰键的组合键会吞掉全局打字
		"t t",           // 不带修饰键的字符双击会在打字打到 tt 时误触发
		"<C-M> <C-M-S>", // 两段不同
	} {
		if _, err := compileGlobalKey(GlobalKey{Keys: bad, Action: "quake.toggle"}); err == nil {
			t.Errorf("compileGlobalKey(%q) 本该报错却通过了", bad)
		}
	}
}

func TestNewGlobalMatcherSkipsDisabledAndInvalid(t *testing.T) {
	gk := GlobalKeymap{DoubleTapMs: 400, Bindings: []GlobalKey{
		{Keys: "M-`", Action: "quake.toggle", Enabled: true},
		{Keys: "C-t", Action: "quake.toggle", Enabled: false},
		{Keys: "t", Action: "quake.toggle", Enabled: true}, // 语法非法
		{Keys: "C-y", Action: "", Enabled: true},           // 没动作
	}}
	m := newGlobalMatcher(gk)
	if len(m.keys) != 1 {
		t.Fatalf("只该留下 1 条有效绑定，实际 %d 条", len(m.keys))
	}
	if m.keys[0].token.String() != "M-`" {
		t.Errorf("留下的是 %q，期望 M-`", m.keys[0].token)
	}
}

// ── 匹配：模拟事件序列 ──────────────────────────────────────────────────

type gkSim struct {
	t   *testing.T
	m   *globalMatcher
	now time.Time
}

func sim(t *testing.T, keys ...GlobalKey) *gkSim {
	t.Helper()
	gk := GlobalKeymap{Enabled: true, DoubleTapMs: 400, Bindings: keys}
	return &gkSim{t: t, m: newGlobalMatcher(gk), now: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
}

func (s *gkSim) step(ms int) { s.now = s.now.Add(time.Duration(ms) * time.Millisecond) }

// press 按下某键，mods 传「按下之后」的修饰键集合（钩子侧就是这么校正的）。
func (s *gkSim) press(vk uint16, mods modMask, ms int) ([]string, bool) {
	s.step(ms)
	return s.m.feed(gkEvent{Vk: vk, Down: true, Mods: mods, At: s.now})
}

func (s *gkSim) release(vk uint16, mods modMask, ms int) ([]string, bool) {
	s.step(ms)
	return s.m.feed(gkEvent{Vk: vk, Down: false, Mods: mods, At: s.now})
}

func actionsOf(got []string) string { return strings.Join(got, ",") }

// first 取 press/release 返回的动作列表，用在「只关心触发了什么、不关心吞不吞键」的断言里。
func first(a []string, _ bool) []string { return a }

// 字母键的 VK 就是它的大写 ASCII，测试里用常量表达更直观。
const (
	vkT = uint16('T')
	vkC = uint16('C')
)

func bind(action, keys string) GlobalKey {
	return GlobalKey{Keys: keys, Action: action, Enabled: true}
}

func TestChordFiresAndSwallows(t *testing.T) {
	s := sim(t, bind("quake.toggle", "M-`"))
	// Alt 按下不触发，反引号按下才触发
	if a, sw := s.press(vkLAlt, modAlt, 10); len(a) != 0 || sw {
		t.Errorf("只按 Alt 不该触发：%v swallow=%v", a, sw)
	}
	a, sw := s.press(vkOem3, modAlt, 30)
	if actionsOf(a) != "quake.toggle" || !sw {
		t.Errorf("Alt+` 应触发并吞键，得到 %v swallow=%v", a, sw)
	}
	s.release(vkOem3, modAlt, 40)
	s.release(vkLAlt, 0, 20)
}

func TestChordToleratesUnrequestedShift(t *testing.T) {
	// 美式键盘上 Alt+~ 就是 Alt+Shift+`，同一物理键，必须都能呼出
	s := sim(t, bind("quake.toggle", "M-`"))
	s.press(vkLAlt, modAlt, 10)
	s.press(vkLShift, modAlt|modShift, 5)
	a, sw := s.press(vkOem3, modAlt|modShift, 5)
	if actionsOf(a) != "quake.toggle" || !sw {
		t.Errorf("Alt+Shift+` 应命中 M-`（Shift 未被要求），得到 %v swallow=%v", a, sw)
	}
}

func TestChordRejectsExtraModifier(t *testing.T) {
	s := sim(t, bind("quake.toggle", "M-`"))
	s.press(vkLCtrl, modCtrl, 10)
	s.press(vkLAlt, modCtrl|modAlt, 5)
	if a, _ := s.press(vkOem3, modCtrl|modAlt, 5); len(a) != 0 {
		t.Errorf("Ctrl+Alt+` 不该命中 M-`，却触发了 %v", a)
	}
}

func TestChordExplicitShiftWinsOverLoose(t *testing.T) {
	// 表里同时有 M-` 与 M-S-`：精确匹配优先，否则先出现的那条会永久遮蔽带 Shift 的那条
	s := sim(t, bind("loose", "M-`"), bind("exact", "M-S-`"))
	s.press(vkLAlt, modAlt, 10)
	s.press(vkLShift, modAlt|modShift, 5)
	a, _ := s.press(vkOem3, modAlt|modShift, 5)
	if actionsOf(a) != "exact" {
		t.Errorf("Alt+Shift+` 应命中显式带 S 的那条，得到 %v", a)
	}
	// 不带 Shift 时仍然是 loose
	s2 := sim(t, bind("loose", "M-`"), bind("exact", "M-S-`"))
	s2.press(vkLAlt, modAlt, 10)
	a2, _ := s2.press(vkOem3, modAlt, 5)
	if actionsOf(a2) != "loose" {
		t.Errorf("Alt+` 应命中 loose，得到 %v", a2)
	}
}

func TestAutoRepeatFiresOnce(t *testing.T) {
	// 按住 Alt+` 不放，系统会连发 ` 的 keydown；只许触发一次
	s := sim(t, bind("quake.toggle", "M-`"))
	s.press(vkLAlt, modAlt, 10)
	if a, _ := s.press(vkOem3, modAlt, 5); actionsOf(a) != "quake.toggle" {
		t.Fatalf("第一次按下没触发：%v", a)
	}
	for i := 0; i < 5; i++ {
		if a, sw := s.press(vkOem3, modAlt, 33); len(a) != 0 || sw {
			t.Fatalf("自动重复不该再次触发（第 %d 次）：%v swallow=%v", i+1, a, sw)
		}
	}
}

func TestDoubleTapModifiersFire(t *testing.T) {
	s := sim(t, bind("quake.toggle", "<C-M> <C-M>"))
	// 第一轮：Ctrl → Alt → 全松
	s.press(vkLCtrl, modCtrl, 10)
	s.press(vkLAlt, modCtrl|modAlt, 20)
	s.release(vkLAlt, modCtrl, 30)
	s.release(vkLCtrl, 0, 20)
	// 第二轮：间隔 60ms < 400ms
	s.press(vkLCtrl, modCtrl, 10)
	a, sw := s.press(vkLAlt, modCtrl|modAlt, 20)
	if actionsOf(a) != "quake.toggle" {
		t.Errorf("双击 Ctrl+Alt 应触发，得到 %v", a)
	}
	if sw {
		t.Errorf("双击绝不能吞修饰键，否则 Ctrl+C 之类的正常操作会坏掉")
	}
}

func TestDoubleTapTooSlowDoesNotFire(t *testing.T) {
	s := sim(t, bind("quake.toggle", "<C-M> <C-M>"))
	s.press(vkLCtrl, modCtrl, 10)
	s.press(vkLAlt, modCtrl|modAlt, 20)
	s.release(vkLAlt, modCtrl, 30)
	s.release(vkLCtrl, 0, 20)
	s.press(vkLCtrl, modCtrl, 500) // 松手后 500ms 才再按：超过 400ms 窗口
	a, _ := s.press(vkLAlt, modCtrl|modAlt, 20)
	if len(a) != 0 {
		t.Errorf("慢速再按不该算双击，得到 %v", a)
	}
	// 但这次按下被当作新一轮的第一次，紧接着再来一次就该触发
	s.release(vkLAlt, modCtrl, 30)
	s.release(vkLCtrl, 0, 20)
	s.press(vkLCtrl, modCtrl, 50)
	a2, _ := s.press(vkLAlt, modCtrl|modAlt, 20)
	if actionsOf(a2) != "quake.toggle" {
		t.Errorf("重新一轮的双击应触发，得到 %v", a2)
	}
}

func TestDoubleTapHeldTooLongDoesNotFire(t *testing.T) {
	// 按住 Ctrl+Alt 办完别的事再松手、顺手一按：总时长超上限（4×400ms）不算双击
	s := sim(t, bind("quake.toggle", "<C-M> <C-M>"))
	s.press(vkLCtrl, modCtrl, 10)
	s.press(vkLAlt, modCtrl|modAlt, 20)
	s.release(vkLAlt, modCtrl, 2000)
	s.release(vkLCtrl, 0, 10)
	s.press(vkLCtrl, modCtrl, 30)
	a, _ := s.press(vkLAlt, modCtrl|modAlt, 20)
	if len(a) != 0 {
		t.Errorf("首轮按住 2 秒不该与第二轮凑成双击，得到 %v", a)
	}
}

func TestDoubleTapInterruptedByPlainTextDoesNotFire(t *testing.T) {
	s := sim(t, bind("quake.toggle", "<C-M> <C-M>"))
	s.press(vkLCtrl, modCtrl, 10)
	s.press(vkLAlt, modCtrl|modAlt, 20)
	// 中间夹了一次 Ctrl+C 复制：按下了普通键，手势作废
	s.press(vkC, modCtrl|modAlt, 30)
	s.release(vkC, modCtrl|modAlt, 30)
	s.release(vkLAlt, modCtrl, 20)
	s.release(vkLCtrl, 0, 20)
	s.press(vkLCtrl, modCtrl, 30)
	a, _ := s.press(vkLAlt, modCtrl|modAlt, 20)
	if len(a) != 0 {
		t.Errorf("期间按过普通键，不该再凑成双击，得到 %v", a)
	}
}

func TestDoubleTapWithExtraShiftNeverFires(t *testing.T) {
	// <C-M> <C-M> 认的是「Ctrl+Alt」这个精确集合，多按一个 Shift 就是另一个手势
	s := sim(t, bind("quake.toggle", "<C-M> <C-M>"))
	dirty := func() []string {
		var out []string
		for _, a := range [][]string{
			first(s.press(vkLCtrl, modCtrl, 10)),
			first(s.press(vkLAlt, modCtrl|modAlt, 20)),
			first(s.press(vkLShift, modCtrl|modAlt|modShift, 10)),
		} {
			out = append(out, a...)
		}
		s.release(vkLShift, modCtrl|modAlt, 20)
		s.release(vkLAlt, modCtrl, 20)
		s.release(vkLCtrl, 0, 20)
		return out
	}
	if a := dirty(); len(a) != 0 {
		t.Errorf("第一轮 Ctrl+Alt+Shift 不该触发：%v", a)
	}
	if a := dirty(); len(a) != 0 {
		t.Errorf("Ctrl+Alt+Shift 的双击不该命中 <C-M> <C-M>：%v", a)
	}
	// 被脏手势作废之后，干净的 Ctrl+Alt 双击必须还能恢复正常
	clean := func() []string {
		var out []string
		out = append(out, first(s.press(vkLCtrl, modCtrl, 10))...)
		out = append(out, first(s.press(vkLAlt, modCtrl|modAlt, 20))...)
		s.release(vkLAlt, modCtrl, 30)
		s.release(vkLCtrl, 0, 20)
		return out
	}
	if a := clean(); len(a) != 0 {
		t.Errorf("干净的第一轮双击只该进入等待：%v", a)
	}
	if a := clean(); actionsOf(a) != "quake.toggle" {
		t.Errorf("干净的第二次双击应恢复触发，得到 %v", a)
	}
}

func TestMissedModifierKeyupRecovers(t *testing.T) {
	// 钩子漏掉一次 Alt 的 keyup（切桌面等）时，修饰键状态以系统快照为准，
	// 下一次 Alt 按下会被当成自动重复忽略 —— 都不能让组合键从此失灵。
	s := sim(t, bind("quake.toggle", "M-`"))
	s.press(vkLAlt, modAlt, 10)
	s.press(vkOem3, modAlt, 10)
	s.release(vkOem3, modAlt, 20)
	s.press(vkLAlt, modAlt, 500) // 补错的按下：被抑制，无所谓
	s.release(vkLAlt, 0, 10)     // 抬起终于补上了
	if a, _ := s.press(vkOem3, modAlt, 30); actionsOf(a) != "quake.toggle" {
		t.Errorf("漏事件之后 Alt+` 仍应命中，得到 %v", a)
	}
}

func TestDoubleTapChordReleasesInAnyOrder(t *testing.T) {
	// C-T C-T：先松基键、后松修饰键，也必须识别成「已全部松开」
	s := sim(t, bind("quake.toggle", "C-T C-T"))
	s.press(vkLCtrl, modCtrl, 10)
	s.press(vkT, modCtrl, 20)
	s.release(vkT, modCtrl, 30) // 基键先抬：修饰键还按着，还不算松完
	s.release(vkLCtrl, 0, 20)
	s.press(vkLCtrl, modCtrl, 40)
	a, sw := s.press(vkT, modCtrl, 20)
	if actionsOf(a) != "quake.toggle" {
		t.Errorf("双击 Ctrl+T 应触发，得到 %v", a)
	}
	if sw {
		t.Errorf("双击不吞键")
	}
}

func TestChordBeatsDoubleTapOnSameToken(t *testing.T) {
	// 同一条 C-T 既有组合键绑定又有双击绑定时，真正按下 C+T 那一下由组合键接管，
	// 双击状态被重置（否则一次按下会同时做两件事）。
	s := sim(t, bind("chord", "C-T"), bind("double", "C-T C-T"))
	s.press(vkLCtrl, modCtrl, 10)
	a, sw := s.press(vkT, modCtrl, 20)
	if actionsOf(a) != "chord" || !sw {
		t.Fatalf("期望 chord 命中并吞键，得到 %v swallow=%v", a, sw)
	}
	s.release(vkT, modCtrl, 30)
	s.release(vkLCtrl, 0, 20)
	s.press(vkLCtrl, modCtrl, 30)
	a2, _ := s.press(vkT, modCtrl, 20)
	if actionsOf(a2) != "chord" {
		t.Errorf("第二次按下仍应只走组合键（双击已被重置），得到 %v", a2)
	}
}

func TestFunctionKeyDoubleTap(t *testing.T) {
	s := sim(t, bind("quake.toggle", "<F9> <F9>"))
	s.press(vkF1+8, 0, 10)
	s.release(vkF1+8, 0, 40)
	a, _ := s.press(vkF1+8, 0, 30)
	if actionsOf(a) != "quake.toggle" {
		t.Errorf("双击 F9 应触发，得到 %v", a)
	}
}

func TestMatcherStaleModifierUp(t *testing.T) {
	// 钩子漏掉一次 keyup（切桌面等）时，修饰键快照会自我纠正：不能让按键卡成「一直按着」
	s := sim(t, bind("quake.toggle", "M-`"))
	s.press(vkLAlt, modAlt, 10)
	s.press(vkOem3, modAlt, 10) // 触发一次
	s.release(vkOem3, modAlt, 20)
	// Alt 的 keyup 丢了：下一次事件里 GetAsyncKeyState 报的是「Alt 已经没按」
	a, _ := s.press(vkOem3, modAlt, 500)
	if actionsOf(a) != "quake.toggle" {
		t.Errorf("漏掉 keyup 之后仍应能再次命中，得到 %v", a)
	}
}

func TestNormalizeGlobalKeymap(t *testing.T) {
	gk := normalizeGlobalKeymap(GlobalKeymap{
		DoubleTapMs: 99999,
		Bindings: []GlobalKey{
			{Keys: "M-`", Action: "quake.toggle"},
			{Keys: "", Action: "quake.toggle"},
			{Keys: "C-x", Action: ""},
			{Keys: "t", Action: "quake.toggle"}, // 语法非法，丢弃
		},
	})
	if gk.DoubleTapMs != defaultDoubleTapMs {
		t.Errorf("双击窗口没被钳回默认值：%d", gk.DoubleTapMs)
	}
	if len(gk.Bindings) != 1 || gk.Bindings[0].Keys != "M-`" {
		t.Errorf("残缺/非法条目没被清干净：%+v", gk.Bindings)
	}
}
