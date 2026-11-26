<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import * as api from '../api'
import type { ComposeContext } from '../terminal'
import { applySeparators, isQuoteLine, normalizeLines, SEP_DEFAULT, SEP_PRESETS, toVertical } from '../posttransform'
import { clipboardImageFileName, MAX_EDITOR_ATTACHMENTS, newAttachmentPaths } from '../editorinput'
import TextImageDialog from './TextImageDialog.vue'

const props = defineProps<{
  context: ComposeContext | null
  draft?: api.EditorDraft | null
  cols: number
  busy?: boolean
  error?: string
}>()
const emit = defineEmits<{
  (e: 'submit', draft: api.EditorDraft): void
  (e: 'close'): void
}>()

function newID(): string {
  return globalThis.crypto?.randomUUID?.() || `draft-${Date.now()}-${Math.random().toString(36).slice(2)}`
}
const now = Date.now()
const draft = reactive<api.EditorDraft>({
  id: props.draft?.id || newID(),
  profileId: props.draft?.profileId || props.context?.profileId || '',
  revision: props.draft?.revision || 0,
  siteKey: props.draft?.siteKey || props.context?.siteKey || '',
  siteName: props.draft?.siteName || props.context?.siteName || '',
  mode: props.draft?.mode || props.context?.mode || 'new',
  board: props.draft?.board || props.context?.board || '',
  title: props.draft?.title ?? props.context?.title ?? '',
  body: normalizeLines(props.draft?.body || ''),
  quote: props.draft?.quote ?? props.context?.quote ?? '',
  attachments: (props.draft?.attachments || []).map((a) => ({ ...a })),
  createdAt: props.draft?.createdAt || now,
  updatedAt: props.draft?.updatedAt || now,
  synced: !!props.draft?.synced,
  transferState: props.draft?.transferState || '',
})

const editor = ref<HTMLTextAreaElement | null>(null)
const saveState = ref<'saved' | 'saving' | 'error'>('saved')
const localError = ref('')
const picking = ref(false)
const addingPaths = ref(false)
const pasting = ref(false)
const dragActive = ref(false)
let offEditorDrop: (() => void) | null = null
const imageOpen = ref(false)
const imageText = ref('')
let imageSource: { start: number; end: number; text: string } | null = null
const findOpen = ref(false)
const find = reactive({ text: '', replacement: '', caseSensitive: false })
const history = ref<string[]>([])
const historyIndex = ref(-1)
let applyingHistory = false
let saveTimer: number | undefined
let saveSeq = 0
let saveChain: Promise<void> = Promise.resolve()
let disposed = false
let submitted = false
let handedOff = false
const submitting = ref(false)
const closing = ref(false)
const locked = computed(() => !!props.busy || submitting.value || closing.value)
const handoffBlocked = computed(() => draft.transferState === 'sending' || draft.transferState === 'sent')
const handoffWarning = computed(() => handoffBlocked.value
  ? '这篇草稿已开始回传，不能重复提交；请先回终端检查远端正文，或新建草稿。'
  : '')

const stats = computed(() => ({
  chars: Array.from(draft.body).length,
  lines: draft.body ? draft.body.split('\n').length : 1,
}))
const selectedText = () => {
  const el = editor.value
  if (!el || el.selectionStart === el.selectionEnd) return ''
  return draft.body.slice(el.selectionStart, el.selectionEnd)
}
function snapshot(): api.EditorDraft {
  return JSON.parse(JSON.stringify({ ...draft, body: normalizeLines(draft.body), synced: false })) as api.EditorDraft
}
function save(): Promise<boolean> {
  if (disposed) return Promise.resolve(false)
  const seq = ++saveSeq
  saveState.value = 'saving'
  // 按调用顺序串行保存；任务真正执行时才取快照，确保队尾一定包含最新改动。
  const task = saveChain.then(async () => {
    try {
      const saved = await api.SaveEditorDraft(snapshot())
      if (!disposed) {
        draft.revision = saved.revision
        draft.updatedAt = saved.updatedAt
        if (seq === saveSeq) saveState.value = 'saved'
      }
      return true
    } catch (e: any) {
      if (!disposed && seq === saveSeq) {
        saveState.value = 'error'
        localError.value = `草稿保存失败：${e?.message || e}`
      }
      return false
    }
  })
  saveChain = task.then(() => undefined)
  return task
}
function scheduleSave() {
  draft.synced = false
  if (saveTimer) window.clearTimeout(saveTimer)
  saveTimer = window.setTimeout(() => { saveTimer = undefined; void save() }, 700)
}
watch(() => [draft.body, draft.title, draft.board, draft.quote, draft.attachments], scheduleSave, { deep: true })
// App 接受回传后才进入 busy；失败恢复 false 时重新允许卸载补存，避免最后改动丢失。
watch(() => props.busy, (busy) => { submitted = !!busy })
// App 在首次写入远端前/完成后更新持久化状态，关闭重开也不能丢掉重复回传保护。
watch(() => props.draft?.transferState, (state) => { if (state) draft.transferState = state })

