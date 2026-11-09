<script setup lang="ts">
/**
 * 预览窗口里「媒体模式」的那一页。
 *
 * 网页模式不会走到这里 —— 那时后端把整个 webview 顶层导航到对方站点（水木一类
 * 发 X-Frame-Options 的页面嵌套不进来，只能这么绕），这一页的脚本也就不在场了。
 * 所以这个组件只管三件事：放图片/音频/视频、在这一屏采到的媒体之间循环、ESC 关窗。
 */
import { computed, nextTick, onBeforeUnmount, onMounted, ref } from 'vue'
import * as api from './api'

type State = 'empty' | 'loading' | 'ready' | 'error'

const items = ref<api.PreviewItem[]>([])
const index = ref(0)
const state = ref<State>('empty')
const maxed = ref(false)
const saving = ref(false)
const note = ref('')
const noteErr = ref(false)

/* --- 图片视图：滚轮缩放 + 左键平移 ------------------------------------------ */

// scale 是「相对适应窗口」的倍率，natScale 是「适应窗口 → 1:1」还差多少。给人看的
// 百分比用 scale / natScale：这样 100% 就是原始像素，跟图片查看器的习惯一致。
const stageEl = ref<HTMLElement | null>(null)
const imgEl = ref<HTMLImageElement | null>(null)
const scale = ref(1)
const natScale = ref(1)
const panX = ref(0)
const panY = ref(0)
const dragging = ref(false)

const SCALE_LO = 0.2
const SCALE_HI = 30

const cur = computed<api.PreviewItem | null>(() => items.value[index.value] ?? null)
const total = computed(() => items.value.length)
const isImage = computed(() => cur.value?.kind === 'image')
const kindLabel = computed(() => ({ image: '图片', audio: '音频', video: '视频', page: '网页' }[cur.value?.kind || 'page'] || '链接'))
const pct = computed(() => Math.round((scale.value / (natScale.value || 1)) * 100))
const canPan = computed(() => isImage.value && scale.value > 1.001)
const imgStyle = computed(() => {
  if (scale.value === 1 && !panX.value && !panY.value) return {}
  return { transform: `translate(${panX.value}px, ${panY.value}px) scale(${scale.value})` }
})

function resetView() {
  scale.value = 1
  natScale.value = 1
  panX.value = 0
  panY.value = 0
}

/** 缩放后图片可能比舞台小，或者被拖出界：把平移量钳在「至少还能看见中心」的范围里。 */
function clampPan() {
  const st = stageEl.value
  const el = imgEl.value
  if (!st || !el) return
  const limX = Math.max(0, (el.clientWidth * scale.value - st.clientWidth) / 2)
  const limY = Math.max(0, (el.clientHeight * scale.value - st.clientHeight) / 2)
  panX.value = Math.min(limX, Math.max(-limX, panX.value))
  panY.value = Math.min(limY, Math.max(-limY, panY.value))
}

/** 以光标为不动点缩放：光标底下那个像素缩放前后得还在光标底下。 */
function zoomBy(mult: number, cx?: number, cy?: number) {
  const st = stageEl.value
  if (!st) return
  const old = scale.value
  const next = Math.min(SCALE_HI, Math.max(SCALE_LO, old * mult))
  if (next === old) return
  if (cx !== undefined && cy !== undefined) {
    const r = st.getBoundingClientRect()
    const ox = cx - (r.left + r.width / 2)
    const oy = cy - (r.top + r.height / 2)
    const k = next / old
    panX.value = ox - k * (ox - panX.value)
    panY.value = oy - k * (oy - panY.value)
  }
  scale.value = next
  clampPan()
}

function setActual() {
  if (!isImage.value) return
  scale.value = natScale.value || 1
  clampPan()
}

function fitView() {
  scale.value = 1
  panX.value = 0
  panY.value = 0
}

function toggleFitActual() {
  if (scale.value === 1) setActual()
  else fitView()
}

function onWheel(ev: WheelEvent) {
  if (!isImage.value || state.value !== 'ready') return
  // 触控板的捏合是 ctrl+wheel，步长得比滚轮细得多
  const mult = ev.deltaY < 0 ? (ev.ctrlKey ? 1.04 : 1.18) : 1 / (ev.ctrlKey ? 1.04 : 1.18)
  zoomBy(mult, ev.clientX, ev.clientY)
}

let drag: { x: number; y: number; px: number; py: number } | null = null

