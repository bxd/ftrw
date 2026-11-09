package main

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

// 系统级（跨应用）热键的表示法、编译与匹配 —— 纯逻辑，不碰任何 Win32 API。
// 按键事件由 kbdhook_windows.go 的 WH_KEYBOARD_LL 低级钩子喂进来，这里只回答
// 「这一下算不算命中、该做什么动作、要不要把键吞掉」。
//
// ── 语法（token 之间用空格分隔，与 frontend/src/globalkey.ts 同一套规则）──────
//
//	组合键     M-`    C-M-t    S-<F1>    C-<Esc>
//	纯修饰键   <C> <M> <S> <W> 以及组合 <C-M> <C-M-S>（别名 CTRL/ALT/SHIFT/WIN/META 也认）
//	双击       两段完全相同的 token：<C-M> <C-M>、C-T C-T、<F9> <F9>
//
//	· 只有两种形状合法：一段「带基键的组合键」，或两段相同 token 的「双击」。多段序列
//	  （M-x p）跨应用做不到 —— 钩子必须在按下那一刻就决断吞不吞，不能等续键。
//	· 单独一个纯修饰键不构成热键：要双击就写 <C-M> <C-M>，不能只写 <C-M>。
//	· 单段组合键必须带至少一个修饰键，否则等于把全局的普通打字全吞掉。
//	· Shift 沿用应用内序列的规矩：绑定没显式要求 Shift 时，多按下的 Shift 不算数
//	  （美式键盘上 Alt+~ 就是 Alt+Shift+`，同一个物理键，不该逼用户绑两条）。纯修饰键
//	  token 反过来要求精确 —— <C-M> <C-M> 不会被 Ctrl+Alt+Shift 的双击触发。
//	· 字符到 VK 的映射按 US Extended 布局；反引号与波浪号共用 VK_OEM_3。
//	· AltGr 在 Windows 上就是 Ctrl+Alt，低级钩子区分不开，故带 C+M 的绑定可能被 AltGr 命中。

const (
	// defaultDoubleTapMs 判的是「全部松开 → 再次按下」的间隔，不是两次按下之间：
	// 抬手再按本身就有两三拍延迟，按按下间隔算的话双击几乎录不成功。
	defaultDoubleTapMs = 400
	// doubleTapTotalFactor 是首次按下到第二次按下的总时长上限（doubleTapMs 的倍数）。
	// 没有这道上限，「按住 Ctrl+Alt 忙完别的事、松手、顺手再按一次」就会误触发。
	doubleTapTotalFactor = 4
)

/* --------------------------------------------------------------------------- */
/* 修饰键位集与虚拟键码                                                          */
/* --------------------------------------------------------------------------- */

// modMask 是修饰键位集，左右手两个键码归到同一位。
type modMask uint8

const (
	modCtrl modMask = 1 << iota
	modAlt
	modShift
	modMeta // Win / Cmd
)

// 本模块用到的 VK 码。数值取自 WinUser.h；这里只做常量定义，不调用任何 API，
// 所以非 Windows 平台也能编译（测试全靠它跑纯逻辑）。
const (
	vkBack     = 0x08
	vkTab      = 0x09
	vkEnter    = 0x0D
	vkShift    = 0x10
	vkControl  = 0x11
	vkMenu     = 0x12
	vkEscape   = 0x1B
	vkSpace    = 0x20
	vkPrior    = 0x21
	vkNext     = 0x22
	vkEnd      = 0x23
	vkHome     = 0x24
	vkLeft     = 0x25
	vkUp       = 0x26
	vkRight    = 0x27
	vkDown     = 0x28
	vkInsert   = 0x2D
	vkDelete   = 0x2E
	vkF1       = 0x70 // F1..F24 连续：vkF1 + n - 1
	vkLWin     = 0x5B
	vkRWin     = 0x5C
	vkLShift   = 0xA0
	vkRShift   = 0xA1
	vkLCtrl    = 0xA2
	vkRCtrl    = 0xA3
	vkLAlt     = 0xA4
	vkRAlt     = 0xA5
	vkOem1     = 0xBA // ;:
	vkOem2     = 0xBB // =+
	vkOemMinus = 0xBD // -_
)

