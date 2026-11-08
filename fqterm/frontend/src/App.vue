<script setup lang="ts">
import { ref, reactive, computed, onMounted, onBeforeUnmount, nextTick } from 'vue'
import * as api from './api'
import { TermTab } from './terminal'
import { Dialogs, Window } from '@wailsio/runtime'
import SiteDialog from './components/SiteDialog.vue'
import PrefsDialog from './components/PrefsDialog.vue'
import SchemeDialog from './components/SchemeDialog.vue'
import ZmodemDialog from './components/ZmodemDialog.vue'

interface TabMeta { id: string; title: string; status: string }

const viewport = ref<HTMLDivElement | null>(null)
const tabs = ref<TabMeta[]>([])        // reactive UI metadata only
const insts: TermTab[] = []            // raw xterm-backed instances (NOT reactive)
const byId = new Map<string, TermTab>()
const active = ref(-1)
const sidebarOpen = ref(true)
const sites = ref<api.Site[]>([])
const schemes = ref<api.ColorScheme[]>([])
const prefs = ref<api.Preferences | null>(null)
const zmodemMsg = ref('')

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
      Title: 'FQTerm',
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
  Window.IsMaximised().then((v) => { isMax.value = v }).catch(() => {})
})

onBeforeUnmount(() => { offData?.(); offStatus?.(); offZ?.(); if (zmHideTimer) clearTimeout(zmHideTimer); window.removeEventListener('resize', onWinResize) })

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
      <span class="brand">FQTerm</span>
      <button @click="sidebarOpen = !sidebarOpen" title="站点管理器">☰ 站点</button>
      <button @click="openQuickConnect" title="新建连接">＋ 连接</button>
      <button @click="uploadFile" :disabled="active < 0" title="ZMODEM 发送文件">⇧ 上传</button>
      <button @click="closeTab(active)" :disabled="active < 0" title="关闭当前标签">✕ 断开</button>
      <div class="spacer" />
      <select v-model="activeScheme" :disabled="active < 0" title="配色方案">
        <option v-for="s in schemes" :key="s.name" :value="s.name">{{ s.name }}</option>
      </select>
      <button @click="schemeDlg.show = true" title="配色方案编辑">🎨</button>
      <button @click="prefsDlg.show = true" title="偏好设置">⚙ 设置</button>
      <div class="wctl">
        <button class="wc" @click="winMin" title="最小化">&#8722;</button>
        <button class="wc" @click="winToggleMax" :title="isMax ? '还原' : '最大化'">{{ isMax ? '❐' : '□' }}</button>
        <button class="wc wc-close" @click="winClose" title="关闭">&#10005;</button>
      </div>
    </div>

    <div class="body">
      <div class="sidebar" v-show="sidebarOpen">
        <div class="side-head">
          <span>站点管理</span>
          <button class="mini" @click="openSiteEditor(-1)">＋</button>
        </div>
        <div class="tree">
          <div v-for="g in grouped()" :key="g.name" class="group">
            <div class="group-name">{{ g.name }}</div>
            <div v-for="it in g.items" :key="it.index" class="site"
                 @dblclick="newTab(it.site)" @contextmenu.prevent="openSiteEditor(it.index)">
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
          <h2>FQTerm · Wails 3</h2>
          <p>Telnet / SSH / BBS 终端</p>
          <div class="hint">点击「＋ 连接」新建会话，或在左侧双击站点开始连接</div>
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
      <span class="zmodem" v-if="zmodemMsg">{{ zmodemMsg }}</span>
    </div>

    <SiteDialog v-if="siteDlg.show" :site="siteDlg.site!" :index="siteDlg.index" :save="siteDlg.save"
                :schemes="schemes" @save="onSiteSaved" @close="siteDlg.show = false" />
    <PrefsDialog v-if="prefsDlg.show" :prefs="prefs!" @save="savePrefs" @close="prefsDlg.show = false" />
    <SchemeDialog v-if="schemeDlg.show" :schemes="schemes" @close="schemeDlg.show = false"
                  @changed="async () => { schemes = await api.ListSchemes(); applySchemeToActive() }" />
    <ZmodemDialog v-if="zm.show" :info="zm" @cancel="onZmCancel" @close="zm.show = false" />
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
.body { flex: 1; display: flex; min-height: 0; }
.sidebar { width: 260px; background: #202329; border-right: 1px solid #33373f; display: flex; flex-direction: column; }
.side-head { display: flex; justify-content: space-between; align-items: center; padding: 8px 10px; font-size: 12px; color: #8a919e; border-bottom: 1px solid #2c2f36; }
.tree { overflow-y: auto; flex: 1; }
.group-name { padding: 6px 10px 2px; font-size: 12px; color: #6a7180; font-weight: 600; }
.site { display: flex; align-items: center; gap: 6px; padding: 5px 10px; cursor: pointer; font-size: 13px; border-radius: 4px; margin: 1px 4px; }
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
</style>
