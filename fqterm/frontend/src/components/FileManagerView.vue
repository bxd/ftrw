<script setup lang="ts">
// Total Commander 式双栏文件管理器标签（launcher 分支，M-x e 呼出）。
//
// 形状照 TC：上方驱动器栏，中间两栏并列、各带路径行 + 可排序表头 + 列表，
// Tab 切栏（活动蓝框 / 非活动橙框），底部状态行。键盘是全功能主路：
// 方向键移动、Enter 打开/进入、Backspace 回上级、Insert 多选、F5 复制 / F6 移动 /
// F7 新建目录 / F8 删除 / F2 重命名 / F3·Space 预览。
//
// 与 App.vue 的 M-x 键序列框架共存：裸方向键/Tab/Enter/Fn/Insert/Backspace 都不在
// 出厂序列里（那些要 M-x 前缀或 Shift+方向），keyseq 判 pass 放行到这里；输入浮层聚焦
// 时 isTextEditing 为真、这里也让路，所以两条路不打架。
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import * as api from '../api'
import { initials, nameMatches } from '../pinyin'

const props = defineProps<{ initialPath?: string }>()

interface Row {
  name: string
  path: string
  size: number
  mtime: number
  isDir: boolean
  hidden: boolean
  special?: 'up' | 'dot'
}

interface Pane {
  path: string
  rows: Row[]
  allRows: Row[]      // 未过滤的排序实体行（不含 . / ..）；rows 由它 + filter 派生
  filter: string      // 敲字母过滤串（字面或拼音首字母）
  editingPath: boolean // 地址栏原地编辑中
  cursor: number
  anchor: number      // Shift 连续多选的锚点（范围选择从这里算起）
  sel: Set<string>
  sortKey: 'name' | 'ext' | 'size' | 'mtime'
  sortAsc: boolean
  showHidden: boolean
  loading: boolean
  error: string
}

const root = ref<HTMLDivElement | null>(null)
const active = ref(0)
const pathDraft = ref('')      // 地址栏原地编辑的草稿（一次只编一栏）
// 地址栏自动完成：候选住在下拉里，base（最后一个分隔符前的目录）变了才回盘取。
const acOpen = ref(false)
const acItems = ref<{ name: string; isDir: boolean }[]>([])
const acIndex = ref(-1)
let acBase = ''
let acCache: { name: string; isDir: boolean }[] = []
const drives = ref<api.DriveInfo[]>([])
const status = ref('')
const plugins = ref<api.PluginTool[]>([])
const menuOpen = ref(false)
const input = reactive({ show: false, title: '', value: '', kind: '' as '' | 'mkdir' | 'rename', target: '' })
const confirmDel = reactive({ show: false, paths: [] as string[], label: '', permanent: false })
const viewer = reactive({ show: false, title: '', text: '', path: '' })

function mkPane(path: string): Pane {
  return { path, rows: [], allRows: [], filter: '', editingPath: false, cursor: 0, anchor: 0, sel: new Set(), sortKey: 'name', sortAsc: true, showHidden: false, loading: false, error: '' }
}
const panes = reactive<Pane[]>([mkPane(''), mkPane('')])
const other = computed(() => (active.value === 0 ? 1 : 0))

/* ---------- 路径工具 ---------- */

function atRoot(p: string): boolean {
  const q = p.replace(/[\\/]+$/, '')
  if (/^[A-Za-z]:$/.test(q)) return true
  return q === ''
}

function parentOf(p: string): string {
  if (atRoot(p)) return p
  const q = p.replace(/[\\/]+$/, '')
  const i = Math.max(q.lastIndexOf('\\'), q.lastIndexOf('/'))
  const sep = i >= 0 && q[i] === '\\' ? '\\' : '/'
  if (i <= 0) {
    const dm = q.match(/^([A-Za-z]:)/)
    return dm ? dm[1] + '\\' : sep
  }
  const base = q.slice(0, i)
  if (/^[A-Za-z]:$/.test(base)) return base + '\\'
  if (base === '') return sep
  return base
}

function joinPath(dir: string, name: string): string {
  if (/[\\:]$/.test(dir)) return dir + name
  return dir + '\\' + name // 展示无妨，后端按 OS 解析；实际新建都传绝对全路径
}

/* ---------- 加载与排序 ---------- */

function extOf(name: string): string {
  const i = name.lastIndexOf('.')
  return i > 0 ? name.slice(i + 1).toLowerCase() : ''
}

function sortRows(list: Row[], pane: Pane): Row[] {
  const dirs = list.filter((r) => r.isDir)
  const files = list.filter((r) => !r.isDir)
  const cmp = (a: Row, b: Row): number => {
    let d = 0
    switch (pane.sortKey) {
      case 'name': d = a.name.localeCompare(b.name, 'zh'); break
      case 'ext': d = extOf(a.name).localeCompare(extOf(b.name)) || a.name.localeCompare(b.name, 'zh'); break
      case 'size': d = a.size - b.size; break
      case 'mtime': d = a.mtime - b.mtime; break
    }
    return pane.sortAsc ? d : -d
  }
  dirs.sort(cmp)
  files.sort(cmp)
  return [...dirs, ...files]
}

