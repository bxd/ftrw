/**
 * 跨应用热键（系统级）的表示法与校验 —— 纯逻辑，不含 UI。
 *
 * 语法与应用内键序列（keyseq.ts）同源，但形状被砍到只剩两种，因为跨应用时钩子必须
 * 在按下那一刻就决断吞不吞键，不能等续键：
 *
 *   组合键   M-`   C-M-t   S-<F1>   C-<Esc>          —— 必须带至少一个修饰键
 *   纯修饰键 <C> <M> <S> <W> 及组合 <C-M> <C-M-S>     —— 别名 CTRL/ALT/SHIFT/WIN/CMD/META
 *   双击     两段完全相同的 token：<C-M> <C-M>、C-T C-T、<F9> <F9>
 *
 * 这里只做「写法对不对、录出来长什么样」；真正的字符→虚拟键码（VK）映射只在后端
 * globalkey.go 里做（VK 是 Windows 的概念，前端不该知道）。两侧语法必须保持一致，
 * 差异只有：'!' 这类上档符号在这里按字面显示，后端会拆成「1 + Shift」。
 */
import { normalizeToken } from './keyseq'
import type { GlobalKey } from './api'

export const MOD_CTRL = 1
export const MOD_ALT = 2
export const MOD_SHIFT = 4
export const MOD_META = 8

const MOD_NAMES: [number, string][] = [[MOD_CTRL, 'C'], [MOD_ALT, 'M'], [MOD_SHIFT, 'S'], [MOD_META, 'W']]

/** 修饰键名别名（大小写无关）。 */
const MOD_ALIASES: Record<string, number> = {
  C: MOD_CTRL, CTRL: MOD_CTRL, CONTROL: MOD_CTRL,
  M: MOD_ALT, ALT: MOD_ALT,
  S: MOD_SHIFT, SHIFT: MOD_SHIFT,
  W: MOD_META, WIN: MOD_META, CMD: MOD_META, SUPER: MOD_META,
}

/** 尖括号内的纯修饰键写法：<C>、<C-M>、<ALT-CTRL>。 */
const MOD_ONLY_RE = /^<([A-Za-z]+)(?:-([A-Za-z]+))*>$/

/** KeyboardEvent.key 里表示「按的是修饰键本身」的值。 */
const MOD_KEYS = new Set(['Control', 'Alt', 'Shift', 'Meta', 'OS', 'AltGraph'])

export function isModifierKey(key: string): boolean {
  return MOD_KEYS.has(key) || key === 'Dead' || key === 'Unidentified'
}

/** 从事件取当前按住的修饰键集合。 */
export function modsFromEvent(ev: KeyboardEvent | MouseEvent): number {
  let m = 0
  if (ev.ctrlKey) m |= MOD_CTRL
  if (ev.altKey) m |= MOD_ALT
  if (ev.shiftKey) m |= MOD_SHIFT
  if (ev.metaKey) m |= MOD_META
  return m
}

/** 把修饰键位集写成规范 token：1|2 → "<C-M>"。顺序固定 C→M→S→W。 */
export function modsToToken(mods: number): string {
  const names = MOD_NAMES.filter(([bit]) => mods & bit).map(([, n]) => n)
  return `<${names.join('-')}>`
}

/**
 * 归一化单个 token；非法返回 null。
 * 纯修饰键写法在这里就地处理，其余全部交给 keyseq.normalizeToken（同一套字符/特殊键规则）。
 */
export function normalizeGlobalToken(raw: string): string | null {
  const t = (raw || '').trim()
  if (!t) return null
  const m = MOD_ONLY_RE.exec(t)
  if (m) {
    const parts = t.slice(1, -1).split('-')
    let mods = 0
    for (const p of parts) {
      const bit = MOD_ALIASES[p.toUpperCase()]
      if (!bit) return null // 混进了非修饰键名（<C-F1>）→ 交给下面的组合键分支
      if (mods & bit) return null // 重复的修饰键
      mods |= bit
    }
    return modsToToken(mods)
  }
  return normalizeToken(t)
}

/** 是否纯修饰键 token（"<C-M>"）。 */
export function isModOnly(token: string): boolean {
  return MOD_ONLY_RE.test(token) && token.slice(1, -1).split('-').every((p) => !!MOD_ALIASES[p.toUpperCase()])
}

/** 是否带至少一个修饰键。 */
export function hasModifier(token: string): boolean {
  if (isModOnly(token)) return true
  return /^[CMSW]-/.test(token)
}

/** 基键是不是可打字字符（不带尖括号的就是字符；字母数字符号都算）。 */
function isPrintableBase(token: string): boolean {
  const stripped = token.replace(/^[CMSW]-/g, '')
  return !stripped.startsWith('<')
}

/** 比较用的折叠形式：字母大小写不敏感（与后端把 C-X 规范成 C-x 同口径）。 */
function foldToken(token: string): string {
  return token.replace(/[A-Z]$/, (c) => c.toLowerCase())
}

