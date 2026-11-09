/**
 * Emacs 风格「应用内键序列」引擎（纯逻辑，不含 UI）。
 *
 * ── 表示法（notation）语法 ─────────────────────────────────────────────
 *   token = [C-][M-][S-] <base>
 *   base  = 单个可打印字符（a-z 0-9 - [ ; …）
 *         | <SPC> <RET> <TAB> <ESC> <BS> <DEL> <INS> <HOME> <END>
 *         | <UP> <DOWN> <LEFT> <RIGHT> <PGUP> <PGDN> <F1>…<F24>
 *   seq   = token 用单个空格分隔，例："C-x C-f"、"C-c <ESC> o"、"C-M-i"、"C-x <SPC>"
 *
 *   · C- = Ctrl，M- = Alt（Emacs 的 Meta），S- = Shift。
 *   · 修饰可写在前面（C-<F1>）也可写在尖括号内（<C-F1>），<S-F1> 同理。
 *   · 规范化：`S-a` ≡ `A`（Shift 对字母的效果已体现在字符大小写里）；
 *     功能键没有大小写，因此保留前缀写成 `S-<F1>`。
 *   · 普通字符不需要尖括号，所以 `<x>` / `<C-x>` 视为非法写法（避免与 `X` 的大小写歧义）。
 *   · 修饰顺序固定输出为 C- → M- → S-，所以 `M-C-x` 与 `C-M-x` 是同一个键。
 *   · 字母 token 匹配时大小写不敏感（`C-x` ≡ `C-X`）：按住 Alt 浏览器会把 ev.key
 *     强制报成小写，CapsLock 又会把它变成大写，区分大小写会让 Alt 序列静默失效。
 *   · 空格在序列里是分隔符，需要它时写 <SPC>（绑定表的录制器会自动处理）。
 *   · 超键（Win/Cmd）在 WebView2 中无法被应用捕获，故不支持。
 *
 * ── 匹配语义（与 Emacs 一致）──────────────────────────────────────────
 *   · 从左往右沿序列走 trie，**一旦某节点带命令就立即执行**（最短匹配优先）。
 *     因此给 `C-x` 绑命令会让 `C-x C-f` 永远不可达 —— 由 analyzeBindings 报警。
 *   · 只命中前缀（如绑了 `C-x C-f` 而只按了 `C-x`）时进入「等待续键」状态并吞掉按键，
 *     超时（timeoutMs）或被取消时丢弃序列，**不会**把已吞的键补发给终端。
 *   · 等待期间按下不匹配的键 → 整个序列作废（同 Emacs 的 quit），该键同样被吞掉，
 *     以免半个序列漏进 BBS 命令行。
 *
 * ── 与终端共存 ────────────────────────────────────────────────────────
 *   整张绑定表都是「应用内」的：只要本应用窗口有焦点就生效，不管焦点在终端、工具栏还是
 *   站点栏，两个入口共用同一棵树：
 *     · 焦点在终端 → xterm 的 customKeyEventHandler 调 feed
 *     · 焦点在窗口其它位置 → window 捕获阶段的 keydown 调 feed（正在往输入框打字时不拦）
 *   系统级（跨应用）热键是另一套东西，由后端 AppService 注册，不在这张表里。
 */
import type { KeyBinding } from './api'

/** feed 的结论：swallow=已被框架消费（不透传终端），pass=交还正常输入 */
export type Verdict = 'swallow' | 'pass'

/** 特殊键名表：所有查表入口（normalizeToken 与 eventToken）都先把名字归一为大写，
 *  因此这里只允许全大写键名（例外：' ' 本身就是空格字符）。 */
const SPECIALS: Record<string, string> = {
  ' ': 'SPC', SPC: 'SPC', ESC: 'ESC', ESCAPE: 'ESC', ENTER: 'RET', RET: 'RET',
  CR: 'RET', LF: 'LF', TAB: 'TAB', BACKSPACE: 'BS', BS: 'BS',
  DELETE: 'DEL', DEL: 'DEL', INS: 'INS', INSERT: 'INS', HOME: 'HOME', END: 'END',
  PAGEUP: 'PGUP', PAGEDOWN: 'PGDN', PGUP: 'PGUP', PGDN: 'PGDN',
  ARROWUP: 'UP', ARROWDOWN: 'DOWN', ARROWLEFT: 'LEFT', ARROWRIGHT: 'RIGHT',
  UP: 'UP', DOWN: 'DOWN', LEFT: 'LEFT', RIGHT: 'RIGHT',
}
// 功能键：<F1>…<F24>（浏览器 ev.key 就是 'F1'…'F12'）
for (let i = 1; i <= 24; i++) { const n = `F${i}`; SPECIALS[n] = n }

