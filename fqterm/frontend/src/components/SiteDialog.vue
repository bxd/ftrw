<script setup lang="ts">
import { reactive, ref, onMounted } from 'vue'
import * as api from '../api'

const props = defineProps<{
  site: api.Site
  index: number
  save: boolean
  schemes: api.ColorScheme[]
}>()
const emit = defineEmits<{
  (e: 'save', site: api.Site, index: number, save: boolean): void
  (e: 'close'): void
}>()

// 旧站点可能缺少新增字段，这里逐字段兜底（不能写成 {默认, ...site} 重复键形式）。
const raw = props.site
const s = reactive<api.Site>({
  ...raw,
  maxIdleSeconds: raw.maxIdleSeconds || 120,
  antiIdleMessage: raw.antiIdleMessage || '^@',
  reconnectInterval: raw.reconnectInterval || 3,
  reconnectRetries: raw.reconnectRetries ?? -1,
  fontNameCjk: raw.fontNameCjk ?? 'SimSun',
  charSpacing: raw.charSpacing ?? 0,
  lineSpacing: raw.lineSpacing ?? 0,
  antiAlias: raw.antiAlias ?? true,
  fontAutoFit: raw.fontAutoFit ?? 1,
})

// 系统可用字体列表（点击字体下拉时展示全部供选择）
const fonts = ref<string[]>([])
onMounted(() => {
  api.ListSystemFonts().then((f) => { fonts.value = f || [] }).catch(() => { fonts.value = [] })
})

// When protocol changes, auto-adjust default port
function onProtocol() {
  if (s.protocolType >= 1 && s.port === 23) s.port = 22
  if (s.protocolType === 0 && s.port === 22) s.port = 23
}

// when hostType BBS toggled, set sensible defaults
function onHostType() {
  if (s.hostType === 0) { s.termType = 'vtnt'; s.serverEncodingID = 0 }
  else { s.termType = 'xterm-256color'; s.serverEncodingID = 2 }
}

function doSave() {
  // 字段归一化：避免旧站点数据缺项导致后端拿到 0 值而行为错乱。
  if (!s.maxIdleSeconds || s.maxIdleSeconds < 10) s.maxIdleSeconds = 120
  if (!s.reconnectInterval || s.reconnectInterval < 1) s.reconnectInterval = 3
  if (s.reconnectRetries === undefined || s.reconnectRetries === null) s.reconnectRetries = -1
  if (s.antiIdleMessage === undefined || s.antiIdleMessage === null) s.antiIdleMessage = '^@'
  emit('save', { ...s }, props.index, props.save)
}
</script>

