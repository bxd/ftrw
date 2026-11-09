// A single terminal tab backed by xterm.js. Rendering/emulation is delegated
// entirely to xterm.js; this class only wires I/O to the Go backend.
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import { WebLinksAddon } from '@xterm/addon-web-links'
import { SearchAddon } from '@xterm/addon-search'
import '@xterm/xterm/css/xterm.css'
import * as api from './api'
import { keyseq } from './keyseq'
import { translateKey, type KeyContext } from './keymap'
import { extractURLSpans, hostSpanAt, ipSpanAt, scanLines, type PreviewItem } from './linkscan'
import { detectPageState, hitTest, inSpan, rowText, type Hit, type PageState, type Screen } from './menuclick'
import { foregroundFor, makeSgrFilter, type SgrFilter } from './sgrfilter'

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

/**
 * 回滚行数（偏好 scrollbackLines）：没给或给了负数用出厂默认 2000，上限 10 万行。
 * xterm.js 的历史缓冲按行存字符元数据，真给个百万行会把内存和 fit 时间一起拖爆，
 * 所以这里钳住；0 是合法值（不留历史）。
 */
export function scrollbackOf(n?: number): number {
  if (n === undefined || n === null || !Number.isFinite(n) || n < 0) return 2000
  return Math.min(Math.floor(n), 100000)
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

const MENU_STYLE_ID = 'ftrw-menu-click'

/**
 * 点菜单的悬停高亮。原版是把菜单那一行重画一遍（drawMenuSelect），这里不能那么干：
 * xterm 的字符归它自己画，我们只在一层 pointer-events:none 的浮框上描边。
 */
function ensureMenuStyle() {
  if (document.getElementById(MENU_STYLE_ID)) return
  const st = document.createElement('style')
  st.id = MENU_STYLE_ID
  st.textContent = `
.ftrw-menu-hl { position:absolute; display:none; pointer-events:none; z-index:3;
  background:rgba(120,190,255,.14); box-shadow:inset 0 0 0 1px rgba(120,190,255,.55); }
`
  document.head.appendChild(st)
}

const IP_STYLE_ID = 'ftrw-ip-osd'

/**
 * IP 归属地的悬停浮层（原版是 OSD，弹在鼠标旁边）。
 *
 * 为什么不只靠底部状态区：那一行字 4 秒就抹、又在右下角，BBS 上抬手查归属地时
 * 眼睛还在屏幕中间，基本注定看不到——而“查不到”和“没反应”在界面上长得一模一样。
 * 浮层把结果放到眼前，同一条路径同一次动作里就能分清两者。
 */
function ensureIPStyle() {
  if (document.getElementById(IP_STYLE_ID)) return
  const st = document.createElement('style')
  st.id = IP_STYLE_ID
  st.textContent = `
.ftrw-ip-osd { position:absolute; display:none; pointer-events:none; z-index:4;
  max-width:70%; padding:2px 8px; border-radius:3px; font-size:12px; line-height:1.5;
  white-space:nowrap; overflow:hidden; text-overflow:ellipsis;
  background:rgba(22,26,32,.94); color:#dfe6f0; border:1px solid rgba(120,190,255,.5);
  box-shadow:0 2px 8px rgba(0,0,0,.45); }
.ftrw-ip-osd.warn { color:#e0b341; border-color:rgba(224,179,65,.5); }
.ftrw-ip-osd.error { color:#e06c5b; border-color:rgba(224,108,91,.5); }
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
  /**
   * 点到超链接时的去处（由 App.vue 注入：开预览窗口，或交给系统浏览器）。
   * 不在这儿直接 window.open：那会开出 WebView2 自己的弹窗，既不受我们控制也关不掉。
   */
  onLink?: (uri: string) => void
  /**
   * 鼠标点菜单（BBS 菜单页/清单页）：只在 hostType==0 且总开关开着时参与。
   * 关掉后 xterm 的鼠标行为回到原样（只选字），所以它是个可以直接给用户的偏好。
   */
  menuClick = true
  /**
   * SGR 改写器：把站点的「总是高亮 / ANSI 颜色」两个开关落到写入流上（sgrfilter.ts）。
   * 两个开关都是默认值时它是恒等函数，不碰一个字节。
   */
  private sgr: SgrFilter
  private scheme: api.ColorScheme | null = null
  private baseFontSize: number
  private cellUnit: { w: number; h: number } | null = null
  private stretchRaf = 0
  private lockAttempts = 0
  // 点菜单的屏幕指纹与判定结果：只在终端内容真变过时重算，否则每一次鼠标移动
  // 都要把整屏单元格扫一遍。
  private menuRev = 0
  private menuCache: { rev: number; state: PageState } | null = null
  private menuHL: HTMLDivElement | null = null
  private menuHover: Hit | null = null
  private menuDown: { row: number; col: number } | null = null
  private menuScreenEl: HTMLElement | null = null
  // 悬停 IP：只缓存上一次那一行的文本。mousemove 一秒上百次，每次重扫整行
  // 单元格（每格一次 getCell）没必要，而菜单判定用的 menuRev 正好也是内容指纹。
  private ipRowCache: { rev: number; row: number; text: string } | null = null
  private ipHover = ''
  private ipOSD: HTMLDivElement | null = null
  // 当前命中的地址落在哪个单元格（行、以及地址本身的首列）：浮层要跟着它对齐，
  // 而查询结果是异步回来的，回来的时候得知道该往哪儿画。
  private ipCell: { row: number; col: number } | null = null
  // 点一下就钉住：鼠标离开也不收浮层。悬停只适合「瞄一眼」，而「挪开鼠标慢慢看」
  // 需要一次明确的动作 —— 顺带也让「浮层到底有没有画出来」变成一个能主动复现的操作。
  private ipPinned = false

  /** 悬停查 IP 归属地的开关，由 App.vue 按偏好设置；关掉就不再扫行。 */
  ipHoverEnabled = true

  constructor(site: api.Site, container: HTMLDivElement, opts?: { scrollback?: number }) {
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
      scrollback: scrollbackOf(opts?.scrollback),
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
    this.sgr = makeSgrFilter(this.displayOpts())
    this.term.loadAddon(this.fit)
    this.term.loadAddon(this.search)
    this.term.loadAddon(new WebLinksAddon((_e, uri) => this.onLink?.(uri)))

    this.term.open(container)
    this.applyScheme(null)
    this.applyAntiAlias()

    // Emacs 式键序列：命中绑定就吞掉该键（不发往远端），未命中一律透传给终端。
    // 吞键时还要顶掉浏览器的默认行为：否则 xterm 自己不处理，由“普通字母”开头的
    // 前缀（如绑定了 `x y` 后按下的 x）会被浏览器写进辅助 textarea，漏进远端输入。
    this.term.attachCustomKeyEventHandler((ev) => {
      if (ev.type !== 'keydown') return true
      if (keyseq.feed(ev as KeyboardEvent) === 'swallow') {
        ev.preventDefault()
        return false
      }
      // 按键方案（keyType）+ 退格键（backspace）：接管与原生编码不同的那几个键。
      // 放在键序列之后，热键绑定永远优先于站点的按键方案。
      const seq = translateKey(ev as KeyboardEvent, this.keyCtx())
      if (seq !== null) {
        // input(…, false)：走 onData 同一条出口发给远端，但不触发 xterm 的“真用户输入”
        // 副作用（清选区、滚动到底），选中的文字不会因为按了 Home 就没了。
        this.term.input(seq, false)
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
      this.menuRev++ // 行列变了，页面类型得重新判
    })
    this.term.onWriteParsed(() => { this.menuRev++ })
    this.term.onScroll(() => { this.menuRev++ }) // 滚出原来的屏，光标行也就不再是对照点
    this.wireMenuClick()
  }

  /** 按键方案/退格键的当前判据：每次按键现取，站点编辑框改完立即生效。 */
  private keyCtx(): KeyContext {
    return {
      keyType: this.site.keyType ?? 0,
      backspace: this.site.backspace ?? 1,
    }
  }

  /** 偏好里的回滚行数改了：套到已经开着的标签，不必重开（xterm 自己截断超出的历史）。 */
  setScrollback(n?: number) {
    this.term.options.scrollback = scrollbackOf(n)
  }

  applyScheme(scheme: api.ColorScheme | null) {
    if (!scheme) return
    this.scheme = scheme
    this.applyTheme()
  }

  /** 站点上两个显示开关的当前值（旧站点缺 useAnsiColor 时按原版的默认「开」处理）。 */
  private displayOpts() {
    return {
      ansiColor: this.site.useAnsiColor !== false,
      alwaysHighlight: !!this.site.alwaysHighlight,
    }
  }

  /**
   * 配色方案 + 显示开关 -> xterm theme。
   *
   * 默认前景只能靠 theme.foreground 表达（正文里大量单元格根本不带 SGR）：原版默认是
   * colors_[7]，开了总是高亮是 colors_[15]，黑白模式恒为 colors_[7]。
   */
  private applyTheme() {
    const scheme = this.scheme
    if (!scheme) return
    const opts = this.displayOpts()
    const theme: any = {
      background: scheme.background,
      foreground: foregroundFor(opts, scheme),
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
    this.sgr = makeSgrFilter(this.displayOpts()) // 高亮/ANSI 开关也走这条路：改完立即生效
    this.applyTheme()
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
    this.term.write(this.sgr(data))
  }

  /**
   * 状态变更。连上（含自动重连）时必须清一次 SGR 状态：远端会把属性复位到默认，
   * 我们记着的上一条 fg/bg 就成了脏数据，黑白模式下会把新画面刷错色。
   */
  setStatus(s: string) {
    this.status = s
    if (s === 'connected') this.sgr.reset()
  }

  /**
   * 当前屏幕上的超链接清单（按出现顺序，同一地址只留一次）。
   *
   * 只扫可见区而不是整个回滚缓冲区：一屏就是一帖的阅读现场，把用户早就翻过去的
   * 老图也收进「下一个」里，循环浏览就变成考古了。
   */
  collectLinks(): PreviewItem[] {
    const b = this.term.buffer.active
    const start = this.viewportTop(b)
    const lines: string[] = []
    for (let i = start; i < start + this.term.rows; i++) {
      lines.push(b.getLine(i)?.translateToString(true) ?? '')
    }
    return scanLines(lines)
  }

  focus() { this.term.focus() }

  /* ---------------------------- 鼠标点菜单 ---------------------------- */

  /**
   * 把鼠标事件接上。不用 xterm 自己的鼠标事件：它的公开 API 只在远端开了鼠标上报
   * 协议（1000/1006 模式）时才给带列行的事件，而 BBS 多数只认键认不用鼠标。
   * DOM 监听自己算单元格，无论远端开不开鼠标模式都一致。
   *
   * 监听都挂在根元素上，但 mousemove/mousedown/mouseup 必须用捕获阶段（capture:true）：
   * xterm 的 SelectionService 在同一个根元素上也听 mousemove，并且一进来就调
   * e.stopImmediatePropagation()（只为它自己拖选时不再通知旁人）。我们是在 term.open()
   * 之后才挂监听的，同一元素、同一阶段就按注册顺序跑，所以冒泡阶段那个永远轮不到 ——
   * 表现为「悬停什么也不会发生」，而在代码里看上去完全正确。捕获阶段跑在子节点目标阶段
   * 与任何冒泡监听之前，因此抢在它前面拿到事件。
   */
  private wireMenuClick() {
    const el = this.term.element
    if (!el) return
    this.menuScreenEl = el.querySelector('.xterm-screen')
    el.addEventListener('mousemove', this.onMenuMove, true)
    el.addEventListener('mouseleave', this.onMenuLeave)
    el.addEventListener('mousedown', this.onMenuDown, true)
    el.addEventListener('mouseup', this.onMenuUp, true)
  }

  /**
   * 可见区第一行在缓冲区里的绝对行号。
   *
   * xterm 的 viewportY 已经是绝对行号（文档原话「The line within the buffer where
   * the top of the viewport is」），baseY 是「滚到底时可见区的行号」，两者相加会把
   * 行号翻倍：一有回滚内容（真实读帖子必然有）就全落在缓冲区外面，getLine 返回
   * undefined，于是整屏读出来是纯空白。表现就是鼠标菜单、链接清单、IP 归属地一起
   * 失灵，而在刚连上、屏幕还没滚动的测试里全都正常 —— 这个坑就是这么躲过自检的。
   */
  private viewportTop(b: { viewportY: number }): number {
    return b.viewportY
  }

  /** 当前可见区的一屏只读视图（单元格按显示列展开，行结果就地缓存）。 */
  private screen(): Screen {
    const b = this.term.buffer.active
    const cols = this.term.cols
    const rows = this.term.rows
    const start = this.viewportTop(b)
    // 光标那对（baseY + cursorY）确实要相加：公开 API 的 cursorY 是相对 baseY 的页内行号
    const caretAbs = b.baseY + b.cursorY
    const caretRow = caretAbs >= start && caretAbs < start + rows ? caretAbs - start : -1
    const cache = new Map<number, string[]>()
    return {
      rows,
      cols,
      caretRow,
      rowCells: (row: number): string[] => {
        let arr = cache.get(row)
        if (arr) return arr
        arr = []
        const line = b.getLine(start + row)
        for (let x = 0; x < cols; x++) {
          const c = line?.getCell(x)
          const ch = c ? c.getChars() : ''
          if (!ch || ch === ' ') {
            arr.push(' ')
            continue
          }
          arr.push(ch)
          if (c && c.getWidth() === 2) { arr.push(''); x++ } // 宽字符占两列，右半格留空串
        }
        if (arr.length > cols) arr.length = cols
        cache.set(row, arr)
        return arr
      },
    }
  }

  private pageStateOf(s: Screen): PageState {
    if (this.menuCache && this.menuCache.rev === this.menuRev) return this.menuCache.state
    const st = detectPageState(s)
    this.menuCache = { rev: this.menuRev, state: st }
    return st
  }

  /** 鼠标像素 -> 单元格；落在屏幕区（行高、滚动条）以外返回 null。 */
  private cellAt(ev: MouseEvent): { row: number; col: number } | null {
    const host = this.menuScreenEl
    const cell = this.readCellPx()
    if (!host || !cell) return null
    const r = host.getBoundingClientRect()
    const col = Math.floor((ev.clientX - r.left) / cell.width)
    const row = Math.floor((ev.clientY - r.top) / cell.height)
    if (row < 0 || col < 0 || row >= this.term.rows || col >= this.term.cols) return null
    return { row, col }
  }

  private menuOn(): boolean {
    // 只给 BBS 站点：hostType!=0 的站点（裸 shell、非 smth 类）上这套启发式没根据。
    if (!this.menuClick || this.site.hostType !== 0 || !this.id) return false
    // 远端已经开了鼠标上报（1000/1002/1003 之类）：那它自己要收鼠标事件，
    // 我们再叠一套按键上去就是同时两个动作（vim/tmux 这类最容易碰）。
    return this.term.modes.mouseTrackingMode === 'none'
  }

  private probeHit(row: number, col: number): Hit | null {
    if (!this.menuOn()) return null
    const s = this.screen()
    const state = this.pageStateOf(s)
    if (state === 'unknown') return null
    const hit = hitTest(s, state, row, col)
    if (!hit) return null
    // 链接优先：整屏都是 URL 的页面（图贴）不该被当成菜单
    if (inSpan(extractURLSpans(rowText(s, row)), col)) return null
    return hit
  }

  /**
   * 某一行当前的纯文本，带一格缓存。
   *
   * 行文本只由「内容指纹 + 行号」决定：menuRev 在每次 onWriteParsed /
   * onScroll / onResize 上都进一，所以不会拿到旧屏。光标移动不改内容，
   * 也就不需要重扫。
   */
  private rowTextOf(row: number): string {
    if (this.ipRowCache && this.ipRowCache.rev === this.menuRev && this.ipRowCache.row === row) {
      return this.ipRowCache.text
    }
    const text = rowText(this.screen(), row)
    this.ipRowCache = { rev: this.menuRev, row, text }
    return text
  }

  /**
   * 把归属地写到鼠标旁边的浮层上。文本由 App.vue 回填（查到什么写什么，
   * 失败与「库里没这条」也写）；空串即收起。
   */
  setIPOsd(text: string, kind: 'info' | 'warn' | 'error' = 'info') {
    const host = this.menuScreenEl
    if (!host) return
    if (!text) {
      if (this.ipOSD) this.ipOSD.style.display = 'none'
      return
    }
    ensureIPStyle()
    if (!this.ipOSD) {
      const d = document.createElement('div')
      d.className = 'ftrw-ip-osd'
      host.appendChild(d)
      this.ipOSD = d
    }
    this.ipOSD.textContent = text
    this.ipOSD.className = 'ftrw-ip-osd' + (kind === 'info' ? '' : ' ' + kind)
    this.ipOSD.style.display = 'block'
    this.placeIPOsd()
  }

  /**
   * 浮层落在地址首列的下一行；贴底翻上、贴右左缩。尺寸是写完文案之后现量
   * 的（中文串长短不一），所以这里只可能在 setIPOsd 之后被调一次才准确。
   */
  private placeIPOsd() {
    const host = this.menuScreenEl
    const osd = this.ipOSD
    const cell = this.ipCell
    if (!host || !osd || osd.style.display === 'none') return
    const px = this.readCellPx()
    if (!cell || !px) return
    const w = osd.offsetWidth
    const h = osd.offsetHeight
    let left = cell.col * px.width
    let top = (cell.row + 1) * px.height + 3
    if (top + h > host.clientHeight) top = Math.max(0, cell.row * px.height - h - 3)
    if (left + w > host.clientWidth) left = Math.max(0, host.clientWidth - w - 2)
    osd.style.left = `${left}px`
    osd.style.top = `${top}px`
  }

  private setMenuHover(hit: Hit | null) {
    const host = this.menuScreenEl
    if (!host) return
    ensureMenuStyle()
    if (!this.menuHL) {
      const d = document.createElement('div')
      d.className = 'ftrw-menu-hl'
      host.appendChild(d)
      this.menuHL = d
    }
    const cell = this.readCellPx()
    if (!hit || !cell) {
      this.menuHL.style.display = 'none'
      host.style.cursor = ''
      this.menuHover = null
      return
    }
    const st = this.menuHL.style
    st.display = 'block'
    st.left = `${hit.x0 * cell.width}px`
    st.top = `${hit.row * cell.height}px`
    st.width = `${Math.max(cell.width, (hit.x1 - hit.x0) * cell.width)}px`
    st.height = `${cell.height}px`
    host.style.cursor = hit.cursor
    this.menuHover = hit
    this.onMenuHint?.(hit.label)
  }

  /**
   * 某一格上压着的地址：IPv4（含打码写法）优先，其次不在 URL 里的域名。
   *
   * 悬停、点击、诊断共用一套判据。屏幕上很多「来源」写的不是 IPv4 而是域名
   * （newsmth.net），只认数字就会出现「鼠标停在上面没反应」；反过来域名要发一次 DNS，
   * 但 App.vue 有 5 分钟前端缓存、后端也有解析缓存，同一屏反复扫不留隐患。
   */
  private addrAt(row: number, col: number): { text: string; start: number } | null {
    if (!this.ipHoverEnabled || !this.id) return null
    const line = this.rowTextOf(row)
    const ip = ipSpanAt(line, col)
    if (ip) return ip
    // URL 区间里的不算：点在 https://x/y 上是要开链接，不是查站点归属地
    if (inSpan(extractURLSpans(line), col)) return null
    return hostSpanAt(line, col)
  }

  private onMenuMove = (ev: MouseEvent) => {
    const at = this.cellAt(ev)
    // 文本上的地址是精确签名，菜单矩形是启发式判出来的：压着地址时按地址走。
    // 以前是菜单优先，后果是菜单页里写着来源 IP 的那几行永远查不出归属地，
    // 而那种页面恰恰最想查。点击送按键的行为（onMenuDown）不动。
    const span = at ? this.addrAt(at.row, at.col) : null
    const ip = span?.text ?? ''
    if (ip !== this.ipHover) {
      // 钉住期间不空发：离开地址格的那一下会顺手把刚钉住的浮层关掉
      if (ip || !this.ipPinned) {
        this.ipHover = ip
        this.onIPHint?.(ip)
      }
    }
    if (span && at) {
      const moved = !this.ipCell || this.ipCell.row !== at.row || this.ipCell.col !== span.start
      this.ipCell = { row: at.row, col: span.start }
      if (moved) this.placeIPOsd()
      if (this.menuHover) this.setMenuHover(null) // 两个提示抢同一行，留地址那个
      return
    }
    this.ipCell = null
    const hit = at ? this.probeHit(at.row, at.col) : null
    // 没变化时不要反复写 style：mousemove 一秒能来上百次
    if ((hit?.label ?? '') === (this.menuHover?.label ?? '') && (hit?.row ?? -1) === (this.menuHover?.row ?? -1)) return
    this.setMenuHover(hit)
  }

  private onMenuLeave = () => {
    this.setMenuHover(null)
    if (this.ipPinned) return // 钉住的那条正是给「鼠标挪走之后还能读」用的
    if (this.ipHover) { this.ipHover = ''; this.onIPHint?.('') }
    this.ipCell = null
    this.setIPOsd('')
  }

  private onMenuDown = (ev: MouseEvent) => {
    // 带了修饰键的点击另有用途（选字、将来的扩展），一律不当点菜单。Alt 单独按下要
    // 留下坐标：Alt+单击是归属地诊断，不看菜单。
    if (ev.button !== 0 || ev.shiftKey || ev.ctrlKey || ev.metaKey) {
      this.menuDown = null
      return
    }
    const at = this.cellAt(ev)
    this.menuDown = at ? { row: at.row, col: at.col } : null
  }

  private onMenuUp = (ev: MouseEvent) => {
    const down = this.menuDown
    this.menuDown = null
    if (ev.button !== 0) return
    const at = this.cellAt(ev)
    // Alt+单击 = 诊断：不管命中与否，都把那一格的坐标与原文报出去（坐标自己算不出
    // 也要说，那就是另一种坏法）。归属地这种事在「什么都没弹」时根本分不开是没识别、
    // 没坐标还是没显示，隔一轮猜代价太高。
    if (ev.altKey && !ev.ctrlKey && !ev.metaKey) {
      if (at) this.probeCell(at.row, at.col)
      else this.reportProbe('诊断 坐标算不出（屏幕元素缺失，或单元格尺寸为 0）')
      return
    }
    if (!down) return
    // 按下与松开不在同一格 = 用户在拉选区，这一点属于选字，不送键
    if (!at || at.row !== down.row || at.col !== down.col) return
    // 地址优先于菜单（与悬停同一套判据）：点在地址上就是钉归属地，不送按键。
    const span = this.addrAt(at.row, at.col)
    if (span) {
      this.pinIP(span.text, at.row, span.start)
      return
    }
    if (this.ipPinned) { this.ipPinned = false; this.setIPOsd('') } // 点别处即收起
    const hit = this.probeHit(at.row, at.col)
    if (!hit) return
    this.term.clearSelection()
    if (this.id) api.Write(this.id, hit.keys.join(''))
    this.setMenuHover(null) // 屏幕马上就要变，留着高亮就是个假提示
  }

  /** 钉住某个地址：浮层按首列对齐，鼠标挪开也不收。 */
  private pinIP(text: string, row: number, start: number) {
    this.ipCell = { row, col: start }
    this.ipPinned = true
    this.ipHover = text
    this.onIPHint?.(text) // 同一个地址会命中 App.vue 的缓存，回来的只是显示
    if (this.menuHover) this.setMenuHover(null)
  }

  /** Alt+单击的诊断：把坐标、列号、那一行原文发回给界面与日志。 */
  private probeCell(row: number, col: number) {
    const line = this.rowTextOf(row)
    const ip = ipSpanAt(line, col)
    const host = ip ? null : hostSpanAt(line, col)
    // 诊断的浮层要就地在点的那一格出现，而不是落到左上角
    this.ipCell = { row, col }
    const head = `诊断 行${row + 1} 列${col + 1}`
    const from = Math.max(0, col - 10)
    const seg = line.slice(from, from + 44).replace(/ +$/, '')
    let msg: string
    if (ip) {
      msg = `${head} 命中 IPv4 ${ip.text}`
      this.pinIP(ip.text, row, ip.start)
    } else if (host) {
      msg = `${head} 命中主机名 ${host.text}`
      this.pinIP(host.text, row, host.start)
    } else {
      msg = `${head} 未命中（开关 ${this.ipHoverEnabled ? '开' : '关'}, id ${this.id ? '有' : '无'}）「${seg}」`
      this.setIPOsd(msg, 'warn')
    }
    this.onIPProbe?.(msg)
  }

  /** 只往日志与浮层写一句诊断，不查库。 */
  private reportProbe(msg: string) {
    this.setIPOsd(msg, 'warn')
    this.onIPProbe?.(msg)
  }

  /** 悬停命中的提示（「菜单项 3」），由 App.vue 接到状态区；不接就不显示。 */
  onMenuHint?: (label: string) => void
  /** 悬停命中 / 离开 IPv4（空串表示离开），由 App.vue 负责查库与显示。 */
  onIPHint?: (ip: string) => void
  /** Alt+单击的诊断文本，由 App.vue 写进后端日志（界面那边已经就地弹了）。 */
  onIPProbe?: (msg: string) => void

  dispose() {
    cancelAnimationFrame(this.stretchRaf)
    const el = this.term.element
    if (el) {
      // 摘除必须跟挂上时同一个 capture 标志，否则照旧收不到事件（dispose 里没人报错）
      el.removeEventListener('mousemove', this.onMenuMove, true)
      el.removeEventListener('mouseleave', this.onMenuLeave)
      el.removeEventListener('mousedown', this.onMenuDown, true)
      el.removeEventListener('mouseup', this.onMenuUp, true)
    }
    this.menuHL?.remove()
    this.menuHL = null
    this.ipOSD?.remove()
    this.ipOSD = null
    this.ipPinned = false
    this.ipCell = null
    this.term.dispose()
  }
}