/** Keyboard.key 为这些值时说明按的是修饰键本身，不构成序列 token。 */
const MODIFIER_ONLY = new Set(['Control', 'Alt', 'Shift', 'Meta', 'OS', 'AltGraph', 'Dead', 'Unidentified'])

const MOD_ORDER = ['C-', 'M-', 'S-']

/** 把任意写法的 token 规范化；非法返回 null。 */
export function normalizeToken(raw: string): string | null {
  let t = (raw || '').trim()
  if (!t) return null
  const mods = new Set<string>()

  // 1) 先剥前缀：C-M-S-x（顺序任意，同一修饰重复出现视为非法）
  for (;;) {
    if (t.startsWith('<')) break // 尖括号段自己处理修饰
    const up = t.toUpperCase()
    const p = MOD_ORDER.find((x) => up.startsWith(x) && up.length > x.length)
    if (!p || mods.has(p)) break
    mods.add(p)
    t = t.slice(p.length)
  }
  if (!t) return null

  // 2) 尖括号写法：<ESC> / <C-F1> / <S-F1>（内部只允许特殊键名）
  if (t.startsWith('<')) {
    const m = /^<([^>]+)>$/.exec(t)
    if (!m) return null
    let inner = m[1].toUpperCase()
    for (;;) {
      const p = MOD_ORDER.find((x) => inner.startsWith(x) && inner.length > x.length)
      if (!p || mods.has(p)) break
      mods.add(p)
      inner = inner.slice(p.length)
    }
    const name = SPECIALS[inner]
    if (!name) return null // 普通字符不需要尖括号，<x> 视为非法
    return assemble(mods, `<${name}>`)
  }

  // 3) 单个字符：Shift 的效果已体现在字符本身（S-a ≡ A；'1' 与 '!' 按字面字符比较）
  if (t.length === 1) {
    if (mods.has('S-')) {
      mods.delete('S-')
      if (/[A-Za-z]/.test(t)) return assemble(mods, t.toUpperCase())
    }
    return assemble(mods, t)
  }

  // 4) 裸写的特殊键名：ESC / F1 / PageUp …
  const name = SPECIALS[t.toUpperCase()]
  if (name) return assemble(mods, `<${name}>`)
  return null
}

function assemble(mods: Set<string>, base: string): string {
  let out = ''
  for (const p of MOD_ORDER) if (mods.has(p)) out += p
  return out + base
}

/**
 * 比较用的归一键名：字母大小写不敏感。
 *
 * 为什么不区分：按住 Alt 时浏览器会把字母的 ev.key 强制报成小写（Windows 上
 * Alt+X 得到 'x'，即便同时按了 Shift），所以序列录制器永远录不出 `M-X`；
 * 若区分大小写，手写的 `M-x p`（出厂绑定）与录制出来的 `M-X p` 就会互不相认，
 * CapsLock 开着时同样会静默失效。展示形式仍保留原始大小写，只有查表折叠。
 */
function cacheKey(tok: string): string {
  const i = tok.lastIndexOf('>')
  const c = i >= 0 ? tok.charCodeAt(tok.length - 2) : tok.charCodeAt(tok.length - 1)
  if (c >= 65 && c <= 90) return i >= 0 ? tok.slice(0, -2) + String.fromCharCode(c + 32) + '>' : tok.slice(0, -1) + String.fromCharCode(c + 32)
  return tok
}

