<script setup lang="ts">
import { ref, reactive, computed, onMounted, onBeforeUnmount, nextTick } from 'vue'
import * as api from './api'
import { TermTab } from './terminal'
import { Dialogs, Window } from '@wailsio/runtime'
import SiteDialog from './components/SiteDialog.vue'
import PrefsDialog from './components/PrefsDialog.vue'
import SchemeDialog from './components/SchemeDialog.vue'
import ZmodemDialog from './components/ZmodemDialog.vue'
import KeymapDialog from './components/KeymapDialog.vue'
import { keyseq, registerCommand, type EchoInfo } from './keyseq'
import { registerBuiltinCommands } from './keymapcmds'
import { applyProbe, asKind, classifyURL, hostSpanAt, ipSpanAt, linkName, looksGenericName, mediaOnly, probeTargets, type PreviewItem } from './linkscan'

interface TabMeta { id: string; title: string; status: string }

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
  const x = ev.clientX - host.getBoundingClientRect().left
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

function onBodyLeave() {
  if (!sidebarHover.value || sidebarFocused.value) return
  sidebarHover.value = false
  scheduleSideHide()
}

function onWindowBlur() {
  // 离开应用（Alt+Tab）也算失焦，收起悬浮面板
  sidebarFocused.value = false
  scheduleSideHide(120)
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
  if (el && viewport.value && viewport.value.contains(el)) return // 焦点在终端，交给 xterm 的回调，不查两遍
  if (isTextEditing(el)) return // 正在往输入框打字，按键一律属于输入内容
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
  ipLookup: (addr) => void onIPHover(insts[active.value] ?? null, addr),
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
  w.__ftrw = { insts, TermTab, onIPHover, showEcho, api, ipSpanAt, hostSpanAt, prefs }
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
}

function applySchemeToActive() {
  const t = insts[active.value]
  if (!t) return
  const sc = schemes.value.find((s) => s.name === t.site.colorScheme) || schemes.value[0]
  t.applyScheme(sc || null)
}

const activeScheme = computed({
  get: () => (active.value >= 0 ? insts[active.value].site.colorScheme : ''),
  set: (v: string) => {
    if (active.value < 0) return
    insts[active.value].site.colorScheme = v
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
  <div class="app" :class="{ quake: isQuake }">
    <div class="topbar">
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
      <div class="spacer" />
      <button @click="toggleWindowMode()" :class="{ 'mode-on': isQuake }"
              :title="isQuake ? '回到普通窗口（M-x M-q）' : '切到 quake 下拉窗口（M-x M-q / 全局 Alt+`）'">{{ isQuake ? '▾ quake' : '▢ 普通' }}</button>
      <select v-model="activeScheme" :disabled="active < 0" title="配色方案">
        <option v-for="s in schemes" :key="s.name" :value="s.name">{{ s.name }}</option>
      </select>
      <button @click="schemeDlg.show = true" title="配色方案编辑">🎨</button>
      <button @click="kmDlg.show = true" title="键序列 / 热键编辑器（Emacs 风格）">⌨ 热键</button>
      <button @click="prefsDlg.show = true" title="偏好设置">⚙ 设置</button>
      <div class="wctl">
        <button class="wc" @click="winMin" title="最小化">&#8722;</button>
        <button class="wc" @click="winToggleMax" v-if="!isQuake" :title="isMax ? '还原' : '最大化'">{{ isMax ? '❐' : '□' }}</button>
        <button class="wc wc-close" @click="winClose" :title="prefs?.closeToTray ? '收起至托盘' : '关闭'">&#10005;</button>
      </div>
    </div>

    <div class="body" @mousemove="onBodyMove" @mouseleave="onBodyLeave">
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
        <div v-if="!tabs.length" class="welcome">
          <h2>FTRW</h2>
          <p>Telnet / SSH / BBS 终端</p>
          <div class="hint">点击「＋ 连接」新建会话；把鼠标移到窗口左缘、或按 Alt+X P 唤出站点栏（失焦自动收起）</div>
        </div>
      </div>
    </div>

    <div class="statusbar">
      <span v-if="cur">
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
