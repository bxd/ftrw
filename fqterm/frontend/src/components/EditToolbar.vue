<script setup lang="ts">
/**
 * 编辑工具栏 —— 浮在终端顶部的一条自动隐藏工具栏，专管发帖时的文本动作。
 *
 * 为什么做成「鼠标移过去才出现」：编辑区的每一行都是站端画出来的正文，一条常驻
 * 工具栏会长期吃掉第一行；只在需要时出现，不用时把整屏还给文章。
 * 显隐本身由 App.vue 按坐标判定（与左侧站点栏一套做法），这里只管内容与开合，
 * 不再自己听 mouseenter —— 透明浮层自己报边界，很容易和外面的热区对错位置。
 *
 * 条子本体背景透明，只有按钮和面板带底色：终端底色随配色方案变，一层半透明深色
 * 底反而会把字糊在一起。
 */
import { computed, nextTick, onBeforeUnmount, ref, watch } from 'vue'
import { SEP_PRESETS, isSafeSep, type SepPreset } from '../posttransform'
import { currentSep, openSepPicker, runSep, runVertical, sepPickerOpen, toolbarPinned } from '../postactions'

defineProps<{ shown: boolean }>()

const panel = ref<HTMLElement | null>(null)
const custom = ref('')
const customErr = ref('')
const busy = ref(false)

const sepNow = computed(() => currentSep())

/** 全角空格在格子里看着就是空的，会被当成“这个按钮坏了”；给它一个可见占位符。 */
function disp(p: SepPreset): string {
  return p.ch.trim() === '' ? '␣' : p.ch
}

/** 面板里点符号 / 随机 / 自定义，都先跑完动作再收：留着的话下一次鼠标一动就穿帮 */
async function act(run: () => Promise<void>) {
  if (busy.value) return
  busy.value = true
  sepPickerOpen.value = false
  try {
    await run()
  } finally {
    busy.value = false
  }
}

function onPreset(ch: string) {
  void act(() => runSep('fixed', ch))
}

function onCustom() {
  const ch = custom.value
  if (!isSafeSep(ch)) {
    // 就地说明比状态区醒目：用户此刻眼睛在这个输入框上，不在右下角
    customErr.value = ch.length > 1
      ? '只能是一个字符'
      : '这个字符在 GBK 站点上会变乱码，换一个（可用可见 ASCII 与常见中文标点）'
    return
  }
  customErr.value = ''
  void act(() => runSep('fixed', ch))
}

function togglePanel() {
  if (sepPickerOpen.value) {
    sepPickerOpen.value = false
    return
  }
  openSepPicker()
}

// 面板一开就把焦点收进输入框：热键 M-x l 打开时用户紧接着就该打字，
// 而不是还要再用鼠标点一下。点面板外任意处也要收起：下拉没有遮罩，
// 不留这条出口就只能靠 Esc。监听只在面板存活期间挂 —— 工具栏收起只是位移出屏外，
// 组件并不卸载，常驻一个每次点击都要跑的处理器没道理。
watch(sepPickerOpen, async (on) => {
  document.removeEventListener('mousedown', onDocDown, true)
  if (!on) { customErr.value = ''; return }
  document.addEventListener('mousedown', onDocDown, true)
  await nextTick()
  panel.value?.querySelector<HTMLInputElement>('input')?.focus()
})

function onDocDown(ev: MouseEvent) {
  if (panel.value && !panel.value.contains(ev.target as Node)) sepPickerOpen.value = false
}

onBeforeUnmount(() => {
  document.removeEventListener('mousedown', onDocDown, true)
  sepPickerOpen.value = false
})
</script>

