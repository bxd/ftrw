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
const kmDlg = reactive({ show: false })
const echoMsg = ref('')
const echoKind = ref<EchoInfo['kind']>('info')
let echoTimer: number | undefined

/** echo area：对应 Emacs 底部临时消息区，pending 不自动消失（由下一个事件接管）。 */
function showEcho(text: string, kind: EchoInfo['kind'] = 'info') {
  echoMsg.value = text
  echoKind.value = kind
  if (echoTimer) { clearTimeout(echoTimer); echoTimer = undefined }
  if (kind !== 'pending') echoTimer = window.setTimeout(() => { echoMsg.value = '' }, 4000)
}

function applyKeymap(c: api.KeymapConfig) {
  km.value = c
  keyseq.configure({ enabled: c.enabled, timeoutMs: c.timeoutMs, echo: c.echo })
  keyseq.load(c.bindings || [])
}

/**
 * 全局作用域入口（window 捕获阶段）。焦点在终端时直接返回，
 * 留给 xterm 的 customKeyEventHandler 按 'terminal' 作用域处理，避免同一键被查两次。
 */
function onGlobalKey(ev: KeyboardEvent) {
  const el = document.activeElement
  if (el && viewport.value && viewport.value.contains(el)) return
  if (keyseq.feed(ev, 'global') === 'swallow') {
    ev.preventDefault()
    ev.stopPropagation()
  }
}

keyseq.onEcho = (info: EchoInfo) => showEcho(info.message, info.kind)

