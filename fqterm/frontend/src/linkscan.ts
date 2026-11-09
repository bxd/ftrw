/**
 * 终端文本里的超链接采集与类型判定 —— 预览窗口的输入端。
 *
 * 为什么自己把屏幕扫一遍，而不是只用 xterm 的链接插件：WebLinksAddon 只交出被点的
 * 那一条，而预览窗口要能在一排图里「上一个 / 下一个」地翻，那需要整屏的链接清单
 * （按出现顺序）。所以点击只决定「从哪条开始」，清单来自这里。
 *
 * 类型判定只认扩展名，认不出来一律当网页：宁可开成真网页，也不要拿 <img> 去赌一个
 * 其实会 302 到 HTML 的地址。
 */

export type PreviewKind = 'image' | 'audio' | 'video' | 'page'

export interface PreviewItem {
  url: string
  kind: PreviewKind
  name: string
}

const IMAGE_EXT = new Set(['jpg', 'jpeg', 'jfif', 'png', 'gif', 'webp', 'bmp', 'avif', 'svg', 'ico', 'tif', 'tiff'])
const AUDIO_EXT = new Set(['mp3', 'wav', 'ogg', 'oga', 'm4a', 'aac', 'flac', 'opus', 'wma', 'amr'])
const VIDEO_EXT = new Set(['mp4', 'webm', 'ogv', 'mov', 'm4v', 'mkv', 'avi', 'wmv', 'flv', 'ts', '3gp'])

