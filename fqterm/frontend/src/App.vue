<script setup lang="ts">
import { ref, reactive, computed, onMounted, onBeforeUnmount, nextTick } from 'vue'
import * as api from './api'
import { TermTab } from './terminal'
import { FmTab } from './fmtab'
import { Dialogs, Window } from '@wailsio/runtime'
import SiteDialog from './components/SiteDialog.vue'
import PrefsDialog from './components/PrefsDialog.vue'
import SchemeDialog from './components/SchemeDialog.vue'
import ZmodemDialog from './components/ZmodemDialog.vue'
import KeymapDialog from './components/KeymapDialog.vue'
import EditToolbar from './components/EditToolbar.vue'
import { keyseq, registerCommand, type EchoInfo } from './keyseq'
import { registerBuiltinCommands } from './keymapcmds'
import {
  configurePost, openSepPicker, runRaw, runSep, runVertical, sepPickerOpen,
  toolbarPinned, toggleToolbarPin,
} from './postactions'
import { applyProbe, asKind, classifyURL, hostSpanAt, ipSpanAt, linkName, looksGenericName, mediaOnly, probeTargets, type PreviewItem } from './linkscan'

interface TabMeta { id: string; title: string; status: string; kind?: 'term' | 'files' }

const viewport = ref<HTMLDivElement | null>(null)
const tabs = ref<TabMeta[]>([])        // reactive UI metadata only
const insts: TermTab[] = []            // raw xterm-backed instances (NOT reactive)
const byId = new Map<string, TermTab>()
const active = ref(-1)
const sites = ref<api.Site[]>([])
const schemes = ref<api.ColorScheme[]>([])
const prefs = ref<api.Preferences | null>(null)
const zmodemMsg = ref('')

/* ---------------- 浮动站点栏 ---------------- */
// 侧栏脱离文档布局浮在终端之上：展开时不再挤压视口（宽度恒定，不触发 re-fit），
// 终端尺寸与滚动内容都不会跳动。
// 可见性只由两个事实决定：鼠标停在左缘/面板上，或者焦点在面板内 —— 所以“失焦
// 自动隐藏”与热键切换共用一套机制，不会出现“卡住收不起来”的互斥开关。
const SIDEBAR_W = 260
const EDGE_PX = 8            // 收起状态下“贴边”的判定宽度（只算坐标，不拿透明层挡住终端）
const sidebarEl = ref<HTMLElement | null>(null)
const sidebarHover = ref(false)
const sidebarFocused = ref(false)
let sideHideTimer: number | undefined

const sidebarShown = computed(() => !dialogOpen.value && (sidebarHover.value || sidebarFocused.value))
const sidebarTransform = computed(() => (sidebarShown.value ? 'none' : `translateX(-${SIDEBAR_W}px)`))

function clearSideHide() {
  if (sideHideTimer) { clearTimeout(sideHideTimer); sideHideTimer = undefined }
}

/** 鼠标离开后延时收起：留出从边缘斜移进面板的时间，也避免扫过时闪烁。 */
function scheduleSideHide(ms = 350) {
  clearSideHide()
  sideHideTimer = window.setTimeout(() => {
    // 定时器只负责“此刻鼠标已不在面板上”，焦点状态由 computed 自行判断
    sidebarHover.value = false
    sideHideTimer = undefined
  }, ms)
}

/**
 * 跟鼠标位置。判定带采用迟滞：收起时只认左缘 EDGE_PX，展开后整块面板宽度都算
 * “还在面板上”。靠坐标而不是 mouseenter/mouseleave：面板刚刚滑到光标底下时，
 * 浏览器不一定会及时重放 mouseenter，靠几何判断不会漏。
 */
function onBodyMove(ev: MouseEvent) {
  if (dialogOpen.value) return
  const host = ev.currentTarget as HTMLElement | null
  if (!host) return
  const rect = host.getBoundingClientRect()
  feedSidebar(ev.clientX - rect.left)
  feedToolbar(ev.clientY - rect.top)
}

/** 站点栏那一半：判定依旧带迟滞，收起时只认左缘 EDGE_PX，展开后整块面板宽度都算。 */
function feedSidebar(x: number) {
  const over = x <= (sidebarShown.value ? SIDEBAR_W : EDGE_PX)
  if (over === sidebarHover.value) return
  if (over) {
    clearSideHide()
    sidebarHover.value = true
  } else {
    sidebarHover.value = false
    if (!sidebarFocused.value) scheduleSideHide()
  }
}

/* ---------------- 编辑工具栏（终端顶部的浮条） ---------------- */
// 与左侧站点栏同一套坐标判定：浮条自身是透明的，再铺一层会吃掉点击的 mouseenter
// 热区不划算；而它贴着 .body 顶边，y 坐标一算就知道进没进。
// 迟滞与侧栏同理：收起时只认顶缘 TB_EDGE_PX，展开后整条高度都算“还在上面”。
const TB_EDGE_PX = 6
const TB_H = 30            // 与 EditToolbar 里 .ebt-bar 的实际高度对齐（运行量得 29px，留 1px 免得贴底那一排抽抖）
const tbHover = ref(false)
let tbHideTimer: number | undefined

function clearTbHide() {
  if (tbHideTimer) { clearTimeout(tbHideTimer); tbHideTimer = undefined }
}

function scheduleTbHide(ms = 350) {
  clearTbHide()
  tbHideTimer = window.setTimeout(() => { tbHover.value = false; tbHideTimer = undefined }, ms)
}

/**
 * 展开条件里为什么单独留着 sepPickerOpen：符号面板比工具栏高得多，鼠标往下够到
 * 「随机一种」时早就出了这条热区，只看 hover 会在半路把面板收掉，看起来像它自己关了。
 */
const tbShown = computed(() => !dialogOpen.value && active.value >= 0
  && (toolbarPinned.value || tbHover.value || sepPickerOpen.value))

function feedToolbar(y: number) {
  const over = y <= (tbShown.value ? TB_H : TB_EDGE_PX)
  if (over) {
    clearTbHide()
    tbHover.value = true
    return
  }
  if (tbHover.value && !toolbarPinned.value && !sepPickerOpen.value) scheduleTbHide()
}

function onBodyLeave() {
  if (sidebarHover.value && !sidebarFocused.value) {
    sidebarHover.value = false
    scheduleSideHide()
  }
  if (tbHover.value && !toolbarPinned.value && !sepPickerOpen.value) scheduleTbHide()
}

function onWindowBlur() {
  // 离开应用（Alt+Tab）也算失焦，收起悬浮面板
  sidebarFocused.value = false
  scheduleSideHide(120)
  if (!toolbarPinned.value && !sepPickerOpen.value) {
    tbHover.value = false
    clearTbHide()
  }
  hideTbFloat()
}

/* ---------------- 全屏（工具条改浮在底缘） ---------------- */
// 全屏要的是「终端吃满整屏」：顶部工具条不再占布局（改成 fixed 浮层）、底部状态条
// 收起。工具条平时整条沉到屏幕下沿外，鼠标在最底几像素停满 1 秒才滑出，往上抬开就收回。
// 1 秒的停留而不是一碰就弹：拖滚动条、选到行尾、右键菜单这类动作都会频繁扫过底缘，
// 路过就弹会把终端最下面几行反复盖住。
const TB_FULL_EDGE = 8        // 收起时「够到底缘」的判定高度
const TB_FULL_H = 40          // 与 .topbar 实际高度对齐（含上下 padding）
const TB_FULL_DWELL_MS = 1000 // 停满 1 秒才浮出
const isFull = ref(false)
const tbFloat = ref(false)
let tbDwellTimer: number | undefined
let tbFloatHideTimer: number | undefined

function clearTbFloatTimers() {
  if (tbDwellTimer) { clearTimeout(tbDwellTimer); tbDwellTimer = undefined }
  if (tbFloatHideTimer) { clearTimeout(tbFloatHideTimer); tbFloatHideTimer = undefined }
}

/** 收起浮动工具条：退出全屏、失焦、指针离开窗口、对话框打开都走它，两个定时器一起清。 */
function hideTbFloat() {
  clearTbFloatTimers()
  tbFloat.value = false
}

/**
 * fromBottom 是光标到窗口底缘的距离。判定带迟滞（与侧栏、编辑工具栏同一套）：
 * 收起时只认最底 TB_FULL_EDGE，浮出后整条高度都算「还在上面」。
 *
 * 停留计时一旦开始就不被后续 mousemove 重置：只要光标还在判定带里，微抖不该把已累计的时间清零。
 */