async function loadPane(idx: number, path: string, keepName = '') {
  const pane = panes[idx]
  pane.loading = true
  pane.error = ''
  try {
    const entries = await api.FileListDir(path)
    const real: Row[] = entries
      .filter((e) => pane.showHidden || !e.hidden)
      .map((e) => ({ name: e.name, path: e.path, size: e.size, mtime: e.mtime, isDir: e.isDir, hidden: e.hidden }))
    pane.path = path
    pane.allRows = sortRows(real, pane)
    pane.filter = ''
    applyFilter(pane)
    pane.sel = new Set()
    // 尽量把光标停回原来在看的那一项（切栏 / 刷新后位置不飘）。
    let ci = 0
    if (keepName) {
      const found = pane.rows.findIndex((r) => r.name === keepName)
      if (found >= 0) ci = found
    }
    pane.cursor = ci
    pane.anchor = ci
  } catch (e: any) {
    pane.error = String(e?.message || e)
    status.value = pane.error
  } finally {
    pane.loading = false
    if (idx === active.value) scrollCursor()
  }
}

// 由 allRows + filter 派生当前可见的 rows：无过滤时带 . / .. 头，过滤时只留命中实体行。
// rows 仍是唯一的光标/选择索引基准，所以下游 moveCursor/activate 等一律不必改。
function applyFilter(pane: Pane) {
  if (pane.filter) {
    pane.rows = pane.allRows.filter((r) => nameMatches(r.name, pane.filter))
  } else {
    const head: Row[] = [{ name: '.', path: pane.path, size: 0, mtime: 0, isDir: true, hidden: false, special: 'dot' }]
    if (!atRoot(pane.path)) head.push({ name: '..', path: parentOf(pane.path), size: 0, mtime: 0, isDir: true, hidden: false, special: 'up' })
    pane.rows = [...head, ...pane.allRows]
  }
  if (pane.cursor >= pane.rows.length) pane.cursor = Math.max(0, pane.rows.length - 1)
}

// 敲字母过滤：改串后重派 rows，光标归到首个命中项。
function setFilter(pane: Pane, q: string) {
  pane.filter = q
  applyFilter(pane)
  pane.cursor = 0
  scrollCursor()
}

function reloadActive() {
  const p = panes[active.value]
  return loadPane(active.value, p.path, p.rows[p.cursor]?.name || '')
}

function resort(pane: Pane) {
  const keep = pane.rows[pane.cursor]?.name || ''
  pane.allRows = sortRows(pane.allRows, pane)
  applyFilter(pane)
  const found = keep ? pane.rows.findIndex((r) => r.name === keep) : -1
  pane.cursor = found >= 0 ? found : 0
  scrollCursor()
}

// 点表头：先切到那一栏再排序；同列翻转升降序，换列默认升序。数据已在内存，不必回盘。
function sortKeyBy(idx: number, key: Pane['sortKey']) {
  active.value = idx
  const pane = panes[idx]
  if (pane.sortKey === key) pane.sortAsc = !pane.sortAsc
  else { pane.sortKey = key; pane.sortAsc = true }
  resort(pane)
  root.value?.focus()
}

function switchPaneTo(idx: number) {
  active.value = idx
  root.value?.focus()
  scrollCursor()
}

// 点栏空白 / 点行：模板里不能写 if 语句，逻辑挪到脚本。
// 点某栏先把活动切过去；点行时同时把该栏光标移到被点的行。
function paneClick(idx: number) {
  if (idx !== active.value) switchPaneTo(idx)
}

function rowClick(idx: number, ri: number, e?: MouseEvent) {
  const pane = panes[idx]
  if (e?.ctrlKey || e?.metaKey) {
    // Ctrl/⌘ + 左键：不连续多选，把这一项加入/移出选区，不影响其它已选项。
    const r = pane.rows[ri]
    if (r && !r.special) {
      if (pane.sel.has(r.path)) pane.sel.delete(r.path)
      else pane.sel.add(r.path)
    }
    pane.cursor = ri
    pane.anchor = ri
  } else if (e?.shiftKey) {
    // Shift + 左键：从锚点到点中的行连续多选。
    pane.cursor = ri
    selectRange(pane, pane.anchor, ri)
  } else {
    // 普通点击（修饰键已放开）：取消多选，光标/锚点落到该行。
    pane.sel.clear()
    pane.cursor = ri
    pane.anchor = ri
  }
  if (idx !== active.value) switchPaneTo(idx)
}

// 隐藏档切换：连带动作放进脚本，模板里只留一个调用。
function toggleHidden() {
  const p = panes[active.value]
  p.showHidden = !p.showHidden
  reloadActive()
}

// 盘符显示去掉尾随反斜杠（"C:\\" → "C:"）。
function driveLetter(d: api.DriveInfo): string {
  return d.root.replace(/[\\/]+$/, '')
}

/* ---------- 光标 / 选择 ---------- */

function curRow(pane: Pane): Row | null {
  return pane.rows[pane.cursor] ?? null
}

function selectedPaths(pane: Pane): string[] {
  const real = pane.rows.filter((r) => !r.special && pane.sel.has(r.path)).map((r) => r.path)
  if (real.length) return real
  const c = curRow(pane)
  return c && !c.special ? [c.path] : []
}

