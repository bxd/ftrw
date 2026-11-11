<script setup lang="ts">
import { computed, reactive, ref, onMounted, onBeforeUnmount } from 'vue'
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
if (p.menuClickEnabled == null) p.menuClickEnabled = true
if (p.ipLocationEnabled == null) p.ipLocationEnabled = true
if (p.ipLocationDb == null) p.ipLocationDb = ''
if (p.ipLocationUrl == null) p.ipLocationUrl = ''
if (p.proxyType == null) p.proxyType = 0
if (p.proxyHost == null) p.proxyHost = ''
if (p.proxyPort == null) p.proxyPort = 0
if (p.proxyUser == null) p.proxyUser = ''
if (p.proxyPass == null) p.proxyPass = ''
if (p.agentApiEnabled == null) p.agentApiEnabled = false
if (p.agentApiPort == null) p.agentApiPort = 0
if (p.agentApiToken == null) p.agentApiToken = ''

// 随系统启动不进 Preferences：它的唯一事实来源是注册表（HKCU\...\Run），
// 在应用里存一份镜像会导致用户从「启动」里手工删掉后又被偷偷写回来。
const autostart = ref(false)
const autostartInit = ref(false)
const autostartNote = ref('')

// 系统可用字体列表
const fonts = ref<string[]>([])

// 更新进度事件：这个窗口与主窗口都听 ip:update，互不干扰
let offIPU: (() => void) | undefined

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

/* ---------------- IP 归属地库 ---------------- */

// 状态行直接拿后端 Status() 的一行字（路径 + 大小 + 更新日期）。库不存在时
// Status() 是报错返回：那句错文本身就写明了「该去哪儿找库」，正好是要给用户看的。
const ipStatusText = ref('')
const ipUpdating = ref(false)

function refreshIPStatus() {
  api.IPStatus()
    .then((s) => { ipStatusText.value = s || '未找到本地库' })
    .catch((e: any) => { ipStatusText.value = e?.message || String(e) })
}

async function doIPUpdate() {
  ipUpdating.value = true
  ipStatusText.value = '正在发起更新…'
  try {
    await api.StartIPUpdate()
  } catch (e: any) {
    ipUpdating.value = false
    ipStatusText.value = `更新启动失败：${e?.message || e}`
  }
  // 进度与结果靠 ip:update 事件收（下面挂了监听），这里不等返回
}

/* ---------------- 网络代理 ---------------- */

const proxyOn = computed(() => p.proxyType !== 0)
const proxyTesting = ref(false)
const proxyTestText = ref('')

// 测的是界面上这份还没存盘的设置：改完就能验，不必先保存再等下一次启动才看出效果。
async function doProxyTest() {
  proxyTesting.value = true
  proxyTestText.value = '正在经代理连 api.github.com…'
  try {
    proxyTestText.value = await api.TestProxy({
      type: p.proxyType,
      host: p.proxyHost.trim(),
      port: p.proxyPort,
      user: p.proxyUser.trim(),
      pass: p.proxyPass,
    })
  } catch (e: any) {
    proxyTestText.value = `测试失败：${e?.message || e}`
  } finally {
    proxyTesting.value = false
  }
}

/* ---------------- Agent API ---------------- */

const agentTokenBusy = ref(false)
const agentTokenNote = ref('')

// 重生成走后端直达（存盘 + 监听器换 token 一气完成）：旧 token 当场作废。
async function doRegenAgentToken() {
  agentTokenBusy.value = true
  agentTokenNote.value = ''
  try {
    p.agentApiToken = await api.RegenerateAgentToken()
    agentTokenNote.value = '新 token 已生效（旧值立刻作废），无需再点保存'
  } catch (e: any) {
    agentTokenNote.value = `重生成失败：${e?.message || e}`
  } finally {
    agentTokenBusy.value = false
  }
}

onMounted(() => {
  avail.value = { w: window.screen.availWidth, h: window.screen.availHeight }
  api.ListSystemFonts().then((f) => { fonts.value = f || [] }).catch(() => { fonts.value = [] })
  api.GetAutostart().then((v) => { autostart.value = !!v; autostartInit.value = !!v }).catch(() => {})
  refreshIPStatus()
  offIPU = api.onIPUpdate((d) => {
    if (d.state === 'downloading') {
      ipUpdating.value = true
      ipStatusText.value = d.message || '正在下载 IP 库…'
      return
    }
    ipUpdating.value = false
    if (d.state === 'error') { ipStatusText.value = d.error || d.message || 'IP 库更新失败'; return }
    // 成功：重新读一次，让状态行换成新库的路径与日期
    refreshIPStatus()
  })
})