function feedTbFloat(fromBottom: number) {
  if (!isFull.value) return
  if (dialogOpen.value) { hideTbFloat(); return } // 滑到遮罩后面就是鬼影
  if (tbFloat.value) {
    if (fromBottom <= TB_FULL_H) { clearTbFloatTimers(); return }
    if (!tbFloatHideTimer) tbFloatHideTimer = window.setTimeout(() => { tbFloat.value = false; tbFloatHideTimer = undefined }, 350)
    return
  }
  if (fromBottom <= TB_FULL_EDGE) {
    if (tbDwellTimer) return
    tbDwellTimer = window.setTimeout(() => { tbDwellTimer = undefined; tbFloat.value = true }, TB_FULL_DWELL_MS)
    return
  }
  if (tbDwellTimer) { clearTimeout(tbDwellTimer); tbDwellTimer = undefined }
}

/**
 * 全屏下工具条浮层的判定挂在根节点（而不是 .body）的 capture 上：
 * 浮条本身是 .app 的直接子节点、盖在 .body 上方，光标落到它身上时事件根本不经过
 * .body 那一路；只在 .body 上听会在浮条弹出的瞬间把它判成「已离开」而当场收回。
 */
function onAppMove(ev: MouseEvent) {
  if (!isFull.value) return
  feedTbFloat(window.innerHeight - ev.clientY)
}

function onAppLeave() {
  if (isFull.value) hideTbFloat()
}

async function toggleFullscreen() {
  try {
    if (isFull.value) await Window.UnFullscreen()
    else await Window.Fullscreen()
    isFull.value = await Window.IsFullscreen()
  } catch (e) {
    showEcho(`全屏切换失败：${e}`, 'error')
    return
  }
  if (!isFull.value) hideTbFloat()
  // 布局里少了工具条与状态条、窗口又换了尺寸，主动重 fit 一次（不等 resize 事件）。
  nextTick(() => { const t = insts[active.value]; if (t) t.fitNow() })
  window.setTimeout(() => { const t = insts[active.value]; if (t) t.fitNow() }, 260)
  showEcho(isFull.value ? '全屏：工具条已收进底缘，鼠标停 1 秒浮出（M-x f 退出）' : '已退出全屏', 'info')
}

/** 焦点真正离开侧栏内部才置假（在侧栏子元素之间移动不算）。 */
function onSidebarFocusOut(ev: FocusEvent) {
  const self = ev.currentTarget as HTMLElement | null
  if (self && ev.relatedTarget instanceof Node && self.contains(ev.relatedTarget)) return
  sidebarFocused.value = false
}

/** 点面板空白处也要能进入“已聚焦”态：容器带 tabindex="-1"，直接把焦点收到根节点。 */
function focusSidebar() {
  clearSideHide()
  sidebarEl.value?.focus({ preventScroll: true })
}

/* 站点列表的方向键导航：焦点逐个落在行元素上，而不另外维护一个“选中序号”，
   避开“高亮跟焦点两套状态打架”的常见坑；屏幕阅读器也能跟上。 */
function siteRowEls(): HTMLElement[] {
  const list = sidebarEl.value?.querySelectorAll<HTMLElement>('.site')
  return list ? Array.from(list) : []
}

/** 当前获焦的行（在行内的 ✎/🗑 按钮上也算），没有则返回 -1。 */
function focusedRowIndex(rows: HTMLElement[]): number {
  const el = document.activeElement
  if (!(el instanceof HTMLElement)) return -1
  const row = el.closest<HTMLElement>('.site')
  return row ? rows.indexOf(row) : -1
}

/**
 * ↑/↓ 在站点间移动（到头不循环，跟列表控件惯例一致）。
 * 焦点还在侧栏容器本身（刚用热键唤出）时，↓ 从第一项、↑ 从最后一项进入。
 */
function moveSiteFocus(delta: number) {
  const rows = siteRowEls()
  if (!rows.length) return
  const cur = focusedRowIndex(rows)
  const i = cur < 0
    ? (delta > 0 ? 0 : rows.length - 1)
    : Math.min(rows.length - 1, Math.max(0, cur + delta))
  rows[i]?.focus()
}

/** Home/End：跳到第/最后一个站点（idx 为 -1 表示末尾）。 */
function jumpSiteFocus(idx: number) {
  const rows = siteRowEls()
  if (!rows.length) return
  rows[idx < 0 ? rows.length - 1 : Math.min(idx, rows.length - 1)]?.focus()
}

/**
 * 侧栏内的键盘导航入口。自己判 ev.key 而不是用 Vue 的 @keydown.up 修饰符：
 * 这样能与“哪些键归列表、哪些键归控件”的分工放在同一处看清楚。
 * 焦点在按钮/输入框上时直接不管（也不 preventDefault），否则＋ 的回车会被吞掉。
 */
function onSidebarKey(ev: KeyboardEvent) {
  const t = ev.target as HTMLElement | null
  if (t?.closest('button, input, select, textarea')) return
  switch (ev.key) {
    case 'ArrowUp': moveSiteFocus(-1); break
    case 'ArrowDown': moveSiteFocus(1); break
    case 'Home': jumpSiteFocus(0); break
    case 'End': jumpSiteFocus(-1); break
    case 'Enter': runFocusedSite(); break
    default: return
  }
  ev.preventDefault()
}

/**
 * Enter：运行当前获焦的站点配置（等效于双击）。
 * 焦点还在容器上时先把第一项纳入选中，需再按一次 Enter 才连接。
 */
function runFocusedSite() {
  const el = document.activeElement as HTMLElement | null
  const row = el?.closest<HTMLElement>('.site') ?? null
  if (!row) {
    siteRowEls()[0]?.focus()
    return
  }
  const site = sites.value[Number(row.dataset.idx ?? -1)]
  if (site) void newTab(site)
  else showEcho('站点已不存在（列表刚被修改），请重新选择', 'warn')
}

/**
 * 命令 / 工具栏按钮入口：切换浮动站点栏。
 * 展开依靠 focus()（而不是一个永久开关）：一旦用户点回终端，失焦隐藏自然生效；
 * 收起则把焦点交还终端，避免焦点留在看不见的面板里。
 */
function toggleSidebar() {
  if (dialogOpen.value) return
  clearSideHide()
  if (sidebarShown.value) {
    sidebarHover.value = false
    sidebarFocused.value = false
    const t = insts[active.value]
    if (t) t.focus()
    else (document.activeElement as HTMLElement | null)?.blur()
    return
  }
  // focus 会触发侧栏的 focusin，因此不用手动改 sidebarFocused
  focusSidebar()
}

/* ---------------- Emacs 式键序列 ---------------- */

const km = ref<api.KeymapConfig>({ enabled: true, timeoutMs: 1000, echo: true, bindings: [] })
// 跨应用热键表：后端低级键盘钩子那份，界面只负责读出来交给编辑器
const gk = ref<api.GlobalKeymap>({ enabled: true, doubleTapMs: 400, bindings: [] })
const kmDlg = reactive({ show: false })
const echoMsg = ref('')
const echoKind = ref<EchoInfo['kind']>('info')
let echoTimer: number | undefined

/** echo area：对应 Emacs 底部临时消息区，pending 不自动消失（由下一个事件接管）。 */
function showEcho(text: string, kind: EchoInfo['kind'] = 'info', sticky = false) {
  echoMsg.value = text
  echoKind.value = kind
  if (echoTimer) { clearTimeout(echoTimer); echoTimer = undefined }
  // sticky：文案长到 4 秒读不完（带路径的手动下载提示）时不自动抹，等下一个事件接管
  if (!sticky && kind !== 'pending') echoTimer = window.setTimeout(() => { echoMsg.value = '' }, 4000)
}

function applyKeymap(c: api.KeymapConfig) {
  km.value = c
  keyseq.configure({ enabled: c.enabled, timeoutMs: c.timeoutMs, echo: c.echo })
  keyseq.load(c.bindings || [])
}

/**
 * 焦点不在终端时的键序列入口（window 捕获阶段）。
 *
 * 绑定表整体是应用内生效的，所以这里不能只查「全局」那一半：否则用户点过任意一个
 * 工具栏按钮后焦点就停在按钮上（不在 viewport 里），M-x p 这类绑定会全部失效。
 */
function onGlobalKey(ev: KeyboardEvent) {
  const el = document.activeElement
  // 焦点在终端里 → 交给 xterm 回调，不查两遍。但活动标签若是文件管理器（也挂在
  // viewport 里、没有 xterm）就不能早退：得让 M-x / S-<方向> 这些出厂序列在这也能用
  // （否则关了个终端就再也 M-x k 不掉文件标签）。普通方向键/Enter/Fn 不在出厂序列里，
  // keyseq 判 pass 放行，仍会落到 FileManagerView 自己的 @keydown。
  if (el && viewport.value && viewport.value.contains(el) && tabs.value[active.value]?.kind !== 'files') return
  if (isTextEditing(el)) return // 正在往输入框打字，按键一律属于输入内容
  // 全屏下的退出出口：只认「焦点不在终端」这条路径。终端里 ESC 是 vim/BBS 的命根子，
  // 在 xterm 那一路抢走会把输终端打残，所以故意不做「按 ESC 处处退出」。
  if (isFull.value && ev.key === 'Escape') {
    ev.preventDefault()
    void toggleFullscreen()
    return
  }
  if (keyseq.feed(ev) === 'swallow') {
    ev.preventDefault()
    ev.stopPropagation()
  }
}