function moveCursor(d: number, keepSel = false) {
  const pane = panes[active.value]
  if (!pane.rows.length) return
  pane.cursor = Math.max(0, Math.min(pane.rows.length - 1, pane.cursor + d))
  pane.anchor = pane.cursor
  if (!keepSel) pane.sel.clear() // 放开修饰键后的普通移动/翻页取消多选（Insert 逐个累加除外）
  scrollCursor()
}

// 选中 [from,to] 闭区间内的实体行（跳过 . / ..）；作为 Shift 连续多选的原子操作。
function selectRange(pane: Pane, from: number, to: number) {
  pane.sel.clear()
  const a = Math.max(0, Math.min(from, to))
  const b = Math.min(pane.rows.length - 1, Math.max(from, to))
  for (let i = a; i <= b; i++) {
    const r = pane.rows[i]
    if (r && !r.special) pane.sel.add(r.path)
  }
}

// Shift + 方向键：移动光标但锚点不动，从锚点到新光标连续选中。
function moveCursorExt(d: number) {
  const pane = panes[active.value]
  if (!pane.rows.length) return
  pane.cursor = Math.max(0, Math.min(pane.rows.length - 1, pane.cursor + d))
  selectRange(pane, pane.anchor, pane.cursor)
  scrollCursor()
}

function scrollCursor() {
  nextTick(() => {
    const el = root.value?.querySelector(`#fm-row-${active.value}-${panes[active.value].cursor}`)
    el?.scrollIntoView({ block: 'nearest' })
  })
}

function toggleSelect() {
  const pane = panes[active.value]
  const r = curRow(pane)
  if (!r || r.special) return
  if (pane.sel.has(r.path)) pane.sel.delete(r.path)
  else pane.sel.add(r.path)
  moveCursor(1, true)
}

function selectAll() {
  const pane = panes[active.value]
  if (pane.sel.size) { pane.sel.clear(); return }
  for (const r of pane.rows) if (!r.special) pane.sel.add(r.path)
}

/* ---------- 导航动作 ---------- */

function activate(row: Row | null) {
  if (!row) return
  if (row.special === 'up') { void loadPane(active.value, row.path, ''); return }
  if (row.special === 'dot') { void reloadActive(); return }
  if (row.isDir) { void loadPane(active.value, row.path, ''); return }
  void api.FileOpenPath(row.path).catch((e) => { status.value = String(e?.message || e) })
}

function goUp() {
  const pane = panes[active.value]
  if (atRoot(pane.path)) return
  void loadPane(active.value, parentOf(pane.path), '')
}

async function goHome() {
  const home = await api.FileHome().catch(() => props.initialPath || '')
  void loadPane(active.value, home || props.initialPath || '', '')
}

function switchPane() {
  active.value = other.value
  root.value?.focus()
  scrollCursor()
}

/* ---------- 文件操作 ---------- */

async function doCopy() {
  const src = selectedPaths(panes[active.value])
  const dst = panes[other.value].path
  if (!src.length) return
  try {
    await api.FileCopy(src, dst)
    status.value = `已复制 ${src.length} 项 → ${dst}`
    await Promise.all([reloadPaneKeep(active.value), reloadPaneKeep(other.value)])
  } catch (e: any) { status.value = String(e?.message || e) }
}

async function doMove() {
  const src = selectedPaths(panes[active.value])
  const dst = panes[other.value].path
  if (!src.length) return
  try {
    await api.FileMove(src, dst)
    status.value = `已移动 ${src.length} 项 → ${dst}`
    await Promise.all([reloadPaneKeep(active.value), reloadPaneKeep(other.value)])
  } catch (e: any) { status.value = String(e?.message || e) }
}

async function reloadPaneKeep(idx: number) {
  const p = panes[idx]
  await loadPane(idx, p.path, p.rows[p.cursor]?.name || '')
}

function askMkdir() {
  input.show = true
  input.kind = 'mkdir'
  input.title = '新建目录（在当前活动栏）'
  input.value = '新建文件夹'
  input.target = panes[active.value].path
  nextTick(() => root.value?.querySelector<HTMLElement>('.dlg-input')?.focus())
}

function askRename() {
  const r = curRow(panes[active.value])
  if (!r || r.special) return
  input.show = true
  input.kind = 'rename'
  input.title = `重命名：${r.name}`
  input.value = r.name
  input.target = r.path
  nextTick(() => root.value?.querySelector<HTMLElement>('.dlg-input')?.focus())
}

// 地址栏原地编辑（M-x d 与双击 pathbar 都走这里）：把该栏 pathbar 换成一个内联
// 输入框，聚焦并全选。带自动完成下拉（见下）。Enter 提交跳转、Esc 取消、失焦也提交。
function startPathEdit(idx: number) {
  active.value = idx
  panes[idx].editingPath = true
  pathDraft.value = panes[idx].path
  acBase = ''
  acItems.value = []
  acIndex.value = -1
  acOpen.value = false
  nextTick(() => {
    const el = root.value?.querySelector<HTMLInputElement>(`#fm-path-${idx}`)
    el?.focus()
    el?.select()
    void refreshAc()
  })
}