function pushHistory() {
  if (applyingHistory) return
  const value = draft.body
  if (history.value[historyIndex.value] === value) return
  history.value = history.value.slice(0, historyIndex.value + 1)
  history.value.push(value)
  if (history.value.length > 120) history.value.shift()
  historyIndex.value = history.value.length - 1
}
function onInput(e: Event) {
  if (locked.value) return
  draft.body = normalizeLines((e.target as HTMLTextAreaElement).value)
  pushHistory()
  localError.value = ''
}
async function setBody(value: string, start = value.length, end = start) {
  if (locked.value) return
  draft.body = value
  pushHistory()
  await nextTick()
  editor.value?.focus()
  editor.value?.setSelectionRange(start, end)
}
function undo() {
  if (locked.value || historyIndex.value <= 0) return
  historyIndex.value--
  applyingHistory = true
  draft.body = history.value[historyIndex.value] || ''
  applyingHistory = false
  void nextTick(() => editor.value?.focus())
}
function redo() {
  if (locked.value || historyIndex.value >= history.value.length - 1) return
  historyIndex.value++
  applyingHistory = true
  draft.body = history.value[historyIndex.value] || ''
  applyingHistory = false
  void nextTick(() => editor.value?.focus())
}
function onEditorKey(e: KeyboardEvent) {
  if (!(e.ctrlKey || e.metaKey)) return
  if (e.key.toLowerCase() === 'z') { e.preventDefault(); e.shiftKey ? redo() : undo() }
  else if (e.key.toLowerCase() === 'y') { e.preventDefault(); redo() }
  else if (e.key.toLowerCase() === 'f') { e.preventDefault(); findOpen.value = true }
}
function replaceSelection(value: string) {
  const el = editor.value
  const text = selectedText()
  if (!el || !text) { localError.value = '请先在正文中框选要变换的文字'; return }
  const start = el.selectionStart
  void setBody(draft.body.slice(0, start) + value + draft.body.slice(el.selectionEnd), start + value.length)
}
function transform(kind: 'sep' | 'random' | 'mixed' | 'vertical') {
  const text = selectedText()
  if (!text) { localError.value = '请先在正文中框选要变换的文字'; return }
  try {
    if (kind === 'vertical') {
      const result = toVertical(text, { cols: props.cols })
      replaceSelection(result.text)
      if (result.note) localError.value = result.note
    } else {
      const mode = kind === 'sep' ? 'fixed' : kind === 'random' ? 'randomOne' : 'randomMixed'
      const seps = kind === 'sep' ? [SEP_DEFAULT] : SEP_PRESETS.map((p) => p.ch)
      replaceSelection(applySeparators(text, { seps, mode }))
      const skipped = normalizeLines(text).split('\n').filter(isQuoteLine).length
      if (skipped) localError.value = `已跳过 ${skipped} 行引用文字`
    }
  } catch (e: any) { localError.value = e?.message || String(e) }
}
function insertQuote() {
  const quote = draft.quote || ''
  if (!quote.trim()) { localError.value = '没有可用的最近文章引用'; return }
  const quoted = quote.split('\n').map((line) => `> ${line}`).join('\n')
  const el = editor.value
  const pos = el ? el.selectionStart : draft.body.length
  const prefix = pos > 0 && draft.body[pos - 1] !== '\n' ? '\n' : ''
  void setBody(draft.body.slice(0, pos) + prefix + quoted + draft.body.slice(pos), pos + prefix.length + quoted.length)
}
function findNext() {
  if (!find.text) return
  const source = find.caseSensitive ? draft.body : draft.body.toLowerCase()
  const needle = find.caseSensitive ? find.text : find.text.toLowerCase()
  const el = editor.value
  const from = el ? Math.max(el.selectionEnd, 0) : 0
  let index = source.indexOf(needle, from)
  if (index < 0) index = source.indexOf(needle)
  if (index < 0) { localError.value = '没有找到匹配文字'; return }
  localError.value = ''
  editor.value?.focus()
  editor.value?.setSelectionRange(index, index + find.text.length)
}
function escapeRegExp(text: string): string { return text.replace(/[.*+?^${}()|[\]\\]/g, '\\$&') }
function replaceOne() {
  const el = editor.value
  if (!el || !find.text) return
  const matches = (value: string) => find.caseSensitive ? value === find.text : value.toLowerCase() === find.text.toLowerCase()
  if (!matches(draft.body.slice(el.selectionStart, el.selectionEnd))) findNext()
  if (!editor.value || !matches(draft.body.slice(editor.value.selectionStart, editor.value.selectionEnd))) return
  const start = editor.value.selectionStart
  void setBody(draft.body.slice(0, start) + find.replacement + draft.body.slice(editor.value.selectionEnd), start + find.replacement.length)
}
function replaceAll() {
  if (!find.text) return
  const re = new RegExp(escapeRegExp(find.text), find.caseSensitive ? 'g' : 'gi')
  const count = (draft.body.match(re) || []).length
  if (!count) { localError.value = '没有找到匹配文字'; return }
  void setBody(draft.body.replace(re, () => find.replacement))
  localError.value = `已替换 ${count} 处`
}
function jumpLine() {
  const answer = window.prompt('跳到第几行？', '1')
  if (!answer) return
  const n = Number(answer)
  if (!Number.isInteger(n) || n < 1 || n > stats.value.lines) { localError.value = '行号超出正文范围'; return }
  const lines = draft.body.split('\n')
  const pos = lines.slice(0, n - 1).reduce((sum, line) => sum + line.length + 1, 0)
  editor.value?.focus()
  editor.value?.setSelectionRange(pos, pos + lines[n - 1].length)
}
// 选文件、粘贴 CF_HDROP、拖放文件共用同一条校验/入库链路。
async function addPaths(paths: string[]): Promise<boolean> {
  if (locked.value || addingPaths.value) return false
  const pending = newAttachmentPaths(paths, draft.attachments)
  if (!pending.length) return false
  const room = MAX_EDITOR_ATTACHMENTS - draft.attachments.length
  if (pending.length > room) {
    localError.value = `一次最多 ${MAX_EDITOR_ATTACHMENTS} 个附件，当前还能添加 ${room} 个`
    return false
  }
  addingPaths.value = true
  localError.value = ''
  try {
    for (const path of pending) {
      const stat = await api.FileStat(path)
      if (stat.isDir) throw new Error(`不能添加目录：${stat.name}`)
      draft.attachments.push({ name: stat.name, path: stat.path, size: stat.size, generated: false })
    }
    return true
  } catch (e: any) {
    localError.value = e?.message || String(e)
    return false
  } finally {
    addingPaths.value = false
  }
}

