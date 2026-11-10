/**
 * 发帖动作层 —— 把 posttransform 的纯函数、当前会话和用户屏幕上的文字串起来。
 *
 * 源头是「编辑区里的用户文字」，不是剪贴板，分两种情形（见 terminal.ts 的 readSelection）：
 * - 鼠标框了选区 —— 就处理框住的那段；
 * - 没框选 —— 以光标为锚，从光标往回括住连续正文当作要处理的那段。
 *   字符模式编辑器里删除本来就只能从光标往回退格，光标就是天然安全锚；这样也正好接住
 *   「打完字直接按热键」这个最自然的用法，不用再强求先框选。
 *
 * 不拿剪贴板当源的两个理由：
 * - 水木这类站的文章编辑器跑在服务器上，客户端读不回用户敲了哪些字，剪贴板里躺着
 *   什么更是没法预料 —— 拿它当源，用户看到的是「我打的字没动，别处的字被改了」；
 * - 屏幕缓冲区同时给了内容和位置：文字按格子读回（宽字符算一个字、行尾空白和站端
 *   折行的假换行都不算字符），位置用来判断能不能安全地原地替换。
 *
 * 替换前把原文存进本模块的一块内存留一份（绝不碰系统剪贴板），删坏了能按 M-x i 灌回。引用原文不会被搅乱：引文是
 * 服务器画下来的，本来就不在用户选的范围里；就算连引文一起框住，applySeparators 也会
 * 按行首前缀再跳过一道。竖排不认前缀，会整段排。
 */
import { ref } from 'vue'
import { Clipboard } from '@wailsio/runtime'
// 只借类型：编译期就擦掉，本模块不依赖 xterm，界面层换实现、单独跑用例都不用拉起终端
import type { SelRead, SelState } from './terminal'
import {
  SEP_DEFAULT,
  SEP_RANDOM_POOL,
  applySeparators,
  isSafeSep,
  quoteLineCount,
  toVertical,
  type SepMode,
} from './posttransform'

/** 灌入目标。只声明用到的这几样（结构类型），因此本模块不 import terminal.ts 的实现。 */
export interface PostTarget {
  readonly termCols: number
  /** 读回鼠标框选的文字，并判断能不能安全地原地替换 */
  readSelection(): SelRead
  /** 删掉选区认定的那段，再把变换结果送回原位 */
  replaceSelection(sel: SelRead, text: string): Promise<{ erased: number; lines: number; chars: number }>
  /** 追加式灌入（只有「原样送入剪贴板」用它，不删任何东西） */
  insertText(text: string): Promise<{ lines: number; chars: number }>
  /** 动作做完把焦点还给终端：点按钮会把焦点从 xterm 拿走，不还不方便接着打字 */
  focus(): void
}

export interface PostDeps {
  /** 当前活动会话；没有会话返回 null */
  target: () => PostTarget | null
  /** 状态区回显（对应 Emacs 的 echo area） */
  echo: (msg: string, kind?: 'info' | 'warn' | 'error' | 'pending') => void
  /** 上次选用的分隔符，存在偏好里；空串表示没选过 */
  getSep: () => string
  setSep: (ch: string) => void
}

let deps: PostDeps = {
  target: () => null,
  echo: () => { /* 没接界面时保持安静，不让动作层在测试里抛 */ },
  getSep: () => SEP_DEFAULT,
  setSep: () => { /* noop */ },
}

/**
 * 原地替换前的原文备份，只存这块进程内内存 —— 绝不写系统剪贴板，免得覆盖用户原本放在
 * 剪贴板里的东西。删坏了按 M-x i 从这里原样灌回，灌回一次即清空。
 */
let savedBackup = ''

export function configurePost(d: PostDeps) {
  deps = d
}

/**
 * 工具栏与符号面板的可见性。放模块级而不是 App.vue 里：按钮、热键、面板三处都要
 * 开合同一个下拉，收在一处才不会各存一份状态、出现「热键开了按钮还看不见」。
 */