// OEM 符号键里除了 '-' 之外都是 0xBC..0xDE 这一段，单独列出来只为可读性。
const (
	vkOemComma  = 0xBC // ,<
	vkOemPeriod = 0xBE // .>
	vkOemSlash  = 0xBF // /?
	vkOem3      = 0xC0 // `~
	vkOem4      = 0xDB // [{
	vkOem5      = 0xDC // \|
	vkOem6      = 0xDD // ]}
	vkOem7      = 0xDE // '"
)

// modNameBits 是纯修饰键 token 的名字别名表（尖括号内使用）。
var modNameBits = map[string]modMask{
	"C": modCtrl, "CTRL": modCtrl, "CONTROL": modCtrl,
	"M": modAlt, "ALT": modAlt,
	"S": modShift, "SHIFT": modShift,
	"W": modMeta, "WIN": modMeta, "CMD": modMeta, "SUPER": modMeta,
}

// modPrefixOrder 决定规范化写法里的修饰键顺序，与 keyseq.ts 一致（C→M→S→W）。
var modPrefixOrder = []struct {
	bit  modMask
	name string
}{
	{modCtrl, "C"}, {modAlt, "M"}, {modShift, "S"}, {modMeta, "W"},
}

// gkSpecialNames 是特殊键的规范名与 VK；aliases 收其它写法（浏览器键名口径）。
var gkSpecialNames = []struct {
	name string
	vk   uint16
}{
	{"SPC", vkSpace}, {"ESC", vkEscape}, {"RET", vkEnter}, {"TAB", vkTab},
	{"BS", vkBack}, {"DEL", vkDelete}, {"INS", vkInsert}, {"HOME", vkHome},
	{"END", vkEnd}, {"PGUP", vkPrior}, {"PGDN", vkNext},
	{"UP", vkUp}, {"DOWN", vkDown}, {"LEFT", vkLeft}, {"RIGHT", vkRight},
}

var gkSpecialAliases = map[string]string{
	"SPACE": "SPC", "ESCAPE": "ESC", "ENTER": "RET", "CR": "RET", "RETURN": "RET",
	"BACKSPACE": "BS", "DELETE": "DEL", "INSERT": "INS",
	"PAGEUP": "PGUP", "PAGEDOWN": "PGDN",
	"ARROWUP": "UP", "ARROWDOWN": "DOWN", "ARROWLEFT": "LEFT", "ARROWRIGHT": "RIGHT",
}

var (
	gkSpecialVK  = map[string]uint16{} // 规范名 + 别名 → VK
	gkSpecialTxt = map[uint16]string{} // VK → "<规范名>"
)

func init() {
	for _, s := range gkSpecialNames {
		gkSpecialVK[s.name] = s.vk
		gkSpecialTxt[s.vk] = "<" + s.name + ">"
	}
	for a, canon := range gkSpecialAliases {
		gkSpecialVK[a] = gkSpecialVK[canon]
	}
}

// charVKs 是未上档符号到 VK 的映射。字母与数字的 VK 就是它们的大写 ASCII，无需列表。
var charVKs = map[rune]uint16{
	';': vkOem1, '=': vkOem2, '-': vkOemMinus, ',': vkOemComma,
	'.': vkOemPeriod, '/': vkOemSlash, '`': vkOem3,
	'[': vkOem4, '\\': vkOem5, ']': vkOem6, '\'': vkOem7,
}

// shiftedVKs 是上档形式字符到「物理键 VK」的映射。用户写 M-! 时表达的是 Alt+Shift+1，
// 这里把它拆成键位加修饰，而不是当成一个根本不存在的 VK。
var shiftedVKs = map[rune]uint16{
	'!': '1', '@': '2', '#': '3', '$': '4', '%': '5',
	'^': '6', '&': '7', '*': '8', '(': '9', ')': '0',
	':': vkOem1, '+': vkOem2, '<': vkOemComma, '>': vkOemPeriod,
	'?': vkOemSlash, '~': vkOem3, '{': vkOem4, '|': vkOem5,
	'}': vkOem6, '"': vkOem7,
}