async function pickFiles() {
  if (locked.value || picking.value) return
  picking.value = true
  localError.value = ''
  try {
    // 原生文件框会另开 HWND 并让主窗失焦，quake 模式下需要同样的失焦护栏。
    await api.SetDialogGuard(true).catch(() => { /* 老后端没有此接口时不影响选文件 */ })
    let paths: string[]
    try { paths = await api.PickEditorFiles() } finally {
      setTimeout(() => { api.SetDialogGuard(false).catch(() => { /* noop */ }) }, 250)
    }
    await addPaths(paths || [])
  } catch (e: any) { localError.value = e?.message || String(e) }
  finally { picking.value = false }
}

async function onPaste(e: ClipboardEvent) {
  if (locked.value) return
  // 必须同步阻止默认粘贴：后端读剪贴板是异步的，等结果回来再 preventDefault
  // 已经晚了一步。没有附件时下面手动插回文字，行为与普通粘贴一致。
  e.preventDefault()
  if (pasting.value) return
  const fallbackText = e.clipboardData?.getData('text/plain') || ''
  pasting.value = true
  try {
    const data = await api.ReadEditorClipboard()
    if (locked.value) return
    if (data.files?.length) {
      await addPaths(data.files)
      return
    }
    if (data.imageBase64) {
      if (draft.attachments.length >= MAX_EDITOR_ATTACHMENTS) {
        localError.value = `一次最多 ${MAX_EDITOR_ATTACHMENTS} 个附件`
        return
      }
      const path = await api.SaveEditorImage(clipboardImageFileName(new Date()), data.imageBase64)
      const stat = await api.FileStat(path)
      appendAttachment({ name: stat.name, path: stat.path, size: stat.size, generated: true })
      return
    }
    if (fallbackText) {
      const el = editor.value
      const start = el?.selectionStart ?? draft.body.length
      const end = el?.selectionEnd ?? start
      const text = normalizeLines(fallbackText)
      void setBody(draft.body.slice(0, start) + text + draft.body.slice(end), start + text.length)
    }
  } catch (err: any) {
    localError.value = `粘贴附件失败：${err?.message || err}`
    if (fallbackText) {
      const el = editor.value
      const start = el?.selectionStart ?? draft.body.length
      const end = el?.selectionEnd ?? start
      const text = normalizeLines(fallbackText)
      void setBody(draft.body.slice(0, start) + text + draft.body.slice(end), start + text.length)
    }
  } finally {
    pasting.value = false
  }
}

