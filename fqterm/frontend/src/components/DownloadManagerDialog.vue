<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import * as api from '../api'

const emit = defineEmits<{ (e: 'close'): void }>()

const root = ref('')
const entries = ref<api.DownloadEntry[]>([])
const selected = ref(-1)
const selectedPaths = ref<string[]>([])
const current = computed(() => entries.value[selected.value] || null)
const allSelected = computed(() => entries.value.length > 0 && selectedPaths.value.length === entries.value.length)
const loading = ref(false)
const busy = ref(false)
const error = ref('')
const notice = ref('')
const confirm = reactive({ show: false, paths: [] as string[] })
const confirmItems = computed(() => entries.value.filter((item) => confirm.paths.includes(item.path)))
const listEl = ref<HTMLUListElement | null>(null)
const cancelBtn = ref<HTMLButtonElement | null>(null)
const confirmBtn = ref<HTMLButtonElement | null>(null)
let anchor = -1
let loadSeq = 0
let actionSeq = 0

function messageOf(e: unknown): string {
  return e instanceof Error ? e.message : String(e)
}

function focusList() {
  nextTick(() => listEl.value?.focus())
}

async function refresh(focus = false) {
  const seq = ++loadSeq
  loading.value = true
  error.value = ''
  try {
    root.value = await api.DownloadRootDir()
    const list = await api.ListDownloads()
    if (seq !== loadSeq) return
    const previous = current.value?.path
    entries.value = list || []
    const present = new Set(entries.value.map((item) => item.path))
    selectedPaths.value = selectedPaths.value.filter((path) => present.has(path))
    const idx = previous ? entries.value.findIndex((item) => item.path === previous) : -1
    selected.value = idx >= 0 ? idx : (entries.value.length ? 0 : -1)
    anchor = selected.value
    if (focus) focusList()
  } catch (e) {
    if (seq === loadSeq) error.value = messageOf(e)
  } finally {
    if (seq === loadSeq) loading.value = false
  }
}

async function preview() {
  const item = current.value
  if (!item || busy.value) return
  const seq = ++actionSeq
  busy.value = true
  error.value = ''
  notice.value = ''
  try {
    const items = await api.PreviewDownload(item.path)
    if (seq !== actionSeq) return
    if (!items?.length) throw new Error('没有可预览的文件')
    await api.OpenPreview(items, 0)
  } catch (e) {
    if (seq === actionSeq) error.value = `预览失败：${messageOf(e)}`
  } finally {
    if (seq === actionSeq) busy.value = false
  }
}

function isPicked(path: string): boolean {
  return selectedPaths.value.includes(path)
}

function togglePicked(path: string) {
  if (busy.value) return
  selectedPaths.value = isPicked(path)
    ? selectedPaths.value.filter((item) => item !== path)
    : [...selectedPaths.value, path]
}

function onRowClick(index: number, e: MouseEvent) {
  if (busy.value) return
  const previousAnchor = anchor
  selected.value = index
  anchor = index
  if (e.shiftKey && previousAnchor >= 0) {
    const start = Math.min(previousAnchor, index)
    const end = Math.max(previousAnchor, index)
    selectedPaths.value = entries.value.slice(start, end + 1).map((item) => item.path)
  } else if (e.ctrlKey || e.metaKey) {
    togglePicked(entries.value[index].path)
  } else {
    selectedPaths.value = []
  }
}

function selectAll() {
  if (busy.value || !entries.value.length) return
  selectedPaths.value = allSelected.value ? [] : entries.value.map((item) => item.path)
}

function askDelete() {
  if (busy.value) return
  const paths = selectedPaths.value.length ? [...selectedPaths.value] : (current.value ? [current.value.path] : [])
  if (!paths.length) return
  error.value = ''
  notice.value = ''
  confirm.paths = paths
  confirm.show = true
  nextTick(() => cancelBtn.value?.focus())
}

function cancelDelete() {
  confirm.show = false
  focusList()
}