function onDown(ev: MouseEvent) {
  if (ev.button !== 0 || !canPan.value) return
  drag = { x: ev.clientX, y: ev.clientY, px: panX.value, py: panY.value }
  dragging.value = true
  window.addEventListener('mousemove', onMove)
  window.addEventListener('mouseup', onUp)
  ev.preventDefault()
}

function onMove(ev: MouseEvent) {
  if (!drag) return
  panX.value = drag.px + (ev.clientX - drag.x)
  panY.value = drag.py + (ev.clientY - drag.y)
  clampPan()
}

function onUp() {
  drag = null
  dragging.value = false
  window.removeEventListener('mousemove', onMove)
  window.removeEventListener('mouseup', onUp)
}

/* --- 条目切换 --------------------------------------------------------------- */

function show(req: api.PreviewRequest | null | undefined) {
  if (!req || !req.items?.length) return
  items.value = req.items
  index.value = Math.min(Math.max(0, req.index), req.items.length - 1)
  restart()
}

/** 换一条就重来一遍载入状态：媒体元素靠 :key 重建，事件会重新派发。 */
function restart() {
  resetView()
  setNote('')
  state.value = cur.value ? 'loading' : 'empty'
}

function step(delta: number) {
  if (total.value < 2) return
  index.value = (index.value + delta + total.value) % total.value
  restart()
}

function jump(i: number) {
  if (!total.value) return
  index.value = Math.min(total.value - 1, Math.max(0, i))
  restart()
}

async function close() {
  try {
    await api.ClosePreview()
  } catch {
    /* 窗口可能已经被关掉 */
  }
}

async function openExternal() {
  if (!cur.value) return
  try {
    await api.OpenExternalURL(cur.value.url)
  } catch {
    state.value = 'error'
  }
}

function onReady() {
  state.value = 'ready'
  if (cur.value?.kind !== 'image') return
  nextTick(() => {
    const el = imgEl.value
    if (!el) return
    // clientWidth 不受 transform 影响：拿它跟原始像素一比，就是 1:1 该放的倍率
    const fw = el.clientWidth
    const nw = el.naturalWidth
    natScale.value = fw > 0 && nw > 0 ? nw / fw : 1
  })
}

function onError() { state.value = 'error' }

/* --- 另存为 / 最大化 -------------------------------------------------------- */

let noteTimer: number | undefined

function setNote(text: string, err = false) {
  if (noteTimer !== undefined) {
    window.clearTimeout(noteTimer)
    noteTimer = undefined
  }
  note.value = text
  noteErr.value = err
  if (text) noteTimer = window.setTimeout(() => { note.value = '' }, 8000)
}

async function saveAs() {
  const it = cur.value
  if (!it || saving.value) return
  saving.value = true
  setNote('保存中…')
  try {
    const path = await api.SavePreviewItem(it.url, it.name)
    setNote(path ? `已保存到 ${path}` : '已取消保存')
  } catch (e) {
    setNote(`保存失败：${e}`, true)
  } finally {
    saving.value = false
  }
}

async function toggleMax() {
  try {
    // 状态以后端 IsMaximised 为准：Win+方向键贴边也能改，前端自己记一定会走偏
    maxed.value = await api.TogglePreviewMaximise()
  } catch {
    /* 窗口可能已经被关掉 */
  }
}

/* --- 键盘 ------------------------------------------------------------------- */

function onKey(ev: KeyboardEvent) {
  switch (ev.key) {
    case 'Escape':
      // 关窗。网页模式下这段脚本不在场，同一个键由后端低级键盘钩子接住
      // （见 preview.go 的 escWatch），两条路都汇到 ClosePreview。
      ev.preventDefault()
      void close()
      break
    case 'ArrowLeft':
    case 'PageUp':
      ev.preventDefault()
      step(-1)
      break
    case 'ArrowRight':
    case 'PageDown':
      ev.preventDefault()
      step(1)
      break
    case 'Home':
      ev.preventDefault()
      jump(0)
      break
    case 'End':
      ev.preventDefault()
      jump(total.value - 1)
      break
    case ' ':
      if (isImage.value) {
        ev.preventDefault()
        toggleFitActual()
      }
      break
    case '+':
    case '=':
      if (isImage.value) {
        ev.preventDefault()
        zoomBy(1.25)
      }
      break
    case '-':
    case '_':
      if (isImage.value) {
        ev.preventDefault()
        zoomBy(1 / 1.25)
      }
      break
    case '0':
      if (isImage.value) {
        ev.preventDefault()
        fitView()
      }
      break
    case '1':
      if (isImage.value) {
        ev.preventDefault()
        setActual()
      }
      break
    default:
      return
  }
}