<template>
  <div class="ebt-root" :class="{ shown, pinned: toolbarPinned }">
    <div class="ebt-bar">
      <button class="ebt-btn" :class="{ active: sepPickerOpen }" title="先鼠标框住编辑区里要处理的文字，再在字间插入分隔符（M-x l 选符号，M-x s 用上次那个）"
              @click="togglePanel">✦ 插符号</button>
      <button class="ebt-btn" title="把选中的文字转成竖排并原地替换（M-x v）" :disabled="busy" @click="act(runVertical)">▦ 竖排</button>
      <span class="ebt-tip">
        只处理鼠标框选的那段文字
        <template v-if="toolbarPinned"> · 已钉住（M-x t 取消）</template>
        <template v-else> · 鼠标移开自动收起</template>
      </span>
    </div>

    <div v-if="sepPickerOpen" ref="panel" class="ebt-panel" @keydown.esc="sepPickerOpen = false">
      <div class="ebt-sec">先在编辑区把字打上（或用鼠标框住要处理的那段），光标留在它末尾（原文会顺手存进内存做备份，不碰系统剪贴板）</div>

      <div class="ebt-sec">预置分隔符（点一个立即替换选区并记住）</div>
      <div class="ebt-grid">
        <button v-for="p in SEP_PRESETS" :key="p.ch" class="ebt-sym" :class="{ now: p.ch === sepNow }"
                :title="p.name" @click="onPreset(p.ch)">{{ disp(p) }}</button>
      </div>

      <div class="ebt-sec">随机（从预置集合里挑）</div>
      <div class="ebt-row">
        <button class="ebt-btn" @click="act(() => runSep('randomOne'))">随机一种</button>
        <button class="ebt-btn" @click="act(() => runSep('randomMixed'))">逐字随机多种</button>
      </div>

      <div class="ebt-sec">自定义一个（单字符，可见 ASCII 或常见中文标点）</div>
      <div class="ebt-row">
        <input v-model="custom" class="ebt-in" maxlength="2" spellcheck="false"
               placeholder="例如 ‡" @input="customErr = ''"
               @keydown.enter="onCustom" />
        <button class="ebt-btn" @click="onCustom">应用</button>
      </div>
      <div v-if="customErr" class="ebt-err">{{ customErr }}</div>

      <div class="ebt-foot">
        当前默认：<b class="ebt-now">{{ sepNow }}</b>
        · M-x s 用默认 / M-x r 随机一种 / M-x m 逐字随机 / M-x i 灌回内存备份
      </div>
    </div>
  </div>
</template>

<style scoped>
.ebt-root {
  position: absolute; top: 0; left: 0; right: 0; z-index: 20;
  transform: translateY(-100%); opacity: 0; pointer-events: none;
  transition: transform .13s ease-out, opacity .13s ease-out;
}
.ebt-root.shown { transform: none; opacity: 1; pointer-events: auto; }

.ebt-bar {
  display: flex; align-items: center; gap: 6px; padding: 3px 8px;
  background: transparent;
}
.ebt-btn {
  background: rgba(47, 51, 59, .82); color: #c8ccd4; border: 1px solid #3d424b;
  border-radius: 5px; padding: 2px 10px; cursor: pointer; font-size: 12px; white-space: nowrap;
}
.ebt-btn:hover:not(:disabled) { background: #3a3f49; border-color: #4a9ae6; }
.ebt-btn:disabled { opacity: .5; cursor: default; }
.ebt-btn.active { border-color: #6ab0f3; color: #fff; }
/* 提示往右靠，避开终端正文（正文从左边开始，左半边被盖住最疼）；
   条子本体透不透明由用户定，但这行字得自己能读得清，所以单独给它一块半透底 */
.ebt-tip {
  margin-left: auto; color: #b6bec9; font-size: 11px; white-space: nowrap;
  overflow: hidden; text-overflow: ellipsis;
  background: rgba(22, 26, 32, .86); border: 1px solid #33373f; border-radius: 4px; padding: 2px 8px;
}

.ebt-panel {
  margin: 0 8px; padding: 8px 10px 9px; width: 380px; max-width: calc(100% - 16px);
  background: rgba(27, 29, 34, .96); border: 1px solid #3d424b; border-top: none;
  border-radius: 0 0 8px 8px; box-shadow: 0 8px 24px rgba(0, 0, 0, .5);
  font-size: 12px; color: #c8ccd4; outline: none;
}
.ebt-sec { color: #8a919e; margin: 6px 0 4px; }
.ebt-grid { display: flex; flex-wrap: wrap; gap: 4px; }
.ebt-sym {
  width: 30px; height: 26px; line-height: 1; font-size: 14px; cursor: pointer;
  background: #2f333b; color: #dfe6f0; border: 1px solid #3d424b; border-radius: 4px;
}
.ebt-sym:hover { background: #3a3f49; border-color: #6ab0f3; }
.ebt-sym.now { border-color: #4ec9b0; color: #4ec9b0; }
.ebt-row { display: flex; gap: 6px; align-items: center; }
.ebt-in {
  width: 72px; background: #1b1d22; color: #dfe6f0; border: 1px solid #3d424b;
  border-radius: 4px; padding: 3px 6px; font-size: 13px;
}
.ebt-in:focus { border-color: #6ab0f3; outline: none; }
.ebt-err { color: #e0b341; margin-top: 4px; }
.ebt-foot { color: #8a919e; margin-top: 8px; border-top: 1px solid #33373f; padding-top: 6px; }
.ebt-now { color: #4ec9b0; font-size: 13px; }
</style>
