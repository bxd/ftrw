<script setup lang="ts">
import { ref, reactive, watch } from 'vue'
import * as api from '../api'

const props = defineProps<{ schemes: api.ColorScheme[] }>()
const emit = defineEmits<{ (e: 'close'): void; (e: 'changed'): void }>()

const list = ref<api.ColorScheme[]>(props.schemes.map((s) => ({ ...s, palette: [...s.palette] })))
const sel = ref(0)
const edit = reactive<api.ColorScheme>(clone(list.value[0]))

function clone(s?: api.ColorScheme): api.ColorScheme {
  if (!s) return { name: 'New', background: '#000000', foreground: '#aaaaaa', cursor: '#aaaaaa', palette: Array(16).fill('#888888') }
  return { ...s, palette: [...s.palette] }
}

watch(sel, () => { Object.assign(edit, clone(list.value[sel.value])) })

const ANSINAMES = ['黑', '红', '绿', '黄', '蓝', '品红', '青', '白', '亮黑', '亮红', '亮绿', '亮黄', '亮蓝', '亮品红', '亮青', '亮白']

async function save() {
  const idx = list.value.findIndex((s) => s.name === edit.name)
  const copy = clone(edit)
  if (idx >= 0) list.value[idx] = copy; else list.value.push(copy)
  await api.SaveScheme(copy)
  emit('changed')
}

async function del() {
  const idx = list.value.findIndex((s) => s.name === edit.name)
  if (idx >= 0) {
    await api.DeleteScheme(edit.name)
    list.value = list.value.filter((s) => s.name !== edit.name)
    sel.value = 0
    emit('changed')
  }
}

function newScheme() {
  Object.assign(edit, clone())
  sel.value = -1
}
</script>

<template>
  <div class="mask" @click.self="emit('close')">
    <div class="dlg">
      <div class="head"><span>配色方案</span><button class="x" @click="emit('close')">✕</button></div>
      <div class="content">
        <div class="left">
          <div class="toolbar"><button class="mini" @click="newScheme">＋ 新建</button></div>
          <ul>
            <li v-for="(s, i) in list" :key="s.name" :class="{ on: sel === i }" @click="sel = i">
              <span class="swatch" :style="{ background: s.background, color: s.foreground }">Aa</span>{{ s.name }}
            </li>
          </ul>
        </div>
        <div class="right">
          <div class="grid">
            <label>名称</label><input v-model="edit.name" />
            <label>背景</label><div class="clr"><input type="color" v-model="edit.background" /><input v-model="edit.background" /></div>
            <label>前景</label><div class="clr"><input type="color" v-model="edit.foreground" /><input v-model="edit.foreground" /></div>
            <label>光标</label><div class="clr"><input type="color" v-model="edit.cursor" /><input v-model="edit.cursor" /></div>
          </div>
          <div class="palette">
            <div v-for="(c, i) in edit.palette" :key="i" class="cell">
              <input type="color" v-model="edit.palette[i]" />
              <span>{{ ANSINAMES[i] }}</span>
            </div>
          </div>
          <div class="preview" :style="{ background: edit.background, color: edit.foreground }">
            <span v-for="(c, i) in 8" :key="i" :style="{ color: edit.palette[i] }">█▓▒░ 文字{{ i }} </span>
          </div>
        </div>
      </div>
      <div class="foot">
        <button @click="del">删除</button>
        <div class="spacer" />
        <button class="primary" @click="save">保存</button>
        <button @click="emit('close')">关闭</button>
      </div>
    </div>
  </div>
</template>

<style scoped>
.mask { position: fixed; inset: 0; background: rgba(0, 0, 0, .55); display: flex; align-items: center; justify-content: center; z-index: 50; }
.dlg { width: 640px; max-height: 88vh; background: #24272e; border: 1px solid #3d424b; border-radius: 10px; display: flex; flex-direction: column; }
.head { display: flex; justify-content: space-between; padding: 12px 16px; font-weight: 600; border-bottom: 1px solid #33373f; }
.x { background: none; border: none; color: #8a919e; cursor: pointer; font-size: 16px; }
.content { display: flex; flex: 1; min-height: 0; }
.left { width: 180px; border-right: 1px solid #33373f; display: flex; flex-direction: column; }
.toolbar { padding: 8px; }
.mini { background: #2f333b; color: #d5d9e0; border: 1px solid #3d424b; border-radius: 5px; padding: 4px 8px; cursor: pointer; }
.left ul { list-style: none; margin: 0; padding: 0; overflow-y: auto; }
.left li { padding: 7px 12px; cursor: pointer; font-size: 13px; display: flex; align-items: center; gap: 8px; }
.left li.on { background: #2f74c0; color: #fff; }
.swatch { padding: 0 4px; border-radius: 3px; border: 1px solid #555; font-size: 11px; }
.right { flex: 1; padding: 12px 16px; overflow-y: auto; }
.grid { display: grid; grid-template-columns: 60px 1fr; gap: 8px 10px; align-items: center; }
.grid label { color: #8a919e; font-size: 13px; }
input { background: #1b1d22; color: #d5d9e0; border: 1px solid #3d424b; border-radius: 5px; padding: 5px 8px; font-size: 13px; box-sizing: border-box; }
.clr { display: flex; gap: 6px; align-items: center; }
.clr input[type="color"] { width: 34px; height: 28px; padding: 0; }
.clr input[type="text"], .clr input:not([type="color"]) { flex: 1; }
.palette { display: grid; grid-template-columns: repeat(4, 1fr); gap: 6px; margin-top: 12px; }
.cell { display: flex; align-items: center; gap: 6px; font-size: 12px; color: #8a919e; }
.cell input { width: 30px; height: 24px; padding: 0; }
.preview { margin-top: 14px; padding: 10px; border-radius: 6px; font-family: monospace; border: 1px solid #33373f; }
.foot { display: flex; gap: 8px; padding: 12px 16px; border-top: 1px solid #33373f; align-items: center; }
.spacer { flex: 1; }
button { background: #2f333b; color: #d5d9e0; border: 1px solid #3d424b; border-radius: 5px; padding: 6px 14px; cursor: pointer; font-size: 13px; }
button.primary { background: #2f74c0; border-color: #2f74c0; color: #fff; }
</style>