// modBitOfVK 把修饰键自身的 VK 归到位；其它键返回 0。
func modBitOfVK(vk uint16) modMask {
	switch vk {
	case vkShift, vkLShift, vkRShift:
		return modShift
	case vkControl, vkLCtrl, vkRCtrl:
		return modCtrl
	case vkMenu, vkLAlt, vkRAlt:
		return modAlt
	case vkLWin, vkRWin:
		return modMeta
	}
	return 0
}

func isModifierVK(vk uint16) bool { return modBitOfVK(vk) != 0 }

/* --------------------------------------------------------------------------- */
/* token：解析结果                                                              */
/* --------------------------------------------------------------------------- */

// gkToken 是一个规范化后的按键 token。vk==0 表示纯修饰键（只能出现在双击里）。
type gkToken struct {
	mods      modMask
	vk        uint16
	base      string // 展示写法："x"、"<F1>"、"'"；纯修饰键 token 留空
	printable bool   // 基键是可打字字符（双击不带修饰键时据此拒绝）
}

// extra 返回实际按下、但绑定没要求的修饰键。绑定没写 Shift 时，多按的 Shift 不算多余。
func (t gkToken) extra(mods modMask) modMask {
	e := mods & ^t.mods
	if t.mods&modShift == 0 {
		e &^= modShift
	}
	return e
}

// modsReady 判断按住的修饰键是否正好满足要求（含 Shift 容忍规则）。
func (t gkToken) modsReady(mods modMask) bool {
	return mods&t.mods == t.mods && t.extra(mods) == 0
}

// tokResult 是一次「按下」相对本 token 的结论。
type tokResult uint8

const (
	resMismatch tokResult = iota // 与本手势无关甚至打架：双击状态作废
	resNeutral                   // 手势进行到一半（还在往上加修饰键）：状态保持不变
	resMatch                     // 命中本 token
)

func (t gkToken) press(mods modMask, vk uint16) tokResult {
	if t.vk == 0 {
		// 纯修饰键 token 要求集合精确：多按一个 Shift 就是另一个手势了，
		// 不能像组合键那样容忍 —— 否则 <C-M> <C-M> 会被 Ctrl+Alt+Shift 双击误触发。
		if !isModifierVK(vk) {
			return resMismatch // 双击修饰键的间隙里按了普通键：这不再是「纯双击」
		}
		if mods == t.mods {
			return resMatch // 最后一个修饰键落下，集合正好等于 token
		}
		if mods != 0 && mods&^t.mods == 0 {
			return resNeutral // 还在往上加修饰键，手势没完成也没跑偏
		}
		return resMismatch
	}
	if t.extra(mods) != 0 {
		return resMismatch // 多按了绑定没要求的修饰键
	}
	if isModifierVK(vk) {
		return resNeutral // 组合键的基键还没按
	}
	if vk != t.vk {
		return resMismatch
	}
	if t.modsReady(mods) {
		return resMatch
	}
	return resNeutral // 基键先按、修饰键后补：既不算命中也不作废（热键要求修饰键先按住）
}

// released 判断某次「松开」之后本 token 是否已全部抬起。down 必须是松开之后的按键表。
func (t gkToken) released(mods modMask, down map[uint16]bool) bool {
	if mods&t.mods != 0 {
		return false // token 要求的修饰键还按着
	}
	return t.vk == 0 || !down[t.vk]
}

// String 输出规范化写法：界面展示、以及「双击两段是不是同一个键」的比较都走它。
func (t gkToken) String() string {
	var names []string
	for _, m := range modPrefixOrder {
		if t.mods&m.bit != 0 {
			names = append(names, m.name)
		}
	}
	if t.vk == 0 {
		return "<" + strings.Join(names, "-") + ">"
	}
	var b strings.Builder
	for _, n := range names {
		b.WriteString(n)
		b.WriteByte('-')
	}
	b.WriteString(t.base)
	return b.String()
}

