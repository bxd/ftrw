<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref } from 'vue'
import * as api from '../api'

const emit = defineEmits<{
  (e: 'edit', draft: api.EditorDraft): void
  (e: 'new'): void
  (e: 'close'): void
}>()

const drafts = ref<api.EditorDraft[]>([])
const query = ref('')
const selected = ref(-1)
const loading = ref(false)
const busy = ref(false)
const error = ref('')
const notice = ref('')
const confirmId = ref('')
const listEl = ref<HTMLUListElement | null>(null)
let loadSeq = 0
let actionSeq = 0

const filtered = computed(() => {
  const q = query.value.trim().toLowerCase()
  if (!q) return drafts.value
  return drafts.value.filter((d) => `${d.siteName} ${d.board} ${d.title} ${d.body}`.toLowerCase().includes(q))
})
const current = computed(() => filtered.value[selected.value] || null)
const confirming = computed(() => drafts.value.find((d) => d.id === confirmId.value) || null)

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
    drafts.value = list || []
    selected.value = drafts.value.length ? 0 : -1
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
function askDelete(draft: api.EditorDraft) {
  if (busy.value) return
  error.value = ''
  notice.value = ''
  confirmId.value = draft.id
}
function cancelDelete() { confirmId.value = ''; focusList() }
async function remove() {
  const draft = confirming.value
  if (!draft || busy.value) return
  const seq = ++actionSeq
  busy.value = true
  error.value = ''
  try {
    await api.DeleteEditorDraft(draft.id)
    if (seq !== actionSeq) return
    confirmId.value = ''
    notice.value = `已删除草稿“${draft.title || draft.board || '未命名'}”`
    await refresh()
    focusList()
  } catch (e) {
    if (seq === actionSeq) error.value = `删除失败：${messageOf(e)}`
  } finally {
    if (seq === actionSeq) busy.value = false
  }
}
function onListKey(e: KeyboardEvent) {
  if (!filtered.value.length || busy.value || confirmId.value) return
  switch (e.key) {
    case 'ArrowDown':
      e.preventDefault(); selected.value = Math.min(filtered.value.length - 1, selected.value + 1); break
    case 'ArrowUp':
      e.preventDefault(); selected.value = Math.max(0, selected.value - 1); break
    case 'Home':
      e.preventDefault(); selected.value = 0; break
    case 'End':
      e.preventDefault(); selected.value = filtered.value.length - 1; break
    case 'Enter':
      e.preventDefault(); if (current.value) edit(current.value); break
    case 'Delete':
      e.preventDefault(); if (current.value) askDelete(current.value); break
  }
}
function onDialogKey(e: KeyboardEvent) {
  if (e.defaultPrevented) return
  if (e.key === 'Escape') {
    e.preventDefault()
    e.stopPropagation()
    if (confirmId.value) cancelDelete()
    else emit('close')
  }
}

onMounted(() => { void refresh(true) })
onBeforeUnmount(() => { loadSeq++; actionSeq++ })
</script>