function onConfirmKey(e: KeyboardEvent) {
  if (e.key === 'Escape') {
    e.preventDefault()
    e.stopPropagation()
    cancelDelete()
    return
  }
  if (e.key === 'Tab' || e.key === 'ArrowUp' || e.key === 'ArrowDown' || e.key === 'ArrowLeft' || e.key === 'ArrowRight') {
    e.preventDefault()
    let target: HTMLButtonElement | null
    if (e.key === 'ArrowLeft' || e.key === 'ArrowUp') target = confirmBtn.value
    else if (e.key === 'ArrowRight' || e.key === 'ArrowDown') target = cancelBtn.value
    else target = document.activeElement === cancelBtn.value ? confirmBtn.value : cancelBtn.value
    target?.focus()
  }
}

async function remove() {
  const items = confirmItems.value
  if (!items.length || busy.value) return
  const paths = items.map((item) => item.path)
  const seq = ++actionSeq
  busy.value = true
  error.value = ''
  try {
    await api.DeleteDownloads(paths)
    if (seq !== actionSeq) return
    confirm.show = false
    notice.value = paths.length > 1
      ? `已将 ${paths.length} 个下载项移入回收站`
      : `已将“${items[0].name}”移入回收站`
    await refresh()
    selected.value = Math.min(selected.value, entries.value.length - 1)
    focusList()
  } catch (e) {
    if (seq === actionSeq) {
      confirm.show = false
      await refresh()
      if (seq !== actionSeq) return
      error.value = `删除失败：${messageOf(e)}`
      focusList()
    }
  } finally {
    if (seq === actionSeq) busy.value = false
  }
}

function onListKey(e: KeyboardEvent) {
  if (!entries.value.length || busy.value) return
  if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'a') {
    e.preventDefault()
    selectAll()
    return
  }
  switch (e.key) {
    case 'ArrowDown':
      e.preventDefault()
      selected.value = Math.min(entries.value.length - 1, selected.value + 1)
      break
    case 'ArrowUp':
      e.preventDefault()
      selected.value = Math.max(0, selected.value - 1)
      break
    case 'Home':
      e.preventDefault()
      selected.value = 0
      break
    case 'End':
      e.preventDefault()
      selected.value = entries.value.length - 1
      break
    case 'Enter':
    case ' ':
    case 'Spacebar':
      e.preventDefault()
      void preview()
      break
    case 'Delete':
      e.preventDefault()
      askDelete()
      break
  }
}