// splitModNames 吃掉以 '-' 分隔的修饰键段，返回位集与剩下的基键部分。
// allMods 为 true 时允许整段都是修饰键（尖括号里的 <C-M>）；否则最后一段一定留给基键，
// 这样 "C-M" 仍然是「Ctrl+Alt+M」这个组合键，而纯修饰键必须写 <C-M> —— 与 keyseq.ts 同规。
func splitModNames(s string, allMods bool) (modMask, string, error) {
	var mods modMask
	parts := strings.Split(s, "-")
	i := 0
	for ; i < len(parts); i++ {
		if !allMods && i == len(parts)-1 {
			break
		}
		bit, ok := modNameBits[strings.ToUpper(parts[i])]
		if !ok {
			break
		}
		if mods&bit != 0 {
			return 0, "", fmt.Errorf("修饰键重复：%s", strings.ToUpper(parts[i]))
		}
		mods |= bit
	}
	return mods, strings.Join(parts[i:], "-"), nil
}

// parseGKBase 解析基键部分：特殊键名（F1 / esc / <PageUp>）或单个字符。
// 自带的尖括号在这里统一剥掉，好让 S-<F1>、C-<Esc> 这种混写与前缀写法走同一条路。
func parseGKBase(raw string) (vk uint16, base string, shift modMask, printable bool, err error) {
	rest := strings.TrimSpace(raw)
	if len(rest) >= 3 && rest[0] == '<' && rest[len(rest)-1] == '>' {
		rest = rest[1 : len(rest)-1]
	}
	up := strings.ToUpper(rest)
	if v, ok := gkSpecialVK[up]; ok {
		return v, gkSpecialTxt[v], 0, false, nil
	}
	// F1..F24：前缀 F + 一到两位数字
	if len(up) > 1 && up[0] == 'F' {
		n := 0
		for _, r := range up[1:] {
			if r < '0' || r > '9' {
				n = 0
				break
			}
			n = n*10 + int(r-'0')
		}
		if n >= 1 && n <= 24 {
			v := uint16(vkF1 + n - 1)
			return v, fmt.Sprintf("<F%d>", n), 0, false, nil
		}
	}
	runes := []rune(rest)
	if len(runes) != 1 {
		return 0, "", 0, false, fmt.Errorf("无法识别的键：%s", rest)
	}
	r := runes[0]
	switch {
	case r >= '0' && r <= '9':
		return uint16(r), string(r), 0, true, nil
	case r >= 'a' && r <= 'z':
		// 字母大小写不影响匹配（同 keyseq：按住 Alt 时系统只会报小写），统一按小写展示
		return uint16(r - 32), string(r), 0, true, nil
	case r >= 'A' && r <= 'Z':
		return uint16(r), string(r + 32), 0, true, nil
	}
	if v, ok := charVKs[r]; ok {
		return v, string(r), 0, true, nil
	}
	if v, ok := shiftedVKs[r]; ok {
		return uint16(v), string(r), modShift, true, nil
	}
	return 0, "", 0, false, fmt.Errorf("无法识别的键：%s", rest)
}

// parseGKToken 解析单个 token，非法返回 error。
func parseGKToken(raw string) (gkToken, error) {
	t := strings.TrimSpace(raw)
	if t == "" {
		return gkToken{}, errors.New("按键为空")
	}
	// 整体尖括号写法：<C-M>（纯修饰键）、<C-F1>、<ESC>
	if len(t) >= 3 && t[0] == '<' && t[len(t)-1] == '>' {
		mods, rest, err := splitModNames(t[1:len(t)-1], true)
		if err != nil {
			return gkToken{}, err
		}
		if rest == "" {
			if mods == 0 {
				return gkToken{}, fmt.Errorf("无法识别的键：%s", t)
			}
			return gkToken{mods: mods}, nil // 纯修饰键 token
		}
		return finishGKToken(mods, rest)
	}
	// 前缀写法：C-M-S-x，基键可以是字符、裸特殊键名，也可以再套一层尖括号（S-<F1>）
	mods, rest, err := splitModNames(t, false)
	if err != nil {
		return gkToken{}, err
	}
	if rest == "" {
		return gkToken{}, fmt.Errorf("缺少基键：%s", t)
	}
	return finishGKToken(mods, rest)
}

