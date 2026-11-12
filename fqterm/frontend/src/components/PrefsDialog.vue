<script setup lang="ts">
// 偏好设置对话框：左侧分组导航（启动器 / 终端 / 通用），右侧显示所选组的设置。
// 「终端」组沿用最早的整页偏好（编码/字体/行为等），「通用」收跨模块的东西
// （代理、IP 归属地、系统集成、quake），「启动器」是 launcher 分支新加的搜索范围设置。
// 保存仍是一次 SetPreferences 全量回写：后端归一化字段，各组改动静默互相覆盖不了。
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
if (p.quakeHideOnBlur == null) p.quakeHideOnBlur = false
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
if (!Array.isArray(p.launcherDirs)) p.launcherDirs = []
if (!Array.isArray(p.launcherExts)) p.launcherExts = ['exe', 'lnk', 'bat', 'ps1', 'com']

// 左侧导航：tab → 右侧面板。默认打开终端组（老用户点开设置 most 常改的还是终端）。
const tabs = [
  { id: 'launcher', label: '启动器' },
  { id: 'terminal', label: '终端' },
  { id: 'general', label: '通用' },
  { id: 'about', label: '关于' },
] as const
type TabId = (typeof tabs)[number]['id']
const tab = ref<TabId>('terminal')

/* ---------------- 启动器：搜索范围 ---------------- */

// 后缀框直接编辑一个逗号/空格分隔的字符串，保存时才拆成数组：编辑中间态
// （刚打完逗号还没打下一个）不值得为它维护逐 token 的控件。
const extsText = ref(p.launcherExts.join(', '))
const dirNote = ref('')

function parseExts(s: string): string[] {
  return s.toLowerCase().split(/[\s,;，；]+/).map((v) => v.replace(/^\./, '').trim()).filter(Boolean)
}

async function addDir() {
  try {
    const d = await api.PickLauncherDir()
    if (!d) return // 取消：空串不是错误
    if (p.launcherDirs.some((v) => v.toLowerCase() === d.toLowerCase())) {
      dirNote.value = `这个目录已经在列表里：${d}`
      return
    }
    p.launcherDirs.push(d)
    dirNote.value = ''
  } catch (e: any) {
    dirNote.value = `打开目录选择框失败：${e?.message || e}`
  }
}

function removeDir(i: number) {
  p.launcherDirs.splice(i, 1)
}

/* ---------------- 随系统启动 ---------------- */

// 随系统启动不进 Preferences：它的唯一事实来源是注册表（HKCU\...\Run），
// 在应用里存一份镜像会导致用户从「启动」里手工删掉后又被偷偷写回来。
const autostart = ref(false)
const autostartInit = ref(false)
const autostartNote = ref('')

// 系统可用字体列表
const fonts = ref<string[]>([])

// 关于页版本号：后端读 exe 版本资源，与构建版本串同源；读不到先占位。
const appVersion = ref('…')

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

