/**
 * 鼠标点菜单 —— 让 BBS 的菜单页与文章列表能用鼠标点。
 *
 * 对齐原版 FQTerm 的链路：detectPageState() → detectMenuRect() → getCursorType()
 * → processLClick()/enterMenuItem()，但**页面类型改判文本结构而不是颜色签名**：
 * 原版看的是「第 0 行背景色索引是不是 4、前景色索引里有没有 14」这类 2008 年水木
 * 排版留下的序号（param_.hostType_==0 才启用），换个配色方案或站改版就全判错，
 * 而 xterm.js 的单元格颜色又是打包过的整数，照抄过来只能猜。这里只认屏面上的
 * 编号项与列表行形状，判不出就返回 unknown。
 *
 * 还有一条与原版不同的护栏，是这个功能能默认开着的前提：**只有鼠标悬停时已经
 * 出现手型光标与整行高亮，点击才会送键**。也就是说判定的错法只会表现成
 * 「这一点没反应」，不会表现成「我不知道它就把按键发给了 BBS」。
 *
 * 本模块是纯函数：不碰 DOM、不依赖 xterm，屏幕内容通过 Screen 接口喂进来
 * （terminal.ts 用 buffer 填）。因此可以拿真实水木屏幕文本单独跑断言。
 */

export type PageState = 'unknown' | 'menu' | 'list'

/**
 * 一屏的只读视图。行/列都是 **显示列**（0 基）：宽字符占两列，所以
 * rowCells 返回的数组长度等于 cols，宽字符右半格是空串。这样上层不必自己
 * 处理中西文宽度，热键字符的位置也就是数组下标。
 */
export interface Screen {
  rows: number
  cols: number
  /** 远端光标所在的可见行；光标被滚出屏幕时是 -1（此时列表页不给判定） */
  caretRow: number
  rowCells(row: number): string[]
}

export interface Hit {
  /** 依次送给远端的原始串（发送时拼成一次写入，与原版逐字节 write 等效） */
  keys: string[]
  row: number
  /** 高亮区间（显示列，含头不含尾） */
  x0: number
  x1: number
  cursor: 'pointer' | 'ns-resize' | 'ew-resize'
  /** 状态区提示文字，顺便当单测里的断言标签 */
  label: string
}

/**
 * 方向键与 Home/End/PageUp/PageDown：与原版 FQTermWindow::directions_ 逐字节一致
 * （fqterm_window.cpp 第 64-75 行）。BBS 的 lrzsz/ curses 类程序认的就是这套 ScTI
 * 写法，别换成 \x1bOH 那种。
 */
export const KEY = {
  home: '\x1b[1~',
  end: '\x1b[4~',
  pageUp: '\x1b[5~',
  pageDown: '\x1b[6~',
  up: '\x1b[A',
  down: '\x1b[B',
  left: '\x1b[D',
  right: '\x1b[C',
  enter: '\r',
}

/**
 * 把一行展开成「一个显示列一个字符」的字符串：空串（宽字符右半格）补成空格。
 *
 * 不能直接 cells.join('')：那会让字串下标比显示列少掉所有中文的那一半，
 * 菜单项区间、URL 区间、鼠标列三者就对不上了。每列只取一个码元，所以
 * 碰上 astral 字（GB18030 扩展汉字、emoji）时会退化成半个代理对 —— 仍然
 * 是个「非空格」，本层只关心这一列有没有内容，判定不受影响。
 */
export function rowText(s: Screen, row: number): string {
  const cells = s.rowCells(row)
  let out = ''
  for (let i = 0; i < s.cols; i++) {
    const ch = cells[i]
    out += ch ? ch[0]! : ' '
  }
  return out
}

function blankAt(text: string, i: number): boolean {
  return i < 0 || i >= text.length || text[i] === ' '
}

/** 行内非空字符数：整行只有边框线、只有空格的行都靠它排掉。 */
function nonBlankCount(text: string): number {
  let n = 0
  for (const ch of text) if (ch !== ' ') n++
  return n
}

const ITEM_MARK = /[0-9a-zA-Z][).\]]/g

export interface MenuEntry {
  /** 热键字符本体，可能是多位数字（10) 送 "10"，比原版只取首字符进一步） */
  token: string
  /** 整个项的显示列区间（含热键与其后的文字），高亮与点击命中都按它 */
  start: number
  end: number
}

/**
 * 取出一行里的编号菜单项。形如 `1)`、`2.`、`a]`，要求：
 * - 序号前面是行首或空白（排掉「v2.0」「10.」这类行文里的东西）；
 * - 序号后面除分隔符外还得有实际文字（排掉句子结尾的「等等.」）；
 * - 项的末尾停在两个连续空格（列间隔）或行尾 —— 与原版取
 *   `cstr.lastIndexOf("   ")` 再算宽度的做法同旨，只是这里直接按列走。
 */