registerBuiltinCommands({
  openEditor: () => { kmDlg.show = true },
  echo: (m, k) => showEcho(m, k || 'info'),
  activeSessionId: () => insts[active.value]?.id || '',
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

async function onKeymapSave(c: api.KeymapConfig) {
  try { await api.SaveKeymap(c) } catch (e) { showEcho(`保存绑定表失败：${e}`, 'error'); return }
  // 必须先等编辑器卸载：它在挂载期间把引擎强制置为静默，卸载时才恢复，
  // 若先 applyKeymap 再卸载，刚保存的开关会被旧值覆盖回去。
  kmDlg.show = false
  await nextTick()
  applyKeymap(c)
  showEcho(`键序列已保存：${c.bindings.length} 条绑定${c.enabled ? '' : '（引擎当前停用）'}`, c.enabled ? 'info' : 'warn')
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

  const tab = new TermTab(site, container)
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

function setActive(i: number) {
  active.value = i
  insts.forEach((t, idx) => { t.container.style.display = idx === i ? 'block' : 'none' })
  nextTick(() => { const t = insts[i]; if (t) { t.fitNow(); t.focus() } })
}

async function closeTab(i: number) {
  const t = insts[i]
  if (!t) return
  if (t.status === 'connected' && prefs.value?.confirmClose) {
    const r = await Dialogs.Question({
      Title: 'FTRW',
      Message: `断开 “${t.title}” ？`,
      Buttons: [{ Label: '断开', IsDefault: true }, { Label: '取消', IsCancel: true }],
    })
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

function grouped(): { name: string; items: { site: api.Site; index: number }[] }[] {
  const map = new Map<string, { site: api.Site; index: number }[]>()
  sites.value.forEach((site, index) => {
    const g = site.group || '默认分组'
    if (!map.has(g)) map.set(g, [])
    map.get(g)!.push({ site, index })
  })
  return [...map.entries()].map(([name, items]) => ({ name, items }))
}

async function uploadFile() {
  const t = insts[active.value]
  if (!t || !t.id) return
  try {
    const picked = await Dialogs.OpenFile({ Title: '选择要发送的文件', CanChooseFiles: true, AllowsMultipleSelection: true })
    if (!picked) return
    const paths = Array.isArray(picked) ? picked : [picked]
    await api.SendFile(t.id, paths)
  } catch { /* cancelled or unsupported */ }
}

let offData: () => void, offStatus: () => void, offZ: () => void

onMounted(async () => {
  prefs.value = await api.GetPreferences()
  await refreshSites()
  schemes.value = await api.ListSchemes()

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
  window.addEventListener('resize', onWinResize)
  window.addEventListener('blur', onWindowBlur)
  try { applyKeymap(await api.GetKeymap()) } catch { /* 读不到就用出厂默认表 */ }
  window.addEventListener('keydown', onGlobalKey, true)
  Window.IsMaximised().then((v) => { isMax.value = v }).catch(() => {})
})

onBeforeUnmount(() => { offData?.(); offStatus?.(); offZ?.(); if (zmHideTimer) clearTimeout(zmHideTimer); if (echoTimer) clearTimeout(echoTimer); clearSideHide(); window.removeEventListener('resize', onWinResize); window.removeEventListener('blur', onWindowBlur); window.removeEventListener('keydown', onGlobalKey, true) })

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

async function savePrefs(p: api.Preferences) { await api.SetPreferences(p); prefs.value = p; prefsDlg.show = false }

const cur = computed(() => (active.value >= 0 ? { meta: tabs.value[active.value], inst: insts[active.value] } : null))
</script>

<template>
  <div class="app">
    <div class="toolbar">
      <span class="brand">FTRW</span>
      <button @click="toggleSidebar" :title="sidebarShown ? '隐藏站点栏（Alt+X P）' : '显示站点栏（Alt+X P，或鼠标移到窗口左缘）'">☰ 站点</button>
      <button @click="openQuickConnect" title="新建连接">＋ 连接</button>
      <button @click="uploadFile" :disabled="active < 0" title="ZMODEM 发送文件">⇧ 上传</button>
      <button @click="closeTab(active)" :disabled="active < 0" title="关闭当前标签">✕ 断开</button>
      <div class="spacer" />
      <select v-model="activeScheme" :disabled="active < 0" title="配色方案">
        <option v-for="s in schemes" :key="s.name" :value="s.name">{{ s.name }}</option>
      </select>
      <button @click="schemeDlg.show = true" title="配色方案编辑">🎨</button>
      <button @click="kmDlg.show = true" title="键序列 / 热键编辑器（Emacs 风格）">⌨ 热键</button>
      <button @click="prefsDlg.show = true" title="偏好设置">⚙ 设置</button>
      <div class="wctl">
        <button class="wc" @click="winMin" title="最小化">&#8722;</button>
        <button class="wc" @click="winToggleMax" :title="isMax ? '还原' : '最大化'">{{ isMax ? '❐' : '□' }}</button>
        <button class="wc wc-close" @click="winClose" title="关闭">&#10005;</button>
      </div>
    </div>

    <div class="body" @mousemove="onBodyMove" @mouseleave="onBodyLeave">
      <!-- 浮动侧栏：收起时整块滑到左缘外，不占据布局 → 终端视口宽度不变 -->
      <div class="sidebar" ref="sidebarEl" tabindex="-1" :style="{ transform: sidebarTransform }"
           @click.self="focusSidebar" @focusin="sidebarFocused = true" @focusout="onSidebarFocusOut"
           @keydown="onSidebarKey">
        <div class="side-head">
          <span>站点管理<i class="tip">↑↓ 选择 · Enter 连接</i></span>
          <button class="mini" @click="openSiteEditor(-1)">＋</button>
        </div>
        <div class="tree">
          <div v-for="g in grouped()" :key="g.name" class="group">
            <div class="group-name">{{ g.name }}</div>
            <div v-for="it in g.items" :key="it.index" class="site"
                 :data-idx="it.index" :tabindex="sidebarShown ? 0 : -1"
                 @dblclick="newTab(it.site)"
                 @contextmenu.prevent="openSiteEditor(it.index)">
              <span class="dot" :class="it.site.protocolType === 0 ? 'tel' : 'ssh'"></span>
              <span class="sname">{{ it.site.name || it.site.host }}</span>
              <span class="sproto">{{ api.PROTOCOLS[it.site.protocolType] }}</span>
              <span class="acts">
                <button class="mini" @click.stop="openSiteEditor(it.index)">✎</button>
                <button class="mini" @click.stop="removeSite(it.index)">🗑</button>
              </span>
            </div>
          </div>
          <div v-if="!sites.length" class="empty">暂无站点，点击左上「＋」添加</div>
        </div>
      </div>

      <div class="main">
        <div class="tabbar" v-if="tabs.length">
          <div v-for="(t, i) in tabs" :key="i" class="tab" :class="{ on: i === active }" @click="setActive(i)">
            <span class="tdot" :class="t.status"></span>
            <span class="ttitle" :title="t.title">{{ t.title }}</span>
            <span class="tclose" @click.stop="closeTab(i)">✕</span>
          </div>
        </div>
        <div class="viewport" ref="viewport" />
        <div v-if="!tabs.length" class="welcome">
          <h2>FTRW · Wails 3</h2>
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
    <KeymapDialog v-if="kmDlg.show" :km="km" @save="onKeymapSave" @close="kmDlg.show = false" />
  </div>
</template>

<style scoped>
.app { display: flex; flex-direction: column; height: 100%; width: 100%; overflow: hidden; background: #1b1d22; color: #c8ccd4; font-family: system-ui, "Segoe UI", sans-serif; }
.toolbar { display: flex; align-items: center; gap: 6px; padding: 6px 10px; background: #24272e; border-bottom: 1px solid #33373f; --wails-draggable: drag; }
.toolbar button, .toolbar select, .wctl { --wails-draggable: no-drag; }
.brand { font-weight: 700; color: #6ab0f3; margin-right: 12px; letter-spacing: 1px; }
.toolbar button, .toolbar select { background: #2f333b; color: #c8ccd4; border: 1px solid #3d424b; border-radius: 5px; padding: 4px 10px; cursor: pointer; font-size: 13px; }
.toolbar button:hover:not(:disabled) { background: #3a3f49; }
.toolbar button:disabled { opacity: .4; cursor: default; }
.spacer { flex: 1; }
.wctl { display: flex; gap: 2px; margin-left: 8px; }
.wctl .wc { width: 32px; height: 26px; padding: 0; line-height: 1; font-size: 14px; border-radius: 4px; background: transparent; border: 1px solid transparent; color: #c8ccd4; }
.wctl .wc:hover { background: #3a3f49 !important; border-color: #3d424b; }
.wctl .wc-close:hover { background: #e05c5c !important; border-color: #e05c5c !important; color: #fff !important; }
.body { flex: 1; display: flex; min-height: 0; position: relative; }
/* 浮动态：脱离文档布局覆盖在终端上，收起时整块滑出左边缘（不挤压视口，终端尺寸不变） */
.sidebar { position: absolute; left: 0; top: 0; bottom: 0; width: 260px; z-index: 20; background: #202329f2; border-right: 1px solid #33373f; box-shadow: 6px 0 18px rgba(0, 0, 0, .45); display: flex; flex-direction: column; transform: translateX(-260px); transition: transform .16s ease-out; outline: none; }
.side-head { display: flex; justify-content: space-between; align-items: center; padding: 8px 10px; font-size: 12px; color: #8a919e; border-bottom: 1px solid #2c2f36; }
.side-head .tip { font-style: normal; margin-left: 8px; color: #5d6472; font-size: 11px; }
.tree { overflow-y: auto; flex: 1; }
.group-name { padding: 6px 10px 2px; font-size: 12px; color: #6a7180; font-weight: 600; }
.site { display: flex; align-items: center; gap: 6px; padding: 5px 10px; cursor: pointer; font-size: 13px; border-radius: 4px; margin: 1px 4px; }
.site:focus { background: #2f3644; outline: 1px solid #4a6b96; outline-offset: -1px; }
.site:focus .sname { color: #fff; }
.site:hover { background: #2b2f37; }
.dot { width: 8px; height: 8px; border-radius: 50%; }
.dot.tel { background: #4ec9b0; } .dot.ssh { background: #6ab0f3; }
.sname { flex: 1; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.sproto { font-size: 11px; color: #6a7180; }
.acts { display: none; gap: 2px; } .site:hover .acts { display: flex; }
.mini { padding: 0 5px !important; font-size: 11px !important; border: none !important; background: transparent !important; color: #8a919e !important; }
.mini:hover { color: #fff !important; }
.empty { padding: 20px; text-align: center; color: #555a64; font-size: 12px; }
.main { flex: 1; display: flex; flex-direction: column; min-width: 0; position: relative; }
.tabbar { display: flex; background: #202329; border-bottom: 1px solid #33373f; overflow-x: auto; }
.tab { display: flex; align-items: center; gap: 6px; padding: 6px 12px; cursor: pointer; border-right: 1px solid #2c2f36; font-size: 13px; max-width: 200px; }
.tab.on { background: #1b1d22; color: #fff; }
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