/** 解析整条序列；返回规范化 token 数组，非法时给出 error。 */
export function parseSeq(seq: string): { tokens: string[]; error?: string } {
  const parts = (seq || '').trim().split(/\s+/).filter(Boolean)
  if (!parts.length) return { tokens: [], error: '序列为空' }
  const tokens: string[] = []
  for (const p of parts) {
    const n = normalizeToken(p)
    if (!n) return { tokens: [], error: `无法识别的键：“${p}”` }
    tokens.push(n)
  }
  return { tokens }
}

/** 从 KeyboardEvent 得出规范化 token；修饰键本身或不支持的键返回 ''。 */
export function eventToken(ev: KeyboardEvent): string {
  const key = ev.key
  if (!key || MODIFIER_ONLY.has(key)) return ''
  let base = ''
  // 空格在序列语法里是 token 分隔符，必须归一为 <SPC>，否则永远匹配不上绑定
  if (key.length === 1 && key !== ' ') {
    base = key
  } else {
    // ev.key 是混合大小写（'Tab' / 'ArrowUp' / 'PageUp'），而表键全大写，必须先归一
    const name = SPECIALS[key.toUpperCase()]
    if (!name) return ''
    base = `<${name}>`
  }
  const mods = new Set<string>()
  if (ev.ctrlKey) mods.add('C-')
  if (ev.altKey) mods.add('M-')
  if (ev.shiftKey) {
    if (base.length === 1) {
      // 同 normalizeToken：Shift 的效果已在字符本身（大小写/上档符号）里
      if (/[A-Za-z]/.test(base)) base = base.toUpperCase()
    } else {
      mods.add('S-')
    }
  }
  return assemble(mods, base)
}

/* --------------------------------------------------------------------------- */
/* 命令注册表                                                                   */
/* --------------------------------------------------------------------------- */

export interface CommandDef {
  id: string            // 绑定里写的主 id
  title: string         // 绑定表展示名
  description?: string
  /** 可选参数字符串，来自绑定 command 中 ":" 之后的部分（如 "term.send:~/"） */
  handler: (arg: string) => void
}

const commands = new Map<string, CommandDef>()

export function registerCommand(def: CommandDef) {
  commands.set(def.id, def)
}

export function listCommands(): CommandDef[] {
  return [...commands.values()].sort((a, b) => a.id.localeCompare(b.id))
}

/** 解析绑定里的 command 字段（支持 "id:arg"）。未注册返回 undefined。 */
export function resolveCommand(spec: string): { def: CommandDef; arg: string } | undefined {
  if (!spec) return undefined
  if (commands.has(spec)) return { def: commands.get(spec)!, arg: '' }
  const i = spec.indexOf(':')
  if (i > 0) {
    const def = commands.get(spec.slice(0, i))
    if (def) return { def, arg: spec.slice(i + 1) }
  }
  return undefined
}

/* --------------------------------------------------------------------------- */
/* Keymap（前缀树）+ 状态机                                                     */
/* --------------------------------------------------------------------------- */

interface KTNode {
  kids: Map<string, KTNode>
  binding?: KeyBinding
}

export interface EchoInfo {
  /** 已按下的部分序列（等待续键时非空） */
  partial: string
  message: string
  kind: 'pending' | 'info' | 'warn' | 'error'
}

export type EchoListener = (info: EchoInfo) => void

class KeySeqEngine {
  enabled = true
  timeoutMs = 1000
  echo = true
  /** 命令被触发时回调（App 用来做真正的动作解耦，可缺省） */
  onEcho: EchoListener = () => { /* 默认无人监听 */ }
  /** 无匹配命令时的兜底（未注册命令 id） */
  onUnknownCommand: (spec: string) => void = (spec) => {
    this.emit('warn', `未注册的命令：${spec}`)
  }

  // 单棵树：所有绑定都是应用内生效，不区分焦点位置。早先分成 terminal/global 两棵，
  // 导致只要焦点不在终端里（比如刚点过工具栏按钮）应用内序列就全部失效。
  private tree: KTNode = { kids: new Map() }
  private node: KTNode | null = null
  private typed: string[] = []
  private timer = 0
  private describeMode = false