export const sepPickerOpen = ref(false)
export const toolbarPinned = ref(false)

/** 记住的分隔符；偏好里的值已经不安全（旧版本存进去的宽字符之类）就退回出厂默认。 */
export function currentSep(): string {
  const s = deps.getSep()
  return isSafeSep(s) ? s : SEP_DEFAULT
}

/**
 * 读不出可用选区时怎么说。用户看不见内部状态，只能靠这一句话明白下一步该做什么，
 * 所以每条都落在「该怎么改这个动作」上，而不是「程序判定为 xxx」。
 */
const SEL_HINT: Record<SelState, string> = {
  empty: '还没有可处理的文字：先在编辑区把字打上，或用鼠标框住要处理的那段',
  offscreen: '要处理的文字不在当前这一屏上（滚回历史里的字删不掉），请翻回编辑区再按',
  before: '光标在选中文字之前了：删除只能从光标往回退格，请把光标移到那段文字末尾再按',
  far: '光标跟选中文字的末尾不在同一行，跨行找位置容易删错：请把光标移到同一行的末尾',
  blank: '光标所在处是空的，没有要处理的文字：先把字打上，或用鼠标框住要处理的那段',
  toolarge: '',
  ok: '',
}

/** 变换计划：文本 + 一句要拼到结果提示后面的话。 */
interface Plan { text: string; note?: string }

/**
 * 读选区 → 变换 → 原地替换。分隔符与竖排共用这条通路，差别只在 plan。
 *
 * 任何一步不满足就一个字节都不发出去，并解释清楚为什么 —— 静默改坏草稿是最糟的结果。
 */
async function onSelection(label: string, plan: (sel: SelRead, t: PostTarget) => Plan) {
  const t = deps.target()
  if (!t) {
    deps.echo('没有活动会话，编辑区还进不去', 'warn')
    return
  }
  const sel = t.readSelection()
  if (sel.state !== 'ok') {
    deps.echo(sel.state === 'toolarge'
      ? `一次替换 ${sel.chars} 字太多（上限 2000），请分段框选`
      : SEL_HINT[sel.state], 'warn')
    return
  }
  const p = plan(sel, t)
  if (!p.text) {
    deps.echo(`${label}：变换后是空的，选区没动`, 'warn')
    return
  }
  // 动手删字之前先留后路：原文存进本模块的内存（绝不写系统剪贴板，免得覆盖用户原本
  // 放在剪贴板里的东西）。站端真吞字把替换做坏了，按 M-x i 能从这块内存原样灌回来。
  savedBackup = sel.text
  deps.echo(`${label}：正在替换 ${sel.rows} 行 / ${sel.chars} 字…`, 'pending')
  try {
    const r = await t.replaceSelection(sel, p.text)
    deps.echo(`${label}：删 ${r.erased} 字、送 ${r.chars} 字 / ${r.lines} 行`
      + (p.note ? `；${p.note}` : ''), 'info')
  } catch (e: any) {
    deps.echo(`${label} 失败：${e?.message || e}；原文已存内存备份，按 M-x i 可原样灌回`, 'error')
  } finally {
    t.focus()
  }
}

/** 把几句可能的补充说明串成一句，空的丢掉。 */
function joinNotes(...parts: Array<string | undefined>): string | undefined {
  const hit = parts.filter((s) => s && s.trim())
  return hit.length ? hit.join('；') : undefined
}

/**
 * 字间插入分隔符。
 *
 * ch 只对 fixed 有意义（面板里点定的那个符号）；选中即记进偏好，下次 M-x s 直接复用。
 * 随机模式固定用预置池，不含全角空格 —— 插进去看不出效果，像没生效。
 */