<template>
  <div class="mask" @click.self="emit('close')">
    <div class="dlg" role="dialog" aria-modal="true" aria-label="草稿管理" @keydown="onDialogKey">
      <div class="head">
        <div><strong>草稿管理</strong><span class="count">{{ drafts.length }} 篇本地草稿</span></div>
        <button type="button" class="x" title="关闭（Esc）" @click="emit('close')">✕</button>
      </div>
      <div class="filter">
        <input v-model="query" type="search" placeholder="筛选站点、版面、标题或正文" autocomplete="off">
        <button type="button" :disabled="loading || busy" @click="refresh(false)">{{ loading ? '刷新中…' : '刷新' }}</button>
        <button type="button" class="primary" :disabled="busy" @click="emit('new')">新建草稿</button>
      </div>
      <ul ref="listEl" class="list" role="listbox" tabindex="0" aria-label="本地文章草稿" @keydown="onListKey">
        <li v-for="(d, i) in filtered" :key="d.id" role="option" :aria-selected="selected === i"
            :class="{ on: selected === i }" @click="selected = i" @dblclick="edit(d)">
          <div class="title"><span>{{ d.title || d.board || '未命名草稿' }}</span><i v-if="d.synced">已回传</i></div>
          <div class="meta">{{ d.siteName || d.siteKey || '未关联站点' }} · {{ d.mode === 'reply' ? '回帖' : '发文' }}<span v-if="d.board"> · {{ d.board }}</span></div>
          <div class="preview">{{ d.body.replace(/\s+/g, ' ').slice(0, 120) || '（正文为空）' }}</div>
          <div class="footline"><span>{{ formatTime(d.updatedAt) }}</span><span>{{ d.attachments.length }} 个附件</span></div>
        </li>
      </ul>
      <div v-if="loading && !drafts.length" class="empty">正在读取草稿…</div>
      <div v-else-if="!filtered.length && !error" class="empty">{{ query ? '没有匹配的草稿' : '暂无草稿。进入 BBS 发文设置页后会自动打开本地编辑器。' }}</div>
      <div v-if="error" class="message error" role="alert">{{ error }}</div>
      <div v-else-if="notice" class="message notice" role="status">{{ notice }}</div>
      <div class="foot">
        <span class="hint">↑↓ 选择 · Enter 编辑 · Delete 删除 · Esc 关闭</span>
        <div class="spacer" />
        <button type="button" :disabled="!current || busy" @click="current && edit(current)">编辑</button>
        <button type="button" class="danger" :disabled="!current || busy" @click="current && askDelete(current)">删除</button>
        <button type="button" @click="emit('close')">关闭</button>
      </div>
      <div v-if="confirming" class="confirm-mask" @click.self="cancelDelete">
        <div class="confirm-box" role="alertdialog" aria-modal="true" aria-label="确认删除草稿" @keydown.esc.stop.prevent="cancelDelete">
          <h3>删除这篇草稿？</h3>
          <p>{{ confirming.title || confirming.board || '未命名草稿' }}</p>
          <p>只删除本地草稿，不影响已经回传到 BBS 的文章。</p>
          <div class="confirm-actions"><button type="button" @click="cancelDelete">取消</button><button type="button" class="danger" :disabled="busy" @click="remove">{{ busy ? '删除中…' : '删除草稿' }}</button></div>
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
.list li { padding: 10px 16px; border-left: 3px solid transparent; cursor: default; }.list li:hover { background: #2b2f37; }.list li.on { background: #2f4c70; border-left-color: #6ab0f3; }
.title { display: flex; align-items: center; gap: 8px; font-size: 14px; }.title span { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }.title i { flex: none; color: #8fc7a5; background: #26392e; border-radius: 3px; padding: 1px 5px; font-size: 11px; font-style: normal; }
.meta, .footline { display: flex; gap: 8px; color: #8a919e; font-size: 12px; margin-top: 4px; }.footline { justify-content: space-between; }
.preview { margin-top: 6px; color: #b5bac4; font-size: 12px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.empty { flex: 1; display: flex; align-items: center; justify-content: center; padding: 30px; color: #737b89; font-size: 13px; text-align: center; pointer-events: none; }
.message { padding: 8px 16px; font-size: 12px; }.message.error { color: #ef8a7d; background: #402a2a; }.message.notice { color: #8fc7a5; background: #26392e; }
.foot { display: flex; align-items: center; gap: 8px; padding: 11px 16px; border-top: 1px solid #33373f; }.hint { color: #737b89; font-size: 11px; }.spacer { flex: 1; }
button { background: #2f333b; color: #d5d9e0; border: 1px solid #3d424b; border-radius: 5px; padding: 6px 14px; cursor: pointer; font-size: 13px; }
button:hover:not(:disabled) { background: #3a3f49; }button:disabled { opacity: .4; cursor: default; }button.primary { background: #2f74c0; border-color: #2f74c0; color: #fff; }button.danger { color: #f09a90; border-color: #71423e; }
button.x { padding: 2px 7px; border: none; background: none; color: #8a919e; font-size: 16px; }
.confirm-mask { position: absolute; inset: 0; z-index: 2; display: flex; align-items: center; justify-content: center; background: rgba(0,0,0,.65); }
.confirm-box { width: min(420px, 86vw); padding: 20px; background: #292d35; border: 1px solid #555c68; border-radius: 9px; box-shadow: 0 12px 36px rgba(0,0,0,.55); }
.confirm-box h3 { margin: 0 0 10px; font-size: 15px; }.confirm-box p { margin: 8px 0; color: #aab1bc; font-size: 13px; line-height: 1.6; }.confirm-actions { display: flex; justify-content: flex-end; gap: 8px; margin-top: 18px; }
</style>
