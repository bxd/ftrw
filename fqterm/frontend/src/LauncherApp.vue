<script setup lang="ts">
// Alfred 式启动器页面（?view=launcher，独立窗口，见 launcher.go）。
//
// 手感三条：呼出即光标在册、输入即搜、回车即走。窗口是预建常驻的，所以
// 「再次呼出」不是重新挂载 —— 靠后端推的 launcher:show 事件重置输入并聚焦。
// IME 合成期间（拼音候选还挂着）绝不抢 ↑↓/Enter：e.isComposing 那一跳是
// 中文用户的第一道体验关。
import { nextTick, onBeforeUnmount, onMounted, ref } from 'vue'
import * as api from './api'

const q = ref('')
const items = ref<api.LauncherResult[]>([])
const sel = ref(0)
const info = ref<api.LauncherInfo>({ ready: false, count: 0, indexing: false })
const err = ref('')
const box = ref<HTMLInputElement | null>(null)
const list = ref<HTMLDivElement | null>(null)

let composing = false
let timer: ReturnType<typeof setTimeout> | null = null
let seq = 0 // 竞态护栏：只采纳最后一次发出的搜索结果

async function runSearch(immediate = false) {
  if (timer) { clearTimeout(timer); timer = null }
  if (!immediate) {
    timer = setTimeout(() => { timer = null; void runSearch(true) }, 80)
    return
  }
  const my = ++seq
  try {
    const r = await api.LauncherSearch(q.value)
    if (my !== seq) return
    items.value = r
    sel.value = 0
  } catch { /* 后端还没就绪就保留旧列表，不让输入框闪成空白 */ }
}

function onInput() {
  err.value = ''
  if (!composing) void runSearch()
}

function onCompositionEnd() {
  composing = false
  onInput()
}

function onKeydown(e: KeyboardEvent) {
  if (e.isComposing || composing) return // 合成期间整条交给 IME
  switch (e.key) {
    case 'ArrowDown': e.preventDefault(); move(1); break
    case 'ArrowUp': e.preventDefault(); move(-1); break
    case 'Enter':
      // Ctrl+Enter = 以管理员身份启动（UAC 由系统弹）；IME 拦在上头，
      // 合成期间不会走到这里。
      e.preventDefault(); void launch(e.ctrlKey); break
    case 'Escape':
      // 后端窗口级绑定与失焦钩子也会收，这里自己再走一遍直连路径，不赌命中。
      e.preventDefault()
      void api.LauncherHide().catch(() => {})
      break
  }
}

function move(d: number) {
  if (!items.value.length) return
  sel.value = (sel.value + d + items.value.length) % items.value.length
  nextTick(() => {
    const el = list.value?.children[sel.value]
    el?.scrollIntoView({ block: 'nearest' })
  })
}

async function launch(elevate = false) {
  const it = items.value[sel.value]
  if (!it) return
  try {
    await api.LauncherInvoke(it.path, elevate)
  } catch (e) {
    err.value = String(e)
  }
}

function reset() {
  q.value = ''
  err.value = ''
  void runSearch(true)
  // 底部计数跟着每次呼出走：onMounted 那一次很可能落在索引完成之前，
  // 窗口是常驻的，不重拉就会永远停在「0 个应用」。
  api.LauncherStatus().then((v) => { info.value = v }).catch(() => {})
  nextTick(() => box.value?.focus())
}

function onWindowFocus() { box.value?.focus() }

let offShow: (() => void) | null = null
onMounted(() => {
  offShow = api.onLauncherShow(reset)
  api.LauncherStatus().then((v) => { info.value = v }).catch(() => {})
  box.value?.focus()
  window.addEventListener('focus', onWindowFocus)
})
onBeforeUnmount(() => {
  offShow?.()
  window.removeEventListener('focus', onWindowFocus)
  if (timer) clearTimeout(timer)
})
</script>

<template>
  <div class="lk">
    <div class="head">
      <input
        ref="box"
        v-model="q"
        class="search"
        type="text"
        placeholder="搜索应用…"
        spellcheck="false"
        @input="onInput"
        @keydown="onKeydown"
        @compositionstart="composing = true"
        @compositionend="onCompositionEnd"
      />
    </div>
    <div ref="list" class="list">
      <div
        v-for="(it, i) in items"
        :key="it.path"
        class="item"
        :class="{ on: i === sel }"
        @mouseenter="sel = i"
        @click="launch()"
      >
        <span class="nm">{{ it.name }}</span>
        <span class="tg">{{ it.target || it.path }}</span>
      </div>
      <div v-if="!items.length" class="empty">
        <template v-if="info.indexing">正在建立索引…</template>
        <template v-else-if="!info.ready">索引还没就绪，稍后再按一次热键。</template>
        <template v-else-if="q">没有匹配「{{ q }}」的应用。</template>
        <template v-else>没有找到任何快捷方式（开始菜单与桌面都是空的？）。<a class="rb" @click="() => { api.LauncherRebuild().catch(() => {}) }">重建索引</a></template>
      </div>
    </div>
    <div class="foot">
      <span class="hint">↑↓ 选择 · Enter 启动 · Ctrl+Enter 管理员 · Esc 收起</span>
      <span v-if="err" class="fail">{{ err }}</span>
      <span v-else class="count">{{ info.count }} 个应用</span>
    </div>
  </div>
</template>

<style>
/* 无边框窗体：整页就是一块浮起来的深色面板。body 留透明边距不做圆角 ——
   Wails frameless 的圆角要窗口级透明，首版不为此开这个口子。 */
html, body { margin: 0; height: 100%; background: #181a1e; overflow: hidden; }
</style>

<style scoped>
.lk { display: flex; flex-direction: column; height: 100vh; color: #d5d9e0; font: 14px/1.5 "Segoe UI", "Microsoft YaHei", sans-serif; }
.head { padding: 14px 14px 8px; }
.search { width: 100%; box-sizing: border-box; background: #1b1d22; color: #e6e9ef; border: 1px solid #3d424b; border-radius: 8px; padding: 9px 12px; font-size: 17px; outline: none; }
.search:focus { border-color: #6ea8fe; }
.list { flex: 1; overflow-y: auto; padding: 0 6px; }
.item { display: flex; align-items: baseline; gap: 10px; padding: 7px 10px; border-radius: 6px; cursor: default; }
.item.on { background: #2c313a; }
.nm { flex: none; max-width: 55%; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-size: 15px; }
.tg { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; color: #6b7280; font-size: 12px; }
.empty { padding: 26px 16px; color: #8a919e; text-align: center; }
.rb { color: #6ea8fe; cursor: pointer; text-decoration: underline; }
.foot { display: flex; justify-content: space-between; align-items: center; padding: 6px 14px 10px; color: #5d6470; font-size: 12px; border-top: 1px solid #262a31; }
.fail { color: #e07171; max-width: 70%; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.count { color: #565d68; }
</style>