  /** 只改动传进来的字段：省略的字段保持现值，否则“只想翻转开关”会默默把超时打回默认。 */
  configure(cfg: { enabled?: boolean; timeoutMs?: number; echo?: boolean }) {
    if (cfg.enabled !== undefined) this.enabled = cfg.enabled
    if (cfg.timeoutMs !== undefined) {
      this.timeoutMs = cfg.timeoutMs >= 200 && cfg.timeoutMs <= 10000 ? cfg.timeoutMs : 1000
    }
    if (cfg.echo !== undefined) this.echo = cfg.echo
    if (!this.enabled) this.cancel('热键序列已停用')
  }

  /** 重建绑定树（幂等，重复序列后写覆盖先写）。 */
  load(list: KeyBinding[]) {
    this.tree = { kids: new Map() }
    this.cancel('')
    for (const b of list || []) {
      if (!b || !b.keys || !b.command) continue
      if (b.enabled === false) continue
      const { tokens, error } = parseSeq(b.keys)
      if (error) continue // 由绑定表界面负责提示
      this.insert(this.tree, tokens, b)
    }
  }

  private insert(tree: KTNode, tokens: string[], b: KeyBinding) {
    let n = tree
    for (const t of tokens) {
      const k = cacheKey(t)
      let kid = n.kids.get(k)
      if (!kid) { kid = { kids: new Map() }; n.kids.set(k, kid) }
      n = kid
    }
    n.binding = b
  }

  isPending() { return this.node !== null }

  pendingText() { return this.typed.join(' ') }

  cancel(msg?: string) {
    if (this.timer) { clearTimeout(this.timer); this.timer = 0 }
    const had = this.node !== null
    this.node = null
    this.typed = []
    if (msg && had) this.emit('warn', msg)
  }

  /** describe-key：下一次按键只显示其绑定，不执行命令。 */
  startDescribe() {
    this.cancel('')
    this.describeMode = true
    this.emit('info', '请按下要查询的键')
  }

  /**
   * 主入口。调用方是谁无关紧要（终端回调或 window 捕获），两边看到同一棵树、
   * 共用同一份 pending 状态。返回 swallow 表示框架已消费该键，调用方必须阻止其抵达终端。
   */
  feed(ev: KeyboardEvent): Verdict {
    if (!this.enabled) return 'pass'
    const tok = eventToken(ev)
    if (!tok) return 'pass' // 单纯按 Ctrl/Alt/Shift 不影响序列

    if (this.describeMode) {
      this.describeMode = false
      const r = this.lookupFrom(this.tree, tok)
      if (r.kind === 'match' && r.binding) this.emit('info', `${tok} → ${r.binding.command}`)
      else if (r.kind === 'prefix') this.emit('info', `${tok} 是前缀（后续还有绑定）`)
      else this.emit('info', `${tok} 未绑定`)
      return 'swallow'
    }

    const from = this.node || this.tree
    const r = this.lookupFrom(from, tok)
    if (r.kind === 'none') {
      if (this.node) {
        // 等待续键中按了无关键：整个序列作废并吞掉此键，避免半个序列漏进远端
        const partial = this.pendingText()
        this.cancel('')
        this.emit('warn', `无效续键 ${tok}，已放弃序列 ${partial}`)
        return 'swallow'
      }
      return 'pass'
    }

    this.typed.push(tok)
    if (r.kind === 'match' && r.binding) {
      const b = r.binding
      this.cancel('')
      this.runBinding(b)
      return 'swallow'
    }

    // 前缀：吞键并等待续键
    this.node = r.node
    this.armTimeout()
    if (this.echo) this.emit('pending', this.typed.join(' ') + ' -')
    return 'swallow'
  }

  private runBinding(b: KeyBinding) {
    const hit = resolveCommand(b.command)
    if (!hit) { this.onUnknownCommand(b.command); return }
    try {
      hit.def.handler(hit.arg)
    } catch (e) {
      this.emit('error', `命令 ${b.command} 执行失败：${e}`)
    }
  }

  private armTimeout() {
    if (this.timer) clearTimeout(this.timer)
    this.timer = setTimeout(() => {
      const partial = this.typed.join(' ')
      this.cancel('')
      if (partial) this.emit('warn', `等待续键超时，已放弃 ${partial}`)
    }, this.timeoutMs)
  }

