<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import * as api from '../api'

const emit = defineEmits<{
  (e: 'edit', draft: api.EditorDraft): void
  (e: 'new'): void
  (e: 'close'): void
}>()

const drafts = ref<api.EditorDraft[]>([])
const query = ref('')
const selected = ref(-1)
const checkedIds = ref<string[]>([])
const confirmIds = ref<string[]>([])
const loading = ref(false)
const busy = ref(false)
const error = ref('')
const notice = ref('')
const listEl = ref<HTMLUListElement | null>(null)
const cancelBtn = ref<HTMLButtonElement | null>(null)
const confirmBtn = ref<HTMLButtonElement | null>(null)
let anchor = -1
let loadSeq = 0
let actionSeq = 0

const filtered = computed(() => {
  const q = query.value.trim().toLowerCase()
  if (!q) return drafts.value
  return drafts.value.filter((d) => `${d.siteName} ${d.board} ${d.title} ${d.body}`.toLowerCase().includes(q))
})
const current = computed(() => filtered.value[selected.value] || null)
const selectedDrafts = computed(() => drafts.value.filter((d) => checkedIds.value.includes(d.id)))
const allSelected = computed(() => filtered.value.length > 0 && filtered.value.every((d) => checkedIds.value.includes(d.id)))
const confirming = computed(() => drafts.value.filter((d) => confirmIds.value.includes(d.id)))

function messageOf(e: unknown): string { return e instanceof Error ? e.message : String(e) }
function formatTime(ms: number): string {
  const d = new Date(ms)
  return Number.isNaN(d.getTime()) ? '—' : d.toLocaleString('zh-CN', { hour12: false })
}
function focusList() { nextTick(() => listEl.value?.focus()) }

async function refresh(focus = false) {
  const seq = ++loadSeq
  loading.value = true
  error.value = ''
  try {
    const list = await api.ListEditorDrafts()
    if (seq !== loadSeq) return
    const previous = current.value?.id
    drafts.value = list || []
    const present = new Set(drafts.value.map((draft) => draft.id))
    checkedIds.value = checkedIds.value.filter((id) => present.has(id))
    const idx = previous ? filtered.value.findIndex((draft) => draft.id === previous) : -1
    selected.value = idx >= 0 ? idx : (filtered.value.length ? 0 : -1)
    anchor = selected.value
    if (focus) focusList()
  } catch (e) {
    if (seq === loadSeq) error.value = `读取草稿失败：${messageOf(e)}`
  } finally {
    if (seq === loadSeq) loading.value = false
  }
}

