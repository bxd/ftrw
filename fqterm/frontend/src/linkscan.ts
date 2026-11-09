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

/** 从一行（或任意一段）文本里取出 URL，保持出现顺序。 */
export function extractURLs(text: string): string[] {
  const out: string[] = []
  for (const m of text.matchAll(URL_RE)) {
    const u = trimURL(m[0])
    if (u.length > 'https://'.length) out.push(u)
  }
  return out
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