function commitPathEdit(idx: number) {
  const pane = panes[idx]
  if (!pane.editingPath) return // Enter 后紧接的 blur 不重复提交
  const val = pathDraft.value.trim()
  pane.editingPath = false
  acOpen.value = false
  if (val && val !== pane.path) void loadPane(idx, val, '')
  root.value?.focus()
}

function cancelPathEdit(idx: number) {
  panes[idx].editingPath = false
  acOpen.value = false
  root.value?.focus()
}

/* ---------- 地址栏自动完成 ---------- */

// 把草稿拆成 base（最后一个分隔符前、含分隔符）+ frag（正在敲的末段）。
// 无分隔符时 base 为空串，FileListDir('') 回主目录，故光敲名字也能补全主目录内容。
function splitBase(draft: string): { base: string; frag: string; sep: string } {
  let idx = -1
  for (let i = draft.length - 1; i >= 0; i--) {
    const c = draft[i]
    if (c === '\\' || c === '/') { idx = i; break }
  }
  const sep = idx >= 0 && draft[idx] === '/' ? '/' : '\\'
  const base = idx >= 0 ? draft.slice(0, idx + 1) : ''
  const frag = idx >= 0 ? draft.slice(idx + 1) : draft
  return { base, frag, sep }
}

// 根据当前草稿刷新候选：base 变了才回盘取目录（否则只重过滤缓存），frag 用字面/拼音首字母前缀筛。
async function refreshAc() {
  const { base, frag } = splitBase(pathDraft.value)
  if (base !== acBase) {
    acBase = base
    let entries: api.FileEntry[] = []
    try { entries = await api.FileListDir(base) } catch { entries = [] }
    // 取盘期间草稿又改了（base 已不是本次）→ 丢弃过期结果。
    if (splitBase(pathDraft.value).base !== base) return
    acCache = entries.filter((e) => !e.hidden).map((e) => ({ name: e.name, isDir: e.isDir }))
  }
  const lf = frag.toLowerCase()
  acItems.value = lf
    ? acCache.filter((e) => e.name.toLowerCase().startsWith(lf) || initials(e.name).toLowerCase().startsWith(lf)).slice(0, 40)
    : acCache.slice(0, 40)
  acIndex.value = acItems.value.length ? 0 : -1
  acOpen.value = acItems.value.length > 0
}

function onPathInput(e: Event) {
  pathDraft.value = (e.target as HTMLInputElement).value
  void refreshAc()
}

function moveAc(d: number) {
  if (!acOpen.value || !acItems.value.length) return
  const n = acItems.value.length
  acIndex.value = (acIndex.value + d + n) % n
}

// 采纳高亮候选：目录则补全并追加分隔符、停在编辑态继续往里选；文件补全后由用户再按 Enter 跳转。
function acceptAc(idx: number) {
  if (!acOpen.value || acIndex.value < 0) return
  const item = acItems.value[acIndex.value]
  if (!item) return
  const { base, sep } = splitBase(pathDraft.value)
  pathDraft.value = base + item.name + (item.isDir ? sep : '')
  acBase = ''
  void refreshAc()
  const el = root.value?.querySelector<HTMLInputElement>(`#fm-path-${idx}`)
  el?.focus()
}

function onPathEnter(idx: number) {
  // Enter 永远跳转到地址栏当前路径（哪怕下拉开着、首项已高亮）；补全只用 Tab / 点候选。
  // 否则像 f:\down\ 这种“已是完整目录”的输入会被误当成去采纳第一条候选。
  commitPathEdit(idx)
}

function onPathEsc(idx: number) {
  if (acOpen.value) { acOpen.value = false; return }
  cancelPathEdit(idx)
}

async function commitInput() {
  const val = input.value.trim()
  if (!val) { closeInput(); return }
  try {
    if (input.kind === 'mkdir') {
      await api.FileMkdir(joinPath(input.target, val))
      status.value = `已新建目录：${val}`
      await reloadPaneKeep(active.value)
    } else if (input.kind === 'rename') {
      const dir = parentOf(input.target)
      const newPath = joinPath(dir, val)
      await api.FileRename(input.target, newPath)
      status.value = `已重命名 → ${val}`
      await reloadPaneKeep(active.value)
    }
  } catch (e: any) { status.value = String(e?.message || e) }
  closeInput()
}

function closeInput() {
  input.show = false
  input.kind = ''
  input.value = ''
  input.target = ''
  root.value?.focus()
}

function askDelete(permanent = false) {
  const paths = selectedPaths(panes[active.value])
  if (!paths.length) return
  confirmDel.paths = paths
  confirmDel.label = paths.length === 1 ? paths[0] : `${paths.length} 项`
  confirmDel.permanent = permanent
  confirmDel.show = true
}

async function commitDelete() {
  const paths = confirmDel.paths
  const permanent = confirmDel.permanent
  try {
    // Del → 回收站（FileRecycle）；Shift+Del / F8 → 永久删除（FileDelete）。
    if (permanent) await api.FileDelete(paths)
    else await api.FileRecycle(paths)
    status.value = `${permanent ? '已永久删除' : '已移入回收站'} ${paths.length} 项`
    await Promise.all([reloadPaneKeep(active.value), loadPane(other.value, panes[other.value].path, '')])
  } catch (e: any) { status.value = String(e?.message || e) }
  confirmDel.show = false
  confirmDel.paths = []
  root.value?.focus()
}