// 可写文本控件：这些控件里的按键属于打字，不得被序列框架抢走。xterm 的辅助
// textarea 也是 textarea，但它被上面 viewport 那行提前拦下，走不到这里。
const NON_TEXT_INPUT_TYPES = ['checkbox', 'radio', 'button', 'submit', 'range', 'color', 'file']
function isTextEditing(el: Element | null): boolean {
  if (!el) return false
  const e = el as HTMLElement
  if (e.isContentEditable) return true
  switch (e.tagName) {
    case 'TEXTAREA':
      return true
    case 'INPUT':
      return !NON_TEXT_INPUT_TYPES.includes((e as HTMLInputElement).type)
    default:
      return false
  }
}

keyseq.onEcho = (info: EchoInfo) => showEcho(info.message, info.kind)

registerBuiltinCommands({
  openEditor: () => { kmDlg.show = true },
  echo: (m, k) => showEcho(m, k || 'info'),
  activeSessionId: () => insts[active.value]?.id || '',
  // 手查与悬停查共用一套：同一个缓存、同一个提示位置（没会话时只进底部状态区）
  ipLookup: (addr) => void onIPHover(activeTerm(), addr),
  ipUpdate: () => void api.StartIPUpdate()
    .then(() => showEcho('正在更新 IP 库…', 'pending'))
    .catch((e: any) => showEcho(`IP 库更新启不起来：${e?.message || e}`, 'error')),
  getConfig: () => km.value,
  applyConfig: applyKeymap,
})

// 业务命令只要注册即可出现在绑定表下拉里，无需改动引擎。
registerCommand({
  id: 'app.toggle-sidebar',
  title: '界面：显示 / 隐藏站点栏（浮动）',
  description: '默认序列 M-x p；也可把鼠标移到窗口左缘唤出，失焦自动收起',
  handler: () => toggleSidebar(),
})

registerCommand({
  id: 'app.toggle-quake-mode',
  title: '窗口：普通 / quake 下拉模式互切',
  description: '默认序列 M-x M-q；跨应用呼出走系统全局热键 Alt+`（两种键位都认）',
  handler: () => { toggleWindowMode() },
})

registerCommand({
  id: 'app.toggle-fullscreen',
  title: '窗口：全屏 / 退出全屏（工具条浮在底缘）',
  description: '默认序列 M-x f；全屏时顶部工具条不再占布局，鼠标在底缘停 1 秒浮出；焦点不在终端时 ESC 也可退出',
  handler: () => { void toggleFullscreen() },
})

registerCommand({
  id: 'tab.prev',
  title: '标签：切到上一个（循环）',
  description: '默认 S-<LEFT>；已在头部时绕到尾部，只剩一个标签时不做动作',
  handler: () => cycleTab(-1),
})

registerCommand({
  id: 'tab.next',
  title: '标签：切到下一个（循环）',
  description: '默认 S-<RIGHT>；已在尾端时绕回头部，只剩一个标签时不做动作',
  handler: () => cycleTab(1),
})

registerCommand({
  id: 'tab.close',
  title: '标签：关闭当前标签',
  description: '默认 M-x k；连着线时按偏好可能弹确认，断开态直接关',
  handler: () => { if (active.value >= 0) void closeTab(active.value) },
})

registerCommand({
  id: 'files.open-manager',
  title: '文件管理器：新建一个双栏文件管理器标签（Total Commander 式）',
  description: '默认序列 M-x e；Tab 切栏，F5/F6 复制移动、F7/F8/F2 建目录/删除/重命名，Enter 打开、F3/Space 预览',
  handler: () => { void newFileManagerTab() },
})

// 编辑路径：命令住在 App 侧，但路径栏状态住在独立的 FileManagerView 里，
// 故用 CustomEvent 广播；组件自己判“可不见”（只活动标签响应）。活动标签不是文件管理器时回一句提示。
registerCommand({
  id: 'files.edit-path',
  title: '文件管理器：编辑当前栏的路径（地址栏）',
  description: '默认序列 M-x d；仅当活动标签是文件管理器时生效，回车跳转、Esc 取消',
  handler: () => {
    if (tabs.value[active.value]?.kind !== 'files') { showEcho('当前标签不是文件管理器（先 M-x e 开一个）', 'info'); return }
    window.dispatchEvent(new CustomEvent('ftrw:fm-edit-path'))
  },
})

/* ---------------- 发帖动作（编辑工具栏背后那套） ---------------- */
// 动作本体住在 postactions.ts（读鼠标选区 → 变换 → 原地替换），这里只把界面接上：
// 目标会话、状态区、以及“上次用过哪个分隔符”那份偏好。
configurePost({
  target: () => {
    const t = active.value >= 0 ? insts[active.value] : null
    return t && t.id ? t : null // 还没连上的标签不算目标（送不进去）
  },
  echo: (m, k) => showEcho(m, k || 'info'),
  getSep: () => prefs.value?.postSepChar || '',
  setSep: (ch) => {
    if (!prefs.value || prefs.value.postSepChar === ch) return
    prefs.value.postSepChar = ch
    // 为一个字符走整圈“保存设置”的重活不值，直接整份回写（后端会自己归一代理那些字段）；
    // 写失败也只是下次记不住，不该打断这一次的插入
    api.SetPreferences(prefs.value).catch(() => { /* noop */ })
  },
})

// 七条命令里除 post.insert 外都是“鼠标选区 → 原地替换”这一个动作的不同做法，默认键位收在
// M-x 前缀的同一族里（s 用上次符号 / r 随机一种 / m 逐字随机 / l 打开面板 / v 竖排 / i 灌剪贴板 / t 钉住）。
// post.sep 允许带参（写成 post.sep:·），用户想给常用符号单独配一键就能配。
registerCommand({
  id: 'post.sep',
  title: '发帖：选中文字字间插入分隔符（可写成 post.sep:符号）',
  description: '默认 M-x s；先鼠标框住编辑区那段字，不带参就用上次选定的那个，参数字符会顺手记成新的默认',
  handler: (arg) => { void runSep('fixed', arg || undefined) },
})

registerCommand({
  id: 'post.sep-random',
  title: '发帖：选中文字随机挑一种分隔符插入',
  description: '默认 M-x r；从预置集合里选一个，选中那段全文用同一个',
  handler: () => { void runSep('randomOne') },
})

registerCommand({
  id: 'post.sep-mixed',
  title: '发帖：选中文字逐字随机分隔符',
  description: '默认 M-x m；每两个字之间抽一个不同的',
  handler: () => { void runSep('randomMixed') },
})

registerCommand({
  id: 'post.sep-pick',
  title: '发帖：打开分隔符选择面板',
  description: '默认 M-x l；预置符号、自定义一个、随机一种、逐字随机都从这里进',
  handler: () => { openSepPicker() },
})

registerCommand({
  id: 'post.vertical',
  title: '发帖：选中文字转竖排',
  description: '默认 M-x v；栅格按当前终端列数算，ASCII 全角化，原地替换掉框住的那段',
  handler: () => { void runVertical() },
})

registerCommand({
  id: 'post.insert',
  title: '发帖：原样送入（优先灌回内存备份，没有才读剪贴板；逐行限速，不删任何东西）',
  description: '默认 M-x i；优先把上一次原地替换存进内存的原文备份灌回（不碰系统剪贴板），内存里没有才逐行限速读并送入剪贴板（只读不写）',
  handler: () => { void runRaw() },
})

registerCommand({
  id: 'post.toolbar-toggle',
  title: '发帖：钉住 / 收起编辑工具栏',
  description: '默认 M-x t；钉住后鼠标移开也不收起，反复点按钮时方便',
  handler: () => { toggleToolbarPin() },
})

// 窗口形态（普通 / quake）。与后端共用同一个原生窗口，这里只镜像状态供界面使用。
const winMode = ref<api.WindowModeState>({ mode: 'window', visible: true })
const isQuake = computed(() => winMode.value.mode === 'quake')

async function toggleWindowMode() {
  try {
    winMode.value = await api.ToggleWindowMode()
  } catch (e) {
    showEcho(`切换窗口模式失败：${e}`, 'error')
  }
}