func finishGKToken(mods modMask, rest string) (gkToken, error) {
	vk, base, shift, printable, err := parseGKBase(rest)
	if err != nil {
		return gkToken{}, err
	}
	return gkToken{mods: mods | shift, vk: vk, base: base, printable: printable}, nil
}

/* --------------------------------------------------------------------------- */
/* 编译与匹配状态机                                                              */
/* --------------------------------------------------------------------------- */

type gkKind uint8

const (
	kindChord     gkKind = iota // 单段组合键
	kindDoubleTap               // 两段相同 token 的双击
)

// 双击状态机阶段。
const (
	dtIdle  = iota // 没在等第二次
	dtHeld         // 第一次已按下，等全部松开
	dtFreed        // 已全部松开，等第二次按下
)

// compiledGK 是一条编译好的绑定，同时携带它自己的双击进度。
type compiledGK struct {
	src   GlobalKey
	kind  gkKind
	token gkToken
	phase int
	armed time.Time
	freed time.Time
}

// compileGlobalKey 把一条绑定编译成可匹配结构；语法不合法时返回 error。
func compileGlobalKey(b GlobalKey) (*compiledGK, error) {
	fields := strings.Fields(b.Keys)
	switch len(fields) {
	case 0:
		return nil, errors.New("序列为空")
	case 1:
		t, err := parseGKToken(fields[0])
		if err != nil {
			return nil, err
		}
		if t.vk == 0 {
			return nil, fmt.Errorf("纯修饰键不能单独成键，双击要写成「%s %s」", t, t)
		}
		if t.mods == 0 {
			return nil, fmt.Errorf("系统级热键必须带修饰键，否则会把全局的普通打字吞掉：%s", b.Keys)
		}
		return &compiledGK{src: b, kind: kindChord, token: t}, nil
	default:
		if len(fields) > 2 {
			return nil, fmt.Errorf("系统级热键只能是一段组合键或两段相同键的双击（当前 %d 段）：%s", len(fields), b.Keys)
		}
		a, err := parseGKToken(fields[0])
		if err != nil {
			return nil, err
		}
		c, err := parseGKToken(fields[1])
		if err != nil {
			return nil, err
		}
		if a.String() != c.String() {
			return nil, fmt.Errorf("双击的两段必须是同一个键：%s / %s", a, c)
		}
		if a.mods == 0 && a.printable {
			// 裸字符双击（t t）会在打字打到「tt」时误触发，且没法跟正常输入区分
			return nil, fmt.Errorf("双击必须带修饰键（例：<C-M> <C-M>、C-T C-T），或用不会打字的键（例：<F9> <F9>）：%s", b.Keys)
		}
		return &compiledGK{src: b, kind: kindDoubleTap, token: a}, nil
	}
}

// gkEvent 是一次按键事件。Mods 是钩子侧取到的修饰键快照（不含本键自身的状态，
// feed 会按 Vk 与 Down 再校正一次，所以调用方不必纠结快照是按下前还是按下后取的）。
type gkEvent struct {
	Vk   uint16
	Down bool
	Mods modMask
	At   time.Time
}

// globalMatcher 是平台无关的匹配状态机。只在钩子线程里被调用，但重新编译可能来自
// 服务线程，因此外部由 AppService 的锁保护，自身不加锁。
type globalMatcher struct {
	keys        []*compiledGK
	doubleTap   time.Duration
	totalWindow time.Duration
	down        map[uint16]bool
}

// newGlobalMatcher 编译整张表；非法条目被跳过并记日志（界面已挡住，这里只防手改文件）。
func newGlobalMatcher(gk GlobalKeymap) *globalMatcher {
	m := &globalMatcher{
		doubleTap:   time.Duration(gk.DoubleTapMs) * time.Millisecond,
		totalWindow: time.Duration(gk.DoubleTapMs*doubleTapTotalFactor) * time.Millisecond,
		down:        make(map[uint16]bool),
	}
	for _, b := range gk.Bindings {
		if !b.Enabled || b.Action == "" {
			continue
		}
		c, err := compileGlobalKey(b)
		if err != nil {
			log.Printf("忽略无效的系统级热键 %q: %v", b.Keys, err)
			continue
		}
		m.keys = append(m.keys, c)
	}
	return m
}

