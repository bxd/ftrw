// A single terminal tab backed by xterm.js. Rendering/emulation is delegated
// entirely to xterm.js; this class only wires I/O to the Go backend.
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import { WebLinksAddon } from '@xterm/addon-web-links'
import { SearchAddon } from '@xterm/addon-search'
import '@xterm/xterm/css/xterm.css'
import * as api from './api'
import { keyseq } from './keyseq'

/**
 * 中英分别字体：xterm.js 只有一个 fontFamily，但 CSS 字体栈是按字符逐个回退的
 * ——ASCII 字形在第一字体（英文）就命中，中文因它无中文字形而回退到第二字体，
 * 效果等价于原版的 englishFont_/nonEnglishFont_ 双字体逐字选字形。
 */
export function fontStack(site: Pick<api.Site, 'fontName' | 'fontNameCjk'>): string {
  const parts: string[] = []
  const push = (f?: string) => {
    const name = (f || '').trim().replace(/['"]/g, '')
    if (name && !parts.includes(`'${name}'`)) parts.push(`'${name}'`)
  }
  push(site.fontName)
  push(site.fontNameCjk)
  if (!parts.length) { parts.push("'Cascadia Code'", "'Consolas'") }
  parts.push('monospace')
  return parts.join(', ')
}

/** 行距 px 换算为 xterm 需要的行高倍率。 */
export function lineHeightOf(fontSize: number, lineSpacing: number): number {
  const fs = fontSize || 14
  return Math.max(0.8, Math.min(3, 1 + (lineSpacing || 0) / fs))
}

const AA_STYLE_ID = 'ftrw-font-smoothing'

/** 抗锯齿开关：xterm 5.5 默认 DOM 渲染器，字符是真实 DOM 文本，CSS 可直接控制。 */
function ensureAAStyle() {
  if (document.getElementById(AA_STYLE_ID)) return
  const st = document.createElement('style')
  st.id = AA_STYLE_ID
  st.textContent = `
.xterm.aa-off .xterm-rows, .xterm.aa-off .xterm-rows span,
.xterm.aa-off .xterm-helper-textarea { -webkit-font-smoothing: none; font-smooth: never; text-rendering: geometricPrecision; }
.xterm.aa-on .xterm-rows, .xterm.aa-on .xterm-rows span { -webkit-font-smoothing: antialiased; }
`
  document.head.appendChild(st)
}

export class TermTab {
  id: string
  title: string
  status = 'connecting'
  site: api.Site
  term: Terminal
  fit: FitAddon
  search: SearchAddon
  container: HTMLDivElement
  encoderID: number

  // FQTerm 式拉伸：固定行数，把字号放大到刚好填满高度，再让列数随宽度回流填满，
  // 上下左右都填满；若有少量残余空隙则居中。
  // 对齐原版 isFontAutoFit_ 三态：1=字号随窗口（默认）0=行列随窗口 2=固定行列。
  fontAutoFit = 1
  centerContent = true
  private baseFontSize: number
  private cellUnit: { w: number; h: number } | null = null
  private stretchRaf = 0
  private lockAttempts = 0

  constructor(site: api.Site, container: HTMLDivElement) {
    this.site = site
    this.id = ''
    this.title = site.name || `${site.host}:${site.port}`
    this.container = container
    this.encoderID = site.serverEncodingID
    this.baseFontSize = site.fontSize || 14
    this.fontAutoFit = site.fontAutoFit ?? 1

    this.term = new Terminal({
      fontFamily: fontStack(site),
      fontSize: this.baseFontSize,
      // 字距：xterm 的 letterSpacing 对中西文一律生效（原版 charSpacing_ 只加在英文字符上），
      // 这是架构差异；BBS 场景下字距通常保持 0，仅用于拉开过密的英文字体。
      letterSpacing: site.charSpacing || 0,
      lineHeight: lineHeightOf(this.baseFontSize, site.lineSpacing),
      cursorStyle: (['block', 'underline', 'bar'][site.cursorType] || 'block') as 'block' | 'underline' | 'bar',
      scrollback: 3000,
      convertEol: false,
      cols: site.numColumns || 80,
      rows: site.numRows || 25,
      theme: {
        background: '#16181c',
        foreground: '#c8ccd4',
      },
    })
    this.fit = new FitAddon()
    this.search = new SearchAddon()
    this.term.loadAddon(this.fit)
    this.term.loadAddon(this.search)
    this.term.loadAddon(new WebLinksAddon((_e, uri) => window.open(uri, '_blank')))

    this.term.open(container)
    this.applyScheme(null)
    this.applyAntiAlias()

    // Emacs 式键序列：命中绑定就吞掉该键（不发往远端），未命中一律透传给终端。
    // 吞键时还要顶掉浏览器的默认行为：否则 xterm 自己不处理，由“普通字母”开头的
    // 前缀（如绑定了 `x y` 后按下的 x）会被浏览器写进辅助 textarea，漏进远端输入。
    this.term.attachCustomKeyEventHandler((ev) => {
      if (ev.type !== 'keydown') return true
      if (keyseq.feed(ev as KeyboardEvent, 'terminal') === 'swallow') {
        ev.preventDefault()
        return false
      }
      return true
    })

    // User input -> backend
    this.term.onData((data) => {
      if (this.id) api.Write(this.id, data)
    })
    // Enter key / paste handled by onData. Resize -> backend
    this.term.onResize(({ cols, rows }) => {
      if (this.id) api.Resize(this.id, cols, rows)
    })
  }

  applyScheme(scheme: api.ColorScheme | null) {
    if (!scheme) return
    const theme: any = {
      background: scheme.background,
      foreground: scheme.foreground,
      cursor: scheme.cursor,
      selectionBackground: scheme.foreground + '33',
    }
    const names = ['black', 'red', 'green', 'yellow', 'blue', 'magenta', 'cyan', 'white',
      'brightBlack', 'brightRed', 'brightGreen', 'brightYellow', 'brightBlue', 'brightMagenta', 'brightCyan', 'brightWhite']
    scheme.palette.forEach((c, i) => { if (names[i]) theme[names[i]] = c })
    this.term.options.theme = theme
  }

  /** 站点编辑后即时套用新的字体/排版（对齐原版“保存即刷新窗口”，无需重连）。 */
  applyFontOptions(site?: api.Site) {
    if (site) this.site = site
    this.baseFontSize = this.site.fontSize || 14
    this.fontAutoFit = this.site.fontAutoFit ?? 1
    this.term.options.fontFamily = fontStack(this.site)
    this.term.options.letterSpacing = this.site.charSpacing || 0
    this.term.options.lineHeight = lineHeightOf(this.baseFontSize, this.site.lineSpacing)
    this.term.options.fontSize = this.baseFontSize
    this.applyAntiAlias()
    this.resetStretch()
    this.fitNow()
  }

  /** 抗锯齿：默认 DOM 渲染器下字符是真实文本，用 CSS 开/关灰度抗锯齿。 */
  applyAntiAlias() {
    ensureAAStyle()
    const el = this.container.querySelector('.xterm') as HTMLElement | null
    if (!el) return
    el.classList.toggle('aa-on', !!this.site.antiAlias)
    el.classList.toggle('aa-off', !this.site.antiAlias)
  }

  fitNow() {
    const node = this.container
    if (!node || node.clientWidth === 0 || node.clientHeight === 0) return
    switch (this.fontAutoFit) {
      case 0: this.fitByGrid(); return
      case 2: this.fitFixed(); return
      default: this.fitStretch(); return
    }
  }

  /** 原版 isFontAutoFit_==0：字号不动，行列数随窗口像素重算；BBS 保证至少 80×24。 */
  private fitByGrid() {
    if (this.term.options.fontSize !== this.baseFontSize) this.term.options.fontSize = this.baseFontSize
    this.cellUnit = null
    try { this.fit.fit() } catch { /* ignore during transition */ }
    let cols = this.term.cols
    let rows = this.term.rows
    if (this.site.hostType === 0) {
      cols = Math.max(cols, 80)
      rows = Math.max(rows, 24)
    } else {
      cols = Math.max(cols, 10)
      rows = Math.max(rows, 10)
    }
    if (cols !== this.term.cols || rows !== this.term.rows) this.term.resize(cols, rows)
  }

  /** 原版 isFontAutoFit_==2（else 分支）：固定用站点配置的行列数，窗口变化不影响。 */
  private fitFixed() {
    if (this.term.options.fontSize !== this.baseFontSize) this.term.options.fontSize = this.baseFontSize
    this.cellUnit = null
    this.term.resize(this.site.numColumns || 80, this.site.numRows || 25)
  }

  /** 原版 isFontAutoFit_==1：字号填满高度，列数随宽度回流（保留现有等比拉伸）。 */
  private fitStretch() {
    const node = this.container
    const base = this.baseFontSize
    const cs = getComputedStyle(node)
    const W = node.clientWidth - parseFloat(cs.paddingLeft) - parseFloat(cs.paddingRight)
    const H = node.clientHeight - parseFloat(cs.paddingTop) - parseFloat(cs.paddingBottom)
    if (W <= 0 || H <= 0) return

    // 测量每 1px 字号对应的单元格像素（需渲染器就绪）。xterm 5.5 的 FitAddon 在
    // dimensions.css.cell 为 0 时会直接 return，因此不能盲信 fit() 结果。
    if (!this.cellUnit) {
      this.term.options.fontSize = base
      try { this.fit.fit() } catch { /* ignore */ }
      const cell = this.readCellPx()
      if (!cell) {
        if (this.lockAttempts++ < 60) {
          cancelAnimationFrame(this.stretchRaf)
          this.stretchRaf = requestAnimationFrame(() => this.fitNow())
        }
        return
      }
      this.cellUnit = { w: cell.width / base, h: cell.height / base }
    }

    const { h: KH } = this.cellUnit
    const targetRows = (this.site.numRows && this.site.numRows > 0) ? this.site.numRows : 25
    // FQTerm 模型：字号由「填满高度」决定（固定行数），再让 fit() 把列数随宽度回流填满。
    const fsH = H / (targetRows * KH)
    let fs = Math.max(6, Math.min(fsH, 64))
    this.term.options.fontSize = fs
    try { this.fit.fit() } catch { /* ignore */ }
    // fit 后 rows≈targetRows、cols 填满宽度；整像素导致总高略超时微缩字号重算。
    cancelAnimationFrame(this.stretchRaf)
    this.stretchRaf = requestAnimationFrame(() => {
      const cell = this.readCellPx()
      if (!cell) return
      const total = cell.height * this.term.rows
      if (total > H) {
        fs = Math.max(6, Math.floor(fs * (H / total)))
        this.term.options.fontSize = fs
        try { this.fit.fit() } catch { /* ignore */ }
      }
    })
  }

  /** 读取 xterm 实际布局的单元格像素尺寸；未就绪时返回 null。 */
  private readCellPx(): { width: number; height: number } | null {
    try {
      const core: any = (this.term as any)._core
      const c = core?._renderService?.dimensions?.css?.cell
      if (c && c.width > 0 && c.height > 0) return { width: c.width, height: c.height }
    } catch { /* ignore */ }
    return null
  }

  /** 字号/字体变化时重置度量，下次 fitNow 重新按新基准测量。 */
  resetStretch() {
    this.cellUnit = null
    this.lockAttempts = 0
  }

  write(data: string) {
    // 水木等 GBK BBS 把选中符 ◆(U+25C6) 当作 2 列宽字符：取消选中时它在该列写
    // 2 个空格，正好清掉 2 列宽的 ◆ 并保留后面的热键字母。xterm 默认把 ◆ 按 1 列
    // 渲染，于是这 2 个空格会连带擦掉后面的字母。给 ◆ 补一个空格让它占 2 列，
    // 既还原字母又与 BBS 的列计算一致（仅对非 UTF-8 的 CJK 会话生效）。
    if (this.encoderID !== 2) data = data.replace(/\u25c6/g, '\u25c6 ')
    this.term.write(data)
  }

  setStatus(s: string) { this.status = s }

  focus() { this.term.focus() }

  dispose() {
    cancelAnimationFrame(this.stretchRaf)
    this.term.dispose()
  }
}
