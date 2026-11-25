<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import * as api from '../api'
import { renderTextImage } from '../textimage'

const props = defineProps<{ initialText: string }>()
const emit = defineEmits<{
  (e: 'add', attachment: api.EditorAttachment): void
  (e: 'close'): void
}>()

const opts = reactive({
  text: props.initialText,
  width: 800,
  fontSize: 24,
  foreground: '#202020',
  background: '#ffffff',
  padding: 28,
  lineGap: 8,
})
const preview = ref('')
const size = ref({ width: 0, height: 0 })
const busy = ref(false)
const error = ref('')
let renderSeq = 0

async function redraw() {
  const seq = ++renderSeq
  try {
    const result = await renderTextImage({ ...opts, text: opts.text })
    if (seq !== renderSeq) return
    preview.value = result.dataUrl
    size.value = { width: result.width, height: result.height }
    error.value = ''
  } catch (e: any) {
    if (seq === renderSeq) {
      preview.value = ''
      error.value = e?.message || String(e)
    }
  }
}

let timer: number | undefined
function scheduleDraw() {
  if (timer) window.clearTimeout(timer)
  timer = window.setTimeout(() => { timer = undefined; void redraw() }, 180)
}

async function add() {
  if (busy.value) return
  busy.value = true
  error.value = ''
  try {
    const result = await renderTextImage({ ...opts, text: opts.text })
    const now = new Date()
    const stamp = `${now.getFullYear()}${String(now.getMonth() + 1).padStart(2, '0')}${String(now.getDate()).padStart(2, '0')}-${String(now.getHours()).padStart(2, '0')}${String(now.getMinutes()).padStart(2, '0')}${String(now.getSeconds()).padStart(2, '0')}`
    const path = await api.SaveEditorImage(`文字图片-${stamp}`, result.dataUrl)
    const stat = await api.FileStat(path)
    emit('add', { name: stat.name, path: stat.path, size: stat.size, generated: true })
  } catch (e: any) {
    error.value = e?.message || String(e)
  } finally {
    busy.value = false
  }
}

const approximateSize = computed(() => preview.value ? Math.ceil((preview.value.length - 22) * 3 / 4) : 0)
onMounted(() => { void redraw() })
onBeforeUnmount(() => { if (timer) window.clearTimeout(timer); renderSeq++ })
</script>

<template>
  <div class="mask" @click.self="emit('close')">
    <div class="dlg" role="dialog" aria-modal="true" aria-label="文字转图片附件" @keydown.esc.stop.prevent="!busy && emit('close')">
      <div class="head"><span>文字转图片附件</span><button class="x" @click="!busy && emit('close')">✕</button></div>
      <div class="body">
        <div class="controls">
          <label>文字<textarea v-model="opts.text" rows="9" @input="scheduleDraw" /></label>
          <div class="grid">
            <label>最大宽度<input v-model.number="opts.width" type="number" min="320" max="1600" step="80" @input="scheduleDraw"></label>
            <label>字号<input v-model.number="opts.fontSize" type="number" min="12" max="48" @input="scheduleDraw"></label>
            <label>留白<input v-model.number="opts.padding" type="number" min="8" max="80" @input="scheduleDraw"></label>
            <label>行距<input v-model.number="opts.lineGap" type="number" min="0" max="40" @input="scheduleDraw"></label>
            <label>文字颜色<input v-model="opts.foreground" type="color" @input="scheduleDraw"></label>
            <label>背景颜色<input v-model="opts.background" type="color" @input="scheduleDraw"></label>
          </div>
          <p class="hint">生成 PNG 后作为当前文章的附件上传，并自动清除编辑器中对应的源文字。图片按实际文字收缩，最长 4096px 高、8 MiB。</p>
        </div>
        <div class="preview">
          <img v-if="preview" :src="preview" alt="文字图片预览">
          <p v-else class="empty">等待生成预览…</p>
          <span v-if="preview">{{ size.width }}×{{ size.height }} · {{ (approximateSize / 1024).toFixed(1) }} KiB</span>
        </div>
      </div>
      <div class="error" v-if="error">{{ error }}</div>
      <div class="foot"><button class="primary" :disabled="busy || !preview" @click="add">{{ busy ? '生成中…' : '生成并添加附件' }}</button><button :disabled="busy" @click="emit('close')">取消</button></div>
    </div>
  </div>
</template>

<style scoped>
.mask { position: fixed; inset: 0; z-index: 70; background: rgba(0,0,0,.6); display: flex; align-items: center; justify-content: center; }
.dlg { width: min(1000px, 94vw); max-height: 90vh; overflow: auto; background: #24272e; border: 1px solid #3d424b; border-radius: 10px; color: #d5d9e0; }
.head, .foot { display: flex; align-items: center; justify-content: space-between; padding: 12px 16px; }
.head { font-weight: 600; border-bottom: 1px solid #33373f; }
.foot { justify-content: flex-end; gap: 8px; border-top: 1px solid #33373f; }
.x { background: none; border: none; color: #8a919e; cursor: pointer; }
.body { display: grid; grid-template-columns: 330px 1fr; gap: 16px; padding: 16px; }
.controls, .grid { display: flex; flex-direction: column; gap: 8px; }
.grid { display: grid; grid-template-columns: 1fr 1fr; }
label { display: flex; flex-direction: column; gap: 4px; color: #8a919e; font-size: 12px; }
input, textarea { width: 100%; box-sizing: border-box; background: #1b1d22; color: #d5d9e0; border: 1px solid #3d424b; border-radius: 5px; padding: 6px 8px; font: inherit; }
textarea { resize: vertical; line-height: 1.5; }
input[type=color] { height: 30px; padding: 2px; }
.preview { min-width: 0; display: flex; flex-direction: column; align-items: center; justify-content: center; gap: 8px; color: #8a919e; font-size: 12px; }
.preview img { max-width: 100%; max-height: 62vh; object-fit: contain; box-shadow: 0 4px 18px rgba(0,0,0,.4); }
.empty { color: #6a7180; }
.hint, .error { color: #6a7180; font-size: 12px; line-height: 1.5; }
.error { padding: 0 16px 12px; color: #e06c5b; }
button { background: #2f333b; color: #d5d9e0; border: 1px solid #3d424b; border-radius: 5px; padding: 6px 14px; cursor: pointer; }
button.primary { background: #2f74c0; border-color: #2f74c0; color: white; }
button:disabled { opacity: .5; cursor: default; }
@media (max-width: 760px) { .body { grid-template-columns: 1fr; } .preview { max-height: 40vh; } }
</style>