export async function runSep(mode: SepMode, ch?: string) {
  let pool: string[]
  if (mode === 'fixed') {
    const picked = ch && isSafeSep(ch) ? ch : currentSep()
    if (ch && isSafeSep(ch)) deps.setSep(picked)
    pool = [picked]
  } else {
    pool = [...SEP_RANDOM_POOL]
  }
  const label = mode === 'fixed'
    ? `字间插入「${pool[0]}」`
    : mode === 'randomOne' ? '随机一种分隔符' : '逐字随机分隔符'
  await onSelection(label, (sel) => {
    const q = quoteLineCount(sel.text)
    return {
      text: applySeparators(sel.text, { seps: pool, mode }),
      note: joinNotes(
        sel.truncated ? '选区比光标靠后，只处理到光标为止那一段' : undefined,
        q ? `${q} 行按引用前缀跳过` : undefined,
      ),
    }
  })
}

/** 竖排：栅格按当前终端列数算，站端折行会把排版搅歪。 */
export async function runVertical() {
  await onSelection('竖排', (sel, t) => {
    const r = toVertical(sel.text, { cols: t.termCols })
    return {
      text: r.text,
      note: joinNotes(
        r.note,
        sel.truncated ? '选区比光标靠后，只处理到光标为止那一段' : undefined,
        quoteLineCount(sel.text) ? `含 ${quoteLineCount(sel.text)} 行疑似引用，竖排不认前缀，已一并处理` : undefined,
      ),
    }
  })
}

/**
 * 原样灌入：优先灌回上一次原地替换存进内存的原文备份（不碰系统剪贴板），内存里没有
 * 才读剪贴板 —— 那是「剪贴板里有现成正文、一次粘进去怕被站端吞字」的独立用法，而且
 * 只读不写，不会破坏用户原本放在剪贴板中的数据。都是追加、不删任何东西。
 */
export async function runRaw() {
  const t = deps.target()
  if (!t) {
    deps.echo('没有活动会话，送不进编辑区', 'warn')
    return
  }
  // 有内存备份先灌回备份；没有才读剪贴板（只读，不改用户剪贴板）
  let raw = savedBackup
  const fromBackup = raw.length > 0
  if (!fromBackup) {
    try {
      raw = await Clipboard.Text()
    } catch (e: any) {
      deps.echo(`读剪贴板失败：${e?.message || e}`, 'error')
      return
    }
  }
  if (!raw.trim()) {
    deps.echo(fromBackup ? '内存备份是空的（还没做过会失败的原地替换）' : '剪贴板里没有文本：原样送入只读剪贴板，不读屏幕', 'warn')
    return
  }
  const text = raw.replace(/\r\n?/g, '\n')
  const src = fromBackup ? '内存备份' : '剪贴板'
  deps.echo(`原样送入${src}：正在送入 ${text.split('\n').length} 行…`, 'pending')
  try {
    const r = await t.insertText(text)
    deps.echo(`原样送入${src}：已送入 ${r.lines} 行 / ${r.chars} 字（追加在光标处，没删任何东西）`, 'info')
    if (fromBackup) savedBackup = ''  // 灌回一次就清掉，免得下次误灌旧备份
  } catch (e: any) {
    deps.echo(`原样送入失败：${e?.message || e}`, 'error')
  } finally {
    t.focus()
  }
}

/** 打开符号面板（热键用；面板已经开着就不动它）。 */
export function openSepPicker() {
  if (sepPickerOpen.value) return
  if (!deps.target()) {
    deps.echo('没有活动会话，符号面板先不弹', 'warn')
    return
  }
  sepPickerOpen.value = true
  deps.echo('选择要插入的分隔符（Esc 关闭）')
}

/** 钉住 / 放开工具栏：钉住后鼠标离开也不收起，方便一边打字一边反复点。 */
export function toggleToolbarPin() {
  toolbarPinned.value = !toolbarPinned.value
  deps.echo(toolbarPinned.value
    ? '编辑工具栏已钉住（再按一次 M-x t 恢复鼠标悬停显隐）'
    : '编辑工具栏恢复悬停显隐')
}