onBeforeUnmount(() => { offIPU?.() })

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
          <label class="chk"><input type="checkbox" v-model="p.menuClickEnabled" /> 鼠标点击 BBS 菜单与文章列表（悬停出现高亮才可点）</label>
        </fieldset>
        <fieldset>
          <legend>网络代理</legend>
          <div class="grid grid-in">
            <label>类型</label>
            <select v-model.number="p.proxyType">
              <option :value="0">不用代理</option>
              <option :value="2">SOCKS4 / 4a</option>
              <option :value="3">SOCKS5</option>
              <option :value="4">HTTP（CONNECT）</option>
            </select>
            <label>地址</label>
            <div class="row2">
              <input v-model="p.proxyHost" placeholder="127.0.0.1" :disabled="!proxyOn" />
              <input type="number" v-model.number="p.proxyPort" class="port" placeholder="端口" :disabled="!proxyOn" />
            </div>
            <label>认证</label>
            <div class="row2">
              <input v-model="p.proxyUser" placeholder="用户名（可空）" :disabled="!proxyOn" />
              <input type="password" v-model="p.proxyPass" placeholder="口令（可空）" :disabled="!proxyOn" />
            </div>
          </div>
          <button class="mini" :disabled="!proxyOn || proxyTesting" @click="doProxyTest">{{ proxyTesting ? '测试中…' : '测试连通' }}</button>
          <div class="hint">{{ proxyTestText || '代理同时管住终端连接（Telnet/SSH）、IP 库下载与链接预览三条出站路；端口留空按类型补默认（SOCKS 1080 / HTTP 8080）。单个站点要直连或换代理，在站点编辑里另有设置。' }}</div>
        </fieldset>
        <fieldset>
          <legend>Agent API（给 AI agent 的本地操控入口）</legend>
          <label class="chk"><input type="checkbox" v-model="p.agentApiEnabled" /> 启用：在本机 127.0.0.1 开一个 HTTP 接口，让 AI agent 读屏幕内容、发按键与文字、跑应用命令</label>
          <div class="grid grid-in">
            <label>端口</label>
            <input type="number" v-model.number="p.agentApiPort" min="1" max="65535" placeholder="37837" :disabled="!p.agentApiEnabled" />
            <label>Token</label>
            <div class="row2">
              <input v-model="p.agentApiToken" readonly spellcheck="false" placeholder="开启并保存后自动生成" />
              <button class="mini inline" :disabled="agentTokenBusy || !p.agentApiToken" @click="doRegenAgentToken">{{ agentTokenBusy ? '生成中…' : '重生成' }}</button>
            </div>
          </div>
          <div class="note" v-if="agentTokenNote">{{ agentTokenNote }}</div>
          <div class="hint">只听回环地址，局域网里谁也连不上；但本机任意进程拿了 token 就能看你的屏（含密码回显）并替你操作 BBS —— token 当密码对待。入口：<code>GET /ftrw/status</code>（探活）、<code>GET /ftrw/sites</code>、<code>GET /ftrw/tabs</code>、<code>GET /ftrw/screen</code>、<code>POST /ftrw/send</code>、<code>POST /ftrw/wait</code>、<code>POST /ftrw/command</code>、<code>POST /ftrw/connect</code>、<code>POST /ftrw/tab/activate|close</code>，鉴权头 <code>X-FTRW-Token</code>。Shell 标签可读回滚区，BBS 标签只返回当前屏。</div>
        </fieldset>
        <fieldset>
          <legend>IP 归属地</legend>
          <label class="chk"><input type="checkbox" v-model="p.ipLocationEnabled" /> 鼠标悬停屏幕上的 IPv4 时显示归属地（只读本地纯真库，不把地址发给在线接口）</label>
          <div class="grid grid-in">
            <label>库文件</label>
            <input v-model="p.ipLocationDb" placeholder="留空自动查找：程序目录 → 配置目录" />
            <label>下载地址</label>
            <input v-model="p.ipLocationUrl" placeholder="留空用内置镜像 metowolf/qqwry.dat" />
          </div>
          <button class="mini" :disabled="ipUpdating" @click="doIPUpdate">{{ ipUpdating ? '更新中…' : '立即更新' }}</button>
          <div class="hint">{{ ipStatusText || '正在读取库状态…' }}</div>
          <div class="hint">本地库缺失或距上次尝试超一天时，后台自动更新一轮。自动更新失败（无网、镜像不可达）可自行下载 qqwry.dat 存到上面“库文件”的路径或配置目录，保存后立即生效。</div>
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
button.mini { margin-top: 10px; padding: 4px 12px; font-size: 12px; }
button.mini.inline { margin-top: 0; flex: 0 0 auto; }
.row2 input.port { flex: 0 0 84px; width: 84px; }
.foot { display: flex; justify-content: flex-end; gap: 8px; padding: 12px 16px; border-top: 1px solid #33373f; }
button { background: #2f333b; color: #d5d9e0; border: 1px solid #3d424b; border-radius: 5px; padding: 6px 16px; cursor: pointer; font-size: 13px; }
button.primary { background: #2f74c0; border-color: #2f74c0; color: #fff; }
</style>
