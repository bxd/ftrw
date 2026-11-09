/**
 * SGR 改写器 —— 站点上「总是高亮」「ANSI 颜色」两个开关的实现处。
 *
 * 对齐原版 fqterm_screen.cpp::drawStr()：
 * - isAlwaysHighlight_（第 1145 行）：`if (GETBRIGHT(ea) || 总是高亮) cp |= SET_FG_HIGHLIGHT(1)`。
 *   调色板 16 色，FGMASK=0x0f、bit3 是高亮位，所以「高亮」就是把前景索引 +8 落进亮色半区
 *   （fqterm.h 第 119-131 行）。原版默认关（fqterm_param.cpp:22）。
 * - isAnsiColor_ 关掉（第 1183-1200 行）：前景/底色一律不看颜色索引，只问「这一格有没有背景色」：
 *   有 → 黑字白底（pen=colors_[0]、brush=colors_[7]）；没有 → 白字（pen=colors_[7]）铺默认底。
 *   等于一键退回纯黑白，专治配色花哨看不清。原版默认开（=1）。
 *
 * xterm.js 不提供逐单元格调色，所以只能在 term.write() 之前改转义流：把 SGR（CSI … m）里的
 * 颜色参数按上面两条规则重写，其余序列（光标、清屏、OSC 标题…）一律原样放过。
 * 两个开关都是默认值时（ANSI 开、高亮关）返回恒等函数，一个字节都不碰。
 *
 * 状态机必须跨写入块记住 fg/bg/reverse：BBS 常把「先设底色、再逐段设前景」分成两条 SGR 发，
 * 只看单条序列判「有没有背景色」会把后一条误判成无底色，黑白模式下就变成白字压在原底色上
 * ——看不见。同理，块尾被切断的半个序列要先攒着（pending），否则参数解析会错半截。
 */

export interface SgrOptions {
  /** site.useAnsiColor：false = 黑白模式 */
  ansiColor: boolean
  /** site.alwaysHighlight：所有前景色取亮色半区 */
  alwaysHighlight: boolean
}

export interface SgrFilter {
  (data: string): string
  /** 重连后必须调：远端会把 SGR 状态复位，我们记的 fg/bg 也得跟着清。 */
  reset(): void
}

const CSI_PARAM = /[\u0030-\u003f]/ // 参数字节：数字、;、:、? 等
const CSI_INTER = /[\u0020-\u002f]/ // 中间字节：见到就不是我们认的 SGR
const ESC_FINAL = /[\u0040-\u007e]/

