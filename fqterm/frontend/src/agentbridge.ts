/**
 * Agent API 的渲染端半边：接后端 "agent:cmd" 往返请求，在 xterm.js 的真缓冲区上
 * 完成读屏 / 发键 / 等屏 / 跑命令 / 管标签，结果经 TerminalService.AgentReply 送回
 * HTTP 请求方（见后端 agentapi.go）。
 *
 * 为什么读屏在这儿做而不是后端自己解析字节流：屏的语义（软折行、光标、回滚区）只有
 * 渲染端知道，后端再挂一套终端解析器迟早和"用户看到的"漂移。agent 要的就是人所见。
 *
 * 读屏口径的两条硬规矩（都是踩过坑的）：
 *   - viewportY 已经是绝对行号，绝不能再加 baseY（加了会读到空白屏）；
 *   - 光标的绝对行是 baseY + cursorY（与 terminal.ts eraseRegion 同式）。
 * Shell 标签（protocolType==3）可带 from/count 读回滚区；BBS 标签只返回当前可见屏。
 */
import { Events } from '@wailsio/runtime'
import * as api from './api'
import type { TermTab } from './terminal'
import { keyseq, parseSeq, resolveCommand } from './keyseq'
import { translateKey, type KeyContext } from './keymap'

export interface AgentDeps {
  /** 标签清单（后端 /tabs 的数据源）；顺序就是界面标签条的顺序。 */
  listTabs: () => any[]
  /** 按会话 id 找标签；id 传空串表示"当前活动标签"（API 的 tab 参数可省略）。 */
  tabById: (id: string) => TermTab | null
  activate: (id: string) => any
  close: (id: string) => any
  /** 按站点名开新标签（存站表里找）；等连上才回，失败报错原文给 agent。 */
  connect: (siteName: string) => Promise<any>
}

const sleep = (ms: number) => new Promise<void>((r) => setTimeout(r, ms))

let deps: AgentDeps | null = null
let wired = false

export function configureAgentBridge(d: AgentDeps) {
  deps = d
  if (wired) return
  wired = true
  Events.On('agent:cmd', (e: any) => { void dispatch(e.data) })
}

async function dispatch(cmd: { id: string; kind: string; payload: string }) {
  if (!cmd || !cmd.id) return
  let payload = ''
  let errMsg = ''
  try {
    if (!deps) throw new Error('渲染端尚未接好标签')
    const p = JSON.parse(cmd.payload || '{}')
    payload = JSON.stringify((await run(cmd.kind, p)) ?? {})
  } catch (err: any) {
    errMsg = String(err?.message || err)
  }
  try { await api.AgentReply(cmd.id, payload, errMsg) } catch { /* 后端超时已收摊，无声即可 */ }
}

async function run(kind: string, p: any): Promise<any> {
  switch (kind) {
    case 'tabs':
      return { tabs: deps!.listTabs() }
    case 'screen':
      return readScreen(needTab(p.tab), p.from, p.count)
    case 'send':
      return await doSend(needTab(p.tab), p)
    case 'wait':
      return await doWait(needTab(p.tab), p)
    case 'command':
      return doCommand(String(p.spec || ''))
    case 'activate':
      return deps!.activate(String(p.tab || ''))
    case 'close':
      return deps!.close(String(p.tab || ''))
    case 'connect':
      return await deps!.connect(String(p.site || ''))
    default:
      throw new Error(`未知命令种类：${kind}`)
  }
}

function needTab(id?: string): TermTab {
  const t = deps!.tabById(typeof id === 'string' ? id : '')
  if (!t) throw new Error(`找不到标签：${id || '（当前活动标签不存在）'}`)
  return t
}

/* ---------------- 读屏 ---------------- */

// 只用到 getLine 一条能力的最小缓冲型（xterm 的 Buffer 自满足，也方便单测塞假屏）。
type BufLike = {
  getLine(y: number): { translateToString(trimRight?: boolean): string; isWrapped: boolean } | undefined
}

function screenLines(b: BufLike, from: number, to: number) {
  const out: { y: number; text: string; wrapped: boolean }[] = []
  for (let y = from; y < to; y++) {
    const l = b.getLine(y)
    if (l) out.push({ y, text: l.translateToString(true), wrapped: l.isWrapped })
  }
  return out
}