export function itemsInRow(s: Screen, row: number): MenuEntry[] {
  const text = rowText(s, row)
  const out: MenuEntry[] = []
  ITEM_MARK.lastIndex = 0
  let m: RegExpExecArray | null
  while ((m = ITEM_MARK.exec(text)) !== null) {
    const mark = m.index
    // 正则只会落在「0)」上，多位序号自己往前并回来
    let tokenStart = mark
    while (tokenStart > 0 && /[0-9]/.test(text[tokenStart - 1]!)) tokenStart--
    if (!blankAt(text, tokenStart - 1)) continue
    // 分隔符后面紧跟字母/数字的是行文里的编号或小数（「3.5 万」、「2.0」），不是菜单项
    if (/[0-9A-Za-z]/.test(text[mark + 2] ?? ' ')) continue
    const token = text.slice(tokenStart, mark + 1)
    // 项后面得真跟着文字：句子结尾的「等等.」不算
    if (!text.slice(mark + 2).trim()) continue

    let end = mark + 2
    const limit = Math.min(text.length, mark + 60)
    while (end < limit) {
      if (blankAt(text, end) && blankAt(text, end + 1)) break
      end++
    }
    // 去掉尾巴上的间隙，高亮框才是贴着文字而不是多出一截空白
    while (end > mark + 2 && blankAt(text, end - 1)) end--
    out.push({ token, start: tokenStart, end })
    ITEM_MARK.lastIndex = end
  }
  return out
}

/** 列表行：像文章/看板清单一行（项目符开头，或有日期列 + 列间结构）。 */
function isListRow(s: Screen, row: number): boolean {
  const text = rowText(s, row)
  if (text.length < 12 || nonBlankCount(text) < 4) return false
  const head = text.trimStart()
  if (/^[◎★☆●✦◆▍■□✱✲▶►»・·•]/.test(head)) return true
  // 「01-02  作者      标题」或「2024/01/02 ...」：日期列 + 至少两处列间隔
  if (!/(^|\s)\d{1,2}-\d{1,2}(\s|$)/.test(text) && !/(^|\s)\d{4}\/\d{1,2}\/\d{1,2}(\s|$)/.test(text)) return false
  const gaps = (text.match(/\s{3,}/g) || []).length
  return gaps >= 2
}

/**
 * 判页面类型。宁可 unknown 也不要乱认：unknown 时整条链路等于不存在。
 *
 * - menu：成片出现的编号项（横排一行里 ≥2 项，或整屏累计 ≥4 项）。
 * - list：≥3 行像清单。列表排在菜单之后判，因为菜单页里也常有带日期的说明行，
 *   而编号项一旦成串就是明确的「按字符选一项」现场。
 */
export function detectPageState(s: Screen): PageState {
  let rowsWithItems = 0
  let totalItems = 0
  let listRows = 0
  for (let r = 0; r < s.rows; r++) {
    const items = itemsInRow(s, r)
    if (items.length >= 2) {
      rowsWithItems++
      totalItems += items.length
    } else if (items.length === 1) {
      totalItems++
    }
    if (isListRow(s, r)) listRows++
  }
  if (rowsWithItems >= 2 || totalItems >= 4) return 'menu'
  if (listRows >= 3) return 'list'
  return 'unknown'
}

function hit(keys: string[], row: number, x0: number, x1: number, cursor: Hit['cursor'], label: string): Hit {
  return { keys, row, x0, x1, cursor, label }
}

/**
 * 命中判定：鼠标停在 (row, col) 时该送什么键。返回 null 表示这里不是菜单区，
 * 上层应当保持 xterm 自己的选择行为、并且不要改光标形状。
 */
export function hitTest(s: Screen, state: PageState, row: number, col: number): Hit | null {
  if (state === 'unknown') return null
  if (row < 0 || row >= s.rows || col < 0 || col >= s.cols) return null

  if (state === 'menu') {
    // 原版 Menu/MailMenu：最左 5 列是「回到上一层」
    if (col < 5) return hit([KEY.left], row, 0, Math.min(5, s.cols), 'pointer', '菜单：上一层')
    for (const it of itemsInRow(s, row)) {
      if (col >= it.start && col <= it.end) {
        return hit([it.token, KEY.enter], row, it.start, it.end, 'pointer', `菜单项 ${it.token}`)
      }
    }
    return null
  }

  // list：分区与原版 getCursorType() 的 ArticleList/BoardList 分支一致
  if (col < 12) return hit([KEY.left], row, 0, Math.min(12, s.cols), 'pointer', '清单：上一层')
  if (row < 3) return hit([KEY.home], row, 0, s.cols, 'pointer', '清单：首页')
  if (row === s.rows - 1) return hit([KEY.end], row, 0, s.cols, 'pointer', '清单：末页')
  if (col > s.cols - 16) {
    return row - 3 <= (s.rows - 4) / 2
      ? hit([KEY.pageUp], row, s.cols - 16, s.cols, 'ns-resize', '清单：上翻页')
      : hit([KEY.pageDown], row, s.cols - 16, s.cols, 'ns-resize', '清单：下翻页')
  }

  if (!isListRow(s, row)) return null
  if (s.caretRow < 0) return null
  const delta = row - s.caretRow
  if (Math.abs(delta) > s.rows) return null // 异常屏幕（光标跑很远）不做无脑连发
  const keys: string[] = []
  for (let i = 0; i < Math.abs(delta); i++) keys.push(delta > 0 ? KEY.down : KEY.up)
  keys.push(KEY.enter)
  return hit(keys, row, 0, s.cols, 'pointer', delta === 0 ? '清单：进入本行' : `清单：移动 ${delta > 0 ? '+' : ''}${delta} 行后进入`)
}

/** 该列是否落在某个 URL 上 —— 落在 URL 上的点击要留给链接本身。 */
export function inSpan(spans: Array<[number, number]>, col: number): boolean {
  return spans.some(([a, b]) => col >= a && col < b)
}