// 原生对话框是独立 HWND，弹出时必然让主窗口失焦。quake 的「失焦即收起」
// 不看这道护栏就会在选完文件、点完“确认断开”后把窗口直接吞掉。
async function withDialogGuard<T>(run: () => Promise<T>): Promise<T> {
  await api.SetDialogGuard(true).catch(() => {})
  try {
    return await run()
  } finally {
    // 对话框已经关闭、焦点正在回到主窗口，稍等一下再拆护栏，免得中间那次
    // 失焦事件被当成「用户切走了」。
    setTimeout(() => { api.SetDialogGuard(false).catch(() => {}) }, 250)
  }
}

async function onKeymapSave(c: api.KeymapConfig, g: api.GlobalKeymap) {
  // 两张表一起提交，但只有应用内那份需要在前端重放：全局热键由后端自己重编译匹配器，
  // 前端只是镜像一份供下次打开编辑器时回填。
  try {
    await api.SaveKeymap(c)
    await api.SaveGlobalKeys(g)
  } catch (e) {
    showEcho(`保存热键表失败：${e}`, 'error')
    return
  }
  gk.value = g
  // 必须先等编辑器卸载：它在挂载期间把引擎强制置为静默，卸载时才恢复，
  // 若先 applyKeymap 再卸载，刚保存的开关会被旧值覆盖回去。
  kmDlg.show = false
  await nextTick()
  applyKeymap(c)
  const gs = g.enabled && g.bindings.length ? `，全局 ${g.bindings.length} 条` : ''
  showEcho(`键序列已保存：${c.bindings.length} 条绑定${gs}${c.enabled ? '' : '（引擎当前停用）'}`, c.enabled ? 'info' : 'warn')
}

// 无边框窗口控制（最小化/最大化切换/关闭）
const isMax = ref(false)
async function winMin() { try { await Window.Minimise() } catch { /* noop */ } }
async function winToggleMax() {
  try { await Window.ToggleMaximise(); isMax.value = await Window.IsMaximised() } catch { /* noop */ }
}
async function winClose() { try { await Window.Close() } catch { /* noop */ } }

// dialog state
const siteDlg = reactive({ show: false, site: null as api.Site | null, index: -1, save: true })
const prefsDlg = reactive({ show: false })
const schemeDlg = reactive({ show: false })

// 任何全屏对话框开着时，悬浮面板不得浮现（它会滑到遮罩后面，看起来像残留的鬼影）。
// sidebarShown 在上方已引用这里，但它的 getter 只在渲染时求值，不会碰到 TDZ。
const dialogOpen = computed(() => siteDlg.show || prefsDlg.show || schemeDlg.show || kmDlg.show)

// ZMODEM 进度弹窗状态（由 zmodem:progress 事件驱动）
const zm = reactive({ show: false, id: '', direction: 'send', filename: '', size: 0, done: 0, state: '', message: '' })
let zmHideTimer: number | undefined

function onZmCancel() {
  if (zm.id) api.CancelTransfer(zm.id).catch(() => { /* session gone */ })
}

const statusText = (s: string) =>
  ({ connecting: '连接中', connected: '已连接', closed: '已断开', error: '错误', zmodem: '传输' }[s] || s)

function idxOf(tab: TermTab): number { return insts.indexOf(tab) }

/* ---------------- IP 归属地（B8） ---------------- */

// 提示统一前缀：离开地址时靠它判断 echo 区那一行还是不是自己的，
// 已经被菜单、zmodem 接管的提示区就不该由我们收走。
const IP_HINT_HEAD = 'IP 归属地：'
// 一屏里常有好几个相同地址，悬停会反复撞上同一个查询。5 分钟 TTL 是给后台换库
// 留的出口：新库写进来之后旧结论不该一直挂着（库本身也是一天一轮）。
const ipCache = new Map<string, { at: number; text: string }>()
const IP_CACHE_MS = 5 * 60 * 1000
// 只认最新一次悬停：查询是异步的，慢回来的旧结果不该盖住已经切走的地址。
let ipSeq = 0

/** 同一句话写两个地方：底部状态区（一眼扫过）与鼠标旁浮层（就地能看到）。 */
function ipSay(tab: TermTab | null, text: string, kind: 'info' | 'warn' | 'error') {
  showEcho(`${IP_HINT_HEAD}${text}`, kind)
  tab?.setIPOsd(text, kind)
}

/** 悬停命中 / 离开一个 IPv4。ip 为空串表示离开，只收自己发出去的那条提示。 */
async function onIPHover(tab: TermTab | null, ip: string) {
  if (!ip) {
    tab?.setIPOsd('')
    if (echoMsg.value.startsWith(IP_HINT_HEAD)) showEcho('', 'info')
    return
  }
  const hit = ipCache.get(ip)
  if (hit && Date.now() - hit.at < IP_CACHE_MS) {
    ipSay(tab, hit.text, 'info')
    return
  }
  const seq = ++ipSeq
  let res: api.IPLocationResult
  try {
    res = await api.IPLocation(ip)
  } catch (e: any) {
    // 失败也要写到浮层上：「没检测到」与「检测到了但查失败」在界面上必须分得开，
    // 不然用户只能看到一个“没反应”，而两者该修的地方完全不同。
    if (seq === ipSeq) ipSay(tab, `${ip} 查询失败：${e?.message || e}`, 'error')
    return
  }
  if (seq !== ipSeq) return
  if (res.found) {
    ipCache.set(ip, { at: Date.now(), text: res.detail })
    ipSay(tab, res.detail, 'info')
    return
  }
  // 查不到不进缓存：库可能正在后台下、或者用户刚把文件放进目录，下次悬停就该拿到结果
  ipSay(tab, res.error || `${ip} 库里没有这条记录`, 'warn')
}

/**
 * 后端更新事件。失败那条里带着「手动下载 qqwry.dat 放到哪儿」，4 秒抹掉就等于没说，
 * 所以用 sticky；成功与进度正常自动消失。
 */
function onIPProgress(d: api.IPUpdateProgress) {
  if (d.state === 'downloading') {
    showEcho(d.message || '正在更新 IP 库…', 'pending')
    return
  }
  ipCache.clear() // 库可能刚被换掉，旧结论一律作废
  if (d.state === 'error') {
    showEcho(`${d.error || d.message || 'IP 库更新失败'}（可在设置里点「立即更新」重试）`, 'warn', true)
    return
  }
  showEcho(d.message || 'IP 库已更新', 'info')
}

/**
 * 只读调试钩子。鼠标坐标、缓冲区文本、浮层元素这类事只能拿运行时数据判断（日志里
 * 一条查询都没发出去，而代码看上去完全正确，已经连着猜了两轮），所以把应用自己的
 * 对象交给 WebView2 的远程调试端口：
 *
 *	$env:WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS = '--remote-debugging-port=9444'
 *	Start-Process .\bin\ftrw.exe
 *
 * 不新增任何能力（这个窗口本来就能开 DevTools），也不绑进 Wails 接口。
 */
function installDebugHook() {
  const w = window as any
  w.__ftrw = {
    insts, TermTab, onIPHover, showEcho, api, ipSpanAt, hostSpanAt, prefs, sites,
    // 切标签 / 读当前标签：CDP 取证必须对指定的那个会话动手，
    // 否则鼠标事件会落到屏幕上正显示的那个标签（隐藏标签的容器是 display:none）
    setActive, active,
    // 开标签也递出去：工具栏只在有标签时挂载，调试时得先有一个标签才能看到它
    // （只走现成的 newTab，不绕过任何校验）
    newTab,
    // 发帖这一套一并交给调试口：灌入的时序（逐行 + 限速）只能拿真会话验，
    // 把这层递出去就能从 CDP 直接驱动，不必用鼠标去碰那条自动隐藏的浮条
    post: { runSep, runVertical, runRaw, openSepPicker, sepPickerOpen, toolbarPinned, tbShown },
  }
}