function onDialogKey(e: KeyboardEvent) {
  if (e.defaultPrevented) return
  if (e.key === 'Escape' && !confirm.show) {
    e.preventDefault()
    e.stopPropagation()
    emit('close')
    return
  }
  if (!confirm.show && (e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'a') {
    e.preventDefault()
    selectAll()
  }
}

function formatSize(size: number): string {
  if (size < 1024) return `${size} B`
  const units = ['KB', 'MB', 'GB', 'TB']
  let value = size / 1024
  let i = 0
  while (value >= 1024 && i < units.length - 1) { value /= 1024; i++ }
  return `${value >= 10 ? value.toFixed(1) : value.toFixed(2)} ${units[i]}`
}

function formatTime(ns: number): string {
  const d = new Date(ns / 1e6)
  return Number.isNaN(d.getTime()) ? '—' : d.toLocaleString('zh-CN', { hour12: false })
}

watch(selected, () => {
  nextTick(() => listEl.value?.querySelector<HTMLElement>(`[data-index="${selected.value}"]`)?.scrollIntoView({ block: 'nearest' }))
})

onMounted(() => { void refresh(true) })
onBeforeUnmount(() => { loadSeq++; actionSeq++ })
</script>

<template>
  <div class="mask" @click.self="emit('close')">
    <div class="dlg" role="dialog" aria-modal="true" aria-label="下载管理器" @keydown="onDialogKey">
      <div class="head">
        <div>
          <strong>下载管理器</strong>
          <span class="count">{{ entries.length }} 个下载项 · 已选 {{ selectedPaths.length }}</span>
        </div>
        <button type="button" class="x" title="关闭（Esc）" @click="emit('close')">✕</button>
      </div>

      <div class="root" :title="root">
        <span class="label">下载目录</span>
        <span class="path">{{ root || '正在解析…' }}</span>
        <button type="button" :disabled="loading || busy" @click="refresh(false)">{{ loading ? '刷新中…' : '刷新' }}</button>
      </div>

      <div class="content">
        <div class="columns" aria-hidden="true">
          <span class="check-col"></span><span>下载项</span><span>文件数</span><span>大小</span><span>下载时间</span>
        </div>
        <ul ref="listEl" class="list" role="listbox" tabindex="0" aria-label="已下载文章" aria-multiselectable="true" @keydown="onListKey">
          <li v-for="(item, i) in entries" :key="item.path" :data-index="i"
              role="option" :aria-selected="isPicked(item.path)" :class="{ on: selected === i, picked: isPicked(item.path) }"
              @click="onRowClick(i, $event)" @dblclick="preview">
            <input class="pick" type="checkbox" :checked="isPicked(item.path)" :aria-label="`选择 ${item.name}`"
                   :disabled="busy" @click.stop @dblclick.stop @change="togglePicked(item.path)">
            <span class="name" :title="item.path">{{ item.name }}</span>
            <span class="num">{{ item.fileCount }}</span>
            <span class="num">{{ formatSize(item.size) }}</span>
            <span class="time">{{ formatTime(item.mtime) }}</span>
          </li>
        </ul>
        <div v-if="loading && !entries.length" class="empty">正在读取下载目录…</div>
        <div v-else-if="!entries.length && !error" class="empty">暂无下载项。完成一次 ZMODEM 下载后会出现在这里。</div>
      </div>

      <div v-if="error" class="message error" role="alert">{{ error }}</div>
      <div v-else-if="notice" class="message notice" role="status">{{ notice }}</div>

      <div class="foot">
        <span class="hint">Ctrl/Shift 点击多选 · Ctrl+A 全选 · 空格预览 · Delete 删除</span>
        <div class="spacer" />
        <button type="button" :disabled="!entries.length || busy" @click="selectAll">{{ allSelected ? '取消全选' : '全选' }}</button>
        <button type="button" :disabled="!current || busy" @click="preview">预览</button>
        <button type="button" class="danger" :disabled="!selectedPaths.length || busy" @click="askDelete">删除所选</button>
        <button type="button" @click="emit('close')">关闭</button>
      </div>

      <div v-if="confirm.show" class="confirm-mask" @click.self="cancelDelete">
        <div class="confirm-box" role="alertdialog" aria-modal="true" aria-label="确认删除下载项" @keydown="onConfirmKey">
          <h3>{{ confirmItems.length > 1 ? `删除选中的 ${confirmItems.length} 个下载项？` : '删除整个下载项？' }}</h3>
          <div class="confirm-names">
            <p v-for="item in confirmItems" :key="item.path" class="confirm-name">{{ item.name }}</p>
          </div>
          <p v-if="confirmItems.length > 1">将把选中的下载目录整体移入回收站，正文和附件会一起删除。</p>
          <p v-else>将把该自动建立的下载目录整体移入回收站，正文和附件会一起删除。</p>
          <div class="confirm-actions">
            <button ref="cancelBtn" type="button" @click="cancelDelete">取消</button>
            <button ref="confirmBtn" type="button" class="danger" :disabled="busy" @click="remove">{{ busy ? '删除中…' : '移入回收站' }}</button>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.mask { position: fixed; inset: 0; z-index: 50; background: rgba(0, 0, 0, .58); display: flex; align-items: center; justify-content: center; }
.dlg { width: min(920px, 92vw); height: min(650px, 88vh); display: flex; flex-direction: column; background: #24272e; border: 1px solid #3d424b; border-radius: 10px; box-shadow: 0 18px 60px rgba(0, 0, 0, .5); overflow: hidden; }
.head { display: flex; align-items: center; justify-content: space-between; padding: 12px 16px; border-bottom: 1px solid #33373f; }
.head > div { display: flex; align-items: baseline; gap: 10px; }
.head strong { font-size: 15px; }
.count { color: #8a919e; font-size: 12px; }
.root { display: flex; align-items: center; gap: 10px; padding: 8px 16px; background: #202329; border-bottom: 1px solid #30343c; font-size: 12px; }
.root .label { color: #8a919e; flex: 0 0 auto; }
.root .path { flex: 1; min-width: 0; color: #b8bec8; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-family: Consolas, monospace; }
.content { flex: 1; min-height: 0; display: flex; flex-direction: column; position: relative; }
.columns, .list li { display: grid; grid-template-columns: 28px minmax(0, 1fr) 90px 110px 165px; align-items: center; gap: 10px; }
.columns { padding: 7px 16px; color: #737b89; font-size: 11px; border-bottom: 1px solid #30343c; }
.list { list-style: none; margin: 0; padding: 4px 0; overflow-y: auto; flex: 1; outline: none; }
.list li { padding: 9px 16px; cursor: default; font-size: 13px; border-left: 3px solid transparent; }
.list li:hover { background: #2b2f37; }
.list li.on { background: #2f4c70; border-left-color: #6ab0f3; }
.list li.picked { background: #293b4e; }
.list li.on.picked { background: #2f4c70; }
.list .pick { width: 14px; height: 14px; margin: 0; accent-color: #6ab0f3; cursor: pointer; }
.list li.on .name { color: #fff; }
.list .name { min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; color: #d5d9e0; }
.num { color: #aab1bc; font-variant-numeric: tabular-nums; }
.time { color: #858d9a; font-size: 12px; font-variant-numeric: tabular-nums; }
.empty { position: absolute; inset: 0; display: flex; align-items: center; justify-content: center; padding: 30px; color: #737b89; font-size: 13px; text-align: center; pointer-events: none; }
.message { padding: 8px 16px; font-size: 12px; border-top: 1px solid #33373f; }
.message.error { color: #ef8a7d; background: #402a2a; }
.message.notice { color: #8fc7a5; background: #26392e; }
.foot { display: flex; align-items: center; gap: 8px; padding: 11px 16px; border-top: 1px solid #33373f; }
.hint { color: #737b89; font-size: 11px; }
.spacer { flex: 1; }
button { background: #2f333b; color: #d5d9e0; border: 1px solid #3d424b; border-radius: 5px; padding: 6px 14px; cursor: pointer; font-size: 13px; }
button:hover:not(:disabled) { background: #3a3f49; }
button:disabled { opacity: .4; cursor: default; }
button:focus-visible, .pick:focus-visible { outline: 2px solid #6ea8fe; outline-offset: 2px; }
button.danger { color: #f09a90; border-color: #71423e; }
button.danger:hover:not(:disabled) { background: #633632; }
button.x { padding: 2px 7px; border: none; background: none; color: #8a919e; font-size: 16px; }
.confirm-mask { position: absolute; inset: 0; z-index: 2; display: flex; align-items: center; justify-content: center; background: rgba(0, 0, 0, .65); }
.confirm-box { width: min(420px, 86vw); padding: 20px; background: #292d35; border: 1px solid #555c68; border-radius: 9px; box-shadow: 0 12px 36px rgba(0, 0, 0, .55); }
.confirm-box h3 { margin: 0 0 10px; font-size: 15px; }
.confirm-box p { margin: 8px 0; color: #aab1bc; font-size: 13px; line-height: 1.6; }
.confirm-box .confirm-name { color: #fff; font-weight: 600; word-break: break-all; }
.confirm-names { max-height: 180px; overflow-y: auto; margin: 10px 0; }
.confirm-names .confirm-name { margin: 4px 0; }
.confirm-actions { display: flex; justify-content: flex-end; gap: 8px; margin-top: 18px; }
@media (max-width: 700px) {
  .columns, .list li { grid-template-columns: 28px minmax(0, 1fr) 65px 90px; }
  .columns span:last-child, .time { display: none; }
  .hint { display: none; }
}
</style>