export function makeSgrFilter(opts: SgrOptions): SgrFilter {
  const identity = opts.ansiColor && !opts.alwaysHighlight
  let fg: number | null = null // null = 默认前景（原版 NO_COLOR 是索引 7）
  let bg: number | null = null // null = 默认背景（原版是索引 0）
  let reverse = false
  let pending = ''

  /** 一条 SGR 的参数串 -> 改写后的完整序列。 */
  function rewrite(body: string): string {
    // 子参数写法（38:5:196）先拆平，后面只按分号形式走
    const tokens: string[] = []
    for (const t of body.split(';')) {
      if (t.includes(':')) tokens.push(...t.split(':'))
      else tokens.push(t)
    }
    const kept: string[] = []
    let k = 0
    while (k < tokens.length) {
      // 空参数在 SGR 里等价于 0（全复位），不能当畸形丢掉
      const raw = tokens[k]!
      const n = raw === '' ? 0 : Number.parseInt(raw, 10)
      if (!Number.isFinite(n)) {
        k++
        continue
      }
      // 扩展色 38/48 后面跟 5;n 或 2;r;g;b，整组一次吞掉
      if (n === 38 || n === 48) {
        const group = takeExtended(tokens, k, n)
        k = group.next
        if (!opts.ansiColor) {
          if (n === 38) fg = group.index ?? 7
          else bg = group.index ?? 7
        } else if (opts.alwaysHighlight && n === 38 && group.index !== null && group.index < 8) {
          kept.push(`38;5;${group.index + 8}`) // 调色板 0-7 是暗色半区，+8 落进亮色
        } else {
          kept.push(group.text)
        }
        continue
      }
      k++
      if (n === 0) {
        fg = null
        bg = null
        reverse = false
        kept.push('0')
        continue
      }
      // 颜色类参数：高亮模式改索引，黑白模式只记状态、参数本身不要了
      if (n >= 30 && n <= 37) {
        if (opts.ansiColor) kept.push(opts.alwaysHighlight ? String(n + 60) : raw)
        else fg = n - 30
      } else if (n >= 90 && n <= 97) {
        if (opts.ansiColor) kept.push(raw)
        else fg = n - 82 // 90 -> 8
      } else if (n === 39) {
        if (opts.ansiColor) kept.push(raw)
        else fg = null
      } else if (n >= 40 && n <= 47) {
        if (opts.ansiColor) kept.push(raw)
        else bg = n - 40
      } else if (n >= 100 && n <= 107) {
        if (opts.ansiColor) kept.push(raw)
        else bg = n - 92 // 100 -> 8
      } else if (n === 49) {
        if (opts.ansiColor) kept.push(raw)
        else bg = null
      } else if (n === 7) {
        // 黑白模式下反相直接算进结果色，所以把 7 从参数里摘掉，免得 xterm 再交换一次
        if (opts.ansiColor) kept.push(raw)
        else reverse = true
      } else if (n === 27) {
        if (opts.ansiColor) kept.push(raw)
        else reverse = false
      } else {
        kept.push(raw) // 粗体/下划线/隐形…一律留着
      }
    }

    if (opts.ansiColor) return `\x1b[${kept.join(';')}m`
    // 黑白模式：只问「这一格最终有没有背景色」，与原版 GETBG(cp)!=0 同判。
    // 颜色参数排在 kept 之后，所以前面可能有的 0（全复位）不会把它们冲掉。
    let b = bg ?? 0
    if (reverse) b = 15 - (fg ?? 7) // 反相后底色位换成原前景（原版 GET_INVERSE_COLOR）
    const colors = b !== 0 ? ['30', '47'] : ['39', '49']
    return `\x1b[${[...kept, ...colors].join(';')}m`
  }

  /** 解析 38/48 扩展色组：`38;5;n` 或 `38;2;r;g;b`（调用前已把冒号形式拆平）。 */
  function takeExtended(tokens: string[], at: number, base: number): { text: string; index: number | null; next: number } {
    const kind = tokens[at + 1]
    const b = base
    if (kind === '5') {
      const n = Number.parseInt(tokens[at + 2] ?? '', 10)
      const index = Number.isFinite(n) ? n : null
      return { text: `${b};5;${index ?? 0}`, index, next: at + 3 }
    }
    if (kind === '2') {
      const rgb = [tokens[at + 2], tokens[at + 3], tokens[at + 4]].map((x) => x ?? '0')
      return { text: `${b};2;${rgb.join(';')}`, index: null, next: at + 5 }
    }
    return { text: String(b), index: null, next: at + 1 }
  }

  const fn = (data: string): string => {
    if (identity) return data
    const s = pending ? pending + data : data
    pending = ''
    let out = ''
    let i = 0
    while (i < s.length) {
      const esc = s.indexOf('\x1b', i)
      if (esc < 0) {
        out += s.slice(i)
        break
      }
      out += s.slice(i, esc)
      const next = s[esc + 1]
      if (next === undefined) {
        pending = s.slice(esc)
        break
      }
      if (next !== '[') {
        // OSC（ESC ] … BEL / ST）里也满是数字与分号，扫到结束符整段原样放过；没扫到就攒着。
        if (next === ']') {
          const end = findOscEnd(s, esc + 2)
          if (end < 0) {
            pending = s.slice(esc)
            break
          }
          out += s.slice(esc, end)
          i = end
          continue
        }
        out += s.slice(esc, esc + 2) // 两字节 ESC 序列（ESC c、ESC 7 之类）
        i = esc + 2
        continue
      }
      let j = esc + 2
      while (j < s.length && CSI_PARAM.test(s[j]!)) j++
      while (j < s.length && CSI_INTER.test(s[j]!)) j++
      if (j >= s.length) {
        pending = s.slice(esc)
        break // 参数还没发全，等下一块
      }
      if (!ESC_FINAL.test(s[j]!)) {
        // 不像话的字节：原样吐一个字符再走，绝不整段丢掉
        out += s[esc]!
        i = esc + 1
        continue
      }
      const final = s[j]!
      out += final === 'm' ? rewrite(s.slice(esc + 2, j)) : s.slice(esc, j + 1)
      i = j + 1
    }
    return out
  }

  fn.reset = () => {
    fg = null
    bg = null
    reverse = false
    pending = ''
  }
  return fn as SgrFilter
}

/** OSC 序列的结束：BEL，或 ST（ESC \）。找不到返回 -1（序列还没收完）。 */
function findOscEnd(s: string, from: number): number {
  for (let i = from; i < s.length; i++) {
    if (s[i] === '\u0007') return i + 1
    if (s[i] === '\x1b' && s[i + 1] === '\\') return i + 2
  }
  return -1
}

/**
 * 两个开关怎么落到 theme 上：一格 SGR 都没碰的正文只能靠 theme.foreground 表达。
 * 原版默认前景是 colors_[7]，高亮后是 colors_[15]，黑白模式恒为 colors_[7]。
 */
export function foregroundFor(opts: SgrOptions, scheme: { foreground: string; palette: string[] }): string {
  if (!opts.ansiColor) return scheme.palette[7] ?? scheme.foreground
  if (opts.alwaysHighlight) return scheme.palette[15] ?? scheme.foreground
  return scheme.foreground
}