async function newTab(site: api.Site) {
  if (!viewport.value) await nextTick()
  const container = document.createElement('div')
  container.className = 'term-container'
  // 动态创建的元素没有 scoped 属性，CSS 里的 .term-container 不生效；用内联样式
  // 强制铺满视口（否则容器只等于 xterm 内容高度，fit 永远算不出足够行数）。
  container.style.position = 'absolute'
  container.style.top = '0'
  container.style.left = '0'
  container.style.right = '0'
  container.style.bottom = '0'
  container.style.padding = '2px'
  container.style.boxSizing = 'border-box'
  container.style.display = 'none'
  viewport.value!.appendChild(container)

  const tab = new TermTab(site, container, { scrollback: prefs.value?.scrollbackLines })
  tab.onLink = (uri) => void onLinkClick(tab, uri)
  // 断线后的按键出口：空格关掉本标签（用 idxOf 现取现算，免得标签前后变动时关错），
  // 回车重连（复用 api.Connect 起新会话、api.Disconnect 清掉旧的死会话）。
  tab.onDisconnectedSpace = () => { const i = idxOf(tab); if (i >= 0) void closeTab(i) }
  tab.onDisconnectedEnter = () => { void reconnectTab(tab) }
  // 鼠标点菜单：开关取偏好（旧 prefs.json 没这个键时算开），悬停命中的提示进 echo 区
  tab.menuClick = prefs.value?.menuClickEnabled !== false
  tab.onMenuHint = (label) => showEcho(`点菜单：${label}`, 'info')
  // 悬停查 IP 归属地：旧 prefs.json 没这个键时算开（与点菜单同一套兼容写法）
  tab.ipHoverEnabled = prefs.value?.ipLocationEnabled !== false
  tab.onIPHint = (ip) => void onIPHover(tab, ip)
  // Alt+单击的诊断：界面已经就地弹了，这里再往文件日志写一份，这样就算
  // 浮层本身坏了（另一种坏法）也能从日志里看出前端到底跑到了哪一步。
  tab.onIPProbe = (msg) => {
    api.IPNote(msg).catch(() => { /* 日志通路掉了不该影响界面 */ })
  }
  // 配色按站点设置立即套用：以前只有去动下拉框才会 applyScheme，新标签一直顶着
  // 内置默认色跑；而「总是高亮 / ANSI 颜色」要靠调色板算默认前景，不套用就判错。
  const sc = schemes.value.find((x) => x.name === site.colorScheme) || schemes.value[0]
  if (sc) tab.applyScheme(sc)
  insts.push(tab)
  tabs.value.push({ id: '', title: tab.title, status: 'connecting' })
  const myIdx = insts.length - 1
  setActive(myIdx)

  try {
    const id = await api.Connect(site)
    tab.id = id
    byId.set(id, tab)
    tab.syncSize() // 首连前 fit 完的 onResize 被丢了，不补这一刀后端一直按 80x25 排版
    // 后端在 Connect() 返回前就已 emitStatus(connected)，但该事件早于上面的 byId.set，
    // 会被 handler 因查不到 tab 而丢弃，导致状态栏永远停在初始「连接中」。Connect 未抛异常
    // 即代表连接已建立，这里手动补一次 connected 状态。
    tab.setStatus('connected')
    if (tabs.value[myIdx]) { tabs.value[myIdx].id = id; tabs.value[myIdx].status = 'connected' }
  } catch (e: any) {
    tab.write(`\r\n\x1b[31m连接失败: ${e?.message || e}\x1b[0m\r\n`)
    tab.setStatus('error')
    if (tabs.value[myIdx]) tabs.value[myIdx].status = 'error'
  }
  // 自动登录（preLogin → 用户名 → 密码 → postLogin）已下沉到后端阶段机处理，
  // 会按登录提示推进且自动重连后会重新登录；这里旧式固定延时发 preLogin 的 hack
  // 必须移除，否则会与后端重复注入一次输入。
}

// activeTerm：活动标签不一定是终端（文件管理器标签混在 insts 里）。凡是要摸
// 终端专属成员（site / term / IP 缓存 / 配色）的地方都先过这道闸，FmTab 没有
// site，返回 null，让那些逻辑对文件标签自然空转，而不是拿到一个假 TermTab 崩属性。
function activeTerm(): TermTab | null {
  const t = active.value >= 0 ? insts[active.value] : null
  return t && t.site ? t : null
}

// newFileManagerTab：命令 / 工具栏入口。开一个 FmTab（独立 Vue 根挂在 viewport 里），
// 复用 newTab 那套铺满容器 + setActive 显隐/聚焦；FmTab 鸭子类型对齐通用标签逻辑，
// 故 cast 进 insts（见 fmtab.ts 注释）。
async function newFileManagerTab() {
  if (!viewport.value) await nextTick()
  const container = document.createElement('div')
  container.className = 'fm-container'
  container.style.position = 'absolute'
  container.style.top = '0'
  container.style.left = '0'
  container.style.right = '0'
  container.style.bottom = '0'
  container.style.display = 'none'
  viewport.value!.appendChild(container)
  const fm = new FmTab(container)
  insts.push(fm as unknown as TermTab)
  tabs.value.push({ id: '', title: '文件管理器', status: 'ready', kind: 'files' })
  setActive(insts.length - 1)
}

/**
 * 点到超链接：交给预览窗口。
 *
 * 清单取自当前屏幕（tab.collectLinks()），所以点开一张图之后还能在这一帖的图里
 * 循环着翻；被点的那条万一不在清单里（跨行折断一类）就单独补一条，保证「点了必有
 * 反应」。扩展名判不准的先问一次服务器：BBS 的图大量走 att.php?... 或纯附件号，
 * 误判成网页就得把整个窗口交给对方文档，缩放/翻页/另存为全都画不出来；而且真文件名
 * 只在服务器的响应头里，不探测就只能拿地址末段当名字。
 * 偏好里关掉预览时退回系统浏览器，不拦截用户。
 */
async function onLinkClick(tab: TermTab, uri: string) {
  if (!prefs.value?.imageViewerEnabled) {
    try {
      await api.OpenExternalURL(uri)
    } catch (e) {
      showEcho(`打开链接失败：${e}`, 'error')
    }
    return
  }
  const scanned = tab.collectLinks()
  const targets = probeTargets(scanned)
  const list = applyProbe(scanned, targets, await probeInfos(targets))
  let clicked = list.find((it) => it.url === uri)
  if (!clicked) {
    // 屏幕里没扫到：单独探这一条，别让它因为扩展名不明就退化成开网页
    const solo = (await probeInfos([uri]))[0]
    const name = (solo?.name || '').trim()
    clicked = {
      url: uri,
      kind: asKind(solo?.kind) || classifyURL(uri),
      name: name && !looksGenericName(name) ? name : linkName(uri),
    }
  }
  let items: PreviewItem[]
  if (clicked.kind === 'page') {
    items = [clicked]
  } else {
    items = mediaOnly(list)
    if (!items.some((it) => it.url === uri)) items = [clicked, ...items]
  }
  const index = Math.max(0, items.findIndex((it) => it.url === uri))
  try {
    await api.OpenPreview(items, index)
  } catch (e) {
    showEcho(`预览打开失败：${e}`, 'error')
  }
}

/** 问后端一批链接的真实类型与文件名。探测不许拖垮点击：失败就当没探到，退回原判。 */
async function probeInfos(urls: string[]): Promise<api.ProbeResult[]> {
  if (!urls.length) return []
  try {
    return (await api.ProbePreviewMany(urls)) || []
  } catch {
    return []
  }
}

function setActive(i: number) {
  active.value = i
  insts.forEach((t, idx) => { t.container.style.display = idx === i ? 'block' : 'none' })
  nextTick(() => { const t = insts[i]; if (t) { t.fitNow(); t.focus() } })
}

/**
 * 在标签间循环切换：delta 为 +1（往后）或 -1（往前）。
 *
 * 取余而不是限幅，所以从尾部再往右会回到头部 —— “循环”就是这条绑定的全部语义。
 * 只有一个标签时直接返回：不做动作也不回显，避开在状态区刷没意义的提示。
 */
function cycleTab(delta: number) {
  const n = insts.length
  if (n < 2 || active.value < 0) return
  setActive((active.value + delta + n) % n)
}

async function closeTab(i: number) {
  const t = insts[i]
  if (!t) return
  if (t.status === 'connected' && prefs.value?.confirmClose) {
    const r = await withDialogGuard(() => Dialogs.Question({
      Title: 'FTRW',
      Message: `断开 “${t.title}” ？`,
      Buttons: [{ Label: '断开', IsDefault: true }, { Label: '取消', IsCancel: true }],
    }))
    if (r !== '断开') return
  }
  if (t.id) { api.Disconnect(t.id); byId.delete(t.id) }
  t.dispose()
  t.container.remove()
  insts.splice(i, 1)
  tabs.value.splice(i, 1)
  if (active.value >= insts.length) active.value = insts.length - 1
  if (insts.length) setActive(active.value)
}

/**
 * 重连一个已断开的标签：先 Disconnect 清掉旧的死会话（顺带停掉它的自动重连协程），
 * 再用站点参数 api.Connect 起一个新会话换上 id；标签本身、滚动缓冲、窗口都不动。
 * 若从没连上过（tab.id 为空，首连就抛错、后端已自行移除），则直接 Connect 补上。
 */