const IMG_EXT = new Set(['png', 'jpg', 'jpeg', 'gif', 'bmp', 'webp', 'ico', 'tif', 'tiff'])

async function view(quick = false) {
  const r = curRow(panes[active.value])
  if (!r || r.isDir) return
  if (IMG_EXT.has(extOf(r.name)) || r.size > (4 << 20)) {
    // 图片或超大文件：交给系统默认程序，别把字节灌进文本框。
    void api.FileOpenPath(r.path).catch((e) => { status.value = String(e?.message || e) })
    return
  }
  try {
    const text = await api.FilePeek(r.path, quick ? 16384 : 65536)
    viewer.path = r.path
    viewer.title = r.name
    viewer.text = text || '（空文件）'
    viewer.show = true
  } catch (e: any) { status.value = String(e?.message || e) }
}

/* ---------- 扩展（Python 插件，本轮恒空） ---------- */

async function loadPlugins() {
  try { plugins.value = await api.FilePlugins() } catch { plugins.value = [] }
}

async function runPlugin(tool: api.PluginTool) {
  const pane = panes[active.value]
  const ctx: api.PluginContext = { activeDir: pane.path, selected: selectedPaths(pane), targetDir: panes[other.value].path }
  menuOpen.value = false
  try {
    const r = await api.FileRunPlugin(tool.id, ctx)
    status.value = r.message || `${tool.title} 完成`
  } catch (e: any) { status.value = String(e?.message || e) }
}

/* ---------- 键盘分发 ---------- */

function onKeydown(e: KeyboardEvent) {
  if (e.isComposing) return
  // 焦点在任何输入控件（地址栏原地编辑 / 浮层输入框）里 → 按键属于打字，
  // 不能落到下面的过滤/导航分发（否则编路径时敲字母会被当成过滤串）。
  const tag = (e.target as HTMLElement | null)?.tagName
  if (tag === 'INPUT' || tag === 'TEXTAREA') return
  // Esc 逐级关浮层/菜单（输入框自己聚焦时已先一步消费，这里兜到 root 兼顾未聚焦的角落）。
  if (e.key === 'Escape') {
    if (input.show) { e.preventDefault(); closeInput(); return }
    if (viewer.show) { e.preventDefault(); viewer.show = false; root.value?.focus(); return }
    if (confirmDel.show) { e.preventDefault(); confirmDel.show = false; root.value?.focus(); return }
    if (menuOpen.value) { menuOpen.value = false; return }
    if (panes[active.value].filter) { e.preventDefault(); setFilter(panes[active.value], ''); return }
  }
  // 其余浮层 / 菜单在场时整体让路。
  if (input.show || confirmDel.show || viewer.show || menuOpen.value) return
  const pane = panes[active.value]
  switch (e.key) {
    case 'ArrowDown': e.preventDefault(); if (e.shiftKey) moveCursorExt(1); else moveCursor(1); break
    case 'ArrowUp': e.preventDefault(); if (e.shiftKey) moveCursorExt(-1); else moveCursor(-1); break
    case 'PageDown': e.preventDefault(); if (e.shiftKey) moveCursorExt(15); else moveCursor(15); break
    case 'PageUp': e.preventDefault(); if (e.shiftKey) moveCursorExt(-15); else moveCursor(-15); break
    case 'Tab': e.preventDefault(); switchPane(); break
    case 'Enter': e.preventDefault(); activate(curRow(pane)); break
    case 'Backspace':
      if (pane.filter) { e.preventDefault(); setFilter(pane, pane.filter.slice(0, -1)) }
      else { e.preventDefault(); goUp() }
      break
    case 'Home':
      if (e.ctrlKey) { e.preventDefault(); void goHome() }
      else { e.preventDefault(); pane.cursor = 0; scrollCursor() }
      break
    case 'End': e.preventDefault(); pane.cursor = Math.max(0, pane.rows.length - 1); scrollCursor(); break
    case 'Insert': e.preventDefault(); toggleSelect(); break
    case ' ': e.preventDefault(); view(true); break
    case '~': e.preventDefault(); void goHome(); break
    case 'Delete': e.preventDefault(); askDelete(e.shiftKey); break
    case 'F2': e.preventDefault(); askRename(); break
    case 'F3': e.preventDefault(); view(false); break
    case 'F5': e.preventDefault(); void doCopy(); break
    case 'F6': e.preventDefault(); void doMove(); break
    case 'F7': e.preventDefault(); askMkdir(); break
    case 'F8': e.preventDefault(); askDelete(true); break
    default:
      if ((e.ctrlKey || e.metaKey) && (e.key === 'a' || e.key === 'A')) { e.preventDefault(); selectAll(); break }
      // 敲字母/数字追进过滤串（大小写不敏感，支持拼音首字母）；带修饰键的组合不吞。
      if (!e.ctrlKey && !e.altKey && !e.metaKey && /^[a-zA-Z0-9]$/.test(e.key)) {
        e.preventDefault()
        setFilter(pane, pane.filter + e.key)
      }
  }
}