// feed 喂入一次按键，返回要执行的动作与是否吞掉这次按键。
// 只有组合键会吞键（对齐 RegisterHotKey 的语义）；双击永远不吞 —— 把 Ctrl/Alt 的
// 按下松开发出去截掉，普通复制粘贴就先坏了，而且第一次按下时还无法预判决不判决。
func (m *globalMatcher) feed(e gkEvent) (actions []string, swallow bool) {
	if e.Down {
		if m.down[e.Vk] {
			return nil, false // 长按自动重复：系统会连发 keydown，只认第一次
		}
		m.down[e.Vk] = true
	} else {
		delete(m.down, e.Vk)
	}
	mods := e.Mods
	if bit := modBitOfVK(e.Vk); bit != 0 {
		if e.Down {
			mods |= bit
		} else {
			mods &^= bit
		}
	}
	if !e.Down {
		m.markFreed(mods, e.At)
		return nil, false
	}
	if hit := m.chordHit(mods, e.Vk); hit != nil {
		m.resetDoubleTaps()
		return []string{hit.src.Action}, true
	}
	var out []string
	for _, b := range m.keys {
		if b.kind != kindDoubleTap {
			continue
		}
		switch b.token.press(mods, e.Vk) {
		case resMismatch:
			b.phase = dtIdle
		case resNeutral:
			// 手势还在进行中，保持阶段不动
		case resMatch:
			switch b.phase {
			case dtFreed:
				if e.At.Sub(b.freed) <= m.doubleTap && e.At.Sub(b.armed) <= m.totalWindow {
					out = append(out, b.src.Action)
					b.phase = dtIdle
				} else {
					b.phase, b.armed = dtHeld, e.At // 太慢，当作新一轮的第一次
				}
			case dtHeld:
				b.armed = e.At
			default:
				b.phase, b.armed = dtHeld, e.At
			}
		}
	}
	return out, false
}

// chordHit 找出发应的组合键。分两轮，让「写了 S- 的绑定」优先于「同键但没写 S- 的绑定」：
// 只走一轮的话，表里先出现的 M-` 会抢走 Alt+Shift+`，把 M-S-` 那条永久遮蔽。
func (m *globalMatcher) chordHit(mods modMask, vk uint16) *compiledGK {
	for _, b := range m.keys {
		if b.kind == kindChord && b.token.vk == vk && mods == b.token.mods {
			return b
		}
	}
	for _, b := range m.keys {
		if b.kind == kindChord && b.token.vk == vk && b.token.modsReady(mods) {
			return b
		}
	}
	return nil
}

// markFreed 在松开事件上推进双击：第一次按下的一串键全部抬起后进入「等第二次」窗口。
func (m *globalMatcher) markFreed(mods modMask, at time.Time) {
	for _, b := range m.keys {
		if b.kind != kindDoubleTap || b.phase != dtHeld {
			continue
		}
		if b.token.released(mods, m.down) {
			b.phase, b.freed = dtFreed, at
		}
	}
}

func (m *globalMatcher) resetDoubleTaps() {
	for _, b := range m.keys {
		if b.kind == kindDoubleTap {
			b.phase = dtIdle
		}
	}
}

// normalizeGlobalKeymap 钳制数值区间并丢弃残缺条目，口径与 normalizeKeymap 一致。
func normalizeGlobalKeymap(gk GlobalKeymap) GlobalKeymap {
	if gk.DoubleTapMs < 100 || gk.DoubleTapMs > 2000 {
		gk.DoubleTapMs = defaultDoubleTapMs
	}
	out := make([]GlobalKey, 0, len(gk.Bindings))
	for _, b := range gk.Bindings {
		if b.Keys == "" || b.Action == "" {
			continue
		}
		if _, err := compileGlobalKey(b); err != nil {
			log.Printf("忽略无效的跨应用热键 %q: %v", b.Keys, err)
			continue
		}
		out = append(out, b)
	}
	gk.Bindings = out
	return gk
}