async function reconnectTab(tab: TermTab) {
  const mi = idxOf(tab)
  if (mi < 0) return
  if (tab.status === 'connected' || tab.status === 'connecting') return
  tab.setStatus('connecting')
  if (tabs.value[mi]) tabs.value[mi].status = 'connecting'
  const old = tab.id
  if (old) { api.Disconnect(old).catch(() => { /* 死会话断开失败无所谓 */ }); byId.delete(old) }
  tab.write('\r\n\x1b[90m[正在重连…]\x1b[0m\r\n')
  try {
    const id = await api.Connect(tab.site)
    tab.id = id
    byId.set(id, tab)
    // 新会话的尺寸是按站点默认（80x25）起步的；标签已经 fit 到位、重连时不会再生
    // onResize，不主动补一刀的话远端会按默认尺寸排版、菜单分栏错位。
    tab.syncSize()
    // 同 newTab：Connect 返回前的 connected 事件早于 byId.set 会被丢弃，这里补一次。
    tab.setStatus('connected')
    if (tabs.value[mi]) { tabs.value[mi].id = id; tabs.value[mi].status = 'connected' }
    showEcho('已重连', 'info')
  } catch (e: any) {
    tab.write(`\r\n\x1b[31m重连失败: ${e?.message || e}\x1b[0m\r\n`)
    tab.setStatus('error')
    if (tabs.value[mi]) tabs.value[mi].status = 'error'
    showEcho(`重连失败: ${e?.message || e}`, 'error')
  }
}

function openQuickConnect() {
  api.DefaultSite().then((s) => { s.name = ''; Object.assign(siteDlg, { show: true, site: s, index: -1, save: false }) })
}

async function openSiteEditor(index: number) {
  const s = index >= 0 ? { ...sites.value[index] } : await api.DefaultSite()
  Object.assign(siteDlg, { show: true, site: s, index, save: true })
}

async function onSiteSaved(site: api.Site, index: number, save: boolean) {
  if (save) { await api.SaveSite(index, site); await refreshSites() }
  siteDlg.show = false
  // 编辑已有站点时，已打开的同名标签即时套用新字体/排版（对齐原版“保存即刷新窗口”，
  // 字体以外的连接类变更仍需重连生效）。
  if (save && index >= 0) {
    for (const t of insts) if (t.site.name === site.name) t.applyFontOptions(site)
  }
  await newTab(site)
}

async function removeSite(index: number) { await api.DeleteSite(index); await refreshSites() }
async function refreshSites() { sites.value = await api.ListSites() }

// 扫描本机 shell：后端探测 cmd/PowerShell/pwsh/Git Bash/WSL 等，落成 shell 组的站点条目。
// 启动时静默跑一次（auto 发现不弹窗），侧栏 ⟳ 手动跑一次则报计数。
const shellScanning = ref(false)
async function scanShells(report: boolean) {
  if (shellScanning.value) return
  shellScanning.value = true
  try {
    const r = await api.SyncShells()
    const changed = (r.added || 0) + (r.updated || 0) + (r.removed || 0)
    if (changed > 0) await refreshSites()
    if (report) {
      showEcho(changed === 0
        ? `本机 shell 无变化（共 ${r.shells?.length || 0} 个）`
        : `本机 shell：新增 ${r.added || 0}、更新 ${r.updated || 0}、移除 ${r.removed || 0}`,
        'info')
    }
  } catch (e: any) {
    if (report) showEcho('扫描本机 shell 失败：' + (e?.message || e), 'warn')
    else console.warn('scanShells', e)
  } finally {
    shellScanning.value = false
  }
}

function dotClass(site: api.Site) {
  if (site.protocolType === 3) return 'local'
  return site.protocolType === 0 ? 'tel' : 'ssh'
}

// shell 条目把协议列换成「shell」，比显示 “Local” 直观；名字空时退回路径末尾的程
// 序名，避免侧栏只剩一个反斜杠串。
function protoLabel(site: api.Site) { return site.protocolType === 3 ? 'shell' : api.PROTOCOLS[site.protocolType] }
function siteLabel(site: api.Site) {
  if (site.name) return site.name
  if (site.protocolType === 3) return String(site.host || '').split(/[\\/]/).pop() || site.host
  return site.host
}

function grouped(): { name: string; items: { site: api.Site; index: number }[] }[] {
  const map = new Map<string, { site: api.Site; index: number }[]>()
  sites.value.forEach((site, index) => {
    const g = site.group || '默认分组'
    if (!map.has(g)) map.set(g, [])
    map.get(g)!.push({ site, index })
  })
  return [...map.entries()].map(([name, items]) => ({ name, items }))
}

// 工具栏不再放「上传」按钮：BBS 的上传由对端跑 sz 触发，收到 ZRQINIT 后
// zmodemEngine.promptUploadFiles 会自己弹原生选文件框，手动入口反而容易误操作。

let offData: () => void, offStatus: () => void, offZ: () => void, offMode: (() => void) | undefined
let offIP: (() => void) | undefined

onMounted(async () => {
  prefs.value = await api.GetPreferences()
  await refreshSites()
  schemes.value = await api.ListSchemes()
  // 首次扫描放后台：WSL 探针最长要 3 秒，不能让它挡在第一帧之前。扫完自动刷新列表。
  void scanShells(false)
  installDebugHook()

  offData = api.onTermData((d) => { byId.get(d.id)?.write(d.data) })
  offStatus = api.onTermStatus((d) => {
    const t = byId.get(d.id)
    if (!t) return
    t.setStatus(d.status)
    if (d.title) t.title = d.title
    const mi = idxOf(t)
    if (mi >= 0 && tabs.value[mi]) {
      tabs.value[mi].status = d.status
      if (d.title) tabs.value[mi].title = d.title
    }
  })
  offZ = api.onZmodem((d) => {
    const t = byId.get(d.id)
    if (!t) return
    if (zmHideTimer) { clearTimeout(zmHideTimer); zmHideTimer = undefined }
    const fin = d.state === 'done' || d.state === 'error' || d.state === 'cancelled' || d.state === 'skipped'
    // 结束事件的 size/filename 可能为空，沿用进行中状态里的值让弹窗信息完整
    zm.show = true
    zm.id = d.id
    zm.direction = d.direction || zm.direction
    zm.filename = d.filename || (fin ? zm.filename : '')
    zm.size = d.size > 0 ? d.size : (fin ? zm.size : 0)
    zm.done = d.done > 0 ? d.done : (d.state === 'done' ? (d.size > 0 ? d.size : zm.done) : (fin ? zm.done : 0))
    if (d.state === 'done' && zm.size > 0) zm.done = zm.size
    zm.state = d.state
    zm.message = d.message
    if (fin) {
      // 终态自动收起：完成/取消 1 秒后消失；失败/跳过留 10 秒供看清原因（也可手动 ✕）
      const st = d.state
      const hideMs = (st === 'done' || st === 'cancelled') ? 1000 : 10000
      zmHideTimer = window.setTimeout(() => { if (zm.state === st) zm.show = false }, hideMs)
    }
    // 状态栏简短提示（保留旧行为）
    if (fin) {
      zmodemMsg.value = d.message
      setTimeout(() => { if (zmodemMsg.value === d.message) zmodemMsg.value = '' }, 5000)
    }
  })
  offIP = api.onIPUpdate(onIPProgress)
  window.addEventListener('resize', onWinResize)
  window.addEventListener('blur', onWindowBlur)
  try { applyKeymap(await api.GetKeymap()) } catch { /* 读不到就用出厂默认表 */ }
  try { gk.value = await api.GetGlobalKeys() } catch { /* 全局表读不到不影响应用内绑定 */ }
  window.addEventListener('keydown', onGlobalKey, true)
  Window.IsMaximised().then((v) => { isMax.value = v }).catch(() => {})
  Window.IsFullscreen().then((v) => { isFull.value = v }).catch(() => {})

  // 窗口形态：先拉一次快照，再订阅后端变更（全局热键与托盘都会广播）。
  // 从 quake 热键呼出时 webview 可能刚从隐藏中恢复，终端需要一次重新 fit + 聚焦，
  // 否则看上去是一片旧尺寸、且键盘输入没落到终端里。
  try { winMode.value = await api.GetWindowMode() } catch { /* 保持默认 */ }
  offMode = api.onWindowMode((d) => {
    const wasVisible = winMode.value.visible
    winMode.value = d
    if (d.visible && !wasVisible) {
      nextTick(() => { const t = insts[active.value]; if (t) { t.fitNow(); t.focus() } })
    }
  })
})

onBeforeUnmount(() => { offData?.(); offStatus?.(); offZ?.(); offMode?.(); offIP?.(); if (zmHideTimer) clearTimeout(zmHideTimer); if (echoTimer) clearTimeout(echoTimer); clearSideHide(); window.removeEventListener('resize', onWinResize); window.removeEventListener('blur', onWindowBlur); window.removeEventListener('keydown', onGlobalKey, true) })