onMounted(() => {
  avail.value = { w: window.screen.availWidth, h: window.screen.availHeight }
  api.ListSystemFonts().then((f) => { fonts.value = f || [] }).catch(() => { fonts.value = [] })
  api.GetAutostart().then((v) => { autostart.value = !!v; autostartInit.value = !!v }).catch(() => {})
  api.AppVersion().then((v) => { appVersion.value = v || 'dev' }).catch(() => { appVersion.value = 'dev' })
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
  // 启动器后缀框落进对象：整列被清空时后端会补回出厂值（那不等于「不搜文件」）。
  p.launcherExts = parseExts(extsText.value)
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
      <div class="body">
        <nav class="side">
          <button v-for="t in tabs" :key="t.id" class="nav" :class="{ on: tab === t.id }" @click="tab = t.id">{{ t.label }}</button>
        </nav>
        <div class="form">
          <!-- ============ 启动器 ============ -->
          <template v-if="tab === 'launcher'">
            <fieldset>
              <legend>额外搜索目录</legend>
              <div class="hint top">开始菜单与桌面永远在搜索范围内，这里只加「额外目录」（比如软件安装目录），递归扫描其中的可执行文件。</div>
              <div class="dirlist">
                <div v-for="(d, i) in p.launcherDirs" :key="d" class="dirrow">
                  <span class="dirpath" :title="d">{{ d }}</span>
                  <button class="del" @click="removeDir(i)">移除</button>
                </div>
                <div v-if="!p.launcherDirs.length" class="hint top">还没有额外目录。</div>
              </div>
              <button class="mini" @click="addDir">添加目录…</button>
              <div class="note" v-if="dirNote">{{ dirNote }}</div>
            </fieldset>
            <fieldset>
              <legend>文件类型（后缀）</legend>
              <input v-model="extsText" placeholder="exe, lnk, bat, ps1, com" />
              <div class="hint">逗号或空格分隔，不带点；只对上面的额外目录生效（开始菜单照常只认 .lnk）。整列清空保存会恢复出厂的 exe, lnk, bat, ps1, com。</div>
            </fieldset>
            <fieldset>
              <legend>呼出</legend>
              <div class="hint top">全局热键默认 <code>双击 Ctrl+Alt</code>（间隔可在「热键」窗口的跨应用热键页调整）；搜索支持应用名、exe 名与中文名的拼音（全拼 / 首字母）。</div>
            </fieldset>
          </template>

          <!-- ============ 终端 ============ -->
          <template v-if="tab === 'terminal'">
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
          </template>

          <!-- ============ 通用 ============ -->
          <template v-if="tab === 'general'">
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
          </template>

          <!-- ============ 关于 ============ -->
          <template v-if="tab === 'about'">
            <div class="about">
              <div class="about-name">FTRW</div>
              <div class="about-ver">版本 {{ appVersion }}</div>
              <div class="hint top">FQTerm 的 Wails 3 重写：Telnet/SSH BBS 终端，带启动器、双栏文件管理器与 ZMODEM 传输。</div>
            </div>
          </template>
        </div>
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
.dlg { width: 720px; max-height: 88vh; display: flex; flex-direction: column; background: #24272e; border: 1px solid #3d424b; border-radius: 10px; }
.head { display: flex; justify-content: space-between; align-items: center; padding: 12px 16px; font-weight: 600; border-bottom: 1px solid #33373f; flex: none; }
.x { background: none; border: none; color: #8a919e; cursor: pointer; font-size: 16px; }
.body { display: flex; min-height: 0; flex: 1; }
.side { display: flex; flex-direction: column; gap: 4px; padding: 12px 8px; border-right: 1px solid #33373f; flex: 0 0 96px; }
.nav { text-align: left; background: none; border: none; border-radius: 6px; padding: 8px 12px; font-size: 14px; color: #8a919e; }
.nav:hover { background: #2b2f37; color: #d5d9e0; }
.nav.on { background: #2c313a; color: #e6e9ef; font-weight: 600; }
.form { padding: 14px 16px; overflow-y: auto; flex: 1; min-width: 0; }
.grid { display: grid; grid-template-columns: 96px 1fr; gap: 8px 10px; align-items: center; }
.grid label { color: #8a919e; font-size: 13px; text-align: right; }
input, select { background: #1b1d22; color: #d5d9e0; border: 1px solid #3d424b; border-radius: 5px; padding: 5px 8px; font-size: 13px; width: 100%; box-sizing: border-box; }
fieldset { border: 1px solid #33373f; border-radius: 6px; margin-top: 12px; padding: 10px 12px; }
fieldset:first-child { margin-top: 0; }
legend { color: #6a7180; font-size: 12px; padding: 0 6px; }
.chk { display: flex; align-items: center; gap: 6px; font-size: 13px; color: #b5bac4; margin: 6px 0; }
.chk input { width: auto; }
.chk:has(input:disabled) { color: #6a7180; }
.grid-in { grid-template-columns: 84px 1fr; }
.unit { color: #6a7180; font-size: 12px; white-space: nowrap; }
.note { color: #6ab0f3; font-size: 12px; margin: 6px 0 0; }
.hint { color: #6a7180; font-size: 12px; margin-top: 6px; }
.hint.top { margin-top: 0; margin-bottom: 8px; }
.hint code { color: #b5bac4; background: #1b1d22; border: 1px solid #3d424b; border-radius: 3px; padding: 0 4px; }
.about { padding: 8px 4px; }
.about-name { font-size: 22px; font-weight: 600; color: #e6e9ef; }
.about-ver { color: #6ab0f3; font-size: 14px; margin: 6px 0 14px; }
.row2 { display: flex; align-items: center; gap: 6px; }
button.mini { margin-top: 10px; padding: 4px 12px; font-size: 12px; }
.row2 input.port { flex: 0 0 84px; width: 84px; }
.dirlist { display: flex; flex-direction: column; gap: 4px; max-height: 180px; overflow-y: auto; }
.dirrow { display: flex; align-items: center; gap: 8px; background: #1b1d22; border: 1px solid #3d424b; border-radius: 5px; padding: 5px 8px; }
.dirpath { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; color: #d5d9e0; font-size: 13px; }
button.del { flex: none; padding: 2px 10px; font-size: 12px; color: #e07171; }
.foot { display: flex; justify-content: flex-end; gap: 8px; padding: 12px 16px; border-top: 1px solid #33373f; flex: none; }
button { background: #2f333b; color: #d5d9e0; border: 1px solid #3d424b; border-radius: 5px; padding: 6px 16px; cursor: pointer; font-size: 13px; }
button.primary { background: #2f74c0; border-color: #2f74c0; color: #fff; }
</style>