function onDragOver(e: DragEvent) {
  if (locked.value || !Array.from(e.dataTransfer?.types || []).includes('Files')) return
  e.preventDefault()
  if (e.dataTransfer) e.dataTransfer.dropEffect = 'copy'
  dragActive.value = true
}

async function onDrop(e: DragEvent) {
  dragActive.value = false
  const paths = Array.from(e.dataTransfer?.files || [])
    .map((file) => (file as File & { path?: string }).path || '')
    .filter(Boolean)
  if (!paths.length) return // 文字拖拽仍走浏览器默认插入。
  e.preventDefault()
  await addPaths(paths)
}
function removeAttachment(index: number) { if (!locked.value) draft.attachments.splice(index, 1) }
function openImage() {
  const el = editor.value
  const start = el?.selectionStart ?? 0
  const end = el?.selectionEnd ?? start
  const selection = selectedText()
  if (selection) {
    imageText.value = selection
    imageSource = { start, end, text: selection }
  } else if (draft.body.length <= 4000) {
    imageText.value = draft.body
    imageSource = draft.body ? { start: 0, end: draft.body.length, text: draft.body } : null
  } else {
    imageText.value = ''
    imageSource = null
  }
  imageOpen.value = true
}
function appendAttachment(a: api.EditorAttachment): boolean {
  if (locked.value) return false
  if (draft.attachments.length >= MAX_EDITOR_ATTACHMENTS) { localError.value = `一次最多 ${MAX_EDITOR_ATTACHMENTS} 个附件`; return false }
  if (draft.attachments.some((item) => item.path.toLowerCase() === a.path.toLowerCase())) {
    localError.value = '该附件已经添加'
    return false
  }
  draft.attachments.push(a)
  return true
}

function addAttachment(a: api.EditorAttachment): boolean {
  if (!appendAttachment(a)) return false
  imageOpen.value = false
  const source = imageSource
  imageSource = null
  if (source) {
    if (draft.body.slice(source.start, source.end) === source.text) {
      const body = draft.body.slice(0, source.start) + draft.body.slice(source.end)
      void setBody(body, source.start)
    } else {
      localError.value = '图片已添加，但原文已变化，未自动清空'
    }
  }
  return true
}
async function submit() {
  if (locked.value || (props.context && handoffBlocked.value)) return
  localError.value = ''
  submitting.value = true
  try {
    if (saveTimer) { window.clearTimeout(saveTimer); saveTimer = undefined }
    if (!await save()) { localError.value = '草稿尚未保存成功，请稍后重试'; return }
    handedOff = true
    emit('submit', snapshot())
  } finally {
    submitting.value = false
  }
}
async function close() {
  if (locked.value) return
  closing.value = true
  try {
    if (saveTimer) { window.clearTimeout(saveTimer); saveTimer = undefined }
    if (!submitted && !await save()) {
      localError.value = '最后改动尚未保存成功，已保留编辑器；请重试关闭或提交'
      return
    }
    emit('close')
  } finally {
    closing.value = false
  }
}
function fmtSize(n: number): string { return n >= 1048576 ? `${(n / 1048576).toFixed(1)} MiB` : `${Math.max(1, Math.round(n / 1024))} KiB` }

/** 遮罩不是关闭按钮：按下时阻止浏览器把焦点交给空白层，再主动交还正文。 */
function keepEditorFocus(e: MouseEvent) {
  if (locked.value) return
  e.preventDefault()
  editor.value?.focus({ preventScroll: true })
}

