<script setup lang="ts">
import { reactive, ref, computed, onMounted } from 'vue'
import * as api from '../api'
import { KEY_TYPE_NAMES } from '../keymap'

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
  // useAnsiColor 不在这里兜底：后端 normalizeSite 已经按「旧站点」补成开，而用户真关掉时
  // 这里再补一次就会把「ANSI 颜色」永远锁在开。alwaysHighlight 出厂为关，缺键正好。
  alwaysHighlight: raw.alwaysHighlight ?? false,
  // 按键方案出厂 BBS（与后端 defaultSite 一致）；SSH 私钥两个字段旧站点根本没有。
  keyType: raw.keyType ?? 0,
  keyFile: raw.keyFile ?? '',
  passphrase: raw.passphrase ?? '',
  // 代理（D3）：旧站点根本没这几个字段，缺键就是「跟随全局设置」，行为与升级前逐字节一致。
  proxyType: raw.proxyType ?? 0,
  proxyHost: raw.proxyHost ?? '',
  proxyPort: raw.proxyPort ?? 0,
  proxyUser: raw.proxyUser ?? '',
  proxyPass: raw.proxyPass ?? '',
})

// 系统可用字体列表（点击字体下拉时展示全部供选择）
const fonts = ref<string[]>([])
onMounted(() => {
  api.ListSystemFonts().then((f) => { fonts.value = f || [] }).catch(() => { fonts.value = [] })
})

// 按键方案的下拉文案住在 keymap.ts（判定处），这里只拿过来用。
const kmNames = KEY_TYPE_NAMES

// 本地 shell：host 存可执行文件路径，port/保活/重连/ZMODEM 都没有意义。
const isLocal = computed(() => s.protocolType === 3)

// SSH 认证方式只是个界面开关：后端看的是 keyFile 有没有填（选「密码」时下面 doSave 会清掉）。
// 不再加一个 authMode 字段，免得两处状态对不上时没人说得清到底听谁的。
const authKind = ref(s.keyFile ? 'key' : 'pass')

function onAuthKind() {
  if (authKind.value !== 'key') { s.keyFile = ''; s.passphrase = '' }
}

const pickErr = ref('')

/** 走原生选文件框（后端 AppService.PickKeyFile，默认落在 ~/.ssh）。取消时后端返空串，不动已填的值。 */
async function pickKeyFile() {
  try {
    const p = await api.PickKeyFile()
    if (p) s.keyFile = p
  } catch (e: any) {
    pickErr.value = String(e?.message || e)
  }
}

// 启动参数在界面上是一行空格分隔的文本，内部是 string[]。带空格的路径或发行版名用
// 双引号括起来就能当成一个参数（"Ubuntu 24.04 LTS"）。
const argsText = ref(joinArgs(raw.shellArgs || []))