function onWinResize() {
  insts[active.value]?.fitNow()
  Window.IsMaximised().then((v) => { isMax.value = v }).catch(() => {})
  Window.IsFullscreen().then((v) => { isFull.value = v }).catch(() => {})
}

function applySchemeToActive() {
  const t = activeTerm()
  if (!t) return
  const sc = schemes.value.find((s) => s.name === t.site.colorScheme) || schemes.value[0]
  t.applyScheme(sc || null)
}

const activeScheme = computed({
  // 活动标签可能是文件管理器（FmTab 没有 site），统一经 activeTerm() 闸，
  // 非终端时读空串 / 写不生效，避免渲染期访问 undefined.site 抛错。
  get: () => activeTerm()?.site.colorScheme || '',
  set: (v: string) => {
    const t = activeTerm()
    if (!t) return
    t.site.colorScheme = v
    applySchemeToActive()
  },
})

async function savePrefs(p: api.Preferences) {
  await api.SetPreferences(p)
  prefs.value = p
  prefsDlg.show = false
  // 点菜单开关与回滚行数对手上已经开着的标签立即生效，不必重开
  for (const t of insts) {
    t.menuClick = p.menuClickEnabled !== false
    t.ipHoverEnabled = p.ipLocationEnabled !== false
    t.setScrollback(p.scrollbackLines)
  }
  ipCache.clear()
  // 库路径 / 下载地址可能刚改过：让后端重找一次，不然这一轮还是拿着旧文件
  await api.IPReload().catch(() => { /* 重读失败不影响偏好已保存 */ })
  // 托盘显隐等原生侧设置需要推给 AppService 重放
  await api.ApplyAppPreferences().catch(() => { /* 不影响偏好已保存 */ })
}

const cur = computed(() => (active.value >= 0 ? { meta: tabs.value[active.value], inst: insts[active.value] } : null))
</script>

<template>
  <div class="app" :class="{ quake: isQuake, full: isFull }"
       @mousemove.capture="onAppMove" @mouseleave="onAppLeave">
    <div class="topbar" :class="{ 'tb-float-on': isFull && tbFloat }">
      <span class="brand">FTRW</span>
      <button @click="toggleSidebar" :title="sidebarShown ? '隐藏站点栏（Alt+X P）' : '显示站点栏（Alt+X P，或鼠标移到窗口左缘）'">☰ 站点</button>
      <button @click="openQuickConnect" title="新建连接">＋ 连接</button>
      <button @click="closeTab(active)" :disabled="active < 0" title="关闭当前标签">✕ 断开</button>
      <!-- 标签页跟工具栏共用一行：多一个会话就少不了一行高，窄屏下标签区自己滚动 -->
      <div class="tabs" v-if="tabs.length">
        <div v-for="(t, i) in tabs" :key="i" class="tab" :class="{ on: i === active }" @click="setActive(i)">
          <span class="tdot" :class="t.status"></span>
          <span class="ttitle" :title="t.title">{{ t.title }}</span>
          <span class="tclose" @click.stop="closeTab(i)">✕</span>
        </div>
      </div>
      <!-- 全屏时状态条收起了，热键/按钮的回显改住在工具条里，否则提示没了落点 -->
      <span class="echo fb-echo" :class="'echo-' + echoKind" v-if="isFull && echoMsg">{{ echoMsg }}</span>
      <div class="spacer" />
      <button @click="toggleWindowMode()" :class="{ 'mode-on': isQuake }"
              :title="isQuake ? '回到普通窗口（M-x M-q）' : '切到 quake 下拉窗口（M-x M-q / 全局 Alt+`）'">{{ isQuake ? '▾ quake' : '▢ 普通' }}</button>
      <button @click="toggleFullscreen" :class="{ 'mode-on': isFull }"
              :title="isFull ? '退出全屏（M-x f；焦点不在终端时 ESC 也退出）' : '全屏（M-x f；工具条收进底缘，鼠标停 1 秒浮出）'">{{ isFull ? '⛶ 退出全屏' : '⛶ 全屏' }}</button>
      <select v-model="activeScheme" :disabled="active < 0" title="配色方案">
        <option v-for="s in schemes" :key="s.name" :value="s.name">{{ s.name }}</option>
      </select>
      <button @click="schemeDlg.show = true" title="配色方案编辑">🎨</button>
      <button @click="kmDlg.show = true" title="键序列 / 热键编辑器（Emacs 风格）">⌨ 热键</button>
      <button @click="prefsDlg.show = true" title="偏好设置">⚙ 设置</button>
      <div class="wctl">
        <button class="wc" @click="winMin" title="最小化">&#8722;</button>
        <button class="wc" @click="winToggleMax" v-if="!isQuake && !isFull" :title="isMax ? '还原' : '最大化'">{{ isMax ? '❐' : '□' }}</button>
        <button class="wc wc-close" @click="winClose" :title="prefs?.closeToTray ? '收起至托盘' : '关闭'">&#10005;</button>
      </div>
    </div>

    <!-- 顶缘/左缘热区靠 mousemove 算坐标，必须用 capture：xterm 在 .xterm 根元素上
         对 mousemove 第一行就 stopImmediatePropagation，冒泡阶段的监听在鼠标落到
         屏幕正文上时根本收不到事件——面板能弹出来（首屏那几像素落在容器的 padding
         上）但再也收不回，看起来就像“工具栏卡在屏幕上了”。 -->
    <div class="body" @mousemove.capture="onBodyMove" @mouseleave="onBodyLeave">
      <!-- 浮动侧栏：收起时整块滑到左缘外，不占据布局 → 终端视口宽度不变 -->
      <div class="sidebar" ref="sidebarEl" tabindex="-1" :style="{ transform: sidebarTransform }"
           @click.self="focusSidebar" @focusin="sidebarFocused = true" @focusout="onSidebarFocusOut"
           @keydown="onSidebarKey">
        <div class="side-head">
          <span>站点管理<i class="tip">↑↓ 选择 · Enter 连接</i></span>
          <span class="head-acts">
            <button class="mini" :disabled="shellScanning" title="扫描本机 Shell（CMD / PowerShell / Git Bash / WSL）"
                    @click="scanShells(true)">{{ shellScanning ? '…' : '⟳' }}</button>
            <button class="mini" title="新建站点" @click="openSiteEditor(-1)">＋</button>
          </span>
        </div>
        <div class="tree">
          <div v-for="g in grouped()" :key="g.name" class="group">
            <div class="group-name" :class="{ shell: g.name === api.SHELL_GROUP }"
                 :title="g.name === api.SHELL_GROUP ? '启动时自动扫描本机生成，可自行编辑或删除' : ''">
              {{ g.name === api.SHELL_GROUP ? 'shell · 本机' : g.name }}
            </div>
            <div v-for="it in g.items" :key="it.index" class="site"
                 :data-idx="it.index" :tabindex="sidebarShown ? 0 : -1"
                 @dblclick="newTab(it.site)"
                 @contextmenu.prevent="openSiteEditor(it.index)">
              <span class="dot" :class="dotClass(it.site)"></span>
              <span class="sname">{{ siteLabel(it.site) }}</span>
              <span class="sproto">{{ protoLabel(it.site) }}</span>
              <span class="acts">
                <button class="mini" @click.stop="openSiteEditor(it.index)">✎</button>
                <button class="mini" @click.stop="removeSite(it.index)">🗑</button>
              </span>
            </div>
          </div>
          <div v-if="!sites.length" class="empty">暂无站点，点左上「＋」新建，或点「⟳」扫描本机 Shell</div>
        </div>
      </div>

      <div class="main">
        <div class="viewport" ref="viewport" />
        <EditToolbar v-if="tabs.length" :shown="tbShown" />
        <div v-if="!tabs.length" class="welcome">
          <h2>FTRW</h2>
          <p>Telnet / SSH / BBS 终端</p>
          <div class="hint">点击「＋ 连接」新建会话；把鼠标移到窗口左缘、或按 Alt+X P 唤出站点栏（失焦自动收起）</div>
          <div class="hint">连上之后把鼠标移到终端顶缘会滑出「编辑工具栏」：先用鼠标框住编辑区里要处理的文字，再一键插分隔符或转竖排（Alt+X 后按 S / V）</div>
        </div>
      </div>
    </div>

    <div class="statusbar">
      <span v-if="cur && cur.inst.site">
        {{ statusText(cur.meta.status) }}
        · {{ cur.inst.site.host || '本地' }}
        · {{ api.ENCODINGS[cur.inst.encoderID] }}
        · {{ cur.inst.term.cols }}×{{ cur.inst.term.rows }}
      </span>
      <span v-else>就绪</span>
      <div class="spacer" />
      <span class="echo" :class="'echo-' + echoKind" v-if="echoMsg">{{ echoMsg }}</span>
      <span class="zmodem" v-if="zmodemMsg">{{ zmodemMsg }}</span>
    </div>

    <SiteDialog v-if="siteDlg.show" :site="siteDlg.site!" :index="siteDlg.index" :save="siteDlg.save"
                :schemes="schemes" @save="onSiteSaved" @close="siteDlg.show = false" />
    <PrefsDialog v-if="prefsDlg.show" :prefs="prefs!" @save="savePrefs" @close="prefsDlg.show = false" />
    <SchemeDialog v-if="schemeDlg.show" :schemes="schemes" @close="schemeDlg.show = false"
                  @changed="async () => { schemes = await api.ListSchemes(); applySchemeToActive() }" />
    <ZmodemDialog v-if="zm.show" :info="zm" @cancel="onZmCancel" @close="zm.show = false" />
    <KeymapDialog v-if="kmDlg.show" :km="km" :gk="gk" @save="onKeymapSave" @close="kmDlg.show = false" />
  </div>