function edit(draft: api.EditorDraft) {
  if (busy.value) return
  emit('edit', draft)
}
function isPicked(id: string): boolean { return checkedIds.value.includes(id) }
function togglePicked(id: string) {
  if (busy.value) return
  checkedIds.value = isPicked(id)
    ? checkedIds.value.filter((item) => item !== id)
    : [...checkedIds.value, id]
}
function onRowClick(index: number, e: MouseEvent) {
  if (busy.value) return
  const previousAnchor = anchor
  selected.value = index
  anchor = index
  if (e.shiftKey && previousAnchor >= 0) {
    const start = Math.min(previousAnchor, index)
    const end = Math.max(previousAnchor, index)
    checkedIds.value = filtered.value.slice(start, end + 1).map((draft) => draft.id)
  } else if (e.ctrlKey || e.metaKey) {
    togglePicked(filtered.value[index].id)
  } else {
    checkedIds.value = []
  }
}
function selectAll() {
  if (busy.value || !filtered.value.length) return
  checkedIds.value = allSelected.value ? [] : filtered.value.map((draft) => draft.id)
}
function askDelete() {
  if (busy.value) return
  const ids = checkedIds.value.length ? [...checkedIds.value] : (current.value ? [current.value.id] : [])
  if (!ids.length) return
  error.value = ''
  notice.value = ''
  confirmIds.value = ids
  nextTick(() => cancelBtn.value?.focus())
}
function cancelDelete() { confirmIds.value = []; focusList() }
function onConfirmKey(e: KeyboardEvent) {
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
  const items = confirming.value
  if (!items.length || busy.value) return
  const ids = items.map((draft) => draft.id)
  const seq = ++actionSeq
  busy.value = true
  error.value = ''
  try {
    await api.DeleteEditorDrafts(ids)
    if (seq !== actionSeq) return
    confirmIds.value = []
    notice.value = ids.length > 1 ? `已删除 ${ids.length} 篇草稿` : `已删除草稿“${items[0].title || items[0].board || '未命名'}”`
    await refresh()
    focusList()
  } catch (e) {
    if (seq === actionSeq) {
      confirmIds.value = []
      error.value = `删除失败：${messageOf(e)}`
      focusList()
    }
  } finally {
    if (seq === actionSeq) busy.value = false
  }
}
function onListKey(e: KeyboardEvent) {
  if (e.target instanceof HTMLInputElement) return
  if (!filtered.value.length || busy.value || confirmIds.value.length) return
  if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'a') {
    e.preventDefault()
    selectAll()
    return
  }
  switch (e.key) {
    case 'ArrowDown':
      e.preventDefault(); selected.value = Math.min(filtered.value.length - 1, selected.value + 1); anchor = selected.value; break
    case 'ArrowUp':
      e.preventDefault(); selected.value = Math.max(0, selected.value - 1); anchor = selected.value; break
    case 'Home':
      e.preventDefault(); selected.value = 0; anchor = 0; break
    case 'End':
      e.preventDefault(); selected.value = filtered.value.length - 1; anchor = selected.value; break
    case 'Enter':
      e.preventDefault(); if (current.value) edit(current.value); break
    case ' ':
    case 'Spacebar':
      e.preventDefault(); if (current.value) togglePicked(current.value.id); break
    case 'Delete':
      e.preventDefault(); askDelete(); break
  }
}
function onDialogKey(e: KeyboardEvent) {
  if (e.defaultPrevented) return
  if (e.key === 'Escape') {
    e.preventDefault()
    e.stopPropagation()
    if (confirmIds.value.length) cancelDelete()
    else emit('close')
    return
  }
  if (!confirmIds.value.length && (e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'a') {
    e.preventDefault()
    selectAll()
  }
}

watch(selected, () => {
  nextTick(() => listEl.value?.querySelector<HTMLElement>(`[data-index="${selected.value}"]`)?.scrollIntoView({ block: 'nearest' }))
})
watch(filtered, (list) => {
  if (selected.value >= list.length) selected.value = Math.max(0, list.length - 1)
  anchor = selected.value
})

onMounted(() => { void refresh(true) })
onBeforeUnmount(() => { loadSeq++; actionSeq++ })
</script>

<template>
  <div class="mask" @click.self="emit('close')">
    <div class="dlg" role="dialog" aria-modal="true" aria-label="草稿管理" @keydown="onDialogKey">
      <div class="head">
        <div><strong>草稿管理</strong><span class="count">{{ drafts.length }} 篇本地草稿 · 已选 {{ checkedIds.length }}</span></div>
        <button type="button" class="x" title="关闭（Esc）" @click="emit('close')">✕</button>
      </div>
      <div class="filter">
        <input v-model="query" type="search" placeholder="筛选站点、版面、标题或正文" autocomplete="off">
        <button type="button" :disabled="loading || busy" @click="refresh(false)">{{ loading ? '刷新中…' : '刷新' }}</button>
        <button type="button" class="primary" :disabled="busy" @click="emit('new')">新建草稿</button>
      </div>
      <ul ref="listEl" class="list" role="listbox" tabindex="0" aria-label="本地文章草稿" aria-multiselectable="true" @keydown="onListKey">
        <li v-for="(d, i) in filtered" :key="d.id" role="option" :aria-selected="isPicked(d.id)" :data-index="i"
            :class="{ on: selected === i }" @click="onRowClick(i, $event)" @dblclick="edit(d)">
          <input class="pick" type="checkbox" :checked="isPicked(d.id)" :disabled="busy" :aria-label="`选择草稿 ${d.title || d.board || '未命名草稿'}`" @click.stop="togglePicked(d.id)" @dblclick.stop>
          <div class="entry">
            <div class="title"><span>{{ d.title || d.board || '未命名草稿' }}</span><i v-if="d.synced">已回传</i></div>
            <div class="meta">{{ d.siteName || d.siteKey || '未关联站点' }} · {{ d.mode === 'reply' ? '回帖' : '发文' }}<span v-if="d.board"> · {{ d.board }}</span></div>
            <div class="preview">{{ d.body.replace(/\s+/g, ' ').slice(0, 120) || '（正文为空）' }}</div>
            <div class="footline"><span>{{ formatTime(d.updatedAt) }}</span><span>{{ d.attachments.length }} 个附件</span></div>
          </div>
        </li>
      </ul>
      <div v-if="loading && !drafts.length" class="empty">正在读取草稿…</div>
      <div v-else-if="!filtered.length && !error" class="empty">{{ query ? '没有匹配的草稿' : '暂无草稿。进入 BBS 发文设置页后会自动打开本地编辑器。' }}</div>
      <div v-if="error" class="message error" role="alert">{{ error }}</div>
      <div v-else-if="notice" class="message notice" role="status">{{ notice }}</div>
      <div class="foot">
        <span class="hint">↑↓ 选择 · 空格 勾选 · Ctrl+A 全选 · Delete 删除 · Enter 编辑 · Esc 关闭</span>
        <div class="spacer" />
        <button type="button" :disabled="!filtered.length || busy" @click="selectAll">{{ allSelected ? '取消全选' : '全选' }}</button>
        <button type="button" :disabled="!current || busy" @click="current && edit(current)">编辑</button>
        <button type="button" class="danger" :disabled="!selectedDrafts.length || busy" @click="askDelete">删除所选</button>
        <button type="button" @click="emit('close')">关闭</button>
      </div>
      <div v-if="confirming.length" class="confirm-mask" @click.self="cancelDelete">
        <div class="confirm-box" role="alertdialog" aria-modal="true" aria-label="确认删除草稿" @keydown.esc.stop.prevent="cancelDelete" @keydown="onConfirmKey">
          <h3>{{ confirming.length > 1 ? `删除选中的 ${confirming.length} 篇草稿？` : '删除这篇草稿？' }}</h3>
          <p v-for="draft in confirming.slice(0, 3)" :key="draft.id">{{ draft.title || draft.board || '未命名草稿' }}</p>
          <p v-if="confirming.length > 3">另有 {{ confirming.length - 3 }} 篇草稿</p>
          <p>只删除本地草稿，不影响已经回传到 BBS 的文章。</p>
          <div class="confirm-actions"><button ref="cancelBtn" type="button" :disabled="busy" @click="cancelDelete">取消</button><button ref="confirmBtn" type="button" class="danger" :disabled="busy" @click="remove">{{ busy ? '删除中…' : '删除草稿' }}</button></div>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.mask { position: fixed; inset: 0; z-index: 50; background: rgba(0,0,0,.58); display: flex; align-items: center; justify-content: center; }
.dlg { width: min(880px, 92vw); height: min(680px, 88vh); display: flex; flex-direction: column; background: #24272e; border: 1px solid #3d424b; border-radius: 10px; color: #d5d9e0; box-shadow: 0 18px 60px rgba(0,0,0,.5); overflow: hidden; }
.head { display: flex; align-items: center; justify-content: space-between; padding: 12px 16px; border-bottom: 1px solid #33373f; }
.head > div { display: flex; align-items: baseline; gap: 10px; }.head strong { font-size: 15px; }.count { color: #8a919e; font-size: 12px; }
.filter { display: flex; gap: 8px; padding: 10px 16px; background: #202329; border-bottom: 1px solid #30343c; }
.filter input { flex: 1; min-width: 0; background: #1b1d22; color: #d5d9e0; border: 1px solid #3d424b; border-radius: 5px; padding: 6px 8px; font: inherit; }
.list { list-style: none; margin: 0; padding: 4px 0; overflow-y: auto; flex: 1; outline: none; }
.list li { display: flex; align-items: flex-start; gap: 10px; padding: 10px 16px; border-left: 3px solid transparent; cursor: default; }.list li:hover { background: #2b2f37; }.list li.on { background: #2f4c70; border-left-color: #6ab0f3; }
.list .pick { flex: none; margin: 3px 0 0; accent-color: #6ab0f3; }.entry { flex: 1; min-width: 0; }
.title { display: flex; align-items: center; gap: 8px; font-size: 14px; }.title span { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }.title i { flex: none; color: #8fc7a5; background: #26392e; border-radius: 3px; padding: 1px 5px; font-size: 11px; font-style: normal; }
.meta, .footline { display: flex; gap: 8px; color: #8a919e; font-size: 12px; margin-top: 4px; }.footline { justify-content: space-between; }
.preview { margin-top: 6px; color: #b5bac4; font-size: 12px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.empty { flex: 1; display: flex; align-items: center; justify-content: center; padding: 30px; color: #737b89; font-size: 13px; text-align: center; pointer-events: none; }
.message { padding: 8px 16px; font-size: 12px; }.message.error { color: #ef8a7d; background: #402a2a; }.message.notice { color: #8fc7a5; background: #26392e; }
.foot { display: flex; align-items: center; flex-wrap: wrap; gap: 8px; padding: 11px 16px; border-top: 1px solid #33373f; }.hint { flex: 1 1 100%; color: #737b89; font-size: 11px; }.spacer { flex: 1; }
button { background: #2f333b; color: #d5d9e0; border: 1px solid #3d424b; border-radius: 5px; padding: 6px 14px; cursor: pointer; font-size: 13px; }
button:hover:not(:disabled) { background: #3a3f49; }button:focus-visible, .pick:focus-visible { outline: 2px solid #6ab0f3; outline-offset: 2px; }button:disabled { opacity: .4; cursor: default; }button.primary { background: #2f74c0; border-color: #2f74c0; color: #fff; }button.danger { color: #f09a90; border-color: #71423e; }
button.x { padding: 2px 7px; border: none; background: none; color: #8a919e; font-size: 16px; }
.confirm-mask { position: absolute; inset: 0; z-index: 2; display: flex; align-items: center; justify-content: center; background: rgba(0,0,0,.65); }
.confirm-box { width: min(420px, 86vw); padding: 20px; background: #292d35; border: 1px solid #555c68; border-radius: 9px; box-shadow: 0 12px 36px rgba(0,0,0,.55); }
.confirm-box h3 { margin: 0 0 10px; font-size: 15px; }.confirm-box p { margin: 8px 0; color: #aab1bc; font-size: 13px; line-height: 1.6; }.confirm-actions { display: flex; justify-content: flex-end; gap: 8px; margin-top: 18px; }
</style>
