<script setup lang="ts">
import { computed, reactive, ref, onMounted } from 'vue'
import * as api from '../api'

const props = defineProps<{ prefs: api.Preferences }>()
const emit = defineEmits<{ (e: 'save', p: api.Preferences): void; (e: 'close'): void }>()

const p = reactive<api.Preferences>({ ...props.prefs })
if (!p.zmodemDir) p.zmodemDir = ''

// 本轮新增的字段在旧 prefs.json 里根本不存在，undefined 会让 checkbox 与数字框
// 进入半选中 / NaN 状态，先把它们落到出厂默认上。
if (p.quakeWidthPct == null || p.quakeWidthPct < 20) p.quakeWidthPct = 100
if (p.quakeHeightPct == null || p.quakeHeightPct < 20) p.quakeHeightPct = 40
if (p.quakeHideOnBlur == null) p.quakeHideOnBlur = true
if (p.trayEnabled == null) p.trayEnabled = true
if (p.closeToTray == null) p.closeToTray = false

// 随系统启动不进 Preferences：它的唯一事实来源是注册表（HKCU\...\Run），
// 在应用里存一份镜像会导致用户从「启动」里手工删掉后又被偷偷写回来。
const autostart = ref(false)
const autostartInit = ref(false)
const autostartNote = ref('')

// 系统可用字体列表
const fonts = ref<string[]>([])

// 下拉尺寸的实时估算：screen.avail* 给的是当前屏幕扣除任务栏后的区域（CSS 像素），
// 与后端 quakeBounds 用的 WorkArea 是同一个基准；多显示器时按窗口所在屏算，仅作提示。
const avail = ref({ w: 0, h: 0 })
const quakeNote = computed(() => {
  if (!avail.value.w || !avail.value.h) return '尺寸改动在下次进入 quake 时生效，不会把正在用的下拉突然跳尺寸'
  const pct = (v: number, def: number) => (v >= 20 && v <= 100 ? v : def)
  const w = Math.round((avail.value.w * pct(p.quakeWidthPct, 100)) / 100)
  const h = Math.round((avail.value.h * pct(p.quakeHeightPct, 40)) / 100)
  const mid = p.quakeWidthPct < 100 ? '，宽度不足全幅时水平居中' : ''
  return `工作区约 ${avail.value.w}×${avail.value.h} → 下拉 ${w}×${h}${mid}；改动在下次进入 quake 时生效`
})

onMounted(() => {
  avail.value = { w: window.screen.availWidth, h: window.screen.availHeight }
  api.ListSystemFonts().then((f) => { fonts.value = f || [] }).catch(() => { fonts.value = [] })
  api.GetAutostart().then((v) => { autostart.value = !!v; autostartInit.value = !!v }).catch(() => {})
})

function doSave() {
  if (autostart.value !== autostartInit.value) {
    api.SetAutostart(autostart.value)
      .then(() => { autostartInit.value = autostart.value; autostartNote.value = '自启动设置已写入，下次登录生效' })
      .catch((e) => { autostartNote.value = `写入自启动失败：${e}` })
  }
  emit('save', { ...p })
}
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
          <label class="chk"><input type="checkbox" v-model="p.imageViewerEnabled" /> 点击超链接时在预览窗口中打开（关闭则交给系统浏览器）</label>
        </fieldset>
        <fieldset>
          <legend>系统集成</legend>
          <label class="chk"><input type="checkbox" v-model="autostart" /> 随系统启动（登录时自动运行）</label>
          <label class="chk"><input type="checkbox" v-model="p.trayEnabled" /> 显示系统托盘图标（左键显示/隐藏，右键菜单）</label>
          <label class="chk"><input type="checkbox" v-model="p.closeToTray" :disabled="!p.trayEnabled" /> 点 ✕ 收起至托盘而不退出</label>
          <div class="note" v-if="autostartNote">{{ autostartNote }}</div>
        </fieldset>
        <fieldset>
          <legend>quake 下拉窗口</legend>
          <div class="grid grid-in">
            <label>下拉宽度</label>
            <div class="row2">
              <input type="number" v-model.number="p.quakeWidthPct" min="20" max="100" step="5" />
              <span class="unit">% 工作区宽度</span>
            </div>
            <label>下拉高度</label>
            <div class="row2">
              <input type="number" v-model.number="p.quakeHeightPct" min="20" max="100" step="5" />
              <span class="unit">% 工作区高度</span>
            </div>
          </div>
          <label class="chk"><input type="checkbox" v-model="p.quakeHideOnBlur" /> 失焦自动收起</label>
          <div class="hint">{{ quakeNote }}</div>
          <div class="hint">切换入口：键序列 <code>M-x M-q</code>（应用内）· 全局热键 <code>Alt+`</code> / <code>Alt+~</code>（跨应用）</div>
        </fieldset>
      </div>
      <div class="foot">
        <button class="primary" @click="doSave">保存</button>
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
.chk:has(input:disabled) { color: #6a7180; }
.grid-in { grid-template-columns: 84px 1fr; }
.unit { color: #6a7180; font-size: 12px; white-space: nowrap; }
.note { color: #6ab0f3; font-size: 12px; margin: 6px 0 0; }
.hint { color: #6a7180; font-size: 12px; margin-top: 6px; }
.hint code { color: #b5bac4; background: #1b1d22; border: 1px solid #3d424b; border-radius: 3px; padding: 0 4px; }
.row2 { display: flex; align-items: center; gap: 6px; }
.foot { display: flex; justify-content: flex-end; gap: 8px; padding: 12px 16px; border-top: 1px solid #33373f; }
button { background: #2f333b; color: #d5d9e0; border: 1px solid #3d424b; border-radius: 5px; padding: 6px 16px; cursor: pointer; font-size: 13px; }
button.primary { background: #2f74c0; border-color: #2f74c0; color: #fff; }
</style>