let offLoad: (() => void) | undefined

onMounted(async () => {
  window.addEventListener('keydown', onKey, true)
  offLoad = api.onPreviewLoad(show)
  // 先取后端攒下的那份：窗口刚建起来时事件一定比监听者早，只能靠这次主动取。
  try {
    show(await api.TakePreviewRequest())
  } catch {
    state.value = 'error'
  }
  if (!cur.value) state.value = 'empty'
})

onBeforeUnmount(() => {
  window.removeEventListener('keydown', onKey, true)
  onUp()
  if (noteTimer !== undefined) window.clearTimeout(noteTimer)
  offLoad?.()
})
</script>

<template>
  <div class="pv">
    <div class="bar">
      <button :disabled="total < 2" @click="step(-1)" title="上一个（←）">◀</button>
      <span class="pos">{{ total ? index + 1 : 0 }} / {{ total }}</span>
      <button :disabled="total < 2" @click="step(1)" title="下一个（→）">▶</button>
      <span class="badge">{{ kindLabel }}</span>
      <span class="name" :title="cur?.url">{{ cur?.name || '—' }}</span>
      <div class="spacer" />
      <template v-if="isImage && state === 'ready'">
        <span class="pos">{{ pct }}%</span>
        <button @click="fitView" title="缩回适应窗口（空格 / 0）">适应</button>
        <button @click="setActual" title="按原始像素显示 1:1（双击图片 / 空格 / 1）">1:1</button>
      </template>
      <button @click="openExternal" title="交给系统默认浏览器（防盗链拦不住它）">↗ 浏览器</button>
      <button class="close" @click="close" title="关闭预览（ESC）">✕ 关闭</button>
    </div>

    <div class="stage" ref="stageEl" :class="{ panning: canPan, grabbing: dragging }" @wheel.prevent="onWheel">
      <!-- 右上角浮动按钮：贴在内容区上沿，不跟工具栏挤在一起 -->
      <div v-if="cur" class="float">
        <button class="fb" :disabled="saving" @click="saveAs" :title="saving ? '保存中…' : '另存为本地文件'">⤓</button>
        <button class="fb" @click="toggleMax" :title="maxed ? '还原窗口大小' : '最大化窗口'">{{ maxed ? '❐' : '□' }}</button>
      </div>
      <div v-if="note" class="note" :class="{ err: noteErr }">{{ note }}</div>

      <!-- 媒体元素必须在 loading 就在场：state 只能由它们自己的 load 事件置成 ready，
           把元素挡在 ready 之后才渲染就等于永远载入中。 -->
      <div v-if="state === 'error'" class="hint err">
        加载失败<br />
        <code>{{ cur?.url }}</code>
        <div class="sub">可能是防盗链、需要登录、或地址已经失效。可以试「↗ 浏览器」。</div>
      </div>
      <div v-else-if="!cur" class="hint">没有可预览的内容<br /><span class="sub">回到终端点一条超链接试试</span></div>

      <template v-else>
        <img v-if="cur.kind === 'image'" :key="cur.url" ref="imgEl" :src="cur.url" :style="imgStyle" alt=""
             draggable="false" referrerpolicy="no-referrer"
             @load="onReady" @error="onError" @dblclick="toggleFitActual" @mousedown="onDown" />
        <video v-else-if="cur.kind === 'video'" :key="cur.url" :src="cur.url" controls autoplay playsinline
               referrerpolicy="no-referrer" @loadedmetadata="onReady" @error="onError" />
        <div v-else class="audio">
          <div class="anote">♫</div>
          <div class="aname">{{ cur.name }}</div>
          <audio :key="cur.url" :src="cur.url" controls autoplay referrerpolicy="no-referrer"
                 @loadedmetadata="onReady" @error="onError" />
        </div>
      </template>

      <div v-if="cur && state === 'loading'" class="busy">载入中…</div>
    </div>

    <div class="foot">
      <span>ESC 关闭</span><span>← / → 翻页</span>
      <span v-if="isImage">滚轮缩放 · 按住左键拖动</span>
      <div class="spacer" />
      <span class="url" :title="cur?.url">{{ cur?.url }}</span>
    </div>
  </div>