async function onDrive(d: api.DriveInfo) {
  void loadPane(active.value, d.root, '')
}

function refreshDrives() {
  api.FileDrives().then((v) => { drives.value = v }).catch(() => {})
}

/* ---------- 展示辅助 ---------- */

function fmtSize(r: Row): string {
  if (r.special) return ''
  if (r.isDir) return '<Dir>'
  const n = r.size
  if (n < 1024) return `${n} B`
  if (n < 1048576) return `${(n / 1024).toFixed(1)} K`
  if (n < 1073741824) return `${(n / 1048576).toFixed(1)} M`
  return `${(n / 1073741824).toFixed(2)} G`
}

function fmtDate(r: Row): string {
  if (r.special || !r.mtime) return ''
  return new Date(Math.round(r.mtime / 1e6)).toLocaleString('zh-CN', { hour12: false })
}

function rowType(r: Row): string {
  if (r.special) return ''
  if (r.isDir) return 'DIR'
  const e = extOf(r.name)
  return e ? e.toUpperCase() : 'FILE'
}

/* ---------- 生命周期 ---------- */

// M-x d 从 App.vue 以 CustomEvent 广播过来：只有当前可见（活动）的文件管理器
// 标签响应（后台标签容器 display:none → offsetParent 为 null），避免多标签一起弹。
function onEditPathEvent() {
  if (!root.value || root.value.offsetParent === null) return
  startPathEdit(active.value)
}

onMounted(async () => {
  window.addEventListener('ftrw:fm-edit-path', onEditPathEvent)
  refreshDrives()
  void loadPlugins()
  let start = props.initialPath || ''
  if (!start) start = await api.FileHome().catch(() => '')
  await Promise.all([loadPane(0, start, ''), loadPane(1, start, '')])
  root.value?.focus()
})

onBeforeUnmount(() => {
  window.removeEventListener('ftrw:fm-edit-path', onEditPathEvent)
})
</script>