onMounted(() => {
  history.value = [draft.body]
  historyIndex.value = 0
  offEditorDrop = api.onEditorFilesDropped((event) => {
    if (!disposed && !locked.value) void addPaths(event.files || [])
  })
  nextTick(() => editor.value?.focus())
})
onBeforeUnmount(() => {
  offEditorDrop?.()
  offEditorDrop = null
  if (saveTimer) { window.clearTimeout(saveTimer); saveTimer = undefined }
  // 正常关闭已 await 最后保存；这里只兜底应用退出/父组件直接卸载。
  if (!submitted && !handedOff) void save()
  disposed = true
})
</script>

<template>
  <div class="mask" @mousedown.self="keepEditorFocus" @click.self="keepEditorFocus">
    <div class="dlg" role="dialog" aria-modal="true" aria-label="本地文章编辑器" @keydown.esc.stop.prevent="close">
      <div class="head">
        <span>本地文章编辑器 <i>{{ context ? (context.mode === 'reply' ? '回帖' : '发文') : '草稿' }}</i></span>
        <button class="x" :disabled="locked" @click="close">✕</button>
      </div>
      <div class="meta">
        <label>站点<input :value="draft.siteName" disabled></label>
        <label>版面<input v-model="draft.board" :disabled="locked" placeholder="（未识别）"></label>
        <label>标题<input v-model="draft.title" :disabled="locked || !!context" :title="context ? '回传只修改正文，标题以 BBS 发文设置为准' : '文章标题'"></label>
      </div>
      <div class="toolbar">
        <button :disabled="locked || historyIndex <= 0" @click="undo" title="Ctrl+Z">撤销</button>
        <button :disabled="locked || historyIndex >= history.length - 1" @click="redo" title="Ctrl+Y">重做</button>
        <button :disabled="locked" @click="findOpen = !findOpen">查找 / 替换</button>
        <button :disabled="locked" @click="jumpLine">跳转行</button>
        <span class="sep" />
        <button :disabled="locked" @click="transform('sep')">插入 ^</button>
        <button :disabled="locked" @click="transform('random')">随机分隔</button>
        <button :disabled="locked" @click="transform('mixed')">逐字随机</button>
        <button :disabled="locked" @click="transform('vertical')">转竖排</button>
        <button :disabled="locked || !draft.quote?.trim()" @click="insertQuote">插入最近引用</button>
        <span class="sep" />
        <button :disabled="locked || picking" @click="pickFiles">{{ picking ? '选择中…' : '添加附件' }}</button>
        <button :disabled="locked" @click="openImage">文字转图片</button>
      </div>
      <div v-if="findOpen" class="findbar">
        <input v-model="find.text" :disabled="locked" placeholder="查找" @keydown.enter="findNext">
        <input v-model="find.replacement" :disabled="locked" placeholder="替换为">
        <label><input v-model="find.caseSensitive" :disabled="locked" type="checkbox">区分大小写</label>
        <button :disabled="locked" @click="findNext">下一个</button><button :disabled="locked" @click="replaceOne">替换</button><button :disabled="locked" @click="replaceAll">全部替换</button>
        <button @click="findOpen = false">收起</button>
      </div>
      <div class="attachments" v-if="draft.attachments.length">
        <div v-for="(a, i) in draft.attachments" :key="a.path" class="attachment">
          <span>{{ a.generated ? '[图片]' : '[附件]' }} {{ a.name }}</span><span>{{ fmtSize(a.size) }}</span><button :disabled="locked" @click="removeAttachment(i)">移除</button>
        </div>
      </div>
      <div class="warning" v-if="context && !context.canUploadAttachments && draft.attachments.length">
        当前页面不支持自动上传附件：需停留在发文设置页，并在站点配置中启用 ZMODEM。
      </div>
      <textarea
        ref="editor"
        class="editor"
        :class="{ dragging: dragActive }"
        :value="draft.body"
        :disabled="locked"
        data-file-drop-target="editor-attachments"
        spellcheck="false"
        wrap="soft"
        title="Ctrl+V 可粘贴文件或剪贴板图片；也可把文件拖到这里作为附件"
        @input="onInput"
        @keydown="onEditorKey"
        @paste="onPaste"
        @dragover="onDragOver"
        @dragleave="dragActive = false"
        @drop="onDrop"
      />
      <div class="error" v-if="localError || error || handoffWarning">{{ localError || error || handoffWarning }}</div>
      <div class="foot">
        <span class="save-state" :class="saveState">{{ saveState === 'saving' ? '正在自动保存…' : saveState === 'saved' ? '草稿已自动保存' : '草稿保存失败' }} · {{ stats.chars }} 字符 · {{ stats.lines }} 行</span>
        <div class="actions">
          <button :disabled="locked" @click="close">关闭</button>
          <button class="primary" :disabled="locked || (!!context && handoffBlocked)" @click="submit">
            {{ locked ? '正在回传…' : context && handoffBlocked ? '已开始回传' : context ? '保存并回传' : '保存草稿' }}
          </button>
        </div>
      </div>
      <TextImageDialog v-if="imageOpen" :initial-text="imageText" @add="addAttachment" @close="imageOpen = false" />
    </div>
  </div>