// URL 字符集用 ASCII 白名单，绝不收非 ASCII：BBS 一行里紧跟在链接后面的往往是中文
// 句子和全角标点，收进来只会得到打不开的地址。
const URL_RE = /https?:\/\/[A-Za-z0-9\-._~:/?#[\]@!$&'()*+,;=%]+/g

// 结尾这些符号在正文里是句子成分（「见 https://a/x.png,」），不是地址的一部分。
// 右括号不在这里：它既可能是地址自带的（维基条目 Foo_(bar)），也可能是正文的
// 「(见 https://a/x.png)」，两种情况只能靠括号成不成对来判，见 trimURL。
const TRAILING_PUNCT = /[,.;:!?"'`\]}><]+$/

/** 去掉正文粘在结尾的标点；括号按成对与否决定去留（维基一类地址自己就带括号）。 */
function trimURL(raw: string): string {
  let u = raw
  let open = 0
  let close = 0
  for (const ch of u) {
    if (ch === '(') open++
    else if (ch === ')') close++
  }
  for (;;) {
    const next = u.replace(TRAILING_PUNCT, '')
    if (next === u) break
    u = next
  }
  // 只有多出来的那几只右括号才是正文标点，成对的留下
  while (close > open && u.endsWith(')')) {
    u = u.slice(0, -1)
    close--
  }
  // 剥掉括号后可能又露出别的标点（「…(bar)，」这类），再走一轮
  for (;;) {
    const next = u.replace(TRAILING_PUNCT, '')
    if (next === u) break
    u = next
  }
  return u
}

/** 一行文本里的 URL，带列区间。区间是给「点在哪」用的，清单只用地址本身。 */
function urlMatches(text: string): Array<{ url: string; start: number; end: number }> {
  const out: Array<{ url: string; start: number; end: number }> = []
  for (const m of text.matchAll(URL_RE)) {
    const u = trimURL(m[0])
    if (u.length > 'https://'.length) out.push({ url: u, start: m.index, end: m.index + u.length })
  }
  return out
}

/** 从一行（或任意一段）文本里取出 URL，保持出现顺序。 */
export function extractURLs(text: string): string[] {
  return urlMatches(text).map((m) => m.url)
}

/**
 * 一行文本里所有 URL 的 [起, 止) 列区间。
 *
 * 鼠标点菜单要靠它避让：URL 字符全在 ASCII 内（宽度 1），所以区间下标就是终端的
 * 显示列，能跟 menuclick.ts 的单元格坐标直接比。点在链接上应当开链接，
 * 不该被当成菜单项。
 */
export function extractURLSpans(text: string): Array<[number, number]> {
  return urlMatches(text).map((m) => [m.start, m.end])
}

function extOf(u: string): string {
  let p = ''
  try {
    p = new URL(u).pathname
  } catch {
    p = u.replace(/^https?:\/\/[^/]+/, '').split(/[?#]/)[0]
  }
  const seg = p.slice(p.lastIndexOf('/') + 1)
  const dot = seg.lastIndexOf('.')
  return dot < 0 ? '' : seg.slice(dot + 1).toLowerCase()
}

export function classifyURL(u: string): PreviewKind {
  const ext = extOf(u)
  if (IMAGE_EXT.has(ext)) return 'image'
  if (AUDIO_EXT.has(ext)) return 'audio'
  if (VIDEO_EXT.has(ext)) return 'video'
  return 'page'
}

/** 给人看的短名：路径末段（还原 %xx），拿不到就退回主机名。 */
export function linkName(u: string): string {
  try {
    const url = new URL(u)
    const segs = url.pathname.split('/').filter(Boolean)
    const last = segs.length ? segs[segs.length - 1] : ''
    if (!last) return url.hostname
    try {
      return decodeURIComponent(last)
    } catch {
      // 地址里的 % 不是转义（100%20 之类残缺写法），原样显示末段即可
      return last
    }
  } catch {
    return u
  }
}

/**
 * 扫描若干行终端文本，产出链接清单（同一地址只留一次）。
 *
 * 去重是必要的：BBS 的图文混排经常在同一篇帖子里重复贴同一个地址，
 * 否则「下一个」会在两张一模一样的图之间来回跳。
 */
export function scanLines(lines: string[]): PreviewItem[] {
  const seen = new Set<string>()
  const out: PreviewItem[] = []
  for (const line of lines) {
    if (!line) continue
    for (const url of extractURLs(line)) {
      if (seen.has(url)) continue
      seen.add(url)
      out.push({ url, kind: classifyURL(url), name: linkName(url) })
    }
  }
  return out
}

/* ---------------- IPv4 采集（IP 归属地查询的输入端） ---------------- */

// 地址样式：四段，数字段严格 0-255。打码写法 `[xX*]` 两种都认（* 与 x），而且
// 任何一段都可以是打码（水木末段打码 202.38.64.*，也有站端打中间段 114.242.*.*、
// 或者用 x 写成 114.242.x.x）；但头一段必须是数字，不然 *.*.*.* 这种问不出东西的
// 也会进来。只认一种的结果就是鼠标停在地址上什么也不弹，而用户无从知道为什么不弹。
// 左右各加一道「邻居不能是数字或点」的护栏：不这样就会从 1.2.3.4.5 里抠出
// 一个看起来很合法的 1.2.3.4，报出一个根本不存在的归属地。
//
// 字节段写成字面量再拿 .source 组合，不直接拼字符串：`'\d'` 在 JS 字符串里会把
// 反斜杠丢掉、只剩一个 d（这个坑很阴：拼错了照样能编译、能跑，就是永远匹配不上）。
const OCT_RE = /25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d/
const MASK = '[xX*]'
const IP_RE = new RegExp(
  `(?<![\\d.])(${OCT_RE.source})\\.(?:${OCT_RE.source}|${MASK})\\.(?:${OCT_RE.source}|${MASK})\\.(?:${OCT_RE.source}|${MASK})(?![\\d.])`,
  'g',
)

/**
 * 一行文本里所有 IPv4 的 [起, 止) 列区间（跟 extractURLSpans 同形）。
 *
 * 地址只含 ASCII，宽度必为 1，所以下标就是终端的显示列，能跟 menuclick.ts 的
 * 单元格坐标直接比。
 */
export function extractIPSpans(text: string): Array<[number, number]> {
  const out: Array<[number, number]> = []
  for (const m of text.matchAll(IP_RE)) out.push([m.index, m.index + m[0].length])
  return out
}

/** 某列上正好压着的 IPv4 及其起始列（浮层要按地址开头对齐，不是按鼠标所在列）。 */
export function ipSpanAt(text: string, col: number): { text: string; start: number } | null {
  for (const m of text.matchAll(IP_RE)) {
    if (col >= m.index && col < m.index + m[0].length) return { text: m[0], start: m.index }
  }
  return null
}

/** 某列上正好压着的 IPv4 文本；不在地址上就返空串。 */
export function ipTextAt(text: string, col: number): string {
  return ipSpanAt(text, col)?.text ?? ''
}

// 主机名（域名）样式。写成字面量再拿 .source 组合，同 IP_RE（字符串里的
// '\d'、'\w' 会丢反斜杠，vue-tsc 一声不响，结果就是正则永远匹配不上）。
// 要点：至少两段，最后一段必须命中常见顶级域名单。不这么收敛会在 BBS 正文里
// 把 att.php / index.html / xterm.js / main.go 这些文件名当成域名，鼠标扫过去弹一排
// 查不到的红字。名单拿不准时宁可漏，不要误判。
const HOST_LABEL_RE = /[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?/
const HOST_TLD_RE = /net|com|org|cn|edu|gov|io|cc|me|info|biz|tv|name|us|uk|jp|hk|tw|sg|co|xyz|top|vip|club|site|online|tech|app|int|mil|museum|ac/
const HOST_RE = new RegExp(
  `(?<![\\w.\\-])(${HOST_LABEL_RE.source}(?:\\.${HOST_LABEL_RE.source})*\\.(?:${HOST_TLD_RE.source}))(?![\\w.\\-])`,
  'g',
)

/**
 * 某列上正好压着的主机名。域名要发一次 DNS 才知道归属地，所以只在两种场合用：
 * 悬停（有 5 分钟前端的缓存与后端 DNS 缓存兜着）与主动点击。
 */
export function hostSpanAt(text: string, col: number): { text: string; start: number } | null {
  for (const m of text.matchAll(HOST_RE)) {
    if (col >= m.index && col < m.index + m[0].length) return { text: m[0], start: m.index }
  }
  return null
}

/** 只保留媒体项（图片/音频/视频）：网页不参与「上一个 / 下一个」。 */
export function mediaOnly(items: PreviewItem[]): PreviewItem[] {
  return items.filter((it) => it.kind !== 'page')
}

/** 后端探回来的字符串得落在四个类型里才算数，否则当没探到。 */
export function asKind(raw: string | null | undefined): PreviewKind | null {
  return raw === 'image' || raw === 'audio' || raw === 'video' || raw === 'page' ? raw : null
}

const SCRIPT_EXT = new Set([
  'php', 'phtml', 'asp', 'aspx', 'jsp', 'jspx', 'cgi', 'pl', 'py', 'ashx', 'cfm',
  'html', 'htm', 'shtml', 'xhtml',
])

/**
 * 名字是否等于没名字：空、纯数字（附件号）、或者脚本/网页后缀（att.php、index.html）。
 *
 * BBS 一整帖图的地址末段都是同一个 att.php，拿它当窗口标题、当「另存为」的默认名，
 * 用户看到的永远是那个没有信息量的脚本名 —— 命中这里就得去问服务器要真名。
 * 后端 preview.go 的 looksGenericName 是同一套判断，改这里要一起改。
 */
export function looksGenericName(name: string | null | undefined): boolean {
  const n = (name || '').trim().replace(/^\/+/, '')
  const cut = n.search(/[?#]/)
  const bare = cut < 0 ? n : n.slice(0, cut)
  if (!bare) return true
  if (/^\d+$/.test(bare)) return true
  const dot = bare.lastIndexOf('.')
  if (dot < 0 || dot === bare.length - 1) return true
  return SCRIPT_EXT.has(bare.slice(dot + 1).toLowerCase())
}

/**
 * 需要问一次服务器的链接：类型判不准的（只按扩展名成了「网页」），以及名字等于没名字的。
 *
 * BBS 的图常常是 att.php?... 或纯附件号：判成网页则预览窗口整个交给对方文档，
 * 翻页/缩放/另存为这些控件全画不出来；不拿回真名字则标题与保存默认名都是 att.php。
 * 上限是为了不把一次点击拖成一堆探测请求。
 */
export function probeTargets(items: PreviewItem[], limit = 24): string[] {
  return items
    .filter((it) => it.kind === 'page' || looksGenericName(it.name))
    .slice(0, limit)
    .map((it) => it.url)
}

/** 一次探测的回执：类型与真文件名，空字段表示没探到。 */
export interface ProbeResult {
  kind?: string | null
  name?: string | null
}

/**
 * 把探测结果合回清单。两条规则都是「探不到就不动原判」：
 * 类型只改判成网页的项，名字只换掉那些「等于没名字」的项。
 */
export function applyProbe(
  items: PreviewItem[],
  urls: string[],
  results: Array<ProbeResult | null | undefined>,
): PreviewItem[] {
  const byUrl = new Map<string, ProbeResult>()
  urls.forEach((u, i) => {
    const r = results[i]
    if (r && (r.kind || r.name)) byUrl.set(u, r)
  })
  if (!byUrl.size) return items
  return items.map((it) => {
    const r = byUrl.get(it.url)
    if (!r) return it
    let next = it
    const kind = asKind(r.kind)
    if (next.kind === 'page' && kind) next = { ...next, kind }
    const name = (r.name || '').trim()
    if (name && looksGenericName(next.name)) next = { ...next, name }
    return next
  })
}