function readScreen(t: TermTab, from?: number, count?: number) {
  const b = t.term.buffer.active
  const rows = t.term.rows
  const shell = (t.site.protocolType ?? 0) === 3
  const total = b.baseY + rows // 缓冲总行数：回滚区占 [0, viewportY)，可见区占末尾 rows 行
  const vpY = b.viewportY // 顶行的绝对行号 —— 它本来就是绝对的，别再加 baseY
  let start = vpY
  let end = Math.min(total, vpY + rows)
  if (shell) {
    // shell 才认 from/count（读回滚区）；越界钳进缓冲而不是报错，agent 少一次往返。
    if (typeof from === 'number' && Number.isFinite(from)) {
      start = Math.max(0, Math.min(total, Math.floor(from)))
      end = Math.min(total, start + Math.max(1, Math.floor(count ?? rows)))
    }
  }
  return {
    mode: shell ? 'shell' : 'bbs',
    cols: t.term.cols,
    rows,
    totalLines: total,
    viewport: { start, end },
    cursor: { x: b.cursorX, y: b.baseY + b.cursorY }, // 绝对行：与 eraseRegion 同式
    status: t.status,
    lines: screenLines(b, start, end),
  }
}

/* ---------------- 发送 ---------------- */

async function doSend(t: TermTab, p: { text?: string; keys?: string; keyDelayMs?: number }) {
  if (t.status !== 'connected') throw new Error(`标签处于 ${t.status} 状态，发不进去（空格关标签 / 回车重连，或重新连接）`)
  let sent = 0
  if (p.text) {
    // input(…, false)：走 onData 同一条出口（api.Write 做站点编码），但不触发清选区等副作用。
    t.term.input(String(p.text), false)
    sent += Array.from(String(p.text)).length
  }
  if (p.keys) {
    const { tokens, error } = parseSeq(String(p.keys))
    if (error) throw new Error(`键记法不通：${error}`)
    const delay = Math.max(0, Math.min(5000, p.keyDelayMs ?? 150))
    const ctx: KeyContext = { keyType: t.site.keyType ?? 0, backspace: t.site.backspace ?? 1 }
    for (let i = 0; i < tokens.length; i++) {
      const tok = tokens[i]
      // 热键优先：命中绑定（或作为序列前缀）时框架吞键并执行命令 —— agent 的按键与
      // 真人同路，"M-x k" 真的会关标签而不是把三股八 ESC 发给 BBS。
      if (keyseq.feedToken(tok) === 'swallow') {
        sent++
      } else {
        const bytes = tokenToBytes(tok, ctx, t.term.modes.applicationCursorKeysMode)
        if (bytes === null) throw new Error(`这键发不了远端：${tok}`)
        t.term.input(bytes, false)
        sent++
      }
      // 逐键间隔：BBS 的菜单在远端，两键间不等刷新就会把后一键打进上一键还没画完的屏里。
      if (delay > 0 && i + 1 < tokens.length) await sleep(delay)
    }
  }
  return { sent }
}

/* ---------------- 等屏 ---------------- */

async function doWait(t: TermTab, p: { pattern: string; regex?: boolean; timeoutMs?: number; full?: boolean }) {
  const pattern = String(p.pattern ?? '')
  if (!pattern) throw new Error('pattern 不能为空')
  let re: RegExp | null = null
  if (p.regex) {
    try { re = new RegExp(pattern) } catch (e: any) { throw new Error(`正则不通：${e?.message || e}`) }
  }
  const timeout = Math.max(0, Math.min(60000, p.timeoutMs ?? 5000))
  const t0 = Date.now()
  for (;;) {
    const b = t.term.buffer.active
    const shell = (t.site.protocolType ?? 0) === 3
    const from = p.full && shell ? 0 : b.viewportY
    const lines = screenLines(b, from, b.baseY + t.term.rows)
    for (const l of lines) {
      if (re ? re.test(l.text) : l.text.includes(pattern)) {
        return { matched: true, y: l.y, text: l.text, waitedMs: Date.now() - t0 }
      }
    }
    const left = t0 + timeout - Date.now()
    if (left <= 0) return { matched: false, waitedMs: Date.now() - t0 }
    await sleep(Math.min(120, left))
  }
}

/* ---------------- 命令 ---------------- */

function doCommand(spec: string) {
  if (!spec) throw new Error('spec 不能为空（命令 id，可带 :参数）')
  const hit = resolveCommand(spec)
  if (!hit) throw new Error(`未注册的命令：${spec}`)
  hit.def.handler(hit.arg)
  return { ran: spec }
}

/* ---------------- token → 远端字节 ---------------- */

