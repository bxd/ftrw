/**
 * 发帖文本变换 —— 编辑工具栏与发帖热键的算法层。
 *
 * 全是纯函数：不碰 DOM、不碰会话、不读剪贴板，所以能脱离界面直接跑用例。
 *
 * 一条硬约束贯穿始终：分隔符与竖排出来的每个字符，都必须能在水木这类按 GBK/GB2312
 * 解码的站点上原样往返。零宽空格、变体选择符这些「隐形」字符一律不用 —— 后端用的是
 * GB18030 编码器，它能编整个 Unicode，所以这类字符不会报错，而是变成 4 字节序列，
 * 站端解出来就是一串乱码（不报错才是最麻烦的）。
 *
 * 竖排里所有 ASCII 先做全角化也是同一原因：全角形式落在 GB2312 第 03 行，宽度无歧义；
 * 半角字符混在宽字符之间，栅格一定歪。
 */

/** 分隔符的三种用法 */
export type SepMode = 'fixed' | 'randomOne' | 'randomMixed'

export interface SepPreset {
  ch: string
  name: string
}

/**
 * 预置分隔符。挑选标准是「GB2312 里一定收得下」+「在正文里看着不像文字」：
 * 可见 ASCII 全部安全（编码后就是单字节），再加几个 GB2312 符号区的宽字符。
 * 空格类只留全角空格：半角空格会被站端的空白折叠吞掉，等于没插。
 */
export const SEP_PRESETS: SepPreset[] = [
  { ch: '^', name: '脱字符 ^' },
  { ch: '|', name: '竖线 |' },
  { ch: '.', name: '句点 .' },
  { ch: ',', name: '逗号 ,' },
  { ch: '_', name: '下划线 _' },
  { ch: '~', name: '波浪 ~' },
  { ch: '*', name: '星号 *' },
  { ch: '+', name: '加号 +' },
  { ch: '-', name: '连字符 -' },
  { ch: '/', name: '斜杠 /' },
  { ch: '、', name: '顿号' },
  { ch: '·', name: '间隔号' },
  { ch: '□', name: '方框' },
  { ch: '　', name: '全角空格' },
]

/** 没选过就用这个：单字节、不占两列、在 GBK 与 UTF-8 站点都不会变样。 */
export const SEP_DEFAULT = '^'

/** 随机模式的候选池：去掉全角空格，它插进去看不出效果，像没生效。 */
export const SEP_RANDOM_POOL = SEP_PRESETS.filter((p) => p.ch !== '　').map((p) => p.ch)

/**
 * 单个字符能不能安全发出去。
 *
 * 只允许一个字符：多字符（比如用户粘了一串）会让宽度与「字间」的语义都失控。
 * 可见 ASCII、半角/全角空格、以及预置表里的宽字符算安全，其余（emoji、生僻 Unicode）
 * 一律拒 —— 它们能编进 GB18030，但站端按 GBK 解出来是乱码。
 */
export function isSafeSep(ch: string): boolean {
  if (!ch) return false
  const chars = Array.from(ch)
  if (chars.length !== 1) return false
  const c = chars[0]!.codePointAt(0)!
  if (c >= 0x21 && c <= 0x7e) return true
  if (c === 0x20 || c === 0x3000) return true
  return SEP_PRESETS.some((p) => p.ch === chars[0])
}

/**
 * 是不是引用行。这些行不参与变换：编辑器里的引用是服务器插进编辑区的，我们不该动它；
 * 而用户从别处拷来的引文同样带这些前缀，跳过一次就都不会被分隔符搅乱。
 */
const QUOTE_PREFIX = ['>', '】', '※', '▌', '◇', '◆', '「', '原文', '转发', '引用']

export function isQuoteLine(line: string): boolean {
  const s = line.replace(/^[\s\u3000]+/, '')
  return QUOTE_PREFIX.some((p) => s.startsWith(p))
}

/**
 * 几行像是引用。只用来给用户报一句「这几行没动」：分隔符跳过引文是看不出来的，
 * 不说的话容易被当成“没生效”。
 */
export function quoteLineCount(text: string): number {
  return normalizeLines(text).split('\n').filter((l) => isQuoteLine(l)).length
}

export interface SepOptions {
  /** 候选符号；fixed 用第一个，randomOne 从里面挑一个用于整篇，randomMixed 逐字挑 */
  seps: string[]
  mode: SepMode
  /** 注入随机源只为测试可复现，平时不传 */
  rng?: () => number
  /** 跳过引用行，默认开 */
  skipQuotes?: boolean
}

function isBlank(ch: string): boolean {
  return ch === ' ' || ch === '\t' || ch === '　'
}

function pickOne(pool: string[], rng: () => number): string {
  const i = Math.min(pool.length - 1, Math.max(0, Math.floor(rng() * pool.length)))
  return pool[i]!
}

/** 只在两个「非空白」相邻字符之间插，行首行尾与空格两侧都不插。 */
function insertBetween(line: string, pick: (index: number) => string): string {
  const chars = Array.from(line)
  let out = ''
  for (let i = 0; i < chars.length; i++) {
    out += chars[i]
    const next = chars[i + 1]
    if (next === undefined) break
    if (isBlank(chars[i]!) || isBlank(next)) continue
    out += pick(i)
  }
  return out
}

