<script setup lang="ts">
import { reactive, ref, onMounted } from 'vue'
import * as api from '../api'

const props = defineProps<{ prefs: api.Preferences }>()
const emit = defineEmits<{ (e: 'save', p: api.Preferences): void; (e: 'close'): void }>()

const p = reactive<api.Preferences>({ ...props.prefs })
if (!p.zmodemDir) p.zmodemDir = ''

// 系统可用字体列表
const fonts = ref<string[]>([])
onMounted(() => {
  api.ListSystemFonts().then((f) => { fonts.value = f || [] }).catch(() => { fonts.value = [] })
})
</script>

<template>
  <div class="mask" @click.self="emit('close')">
    <div class="dlg">
      <div class="head"><span>偏好设置</span><button class="x" @click="emit('close')">✕</button></div>
      <div class="form">
        <div class="grid">
          <label>默认编码</label>
          <select v-model.number="p.defaultEncodingID">
            <option v-for="(e, i) in api.ENCODINGS" :key="i" :value="i">{{ e }}</option>
          </select>
          <label>默认终端</label>
          <select v-model="p.defaultTermType">
            <option v-for="t in api.TERM_TYPES" :key="t" :value="t">{{ t }}</option>
          </select>
          <label>默认配色</label><input v-model="p.defaultScheme" />
          <label>字体</label>
                    <select v-model="p.fontName">
                      <option value="">（默认：等宽）</option>
                      <option v-for="f in fonts" :key="f" :value="f">{{ f }}</option>
                    </select>
          <label>字号</label><input type="number" v-model.number="p.fontSize" min="8" max="32" />
          <label>列×行</label>
          <div class="row2">
            <input type="number" v-model.number="p.defaultColumns" min="20" /> ×
            <input type="number" v-model.number="p.defaultRows" min="6" />
          </div>
          <label>回滚行数</label><input type="number" v-model.number="p.scrollbackLines" min="100" />
          <label>下载目录</label><input v-model="p.zmodemDir" placeholder="留空使用默认目录" />
        </div>
        <fieldset>
          <legend>行为</legend>
          <label class="chk"><input type="checkbox" v-model="p.confirmClose" /> 关闭连接时确认</label>
          <label class="chk"><input type="checkbox" v-model="p.imageViewerEnabled" /> 启用 BBS 图片浏览</label>
        </fieldset>
      </div>
      <div class="foot">
        <button class="primary" @click="emit('save', { ...p })">保存</button>
        <button @click="emit('close')">取消</button>
      </div>
    </div>
  </div>
</template>

<style scoped>
.mask { position: fixed; inset: 0; background: rgba(0, 0, 0, .55); display: flex; align-items: center; justify-content: center; z-index: 50; }
.dlg { width: 480px; max-height: 88vh; overflow-y: auto; background: #24272e; border: 1px solid #3d424b; border-radius: 10px; }
.head { display: flex; justify-content: space-between; align-items: center; padding: 12px 16px; font-weight: 600; border-bottom: 1px solid #33373f; }
.x { background: none; border: none; color: #8a919e; cursor: pointer; font-size: 16px; }
.form { padding: 14px 16px; }
.grid { display: grid; grid-template-columns: 96px 1fr; gap: 8px 10px; align-items: center; }
.grid label { color: #8a919e; font-size: 13px; text-align: right; }
input, select { background: #1b1d22; color: #d5d9e0; border: 1px solid #3d424b; border-radius: 5px; padding: 5px 8px; font-size: 13px; width: 100%; box-sizing: border-box; }
fieldset { border: 1px solid #33373f; border-radius: 6px; margin-top: 12px; padding: 10px 12px; }
legend { color: #6a7180; font-size: 12px; padding: 0 6px; }
.chk { display: flex; align-items: center; gap: 6px; font-size: 13px; color: #b5bac4; margin: 6px 0; }
.chk input { width: auto; }
.row2 { display: flex; align-items: center; gap: 6px; }
.foot { display: flex; justify-content: flex-end; gap: 8px; padding: 12px 16px; border-top: 1px solid #33373f; }
button { background: #2f333b; color: #d5d9e0; border: 1px solid #3d424b; border-radius: 5px; padding: 6px 16px; cursor: pointer; font-size: 13px; }
button.primary { background: #2f74c0; border-color: #2f74c0; color: #fff; }
</style>
