// 拼音首字母工具：给文件管理器的「敲字母过滤」用。
//
// 目标是不引入拼音字典也能把中文名按拼音首字母匹配（敲 'zw' 命中「中文」）。
// 做法靠 Intl.Collator('zh') —— Windows / WebView2 的 ICU 对 zh 默认就是拼音排序 ——
// 把每个汉字与 23 个「首字母分段代表字」比较，落在哪一段就取那个首字母。
// 比打包整本字典轻得多；万一某平台 zh 排序不是拼音，退化后只剩字面匹配，不影响正确性。

// 每个拼音首字母分段的最小代表字（按字母升序，I/U/V 不作声母故略）。
const BOUNDS: Array<[string, string]> = [
  ['A', '啊'], ['B', '芭'], ['C', '擦'], ['D', '搭'], ['E', '蛾'], ['F', '发'],
  ['G', '噶'], ['H', '哈'], ['J', '击'], ['K', '喀'], ['L', '垃'], ['M', '妈'],
  ['N', '拿'], ['O', '欧'], ['P', '啪'], ['Q', '期'], ['R', '然'], ['S', '撒'],
  ['T', '塌'], ['W', '挖'], ['X', '昔'], ['Y', '压'], ['Z', '匝'],
]

const collator = new Intl.Collator('zh-Hans-CN')

// 单个汉字 → 首字母（大写）；找不到分段落点返回空串。
function hanInitial(ch: string): string {
  let res = ''
  for (const [letter, bound] of BOUNDS) {
    if (collator.compare(ch, bound) >= 0) res = letter
    else break
  }
  return res
}

const cache = new Map<string, string>()

// 整个名字 → 逐字首字母串（ASCII 字母转大写、数字/符号原样、汉字取拼音首字母）。
export function initials(name: string): string {
  const hit = cache.get(name)
  if (hit !== undefined) return hit
  let out = ''
  for (const ch of Array.from(name)) {
    const code = ch.codePointAt(0)!
    if (code >= 0x41 && code <= 0x5a) out += ch // A-Z
    else if (code >= 0x61 && code <= 0x7a) out += ch.toUpperCase() // a-z
    else if (code >= 0x4e00 && code <= 0x9fff) out += hanInitial(ch) // 基本汉字
    else out += ch
  }
  cache.set(name, out)
  return out
}

// 名字是否匹配过滤串：字面（大小写不敏感子串）命中，或拼音首字母串命中。
export function nameMatches(name: string, q: string): boolean {
  if (!q) return true
  const lq = q.toLowerCase()
  if (name.toLowerCase().includes(lq)) return true
  return initials(name).toLowerCase().includes(lq)
}