/** 统一成 \n 的 internal 表示；灌入时再按行还原成回车。 */
export function normalizeLines(text: string): string {
  return text.replace(/\r\n?/g, '\n').replace(/[\u0000-\u0008\u000b-\u001f\u007f]/g, '')
}

/**
 * 字间插入分隔符。返回 \n 分隔的文本（交给灌入通道逐行发）。
 *
 * 控制字符在这里一次性抹掉：剪贴板里可能带着 ESC/BS，发出去会被编辑器当成真按键，
 * 后果是把已写的正文擦掉或者触发站端命令，比丢几个不可见字符严重得多。
 */
export function applySeparators(text: string, o: SepOptions): string {
  const pool = o.seps.filter((s) => s)
  if (!pool.length) return normalizeLines(text)
  const rng = o.rng ?? Math.random
  const skip = o.skipQuotes !== false
  const whole = o.mode === 'randomMixed' ? '' : pickOne(pool, rng)
  return normalizeLines(text)
    .split('\n')
    .map((line) => {
      if (skip && isQuoteLine(line)) return line
      if (o.mode === 'randomMixed') return insertBetween(line, () => pickOne(pool, rng))
      return insertBetween(line, () => whole)
    })
    .join('\n')
}

export interface VerticalOptions {
  /** 终端列数：竖排块总宽不能超过它，否则站端自己折行，折完就歪 */
  cols: number
  /** 每列最多几个字（决定块的高度）。超过就自动换到下一列；不给就按出厂的每列上限排 */
  height?: number
  /** 从右往左排（传统竖排读序） */
  rightToLeft?: boolean
}

export interface VerticalResult {
  text: string
  /** 排不下的部分怎么处理的，要原样报给用户 */
  note?: string
}

// 每列最多 10 字：超过 10 字就自动续到下一列。
const V_HEIGHT = 10
const V_MAX_HEIGHT = 10

/**
 * ASCII → 全角形式（U+FF01-FF5E），空格 → 全角空格。其余字符原样：
 * 中文本来就占两列，GB2312 符号区那些字符在站端也按两列渲染，不需要动它们。
 */
export function fullwidthCell(ch: string): string {
  const c = ch.codePointAt(0)!
  if (c === 0x20) return String.fromCharCode(0x3000)
  if (c >= 0x21 && c <= 0x7e) return String.fromCodePoint(c + 0xfee0)
  return ch
}

/**
 * 文字转竖排：每列从上往下读，列与列之间按传统从右往左（可关）。
 *
 * 每列最多 height 字，超过就自动续到下一列；列与列之间（不管是同一段的续列还是不同段落）
 * 都隔一个空列。空格在竖排里没有位置可言，直接抹掉。
 * 宽度按「一格两列」算：一屏 80 列最多 40 格，装不下的列会被截掉并通过 note 说明，
 * 而不是默默少发几段 —— 用户看不到 note 就等于看到内容丢了。
 */
export function toVertical(text: string, o: VerticalOptions): VerticalResult {
  const cols = Math.max(20, Math.min(200, Math.floor(o.cols) || 80))
  const lines = normalizeLines(text)
    .split('\n')
    .map((l) => l.replace(/[\s\u3000]+$/, ''))
    .filter((l) => l.replace(/[\s\u3000]/g, '') !== '')
  if (!lines.length) return { text: '' }

  // 段落 → 定长列。每列之间都插一个 null 空列（算一格宽）；长段落按 height 切成续列
  const columnsFor = (height: number): Array<string[] | null> => {
    const out: Array<string[] | null> = []
    for (const line of lines) {
      const cells = Array.from(line).filter((c) => !isBlank(c)).map(fullwidthCell)
      for (let i = 0; i < cells.length; i += height) {
        if (out.length) out.push(null)  // 列与列之间隔一个空列
        out.push(cells.slice(i, i + height))
      }
    }
    return out
  }

  const widthOf = (colsArr: Array<string[] | null>) => colsArr.length * 2

  let height = o.height && o.height > 0 ? Math.min(V_MAX_HEIGHT, Math.max(1, Math.floor(o.height))) : V_HEIGHT
  let columns = columnsFor(height)
  let note: string | undefined
  // 太宽就先加高（每列字数变多、列数变少），这是唯一不破坏读序的收法
  if (widthOf(columns) > cols && !(o.height && o.height > 0)) {
    while (height < V_MAX_HEIGHT && widthOf(columns) > cols) {
      height += 1
      columns = columnsFor(height)
    }
  }
  if (widthOf(columns) > cols) {
    const keep = Math.floor(cols / 2)
    note = `竖排超出 ${cols} 列，只保留前 ${keep} 列（每列 ${height} 字，可加高或缩短原文）`
    columns = columns.slice(0, keep)
  }

  const order = o.rightToLeft ? [...columns].reverse() : columns
  const rows: string[] = []
  for (let r = 0; r < height; r++) {
    let line = ''
    for (const col of order) {
      const cell = col ? (col[r] ?? '　') : '　'
      line += cell
    }
    rows.push(line.replace(/[\s\u3000]+$/, ''))
  }
  // 尾巴上的空行裁掉：栅格高度按内容最长的列算，多出来那几行发过去只是让编辑区白滚
  while (rows.length && rows[rows.length - 1] === '') rows.pop()
  return { text: rows.join('\n'), note }
}
