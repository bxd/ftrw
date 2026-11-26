<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import * as api from '../api'
import { renderTextImage, TEXT_IMAGE_DEFAULTS, TEXT_IMAGE_PRESETS, textImageFileName, textImagePresetOptions, type TextImageStyleId } from '../textimage'

const props = defineProps<{ initialText: string }>()
const emit = defineEmits<{
  (e: 'add', attachment: api.EditorAttachment): void
  (e: 'close'): void
}>()

const opts = reactive({
  text: props.initialText,
  ...TEXT_IMAGE_DEFAULTS,
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

function pickStyle(id: TextImageStyleId) {
  Object.assign(opts, textImagePresetOptions(id))
  scheduleDraw()
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
    const path = await api.SaveEditorImage(textImageFileName(new Date()), result.dataUrl)
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
          <div class="style-picker">
            <div class="section-title">样式方案</div>
            <div class="styles">
              <button v-for="preset in TEXT_IMAGE_PRESETS" :key="preset.id" type="button"
                      class="style" :class="[`style-${preset.id}`, { on: opts.style === preset.id }]"
                      :aria-pressed="opts.style === preset.id" @click="pickStyle(preset.id)">
                <span class="swatch" :class="`swatch-${preset.id}`"
                      :style="{ background: preset.cardBackground, color: preset.foreground, borderColor: preset.borderColor }">Aa</span>
                <span class="style-copy"><strong>{{ preset.name }}</strong><small>{{ preset.description }}</small></span>
              </button>
            </div>
          </div>
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
.style-picker { display: flex; flex-direction: column; gap: 6px; padding-bottom: 10px; border-bottom: 1px solid #33373f; }
.section-title { color: #8a919e; font-size: 12px; }
.styles { display: grid; grid-template-columns: 1fr 1fr; gap: 7px; }
.style { position: relative; isolation: isolate; overflow: hidden; display: flex; align-items: center; gap: 8px; min-width: 0; padding: 8px 7px; text-align: left; background: linear-gradient(135deg, rgba(255,255,255,.045), rgba(255,255,255,.015)); transition: border-color .15s, box-shadow .15s, transform .15s; }
.style::after { content: ''; position: absolute; z-index: -1; inset: 0; opacity: 0; background: linear-gradient(115deg, transparent 12%, rgba(255,255,255,.1) 46%, transparent 78%); transform: translateX(-55%); transition: opacity .15s, transform .25s; pointer-events: none; }
.style:hover { border-color: #667085; transform: translateY(-1px); }
.style:hover::after, .style.on::after { opacity: 1; transform: translateX(45%); }
.style.on { border-color: #7dd3fc; background: linear-gradient(135deg, #263b50, #29263f); box-shadow: inset 0 0 0 1px rgba(125,211,252,.55), 0 0 14px rgba(56,189,248,.12); }
.style-classic.on { border-color: #35e7ff; box-shadow: inset 0 0 0 1px rgba(53,231,255,.5), 0 0 14px rgba(53,231,255,.14); }
.style-gold.on { border-color: #d6af62; background: linear-gradient(135deg, #3b3020, #242017); box-shadow: inset 0 0 0 1px rgba(214,175,98,.45), 0 0 14px rgba(214,175,98,.12); }
.style-glitch.on { border-color: #ff4fd8; background: linear-gradient(135deg, #25213c, #351c33); box-shadow: inset 0 0 0 1px rgba(255,79,216,.45), 2px 0 0 rgba(85,246,255,.18); }
.swatch { position: relative; overflow: hidden; flex: 0 0 36px; width: 36px; height: 36px; display: grid; place-items: center; border: 1px solid; font: 600 13px/1 "Segoe UI", sans-serif; }
.swatch-classic { box-shadow: inset 0 0 12px rgba(53,231,255,.12), 0 0 9px rgba(53,231,255,.24); }
.swatch-aurora { border-radius: 10px; box-shadow: inset 0 0 0 3px rgba(255,255,255,.07), 0 0 12px rgba(103,232,249,.2); backdrop-filter: blur(3px); }
.swatch-gold { border-radius: 2px; box-shadow: inset 0 0 0 3px #100d09, inset 0 0 0 4px rgba(214,175,98,.5), 0 0 10px rgba(214,175,98,.18); }
.swatch-glitch { border-radius: 0; text-shadow: -2px 0 #55f6ff, 2px 0 #ff4fd8; }
.swatch-glitch::after { content: ''; position: absolute; inset: 0; background: repeating-linear-gradient(0deg, rgba(0,0,0,.16) 0 1px, transparent 1px 4px); pointer-events: none; }
.style-copy { min-width: 0; display: flex; flex-direction: column; gap: 3px; }
.style-copy strong { color: #d5d9e0; font-size: 12px; font-weight: 600; }
.style-copy small { color: #8a919e; font-size: 10px; line-height: 1.35; }
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