<template>
  <div
    ref="root"
    class="fm"
    tabindex="0"
    @keydown="onKeydown"
    @click="root?.focus()"
  >
    <div class="drives">
      <button class="dbtn home" @click="goHome()">🏠 主目录</button>
      <button
        v-for="d in drives"
        :key="d.root"
        class="dbtn"
        :class="{ off: !d.ready }"
        :title="`${d.root}${d.label ? ' (' + d.label + ')' : ''}`"
        @click="onDrive(d)"
      >{{ driveLetter(d) }}<span v-if="d.label" class="dlabel"> {{ d.label }}</span></button>
      <span class="spacer"></span>
      <button class="dbtn" @click="refreshDrives()">⟳ 盘符</button>
      <button class="dbtn" :class="{ on: panes[active].showHidden }" @click="toggleHidden()">隐藏档</button>
      <div class="menu-wrap">
        <button class="dbtn" :class="{ on: menuOpen }" @click="menuOpen = !menuOpen; menuOpen && loadPlugins()">扩展 ▾</button>
        <div v-if="menuOpen" class="menu">
          <div v-if="!plugins.length" class="menu-empty">（尚未接入 Python 扩展）</div>
          <button v-for="t in plugins" :key="t.id" class="menu-item" @click="runPlugin(t)">{{ t.title }}</button>
        </div>
      </div>
    </div>

    <div class="panes">
      <div
        v-for="(pane, idx) in panes"
        :key="idx"
        class="pane"
        :class="{ on: idx === active }"
        @click="paneClick(idx)"
      >
        <div v-if="!pane.editingPath" class="pathbar" :title="pane.path" @dblclick="startPathEdit(idx)">{{ pane.path }}</div>
        <input
          v-else
          :id="`fm-path-${idx}`"
          class="pathbar path-edit"
          type="text"
          :value="pathDraft"
          spellcheck="false"
          placeholder="输入路径 · Tab 补全 · ↑↓ 选 · Enter 跳转 · Esc 取消"
          @input="onPathInput($event)"
          @keydown.tab.prevent="acceptAc(idx)"
          @keydown.down.prevent="moveAc(1)"
          @keydown.up.prevent="moveAc(-1)"
          @keydown.enter.prevent="onPathEnter(idx)"
          @keydown.esc.prevent="onPathEsc(idx)"
          @blur="commitPathEdit(idx)"
          @click.stop
          @dblclick.stop
        />
        <div v-if="pane.editingPath && acOpen" class="acbox">
          <div
            v-for="(it, ai) in acItems"
            :key="it.name"
            class="acitem"
            :class="{ sel: ai === acIndex, dir: it.isDir }"
            @mousedown.prevent="acIndex = ai; acceptAc(idx)"
          >{{ it.name }}<span v-if="it.isDir" class="acdir">/</span></div>
        </div>
        <div v-if="pane.filter" class="filterbar">过滤：{{ pane.filter }}<span class="fhint">（Esc 清除 · Backspace 删字）</span></div>
        <div class="thead">
          <span class="c-name" :class="{ sorted: pane.sortKey === 'name' }" @click="sortKeyBy(idx, 'name')">名称</span>
          <span class="c-type" :class="{ sorted: pane.sortKey === 'ext' }" @click="sortKeyBy(idx, 'ext')">类型</span>
          <span class="c-size" :class="{ sorted: pane.sortKey === 'size' }" @click="sortKeyBy(idx, 'size')">大小</span>
          <span class="c-date" :class="{ sorted: pane.sortKey === 'mtime' }" @click="sortKeyBy(idx, 'mtime')">修改日期</span>
        </div>
        <div class="rows">
          <div v-if="pane.loading" class="row-loading">载入中…</div>
          <div
            v-for="(row, ri) in pane.rows"
            :id="`fm-row-${idx}-${ri}`"
            :key="row.path + ri"
            class="row"
            :class="{ cur: ri === pane.cursor && idx === active, mark: !row.special && pane.sel.has(row.path), dir: row.isDir, special: row.special }"
            @click="rowClick(idx, ri, $event)"
            @dblclick="activate(row)"
          >
            <span class="c-name">{{ row.name }}</span>
            <span class="c-type">{{ rowType(row) }}</span>
            <span class="c-size">{{ fmtSize(row) }}</span>
            <span class="c-date">{{ fmtDate(row) }}</span>
          </div>
          <div v-if="!pane.loading && pane.filter && !pane.rows.length" class="row-empty">（无匹配项）</div>
          <div v-else-if="!pane.loading && !pane.filter && pane.rows.length <= 2" class="row-empty">（空目录）</div>
        </div>
      </div>
    </div>

    <div class="foot">
      <span class="hint">Tab 切栏 · Enter 打开 · F5 复制 / F6 移动 / F7 目录 · Del 回收站 / Shift+Del 永删 · F2 重命名 · F3 预览 · Insert 多选 · Shift+↑↓ / Shift+点击 连续多选 · Ctrl+点击 跳选 · 敲字母过滤（支持拼音）· 双击地址栏 / Alt+X d 改路径</span>
      <span v-if="status" class="status">{{ status }}</span>
    </div>

    <!-- 输入浮层（新建目录 / 重命名） -->
    <div v-if="input.show" class="mask" @click.self="closeInput()">
      <div class="dlg">
        <div class="dlg-head">{{ input.title }}</div>
        <input
          class="dlg-input"
          type="text"
          v-model="input.value"
          @keydown.enter="commitInput()"
          @keydown.esc="closeInput()"
        />
        <div class="dlg-btns">
          <button @click="commitInput()">确定</button>
          <button @click="closeInput()">取消</button>
        </div>
      </div>
    </div>

    <!-- 删除确认 -->
    <div v-if="confirmDel.show" class="mask" @click.self="confirmDel.show = false">
      <div class="dlg">
        <div class="dlg-head">{{ confirmDel.permanent ? '确认永久删除' : '确认删除到回收站' }}</div>
        <div class="dlg-msg">{{ confirmDel.label }}</div>
        <div class="dlg-note">{{ confirmDel.permanent ? '此操作不走回收站，删除后无法恢复。' : '文件将移入回收站，可从回收站还原。' }}</div>
        <div class="dlg-btns">
          <button class="danger" @click="commitDelete()">{{ confirmDel.permanent ? '永久删除' : '删除' }}</button>
          <button @click="confirmDel.show = false; root?.focus()">取消</button>
        </div>
      </div>
    </div>

    <!-- F3 查看器 -->
    <div v-if="viewer.show" class="mask" @click.self="viewer.show = false; root?.focus()">
      <div class="dlg wide">
        <div class="dlg-head">{{ viewer.title }} <span class="viewer-path">{{ viewer.path }}</span></div>
        <pre class="view-body">{{ viewer.text }}</pre>
        <div class="dlg-btns"><button @click="viewer.show = false; root?.focus()">关闭 (Esc)</button></div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.fm { display: flex; flex-direction: column; height: 100%; color: #d5d9e0; font: 13px/1.4 "Segoe UI", "Microsoft YaHei", monospace; outline: none; background: #181a1e; }
.drives { display: flex; align-items: center; gap: 4px; padding: 6px 8px; background: #202329; border-bottom: 1px solid #2c3038; flex-wrap: wrap; }
.dbtn { background: #2b2f37; color: #c6ccd6; border: 1px solid #3d424b; border-radius: 4px; padding: 3px 8px; font-size: 12px; cursor: pointer; }
.dbtn:hover { background: #3a3f49; }
.dbtn.on { border-color: #6ea8fe; color: #fff; }
.dbtn.off { color: #5d6470; }
.dbtn.home { background: #24344d; }
.dlabel { color: #8a919e; }
.spacer { flex: 1; }
.menu-wrap { position: relative; }
.menu { position: absolute; right: 0; top: 26px; z-index: 20; background: #24272e; border: 1px solid #3d424b; border-radius: 6px; padding: 6px; min-width: 180px; box-shadow: 0 6px 20px rgba(0,0,0,.5); }
.menu-empty { color: #6b7280; font-size: 12px; padding: 4px 6px; }
.menu-item { display: block; width: 100%; text-align: left; background: none; border: none; color: #c6ccd6; padding: 5px 6px; cursor: pointer; border-radius: 4px; }
.menu-item:hover { background: #343943; }
.panes { flex: 1; display: flex; min-height: 0; }
.pane { flex: 1; display: flex; flex-direction: column; min-width: 0; border: 2px solid transparent; position: relative; }
.pane.on { border-color: #2f5aa0; }
.pane:not(.on) { border-color: #7a4a1e; }
.pathbar { padding: 3px 8px; background: #1b1d22; color: #9aa1ad; font-size: 12px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; border-bottom: 1px solid #2c3038; }
.path-edit { width: 100%; box-sizing: border-box; color: #e6e9ef; background: #10233b; border-bottom: 1px solid #6ea8fe; outline: none; font: 12px/1.4 "Segoe UI", "Microsoft YaHei", monospace; cursor: text; }
.path-edit::placeholder { color: #5f6b80; }
.acbox { position: absolute; top: 24px; left: 2px; right: 2px; z-index: 30; max-height: 240px; overflow-y: auto; background: #171d29; border: 1px solid #3d424b; border-radius: 0 0 6px 6px; box-shadow: 0 8px 22px rgba(0,0,0,.55); }
.acitem { padding: 3px 10px; font-size: 12px; color: #c6ccd6; cursor: pointer; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
.acitem:hover { background: #232a38; }
.acitem.sel { background: #2f5aa0; color: #fff; }
.acitem.dir { color: #7fb0ff; }
.acitem.dir.sel { color: #fff; }
.acdir { color: #6b7f9e; margin-left: 2px; }
.filterbar { padding: 2px 8px; background: #24344d; color: #9fc3ff; font-size: 12px; border-bottom: 1px solid #2c3038; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
.filterbar .fhint { color: #6b7f9e; margin-left: 6px; }
.thead { display: flex; background: #262a31; color: #8a919e; font-size: 12px; border-bottom: 1px solid #333844; user-select: none; }
.thead span { padding: 3px 6px; cursor: pointer; }
.thead span.sorted { color: #6ea8fe; }
.rows { flex: 1; overflow-y: auto; }
.row { display: flex; padding: 1px 0; cursor: default; white-space: nowrap; }
.row .c-name, .thead .c-name { flex: 1 1 45%; min-width: 0; overflow: hidden; text-overflow: ellipsis; padding: 1px 6px; }
.row .c-type, .thead .c-type { flex: 0 0 12%; padding: 1px 6px; color: #8a919e; }
.row .c-size, .thead .c-size { flex: 0 0 16%; text-align: right; padding: 1px 6px; color: #b5bac4; }
.row .c-date, .thead .c-date { flex: 0 0 27%; padding: 1px 6px; color: #8a919e; }
.row.dir .c-name { color: #7fb0ff; }
.row.mark { background: #21324a; }
.row.cur { background: #2c313a; outline: 1px solid #4a6b96; }
.row.special .c-name { color: #8a919e; }
.row-loading, .row-empty { padding: 8px 12px; color: #6b7280; }
.foot { display: flex; justify-content: space-between; align-items: center; padding: 4px 10px; color: #5d6470; font-size: 12px; border-top: 1px solid #262a31; background: #1b1d22; }
.status { color: #b5bac4; max-width: 50%; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.mask { position: fixed; inset: 0; background: rgba(0,0,0,.45); display: flex; align-items: center; justify-content: center; z-index: 50; }
.dlg { background: #24272e; border: 1px solid #3d424b; border-radius: 8px; padding: 14px 16px; min-width: 360px; max-width: 90%; box-shadow: 0 10px 40px rgba(0,0,0,.6); }
.dlg.wide { width: 80%; max-width: 960px; height: 80%; display: flex; flex-direction: column; }
.dlg-head { font-size: 14px; color: #e6e9ef; margin-bottom: 10px; }
.viewer-path { color: #6b7280; font-size: 11px; margin-left: 8px; }
.view-body { flex: 1; overflow: auto; background: #16181c; color: #cfd4dd; border: 1px solid #2c3038; border-radius: 6px; padding: 10px; margin: 0; white-space: pre-wrap; word-break: break-all; font-family: Consolas, "Courier New", monospace; font-size: 12px; }
.dlg-msg { color: #d5d9e0; padding: 4px 0; word-break: break-all; }
.dlg-note { color: #8a919e; font-size: 12px; margin: 6px 0; }
.dlg-input { width: 100%; box-sizing: border-box; background: #1b1d22; color: #e6e9ef; border: 1px solid #3d424b; border-radius: 6px; padding: 7px 10px; font-size: 14px; outline: none; }
.dlg-input:focus { border-color: #6ea8fe; }
.dlg-btns { display: flex; justify-content: flex-end; gap: 8px; margin-top: 12px; }
.dlg-btns button { background: #2b2f37; color: #c6ccd6; border: 1px solid #3d424b; border-radius: 6px; padding: 5px 14px; cursor: pointer; }
.dlg-btns button:hover { background: #3a3f49; }
.dlg-btns button.danger { background: #5a2b2b; border-color: #7a3a3a; color: #f0d0d0; }
</style>
