/**
 * 站点「按键方案」（keyType）与「退格键」（backspace）两个配置的落地处。
 *
 * 先说清与原版的差异，免得对着旧源码找不着：原版 keyboardType_（fqterm_param.h:127
 * 注释 0--BBS 1--XTERM 2--VT100）只在参数读写和站点对话框里出现，按键发送代码
 * 从头到尾没读过它 —— 那是个填了不生效的死字段。真正发出去的东西在
 * fqterm_window.cpp:2064-2264，由三个终端模式决定：
 *   · isAnsiMode() + isCursorMode()（即 DECCKM）：方向键走 \x1bOA/OB/OC/OD 还是 \x1b[A/B/C/D；
 *   · 光标之外的定位键一律查唯一那张 directions_ 表（第 64-75 行）：
 *     Home=\x1b[1~ End=\x1b[4~ PgUp=\x1b[5~ PgDn=\x1b[6~（注意 Home/End 用的是老式 ScTI 编码）；
 *   · F1-F12 = \x1b[11~ 12~ 13~ 14~ 15~ 17~ 18~ 19~ 20~ 21~ 23~ 24~；
 *   · 退格键按 backspaceType_ 发 \x08 或 \x7f（第 2242-2252 行）。
 *
 * 于是本模块只做「与 xterm.js 原生行为不同」的那部分：xterm.js 本身就是一个 xterm，
 * DECCKM、PgUp/PgDn、F5-F12、Insert/Delete、方向键的原生编码和上面完全一致，接管过来
 * 只会多一条出错路径。XTERM 方案因此一个键都不接（纯透传）。
 *
 * 返回 null = 交回 xterm.js 原生处理；返回字符串 = 已接管，调用方把它发给远端。
 * 带 Ctrl/Alt/Meta/Shift 的组合一律不接：xterm 的增强编码（\x1b[1;5H、\x1b[13;2~ 之类）
 * 比这套 2008 年的表更有用，而且 bash/vim 都认。
 */

export const KEY_BBS = 0
export const KEY_XTERM = 1
export const KEY_VT100 = 2

/** 站点编辑框下拉用同一份文案，避免界面和引擎两处各说各话。 */
export const KEY_TYPE_NAMES = [
  'BBS（Home/End 用老式 \\x1b[1~ / \\x1b[4~）',
  'XTERM（交给终端原生编码）',
  'VT100（光标键恒为应用模式 + 小键盘转义）',
]

export interface KeyContext {
  /** site.keyType：0=BBS 1=XTERM 2=VT100 */
  keyType: number
  /** site.backspace：0=^H 1=^? */
  backspace: number
}

/** 只取这几个字段，方便脱离 DOM 做断言。 */
export interface KeyEventLike {
  key: string
  type?: string
  ctrlKey: boolean
  altKey: boolean
  shiftKey: boolean
  metaKey: boolean
  /** DOM_KEY_LOCATION_NUMPAD === 3 */
  location?: number
}

// 原版 directions_ 的 Home/End：水木一类 BBS 的输入行只认这两种老编码，
// 收到 xterm 原生的 \x1b[H / \x1b[F 会当成未知序列吐字。
const BBS_HOME = '\x1b[1~'
const BBS_END = '\x1b[4~'

// 原版 F1-F4 走 [11~..14~，xterm.js 原生是 \x1bOP..OS（VT220 口径）。
// F5-F12 两边逐字节相同，故不列在这里。
const BBS_FN: Record<string, string> = {
  F1: '\x1b[11~',
  F2: '\x1b[12~',
  F3: '\x1b[13~',
  F4: '\x1b[14~',
}

const APP_ARROW: Record<string, string> = {
  ArrowUp: '\x1bOA',
  ArrowDown: '\x1bOB',
  ArrowRight: '\x1bOC',
  ArrowLeft: '\x1bOD',
}

// VT100 应用小键盘：与原版 fqterm_window.cpp:2118-2149 逐字符一致（'p' + 数字，
// '+' = l、'-' = m、'.' = n、'*' = M）。'/' 原版没管，这里也不接。
const NUMPAD: Record<string, string> = {
  '0': '\x1bOp', '1': '\x1bOq', '2': '\x1bOr', '3': '\x1bOs', '4': '\x1bOt',
  '5': '\x1bOu', '6': '\x1bOv', '7': '\x1bOw', '8': '\x1bOx', '9': '\x1bOy',
  '+': '\x1bOl', '-': '\x1bOm', '.': '\x1bOn', '*': '\x1bOM',
}

/**
 * 主入口：这个键该由本模块发什么？null 表示不归我们管。
 *
 * 判据里没有 DECCKM：方向键的两种编码 xterm.js 本来就跟着远端的 \x1b[?1h 切换
 * （BBS 方案因此直接透传），而 VT100 方案要的恰恰是无视它恒发应用光标键。
 */
export function translateKey(ev: KeyEventLike, ctx: KeyContext): string | null {
  if (ev.type && ev.type !== 'keydown') return null
  const k = ev.key
  if (!k) return null

  // 退格键（A3）：^? 正是 xterm.js 的原生行为，只有 ^H 需要我们接管。
  // Ctrl+Backspace 原生已经发 \b，不能抢。
  if (k === 'Backspace') {
    if (ev.ctrlKey || ev.altKey || ev.metaKey) return null
    return ctx.backspace === 0 ? '\x08' : null
  }

  if (ctx.keyType === KEY_VT100) return vt100Key(ev, k)
  if (ctx.keyType === KEY_BBS) return bbsKey(ev, k)
  return null
}

function bbsKey(ev: KeyEventLike, k: string): string | null {
  if (modified(ev)) return null
  if (k === 'Home') return BBS_HOME
  if (k === 'End') return BBS_END
  return BBS_FN[k] ?? null
}

function vt100Key(ev: KeyEventLike, k: string): string | null {
  // 小键盘优先：NumLock 开着时数字键与主键盘同名，只能靠 location 分辨。
  if (ev.location === 3 && !modified(ev)) {
    const np = NUMPAD[k]
    if (np) return np
  }
  if (modified(ev)) return null
  if (k === 'Home') return '\x1bOH'
  if (k === 'End') return '\x1bOF'
  return APP_ARROW[k] ?? null
}

function modified(ev: KeyEventLike): boolean {
  return ev.ctrlKey || ev.altKey || ev.shiftKey || ev.metaKey
}