function joinArgs(a: string[]): string {
  return a.map((x) => (/\s|"/.test(x) ? `"${x.replace(/"/g, '')}"` : x)).join(' ')
}

function splitArgs(t: string): string[] {
  const m = t.match(/"[^"]*"|\S+/g)
  return (m || []).map((x) => x.replace(/^"|"$/g, '')).filter(Boolean)
}

// When protocol changes, auto-adjust default port
function onProtocol() {
  if (s.protocolType >= 1 && s.port === 23) s.port = 22
  if (s.protocolType === 0 && s.port === 22) s.port = 23
  // 本地 shell 没有服务器：把连接类开关一律关掉。留着它们不只是没意义，保活还会
  // 定时往 shell 里灌 NUL 字节。
  if (s.protocolType === 3) {
    s.port = 0
    s.antiIdle = false
    s.autoReconnect = false
    s.enableZmodem = false
    s.autoLogin = false
    if (!s.group) s.group = api.SHELL_GROUP
    // 从一个远程站点切过来时，主机名当路径用没意义，清空让它回退到系统默认 shell。
    // 不带分隔符的名字（pwsh.exe 这种）不动：CreateProcess 会自己在 PATH 里找。
    if (s.host && !/[\\/]/.test(s.host)) s.host = ''
  }
}

// when hostType BBS toggled, set sensible defaults
function onHostType() {
  if (s.hostType === 0) { s.termType = 'vtnt'; s.serverEncodingID = 0 }
  else { s.termType = 'xterm-256color'; s.serverEncodingID = 2 }
}

// 代理那一栏的提示跟着选择变：这里只解释「这一档是什么意思」，全局那份的详情在偏好设置里。
const proxyHint = computed(() => {
  switch (s.proxyType) {
    case 0:
      return '跟「偏好设置 → 网络代理」里那一份一致；那里没配就是直连。绝大多数站点选这个。'
    case 1:
      return '即使全局配了代理，这一条也直接连出去 —— 内网机器、局域网 BBS 走代理反而不通。'
    default:
      return '只影响这一条连接，不回填到全局设置。地址留空等于不用代理；端口留空按类型补默认（SOCKS 1080 / HTTP 8080）。'
  }
})

function doSave() {
  if (isLocal.value) {
    // 本地 shell 不参与下面那套「旧站点缺字段」兼容归一化：它的 maxIdleSeconds=0 是
    // 故意关掉，补成 120 会让保活重新启动。
    s.shellArgs = splitArgs(argsText.value)
    emit('save', { ...s }, props.index, props.save)
    return
  }
  // 字段归一化：避免旧站点数据缺项导致后端拿到 0 值而行为错乱。
  if (authKind.value !== 'key') { s.keyFile = ''; s.passphrase = '' }
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
          <template v-if="!isLocal">
            <label>主机</label><input v-model="s.host" placeholder="bbs.example.com" />
            <label>端口</label><input type="number" v-model.number="s.port" />
          </template>
          <template v-else>
            <label>程序</label><input v-model="s.host" placeholder="C:\Windows\System32\cmd.exe" />
            <label>参数</label><input v-model="argsText" placeholder="-d Ubuntu 或 --login -i" />
          </template>
          <label>类型</label>
          <select v-model.number="s.hostType" @change="onHostType">
            <option :value="0">BBS</option><option :value="1">*NIX</option>
          </select>
          <label>分组</label><input v-model="s.group" placeholder="默认分组" />
        </div>

        <p v-if="isLocal" class="hint">
          本地 shell 直接起一个本机进程（Windows 走 ConPTY 伪终端，输出统一为 UTF-8，服务器编码请保持 UTF-8）。
          路径写完整绝对路径；参数空格分隔，带空格的值用双引号括起来。点「保存并连接」立刻开一个 shell 终端。
        </p>

        <fieldset v-if="s.protocolType >= 1 && s.protocolType <= 2">
          <legend>SSH 登录</legend>
          <div class="grid">
            <label>用户名</label><input v-model="s.userName" />
            <label>认证方式</label>
            <select v-model="authKind" @change="onAuthKind">
              <option value="pass">密码</option>
              <option value="key">公钥（私钥文件）</option>
            </select>
            <template v-if="authKind === 'pass'">
              <label>密码</label><input type="password" v-model="s.password" />
            </template>
            <template v-else>
              <label>私钥文件</label>
              <div class="row2">
                <input v-model="s.keyFile" placeholder="C:\Users\me\.ssh\id_ed25519" />
                <button class="pick" @click="pickKeyFile">浏览…</button>
              </div>
              <label>私钥口令</label><input type="password" v-model="s.passphrase" placeholder="无口令留空" />
            </template>
          </div>
          <p class="hint">私钥要 OpenSSH / PEM 格式（PuTTY 的 .ppk 不认）。读不到文件或口令错会在状态栏直接报「read key / parse key」。选了公钥就不再拿密码去做密码认证，也不再往远端 shell 灌凭据。</p>
          <p class="hint" v-if="pickErr">选文件失败：{{ pickErr }}</p>
        </fieldset>

        <fieldset v-if="!isLocal">
          <legend>自动登录</legend>
          <label class="chk"><input type="checkbox" v-model="s.autoLogin" /> 启用自动登录</label>
          <div class="grid">
            <label>登录前</label><input v-model="s.preLogin" placeholder="new / guest" />
            <label>登录后</label><input v-model="s.postLogin" />
          </div>
        </fieldset>

        <fieldset v-if="!isLocal">
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

        <fieldset v-if="!isLocal">
          <legend>网络代理</legend>
          <div class="grid">
            <label>代理</label>
            <select v-model.number="s.proxyType">
              <option :value="0">跟随全局设置</option>
              <option :value="1">本站点强制直连</option>
              <option :value="2">SOCKS4 / 4a</option>
              <option :value="3">SOCKS5</option>
              <option :value="4">HTTP（CONNECT）</option>
            </select>
          </div>
          <div class="grid" v-if="s.proxyType >= 2">
            <label>地址</label>
            <div class="row2">
              <input v-model="s.proxyHost" placeholder="127.0.0.1" />
              <input type="number" v-model.number="s.proxyPort" class="port" placeholder="端口" />
            </div>
            <label>认证</label>
            <div class="row2">
              <input v-model="s.proxyUser" placeholder="用户名（可空）" />
              <input type="password" v-model="s.proxyPass" placeholder="口令（可空）" />
            </div>
          </div>
          <p class="hint">{{ proxyHint }} SOCKS5 与 HTTP 会把域名原样交给代理去解析，本地 DNS 不通也不影响连接。</p>
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
            <label>按键方案</label>
            <select v-model.number="s.keyType">
              <option v-for="(n, i) in kmNames" :key="i" :value="i">{{ n }}</option>
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
          <div class="chkrow">
            <label class="chk"><input type="checkbox" v-model="s.alwaysHighlight" /> 总是高亮</label>
            <label class="chk"><input type="checkbox" v-model="s.useAnsiColor" /> ANSI 颜色</label>
          </div>
          <p class="hint">总是高亮：所有文字按调色板的亮色半区绘制（等价于原版把每一格前景都当加粗），暗色版面更醒目。ANSI 颜色关掉后只分黑白：带底色的地方黑字白底，其余白字铺默认底，专治配色花哨看不清。</p>
          <p class="hint">按键方案只管与终端原生编码不同的那几个键：BBS 把 Home/End 改成老式 \x1b[1~ / \x1b[4~、F1-F4 改成 \x1b[11~…（水木的输入行只认这套）；XTERM 一个键都不接管；VT100 则不管远端怎么设，方向键/Home/End 一律发应用光标键，小键盘走 \x1bOp… 转义。退格键：^H 给只认 0x08 的老站，^? 是绝大多数系统的标准。</p>
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
.chkrow { display: flex; gap: 28px; }
.row2 { display: flex; align-items: center; gap: 6px; }
.row2 input.port { flex: 0 0 88px; width: 88px; }
.pick { flex: none; padding: 5px 10px; }
.unit { color: #6a7180; font-size: 12px; white-space: nowrap; }
.hint { color: #6a7180; font-size: 12px; line-height: 1.5; margin: 6px 0 0; }
.tabs { padding: 8px 16px 0; } .tb { font-size: 13px; color: #6ab0f3; border-bottom: 2px solid #6ab0f3; padding-bottom: 6px; }
.foot { display: flex; justify-content: flex-end; gap: 8px; padding: 12px 16px; border-top: 1px solid #33373f; }
button { background: #2f333b; color: #d5d9e0; border: 1px solid #3d424b; border-radius: 5px; padding: 6px 16px; cursor: pointer; font-size: 13px; }
button.primary { background: #2f74c0; border-color: #2f74c0; color: #fff; }
button.primary:hover { background: #3a82cf; }
</style>