</template>

<style scoped>
.app { display: flex; flex-direction: column; height: 100%; width: 100%; overflow: hidden; background: #1b1d22; color: #c8ccd4; font-family: system-ui, "Segoe UI", sans-serif; }
.topbar { display: flex; align-items: center; gap: 6px; padding: 5px 10px; background: #24272e; border-bottom: 1px solid #33373f; --wails-draggable: drag; }
.topbar button, .topbar select, .topbar .wctl, .topbar .tabs { --wails-draggable: no-drag; }
/* 右侧控件不参与收缩：标签再多也只能挤压标签区自己滚动，不能把关闭/最小化按钮顶出窗口 */
.topbar > button, .topbar > select, .topbar .wctl { flex: 0 0 auto; }
.brand { font-weight: 700; color: #6ab0f3; margin-right: 12px; letter-spacing: 1px; }
.topbar button, .topbar select { background: #2f333b; color: #c8ccd4; border: 1px solid #3d424b; border-radius: 5px; padding: 4px 10px; cursor: pointer; font-size: 13px; }
.topbar button:hover:not(:disabled) { background: #3a3f49; }
.topbar button:disabled { opacity: .4; cursor: default; }
/* quake 模式下的形态开关高亮：窗口已是置顶下拉，需要一眼看出当前形态 */
.topbar button.mode-on { background: #2f74c0; border-color: #2f74c0; color: #fff; }
.spacer { flex: 1; min-width: 6px; }
/* quake：合成一行后把这一行整条放到窗口最下面，终端从屏幕顶部垂下来；
   状态条夹在中间，两道上边框把三层分开。靠 flex order 实现，不搬 DOM。*/
.app.quake .body { order: 1; }
.app.quake .statusbar { order: 2; }
.app.quake .topbar { order: 3; border-bottom: none; border-top: 1px solid #33373f; }
/* 全屏：顶部工具条改为贴在底缘的浮层，状态条收起，终端吃满整屏。
   必须用 fixed 而不是像 quake 那样靠 flex order —— 工具条一旦还占布局，终端就少一行，
   「全屏」就白做了。收起态整条沉到屏幕下沿外（translateY 超出自身高度），
   只留最底几像素当唤出判定带；z-index 故意压在对话框遮罩（50/60）下面。*/
.app.full .topbar { position: fixed; left: 0; right: 0; top: auto; bottom: 0; order: 0; z-index: 30;
  border-bottom: none; border-top: 1px solid #33373f; background: #24272ef2; box-shadow: 0 -6px 18px rgba(0, 0, 0, .4);
  transform: translateY(110%); transition: transform .16s ease-out; }
.app.full .topbar.tb-float-on { transform: none; }
.app.full .statusbar { display: none; }
/* 工具条里的回显文本：只能占自己剩下来的宽度，长了就省略，不能把右边按钮顶出窗口 */
.fb-echo { min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-size: 12px; }
.topbar .echo-pending { color: #d9a441; font-family: Consolas, monospace; }
.topbar .echo-info { color: #8fb5e8; }
.topbar .echo-warn { color: #d9a441; }
.topbar .echo-error { color: #e06c5b; }
.wctl { display: flex; gap: 2px; margin-left: 8px; }
.wctl .wc { width: 32px; height: 26px; padding: 0; line-height: 1; font-size: 14px; border-radius: 4px; background: transparent; border: 1px solid transparent; color: #c8ccd4; }
.wctl .wc:hover { background: #3a3f49 !important; border-color: #3d424b; }
.wctl .wc-close:hover { background: #e05c5c !important; border-color: #e05c5c !important; color: #fff !important; }
.body { flex: 1; display: flex; min-height: 0; position: relative; }
/* 浮动态：脱离文档布局覆盖在终端上，收起时整块滑出左边缘（不挤压视口，终端尺寸不变） */
.sidebar { position: absolute; left: 0; top: 0; bottom: 0; width: 260px; z-index: 20; background: #202329f2; border-right: 1px solid #33373f; box-shadow: 6px 0 18px rgba(0, 0, 0, .45); display: flex; flex-direction: column; transform: translateX(-260px); transition: transform .16s ease-out; outline: none; }
.side-head { display: flex; justify-content: space-between; align-items: center; padding: 8px 10px; font-size: 12px; color: #8a919e; border-bottom: 1px solid #2c2f36; }
.side-head .tip { font-style: normal; margin-left: 8px; color: #5d6472; font-size: 11px; }
.head-acts { display: flex; gap: 2px; }
.tree { overflow-y: auto; flex: 1; }
.group-name { padding: 6px 10px 2px; font-size: 12px; color: #6a7180; font-weight: 600; }
.group-name.shell { color: #d7b45a; }
.site { display: flex; align-items: center; gap: 6px; padding: 5px 10px; cursor: pointer; font-size: 13px; border-radius: 4px; margin: 1px 4px; }
.site:focus { background: #2f3644; outline: 1px solid #4a6b96; outline-offset: -1px; }
.site:focus .sname { color: #fff; }
.site:hover { background: #2b2f37; }
.dot { width: 8px; height: 8px; border-radius: 50%; }
.dot.tel { background: #4ec9b0; } .dot.ssh { background: #6ab0f3; } .dot.local { background: #d7b45a; }
.sname { flex: 1; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.sproto { font-size: 11px; color: #6a7180; }
.acts { display: none; gap: 2px; } .site:hover .acts { display: flex; }
.mini { padding: 0 5px !important; font-size: 11px !important; border: none !important; background: transparent !important; color: #8a919e !important; }
.mini:hover { color: #fff !important; }
.empty { padding: 20px; text-align: center; color: #555a64; font-size: 12px; }
.main { flex: 1; display: flex; flex-direction: column; min-width: 0; position: relative; }
/* 标签 chips 内联在工具栏行里：只占自己需要的宽度，不够就横向滚动 */
.tabs { display: flex; align-items: center; gap: 4px; min-width: 0; overflow-x: auto; overflow-y: hidden; scrollbar-width: thin; }
.tab { display: flex; align-items: center; gap: 6px; flex: 0 0 auto; max-width: 180px; padding: 3px 8px; font-size: 13px; cursor: pointer; background: #2f333b; color: #b5bac4; border: 1px solid #3d424b; border-radius: 5px; }
.tab:hover { background: #3a3f49; }
.tab.on { background: #1b1d22; border-color: #4a6b96; color: #fff; }
.tab .ttitle { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.tdot { width: 8px; height: 8px; border-radius: 50%; background: #d0d0d0; }
.tdot.connected { background: #57d25a; } .tdot.connecting { background: #e0b341; } .tdot.error { background: #e05c5c; } .tdot.closed { background: #777; }
.tclose { color: #6a7180; } .tclose:hover { color: #fff; }
.viewport { flex: 1; min-height: 0; position: relative; }
.term-container { position: absolute; inset: 0; padding: 4px; }
.welcome { position: absolute; inset: 0; display: flex; flex-direction: column; align-items: center; justify-content: center; color: #55606f; pointer-events: none; }
.welcome h2 { color: #6ab0f3; margin-bottom: 4px; } .welcome .hint { margin-top: 16px; font-size: 12px; }
.statusbar { display: flex; align-items: center; gap: 10px; padding: 3px 12px; background: #24272e; border-top: 1px solid #33373f; font-size: 12px; color: #8a919e; height: 24px; }
.statusbar .zmodem { color: #e0b341; }
.statusbar .echo { font-size: 12px; }
.statusbar .echo-pending { color: #d9a441; font-family: Consolas, monospace; }
.statusbar .echo-info { color: #8fb5e8; }
.statusbar .echo-warn { color: #d9a441; }
.statusbar .echo-error { color: #e06c5b; }
</style>