  private lookupFrom(from: KTNode, tok: string):
    { kind: 'none' } | { kind: 'prefix'; node: KTNode } | { kind: 'match'; binding?: KeyBinding; node: KTNode } {
    const kid = from.kids.get(cacheKey(tok))
    if (!kid) return { kind: 'none' }
    if (kid.binding) return { kind: 'match', binding: kid.binding, node: kid }
    if (kid.kids.size) return { kind: 'prefix', node: kid }
    return { kind: 'none' }
  }

  private emit(kind: EchoInfo['kind'], message: string) {
    if (!this.echo && kind === 'pending') return
    this.onEcho({ partial: this.pendingText(), message, kind })
  }

  /** 查询某个序列当前绑到什么（describe-key / 编辑界面用）。 */
  findBinding(seq: string): KeyBinding | undefined {
    const { tokens } = parseSeq(seq)
    let n = this.tree
    for (const t of tokens) {
      const k = n.kids.get(cacheKey(t))
      if (!k) return undefined
      n = k
    }
    return n.binding
  }

  /** 节点是否为前缀（有子但没有命令）。 */
  isPrefix(seq: string): boolean {
    const { tokens } = parseSeq(seq)
    let n = this.tree
    for (const t of tokens) {
      const k = n.kids.get(cacheKey(t))
      if (!k) return false
      n = k
    }
    return !n.binding && n.kids.size > 0
  }
}

/** 全局单例：终端与窗口两个入口共用同一 pending 状态（Emacs 的 unread events 语义）。*/
export const keyseq = new KeySeqEngine()

/* --------------------------------------------------------------------------- */
/* 绑定表静态分析：给编辑界面做冲突/遮蔽提示                                    */
/* --------------------------------------------------------------------------- */

export interface BindingIssue {
  level: 'warn' | 'error'
  text: string
}

/**
 * 分析整张表：重复序列、最短匹配导致的遮蔽（C-x 有命令 ⇒ C-x C-f 不可达）、
 * 以及会牺牲远端高频按键的前缀（<ESC>、C-c 等）。
 */
export function analyzeBindings(list: KeyBinding[]): Map<number, BindingIssue[]> {
  const out = new Map<number, BindingIssue[]>()
  const push = (i: number, it: BindingIssue) => {
    const arr = out.get(i) || []
    arr.push(it)
    out.set(i, arr)
  }
  // 停用行不参与冲突/遮蔽判定，但语法仍要检（否则启用时才发现错）
  const toks = list.map((b) => parseSeq(b.keys || '').tokens)
  // 与 trie 一致的比较口径：字母大小写不敏感，否则两处判定的结果会互相矛盾
  const keys = toks.map((ts) => ts.map(cacheKey).join(' '))
  const eq = (a: string[], b: string[]) => a.length === b.length && a.every((x, i) => cacheKey(x) === cacheKey(b[i]))
  const active = (i: number) => list[i].enabled !== false
  const seen = new Map<string, number>()
  list.forEach((b, i) => {
    const t = toks[i]
    if (!t.length) { push(i, { level: 'error', text: '序列为空或语法错误' }); return }
    if (!active(i)) return
    const key = keys[i]
    const prev = seen.get(key)
    if (prev !== undefined) push(i, { level: 'error', text: `与第 ${prev + 1} 条重复，后者覆盖前者` })
    seen.set(key, i)
    if (t[0] === '<ESC>') push(i, { level: 'warn', text: '<ESC> 是水木等 BBS 的返回主键，作为前缀会吞掉它' })
    if (t[0] === 'C-c') push(i, { level: 'warn', text: 'C-c（0x03）常用于中断/取消当前 BBS 操作，接管后需靠序列超时退回' })
  })
  // 遮蔽检测：某条序列是另一条的前缀，且它在前面（最短匹配优先执行）
  list.forEach((a, i) => {
    const ta = toks[i]
    if (!ta.length || !active(i)) return
    list.forEach((b, j) => {
      if (i === j || !active(j)) return
      const tb = toks[j]
      if (tb.length <= ta.length) return
      if (eq(ta, tb.slice(0, ta.length))) {
        push(i, { level: 'warn', text: `本条会遮蔽更长的序列「${b.keys}」（最短匹配优先执行）` })
      }
    })
  })
  return out
}
