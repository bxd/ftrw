<script setup lang="ts">
import { computed, ref, watch, onBeforeUnmount } from 'vue'

interface ZmInfo {
  id: string
  direction: string
  filename: string
  size: number
  done: number
  state: string
  message: string
}

const props = defineProps<{ info: ZmInfo }>()
const emit = defineEmits<{ (e: 'cancel'): void; (e: 'close'): void }>()

const cancelling = ref(false)

const pct = computed(() => {
  if (props.info.size <= 0) return 0
  return Math.min(100, Math.round((props.info.done / props.info.size) * 100))
})

const finished = computed(() => ['done', 'error', 'cancelled', 'skipped'].includes(props.info.state))

const title = computed(() => {
  const d = props.info.direction === 'send' ? '上传' : '下载'
  switch (props.info.state) {
    case 'done': return `${d}完成`
    case 'error': return `${d}失败`
    case 'cancelled': return '已取消'
    case 'skipped': return '被服务器跳过'
    default: return `${d}中…`
  }
})

// 速率：最近 3s 滑动窗口采样
const samples = ref<{ t: number; d: number }[]>([])
const speed = ref(0)
watch(() => props.info.done, (d) => {
  const now = performance.now()
  samples.value.push({ t: now, d })
  while (samples.value.length > 2 && now - samples.value[0].t > 3000) samples.value.shift()
  const dt = (now - samples.value[0].t) / 1000
  speed.value = dt > 0.2 ? Math.max(0, (d - samples.value[0].d) / dt) : 0
})
onBeforeUnmount(() => { samples.value = [] })

function fmtBytes(n: number): string {
  if (n >= 1048576) return (n / 1048576).toFixed(2) + ' MB'
  if (n >= 1024) return (n / 1024).toFixed(1) + ' KB'
  return n + ' B'
}
function fmtSpeed(bps: number): string {
  if (bps <= 0) return '--'
  if (bps >= 1048576) return (bps / 1048576).toFixed(2) + ' MB/s'
  if (bps >= 1024) return (bps / 1024).toFixed(1) + ' KB/s'
  return bps.toFixed(0) + ' B/s'
}

function onCancel() {
  cancelling.value = true
  emit('cancel')
}
watch(finished, (f) => { if (f) cancelling.value = false })
</script>

<template>
  <div class="zm-card">
    <div class="zm-head">
      <span class="zm-icon" :class="info.direction">{{ info.direction === 'send' ? '⬆' : '⬇' }}</span>
      <span class="zm-title">{{ title }}</span>
      <button v-if="finished" class="zm-x" @click="emit('close')" title="关闭">✕</button>
    </div>
    <div class="zm-file" :title="info.filename">{{ info.filename || '（无文件名）' }}</div>
    <div class="zm-bar">
      <div class="zm-fill" :class="{ done: info.state === 'done', err: info.state === 'error', cancelled: info.state === 'cancelled' || info.state === 'skipped' }" :style="{ width: pct + '%' }"></div>
    </div>
    <div class="zm-stats">
      <span>{{ fmtBytes(info.done) }} / {{ fmtBytes(info.size) }}</span>
      <span class="zm-pct">{{ pct }}%</span>
      <span>{{ fmtSpeed(speed) }}</span>
    </div>
    <div class="zm-msg" v-if="info.message">{{ info.message }}</div>
    <div class="zm-foot" v-if="!finished">
      <button class="zm-cancel" :disabled="cancelling" @click="onCancel">{{ cancelling ? '取消中…' : '取消传输' }}</button>
    </div>
  </div>
</template>

<style scoped>
.zm-card { position: fixed; right: 16px; bottom: 40px; width: 340px; z-index: 60; background: #24272e; border: 1px solid #3d424b; border-radius: 10px; box-shadow: 0 8px 28px rgba(0, 0, 0, .55); color: #d5d9e0; font-size: 13px; }
.zm-head { display: flex; align-items: center; gap: 8px; padding: 10px 14px 4px; font-weight: 600; }
.zm-icon { color: #6ab0f3; }
.zm-icon.send { color: #4ec9b0; }
.zm-title { flex: 1; }
.zm-x { background: none; border: none; color: #8a919e; cursor: pointer; font-size: 14px; padding: 0 2px; }
.zm-x:hover { color: #fff; }
.zm-file { padding: 0 14px 8px; color: #8a919e; font-size: 12px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.zm-bar { margin: 0 14px; height: 10px; background: #1b1d22; border: 1px solid #33373f; border-radius: 5px; overflow: hidden; }
.zm-fill { height: 100%; background: linear-gradient(90deg, #2f74c0, #4a9ae6); border-radius: 4px; transition: width .15s linear; }
.zm-fill.done { background: #57d25a; }
.zm-fill.err { background: #e05c5c; }
.zm-fill.cancelled { background: #777; }
.zm-stats { display: flex; justify-content: space-between; padding: 6px 14px 0; font-size: 12px; color: #8a919e; }
.zm-pct { color: #d5d9e0; font-weight: 600; }
.zm-msg { padding: 6px 14px 0; font-size: 12px; color: #e0b341; }
.zm-foot { padding: 10px 14px 12px; display: flex; justify-content: flex-end; }
.zm-cancel { background: #2f333b; color: #d5d9e0; border: 1px solid #3d424b; border-radius: 5px; padding: 5px 16px; cursor: pointer; font-size: 13px; }
.zm-cancel:hover:not(:disabled) { background: #3a3f49; border-color: #e05c5c; color: #fff; }
.zm-cancel:disabled { opacity: .5; cursor: default; }
</style>