// 规范化 token（normalizeToken 的产物）里 <> 特殊键到 KeyboardEvent.key 名的映射。
const KEYNAME_BY_BASE: Record<string, string> = {
  RET: 'Enter', TAB: 'Tab', ESC: 'Escape', BS: 'Backspace', DEL: 'Delete',
  INS: 'Insert', SPC: ' ', HOME: 'Home', END: 'End', PGUP: 'PageUp', PGDN: 'PageDown',
  UP: 'ArrowUp', DOWN: 'ArrowDown', LEFT: 'ArrowLeft', RIGHT: 'ArrowRight',
}

// translateKey（站点按键方案表）不管的键，按 xterm.js 原生编码发 —— 与真人手按时
// 到达远端的字节一致；F1-F4 在 XTERM 方案下也走这张表（原生 \x1bOP 系）。
const NATIVE: Record<string, string> = {
  Enter: '\r', Tab: '\t', Escape: '\x1b', Backspace: '\x7f',
  Delete: '\x1b[3~', Insert: '\x1b[2~', Home: '\x1b[H', End: '\x1b[F',
  PageUp: '\x1b[5~', PageDown: '\x1b[6~',
  F1: '\x1bOP', F2: '\x1bOQ', F3: '\x1bOR', F4: '\x1bOS',
  F5: '\x1b[15~', F6: '\x1b[17~', F7: '\x1b[18~', F8: '\x1b[19~',
  F9: '\x1b[20~', F10: '\x1b[21~', F11: '\x1b[23~', F12: '\x1b[24~',
}

const ARROW_APP: Record<string, string> = {
  ArrowUp: '\x1bOA', ArrowDown: '\x1bOB', ArrowRight: '\x1bOC', ArrowLeft: '\x1bOD',
}
const ARROW_CSI: Record<string, string> = {
  ArrowUp: '\x1b[A', ArrowDown: '\x1b[B', ArrowRight: '\x1b[C', ArrowLeft: '\x1b[D',
}

/**
 * 把一个规范化 token 翻成该发给远端的字节串；null = 发不了（调用方报错，绝不瞎编）。
 *
 * 优先级与真人按键一致：先问 translateKey（站点 keyType/backspace 的接管件），
 * 再查原生表。M- 前缀按终端惯例发 ESC + 主体；C- 只支持字母与 @_[\]^（控制字符
 * 公式 c & 0x1f），C-<特殊键> 各家编码不一律拒绝，让调用方知道"没人替你别扭"。
 */
export function tokenToBytes(tok: string, ctx: KeyContext, appCursor: boolean): string | null {
  let rest = tok
  let ctrl = false
  let alt = false
  let shift = false
  for (;;) {
    if (rest.startsWith('C-') && rest.length > 2) { ctrl = true; rest = rest.slice(2); continue }
    if (rest.startsWith('M-') && rest.length > 2) { alt = true; rest = rest.slice(2); continue }
    if (rest.startsWith('S-') && rest.length > 2) { shift = true; rest = rest.slice(2); continue }
    break
  }

  let key = ''
  const m = /^<([A-Z0-9]+)>$/.exec(rest)
  if (m) {
    if (m[1] === 'LF') return alt ? '\x1b\n' : '\n' // LF 只在 <> 写法里存在，单独给 \n
    const name = KEYNAME_BY_BASE[m[1]]
    key = name ?? m[1] // F1…F24：基名本身就是 KeyboardEvent.key
  } else if (rest.length === 1) {
    key = rest
  } else {
    return null
  }

  const plain = plainBytes(key, { ctrl, shift }, ctx, appCursor)
  if (plain === null) return null
  return alt ? '\x1b' + plain : plain
}

function plainBytes(
  key: string,
  mods: { ctrl: boolean; shift: boolean },
  ctx: KeyContext,
  appCursor: boolean,
): string | null {
  if (mods.ctrl) {
    // 控制字符公式只覆盖字母与 @ _ [ \ ] ^（xterm 口径 c & 0x1f）；其余拒发。
    if (/^[a-zA-Z@_[\]^]$/.test(key)) {
      return String.fromCharCode(key.toUpperCase().charCodeAt(0) & 0x1f)
    }
    return null
  }
  const taken = translateKey(
    { key, ctrlKey: false, altKey: false, shiftKey: mods.shift, metaKey: false },
    ctx,
  )
  if (taken !== null) return taken
  if (key.startsWith('Arrow')) return (appCursor ? ARROW_APP : ARROW_CSI)[key] ?? null
  if (NATIVE[key] !== undefined) return NATIVE[key]
  if (key.length === 1) return key
  return null
}