</template>

<style scoped>
.mask { position: fixed; inset: 0; z-index: 50; background: rgba(0,0,0,.58); display: flex; align-items: center; justify-content: center; }
.dlg { width: min(1100px, 96vw); height: min(820px, 92vh); display: flex; flex-direction: column; background: #24272e; border: 1px solid #3d424b; border-radius: 10px; color: #d5d9e0; box-shadow: 0 12px 40px rgba(0,0,0,.5); }
.head { display: flex; justify-content: space-between; align-items: center; padding: 10px 14px; font-weight: 600; border-bottom: 1px solid #33373f; }
.head i { margin-left: 8px; color: #6ab0f3; font-size: 12px; font-style: normal; }
.x { background: none; border: none; color: #8a919e; cursor: pointer; }
.meta { display: grid; grid-template-columns: 1fr 180px 1.4fr; gap: 10px; padding: 10px 14px 6px; }
label { color: #8a919e; font-size: 12px; }
input, textarea { box-sizing: border-box; background: #1b1d22; color: #d5d9e0; border: 1px solid #3d424b; border-radius: 5px; padding: 6px 8px; font: inherit; }
.meta label { display: flex; align-items: center; gap: 6px; }
.meta input { min-width: 0; flex: 1; }
.toolbar, .findbar { display: flex; align-items: center; flex-wrap: wrap; gap: 5px; padding: 6px 14px; }
.toolbar { border-bottom: 1px solid #33373f; }
.toolbar button, .findbar button { padding: 4px 8px; font-size: 12px; }
.toolbar .sep { width: 1px; height: 18px; background: #3d424b; margin: 0 3px; }
.findbar { background: #202329; border-bottom: 1px solid #33373f; }
.findbar input:not([type=checkbox]) { width: 150px; padding: 4px 6px; }
.findbar label { display: flex; align-items: center; gap: 4px; }
.findbar input[type=checkbox] { width: auto; }
.attachments { max-height: 90px; overflow: auto; padding: 4px 14px; background: #202329; border-bottom: 1px solid #33373f; }
.attachment { display: flex; align-items: center; gap: 10px; padding: 3px 0; font-size: 12px; }
.attachment span:first-child { flex: 1; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.attachment span:nth-child(2) { color: #8a919e; }
.attachment button { padding: 2px 7px; font-size: 11px; }
.warning { padding: 7px 14px; color: #e0b341; background: #352f20; font-size: 12px; }
.editor { flex: 1; min-height: 120px; resize: none; border: none; border-radius: 0; padding: 14px 16px; outline: none; font: 14px/1.65 Consolas, "Microsoft YaHei UI", monospace; tab-size: 4; }
.editor.dragging { background: #202c3b; box-shadow: inset 0 0 0 2px #6ab0f3; }
.error { padding: 6px 14px; color: #e06c5b; background: #352326; font-size: 12px; white-space: pre-wrap; }
.foot { display: flex; justify-content: space-between; align-items: center; gap: 10px; padding: 10px 14px; border-top: 1px solid #33373f; }
.save-state { color: #8a919e; font-size: 12px; }
.save-state.saving { color: #e0b341; }.save-state.error { color: #e06c5b; }
.actions { display: flex; gap: 8px; }
button { background: #2f333b; color: #d5d9e0; border: 1px solid #3d424b; border-radius: 5px; padding: 6px 14px; cursor: pointer; }
button.primary { background: #2f74c0; border-color: #2f74c0; color: white; }
button:disabled { opacity: .45; cursor: default; }
</style>