export type GlobalSeqKind = 'chord' | 'double'

export interface GlobalSeqInfo {
  tokens: string[]
  kind: GlobalSeqKind
  error?: string
}

/** 解析一条完整的跨应用热键写法。 */
export function parseGlobalSeq(seq: string): GlobalSeqInfo {
  const parts = (seq || '').trim().split(/\s+/).filter(Boolean)
  if (!parts.length) return { tokens: [], kind: 'chord', error: '序列为空' }
  const tokens: string[] = []
  for (const p of parts) {
    const n = normalizeGlobalToken(p)
    if (!n) return { tokens: [], kind: 'chord', error: `无法识别的键：“${p}”` }
    tokens.push(n)
  }
  if (tokens.length === 1) {
    const t = tokens[0]
    if (isModOnly(t)) {
      return { tokens, kind: 'chord', error: `纯修饰键不能单独成键，双击要写成「${t} ${t}」` }
    }
    if (!hasModifier(t)) {
      return { tokens, kind: 'chord', error: '跨应用热键必须带修饰键（C- / M- / S- / W-），否则会把全局的普通打字吞掉' }
    }
    return { tokens, kind: 'chord' }
  }
  if (tokens.length === 2) {
    if (foldToken(tokens[0]) !== foldToken(tokens[1])) {
      return { tokens, kind: 'double', error: `双击的两段必须是同一个键：${tokens[0]} / ${tokens[1]}` }
    }
    if (!hasModifier(tokens[0]) && isPrintableBase(tokens[0])) {
      return { tokens, kind: 'double', error: '双击不带修饰键时只能用不会打字的键（例：<F9> <F9>），否则打字打到「tt」就会误触发' }
    }
    return { tokens, kind: 'double' }
  }
  return { tokens, kind: 'double', error: `跨应用热键只能是一段组合键或两段相同键的双击（当前 ${tokens.length} 段）` }
}

export interface GlobalKeyIssue {
  level: 'warn' | 'error'
  text: string
}

/** 只带 Ctrl 的编辑键：全局吞掉它会让所有软件都不能复制粘贴，值得单独警告。 */
const EDIT_KEYS = new Set(['c', 'v', 'x', 'z', 'y', 'a', 's'])

/**
 * 整张表的静态检查：语法、重复、以及「会把别的应用的正常操作吞掉」的危险绑定。
 * 返回值按行号索引，界面在每行下面列出对应提示。
 */
export function analyzeGlobalKeys(list: GlobalKey[], actionIds: Set<string>): Map<number, GlobalKeyIssue[]> {
  const out = new Map<number, GlobalKeyIssue[]>()
  const push = (i: number, it: GlobalKeyIssue) => {
    const arr = out.get(i) || []
    arr.push(it)
    out.set(i, arr)
  }
  const seen = new Map<string, number>()
  list.forEach((b, i) => {
    const info = parseGlobalSeq(b.keys || '')
    if (info.error) {
      push(i, { level: 'error', text: info.error })
      return
    }
    if (b.action && !actionIds.has(b.action)) {
      push(i, { level: 'error', text: `动作 “${b.action}” 不存在，可能被新版本改名` })
    }
    if (b.enabled === false) return // 停用行不参与冲突判定
    const key = info.tokens.map(foldToken).join(' ').toUpperCase()
    const prev = seen.get(key)
    if (prev !== undefined) push(i, { level: 'error', text: `与第 ${prev + 1} 条重复，后者永远不会生效` })
    seen.set(key, i)
    const first = info.tokens[0]
    if (info.kind === 'chord' && !isModOnly(first)) {
      const mods = first.replace(/[^CMSW]-/g, '')
      const base = first.replace(/^[CMSW]-/g, '').toLowerCase()
      if (mods === 'C-' && EDIT_KEYS.has(base)) {
        push(i, { level: 'warn', text: `只带 Ctrl 的 ${first} 会在所有软件里吞掉这个编辑键（复制/粘贴/撤销都会受影响），建议加 Alt` })
      }
    }
    if (info.kind === 'double' && !hasModifier(first)) {
      push(i, { level: 'warn', text: '不带修饰键的双击容易在连续打字时误触发，已允许但不推荐' })
    }
  })
  return out
}

/** 给界面的一句话说明：这条写法到底代表什么手势。 */
export function describeGlobalSeq(seq: string): string {
  const info = parseGlobalSeq(seq)
  if (info.error) return info.error
  if (info.kind === 'double') {
    return isModOnly(info.tokens[0])
      ? `双击 ${info.tokens[0].slice(1, -1).split('-').join('+')}（两次按下并松开同一组修饰键）`
      : `连按两次 ${info.tokens[0]}`
  }
  return `按下 ${info.tokens[0]}`
}