</template>

<style scoped>
.pv { display: flex; flex-direction: column; height: 100%; width: 100%; overflow: hidden; background: #14161a; color: #c8ccd4; font-family: system-ui, "Segoe UI", sans-serif; }
/* 窗口是 frameless，这条工具栏就是标题栏：空白处可拖，控件本身不参与拖拽。 */
.bar { display: flex; align-items: center; gap: 6px; padding: 5px 8px; background: #24272e; border-bottom: 1px solid #33373f; flex: 0 0 auto; --wails-draggable: drag; }
.bar button { background: #2f333b; color: #c8ccd4; border: 1px solid #3d424b; border-radius: 5px; padding: 3px 9px; font-size: 12px; cursor: pointer; --wails-draggable: no-drag; }
.bar button:hover:not(:disabled) { background: #3a3f49; }
.bar button:disabled { opacity: .35; cursor: default; }
.bar button.close { border-color: #4a4f58; }
.bar button.close:hover { background: #e05c5c; border-color: #e05c5c; color: #fff; }
.pos { font-size: 12px; color: #8a919e; font-family: Consolas, monospace; min-width: 44px; text-align: center; }
.badge { font-size: 11px; color: #6ab0f3; border: 1px solid #33506b; border-radius: 4px; padding: 1px 6px; }
.name { font-size: 12px; color: #b5bac4; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; max-width: 34%; }
.spacer { flex: 1; min-width: 6px; }
.stage { position: relative; flex: 1; min-height: 0; display: flex; align-items: center; justify-content: center; overflow: hidden; background: #101215; }
.stage img { max-width: 100%; max-height: 100%; object-fit: contain; user-select: none; }
/* 缩放后才是「拖动看局部」的语义：适应窗口时拖动没有意义，光标也不给抓手 */
.stage.panning img { cursor: grab; }
.stage.grabbing img { cursor: grabbing; }
.stage video { max-width: 100%; max-height: 100%; background: #000; }
.float { position: absolute; top: 8px; right: 10px; z-index: 6; display: flex; gap: 6px; }
.float .fb { width: 27px; height: 25px; display: flex; align-items: center; justify-content: center; background: rgba(36, 39, 46, .74); color: #c8ccd4; border: 1px solid rgba(96, 102, 114, .7); border-radius: 5px; font-size: 14px; line-height: 1; cursor: pointer; }
.float .fb:hover:not(:disabled) { background: #3a3f49; border-color: #6ab0f3; }
.float .fb:disabled { opacity: .45; cursor: default; }
.note { position: absolute; left: 12px; bottom: 10px; z-index: 6; max-width: 72%; padding: 3px 10px; border-radius: 5px; background: rgba(36, 39, 46, .88); border: 1px solid #3d424b; color: #b5bac4; font-size: 12px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.note.err { color: #e06c5b; border-color: #5c3130; }
.audio { display: flex; flex-direction: column; align-items: center; gap: 12px; padding: 20px; width: min(560px, 90%); }
.audio .anote { font-size: 48px; color: #4a6b96; }
.audio .aname { font-size: 13px; color: #b5bac4; word-break: break-all; text-align: center; }
.audio audio { width: 100%; }
.hint { color: #6a7180; font-size: 13px; text-align: center; line-height: 1.9; padding: 20px; }
.hint.err { color: #e06c5b; }
.hint code { display: block; margin: 6px auto; max-width: 80vw; overflow: hidden; text-overflow: ellipsis; color: #8a919e; font-size: 12px; word-break: break-all; }
.hint .sub { color: #6a7180; font-size: 12px; }
/* 载入提示浮在内容上：不能占布局位置，否则会把正在加载的图挤到一边 */
.busy { position: absolute; top: 10px; left: 50%; transform: translateX(-50%); z-index: 5; padding: 3px 12px; border-radius: 5px; background: rgba(36, 39, 46, .88); border: 1px solid #3d424b; color: #8a919e; font-size: 12px; }
/* 底栏不当拖拽区：那行完整 URL 得能选中复制 */
.foot { display: flex; align-items: center; gap: 14px; padding: 3px 10px; background: #24272e; border-top: 1px solid #33373f; font-size: 11px; color: #6a7180; flex: 0 0 auto; height: 22px; }
.foot .url { color: #4d5460; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; max-width: 60%; }
</style>