<template>
  <div class="mask" @click.self="emit('close')">
    <div class="dlg">
      <div class="head">
        <span>{{ index >= 0 ? '编辑站点' : (save ? '新建站点' : '连接到站点') }}</span>
        <button class="x" @click="emit('close')">✕</button>
      </div>
      <div class="tabs"><span class="tb on">连接设置</span></div>
      <div class="form">
        <div class="grid">
          <label>名称</label><input v-model="s.name" placeholder="站点显示名" />
          <label>协议</label>
          <select v-model.number="s.protocolType" @change="onProtocol">
            <option v-for="(p, i) in api.PROTOCOLS" :key="i" :value="i">{{ p }}</option>
          </select>
          <label>主机</label><input v-model="s.host" placeholder="bbs.example.com" />
          <label>端口</label><input type="number" v-model.number="s.port" />
          <label>类型</label>
          <select v-model.number="s.hostType" @change="onHostType">
            <option :value="0">BBS</option><option :value="1">*NIX</option>
          </select>
          <label>分组</label><input v-model="s.group" placeholder="默认分组" />
        </div>

        <fieldset v-if="s.protocolType >= 1 && s.protocolType <= 2">
          <legend>SSH 登录</legend>
          <div class="grid">
            <label>用户名</label><input v-model="s.userName" />
            <label>密码</label><input type="password" v-model="s.password" />
          </div>
        </fieldset>

        <fieldset>
          <legend>自动登录</legend>
          <label class="chk"><input type="checkbox" v-model="s.autoLogin" /> 启用自动登录</label>
          <div class="grid">
            <label>登录前</label><input v-model="s.preLogin" placeholder="new / guest" />
            <label>登录后</label><input v-model="s.postLogin" />
          </div>
        </fieldset>

        <fieldset>
          <legend>空闲保活 / 自动重连</legend>
          <label class="chk"><input type="checkbox" v-model="s.antiIdle" /> 启用防空闲（挂机不被踢下线）</label>
          <div class="grid">
            <label>空闲阈值</label>
            <div class="row2">
              <input type="number" v-model.number="s.maxIdleSeconds" min="10" />
              <span class="unit">秒无输入即发保活字节</span>
            </div>
            <label>保活内容</label>
            <input v-model="s.antiIdleMessage" placeholder="^@" title="支持 FQTerm 转义：^@ = NUL（无显示）、^M = 回车、\n = CR" />
          </div>
          <label class="chk"><input type="checkbox" v-model="s.autoReconnect" /> 断线自动重连</label>
          <div class="grid" v-if="s.autoReconnect">
            <label>重连间隔</label>
            <div class="row2"><input type="number" v-model.number="s.reconnectInterval" min="1" /> <span class="unit">秒</span></div>
            <label>重试次数</label>
            <div class="row2"><input type="number" v-model.number="s.reconnectRetries" min="-1" /> <span class="unit">-1 为无限重试</span></div>
          </div>
        </fieldset>

        <fieldset>
          <legend>显示 / 编码</legend>
          <div class="grid">
            <label>服务器编码</label>
            <select v-model.number="s.serverEncodingID">
              <option v-for="(e, i) in api.ENCODINGS" :key="i" :value="i">{{ e }}</option>
            </select>
            <label>配色方案</label>
            <select v-model="s.colorScheme">
              <option v-for="sc in schemes" :key="sc.name" :value="sc.name">{{ sc.name }}</option>
            </select>
            <label>终端类型</label>
            <select v-model="s.termType">
              <option v-for="t in api.TERM_TYPES" :key="t" :value="t">{{ t }}</option>
            </select>
            <label>列×行</label>
            <div class="row2">
              <input type="number" v-model.number="s.numColumns" min="20" /> ×
              <input type="number" v-model.number="s.numRows" min="6" />
            </div>
            <label>退格键</label>
            <select v-model.number="s.backspace">
              <option :value="1">DEL (^?)</option><option :value="0">BS (^H)</option>
            </select>
          </div>
          <label class="chk"><input type="checkbox" v-model="s.enableZmodem" /> 启用 ZMODEM 文件传输</label>
        </fieldset>

        <fieldset>
          <legend>字体 / 排版</legend>
          <div class="grid">
            <label>英文字体</label>
            <select v-model="s.fontName">
              <option value="">（默认：等宽）</option>
              <option v-for="f in fonts" :key="f" :value="f">{{ f }}</option>
            </select>
            <label>中文字体</label>
            <select v-model="s.fontNameCjk">
              <option value="">（跟随系统回退）</option>
              <option v-for="f in fonts" :key="f" :value="f">{{ f }}</option>
            </select>
            <label>字号</label><input type="number" v-model.number="s.fontSize" min="8" max="32" />
            <label>字距</label>
            <div class="row2">
              <input type="number" v-model.number="s.charSpacing" min="-4" max="12" />
              <span class="unit">px，追加在每个字符宽之后</span>
            </div>
            <label>行距</label>
            <div class="row2">
              <input type="number" v-model.number="s.lineSpacing" min="-6" max="20" />
              <span class="unit">px，加在行高上</span>
            </div>
            <label>自适应</label>
            <select v-model.number="s.fontAutoFit">
              <option v-for="(m, i) in api.FONT_ADJUST_MODES" :key="i" :value="i">{{ m }}</option>
            </select>
          </div>
          <label class="chk"><input type="checkbox" v-model="s.antiAlias" /> 抗锯齿（关闭后文字更接近点阵锐利观感）</label>
          <p class="hint">英文字符命中「英文字体」，中文因字形缺失自动回退到「中文字体」；xterm 为单字号架构，中西文共用同一字号。</p>
        </fieldset>
      </div>
      <div class="foot">
        <button class="primary" @click="doSave">{{ save ? '保存并连接' : '连接' }}</button>
        <button @click="emit('close')">取消</button>
      </div>
    </div>
  </div>
</template>

<style scoped>
.mask { position: fixed; inset: 0; background: rgba(0, 0, 0, .55); display: flex; align-items: center; justify-content: center; z-index: 50; }
.dlg { width: 560px; max-height: 88vh; overflow-y: auto; background: #24272e; border: 1px solid #3d424b; border-radius: 10px; box-shadow: 0 12px 40px rgba(0, 0, 0, .5); }
.head { display: flex; justify-content: space-between; align-items: center; padding: 12px 16px; font-weight: 600; border-bottom: 1px solid #33373f; }
.x { background: none; border: none; color: #8a919e; cursor: pointer; font-size: 16px; }
.form { padding: 14px 16px; }
.grid { display: grid; grid-template-columns: 96px 1fr; gap: 8px 10px; align-items: center; }
.grid label { color: #8a919e; font-size: 13px; text-align: right; }
input, select { background: #1b1d22; color: #d5d9e0; border: 1px solid #3d424b; border-radius: 5px; padding: 5px 8px; font-size: 13px; width: 100%; box-sizing: border-box; }
fieldset { border: 1px solid #33373f; border-radius: 6px; margin-top: 12px; padding: 10px 12px; }
legend { color: #6a7180; font-size: 12px; padding: 0 6px; }
.chk { display: flex; align-items: center; gap: 6px; font-size: 13px; color: #b5bac4; margin: 8px 0; }
.chk input { width: auto; }
.row2 { display: flex; align-items: center; gap: 6px; }
.unit { color: #6a7180; font-size: 12px; white-space: nowrap; }
.hint { color: #6a7180; font-size: 12px; line-height: 1.5; margin: 6px 0 0; }
.tabs { padding: 8px 16px 0; } .tb { font-size: 13px; color: #6ab0f3; border-bottom: 2px solid #6ab0f3; padding-bottom: 6px; }
.foot { display: flex; justify-content: flex-end; gap: 8px; padding: 12px 16px; border-top: 1px solid #33373f; }
button { background: #2f333b; color: #d5d9e0; border: 1px solid #3d424b; border-radius: 5px; padding: 6px 16px; cursor: pointer; font-size: 13px; }
button.primary { background: #2f74c0; border-color: #2f74c0; color: #fff; }
button.primary:hover { background: #3a82cf; }
</style>
