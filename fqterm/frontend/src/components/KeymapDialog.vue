<script setup lang="ts">
import { reactive, ref, computed, onMounted, onBeforeUnmount } from 'vue'
import * as api from '../api'
import { analyzeBindings, eventToken, keyseq, listCommands, parseSeq } from '../keyseq'
import {
  analyzeGlobalKeys, describeGlobalSeq, isModOnly, isModifierKey, modsFromEvent, modsToToken, parseGlobalSeq,
} from '../globalkey'

const props = defineProps<{ km: api.KeymapConfig; gk: api.GlobalKeymap }>()
const emit = defineEmits<{
  (e: 'save', km: api.KeymapConfig, gk: api.GlobalKeymap): void
  (e: 'close'): void
}>()

/** 编辑态用拆开的 cmdId/cmdArg，保存时再拼回 "id:arg"，避免用户手写冒号。 */
interface Row {
  enabled: boolean
  keys: string
  cmdId: string
  cmdArg: string
  note: string
}

const cmds = listCommands()

function splitCommand(spec: string): { id: string; arg: string } {
  const i = spec.indexOf(':')
  return i < 0 ? { id: spec, arg: '' } : { id: spec.slice(0, i), arg: spec.slice(i + 1) }
}

const cfg = reactive({
  enabled: props.km.enabled !== false,
  timeoutMs: props.km.timeoutMs || 1000,
  echo: props.km.echo !== false,
})

const rows = reactive<Row[]>(
  (props.km.bindings || []).map((b) => {
    const { id, arg } = splitCommand(b.command)
    return { enabled: b.enabled !== false, keys: b.keys, cmdId: id, cmdArg: arg, note: b.note || '' }
  }),
)

function addRow() {
  rows.push({ enabled: true, keys: '', cmdId: cmds[0]?.id || 'demo.echo', cmdArg: '', note: '' })
}

/* ---------------- 跨应用热键（系统级，可编辑）---------------- */

// 与下面的应用内绑定是两套东西：由后端 WH_KEYBOARD_LL 低级键盘钩子匹配，本窗口
// 没焦点也生效。形状只有单段组合键与两段相同键的双击，语法见 globalkey.ts。
interface GRow {
  enabled: boolean
  keys: string
  action: string
  note: string
}

const gcfg = reactive({
  enabled: props.gk?.enabled !== false,
  doubleTapMs: props.gk?.doubleTapMs || 400,
})

const grows = reactive<GRow[]>(
  (props.gk?.bindings || []).map((b) => ({ enabled: b.enabled !== false, keys: b.keys, action: b.action, note: b.note || '' })),
)

const gacts = ref<api.GlobalAction[]>([])
const gactsErr = ref('')

function addGRow() {
  grows.push({ enabled: true, keys: '', action: gacts.value[0]?.id || 'quake.toggle', note: '' })
}

/* ---------------- 全局热键录制器 ---------------- */

// 修饰键双击靠的是「一整轮」的边界：从零修饰键按下、到全部松开，才算一次。
// 期间累计按过的修饰键位，松手那一刻映成 <C-M> 这样的 token；下一轮得到同样的
// token 就凑成双击。非修饰键走 eventToken 直接成一段组合键——跨应用时钩子必须在
// 按下那一刻决断，所以录制器也只在这两种形状上给结果。
const recG = reactive({ row: -1, buf: [] as string[] })
const recGHint = ref('')
let cycleMods = 0

const GK_REC_HINT = '组合键直接按下即录；双击请按下并松开修饰键（如 Ctrl+Alt）两次；回车结束，Esc 取消'

function toggleRecG(i: number) {
  if (recG.row === i) { stopRecG(); return }
  stopRec() // 两个录制器互斥：同时开着会抢同一个 keydown
  // 录制一律从空开始，不能像应用内录制器那样预装已有值：全局表只有「一段组合键」和
  // 「两段相同 token 的双击」两种形状，预装了 <C-M> <C-M> 的双击行会在第一轮松开时
  // 就被当成「两段已齐」直接收工，看上去像按一次就退出且什么都没录上。
  const cur = (grows[i]?.keys || '').trim()
  recG.buf = []
  recG.row = i
  cycleMods = 0
  recGHint.value = cur ? `原值 ${cur} · ${GK_REC_HINT}` : GK_REC_HINT
  // 焦点留在「录制」按钮上时，再按空格/回车会二次触发按钮而把录制取消掉
  if (document.activeElement instanceof HTMLElement) document.activeElement.blur()
}

function stopRecG() {
  recG.row = -1
  recG.buf = []
  recGHint.value = ''
  cycleMods = 0
}

function onRecGKey(ev: KeyboardEvent) {
  if (recG.row < 0) return
  if (ev.key === 'Escape') {
    ev.preventDefault(); ev.stopPropagation()
    stopRecG()
    return
  }
  if (ev.key === 'Enter') {
    ev.preventDefault(); ev.stopPropagation()
    if (recG.buf.length) finishRecG()
    return
  }
  if (isModifierKey(ev.key)) {
    ev.preventDefault(); ev.stopPropagation() // 别让 Alt 抢走焦点、弹出系统菜单
    if (!ev.repeat) cycleMods |= modsFromEvent(ev)
    return
  }
  const tok = eventToken(ev)
  if (!tok) return
  ev.preventDefault(); ev.stopPropagation()
  grows[recG.row].keys = tok
  stopRecG() // 单段组合键录到即完成
}

function onRecGKeyUp(ev: KeyboardEvent) {
  if (recG.row < 0) return
  // 只认「最后一个松开的修饰键」作为一轮的结束：中途松手时其余修饰键还按着，本轮未完
  if (modsFromEvent(ev) !== 0) return
  if (!cycleMods) return
  const tok = modsToToken(cycleMods)
  cycleMods = 0
  if (!recG.buf.length) {
    recG.buf = [tok]
    recGHint.value = `已录到 ${tok}，再按一次同样的组合键构成双击，或按回车结束`
    return
  }
  if (recG.buf[0] === tok) { finishRecG(); return }
  // 两轮不是同一个组合键：以这一轮重新起头，否则用户只能先 Esc 再重来
  const prev = recG.buf[0]
  recG.buf = [tok]
  recGHint.value = `与第一段 ${prev} 不同，已重新起头；双击要按同一个组合键`
}

function finishRecG() {
  const i = recG.row
  const tokens = [...recG.buf]
  stopRecG()
  if (i < 0 || !tokens.length) return
  // 只录到一段纯修饰键就回车：用户要的就是双击，替他补齐第二段
  grows[i].keys = tokens.length === 1 && isModOnly(tokens[0]) ? `${tokens[0]} ${tokens[0]}` : tokens.join(' ')
}

/* ---------------- 按键录制 ---------------- */

const rec = reactive({ row: -1, buf: [] as string[] })
const recHint = ref('')
// 编辑期间需要让引擎静默，否则录制时按下的 C-x 会被真的当作热键执行。
let savedEnabled = true

function toggleRec(i: number) {
  if (rec.row === i) { stopRec(); return }
  stopRecG() // 两个录制器互斥：同时开着会抢同一个 keydown
  // 录制一律从空开始：用户要从头录一条新序列，不是在旧值后面追加；
  // 旧值只用文案提示（与全局录制器 toggleRecG 同一形状）。
  const cur = (rows[i]?.keys || '').trim()
  rec.buf = []
  rec.row = i
  recHint.value = cur ? `原值 ${cur} · 从头按下按键组成新序列，回车结束` : '按下按键组成序列，回车结束'
  // 焦点留在「录制」按钮上时，再按空格/回车会二次触发按钮而把录制取消掉
  if (document.activeElement instanceof HTMLElement) document.activeElement.blur()
}

function stopRec() {
  rec.row = -1
  rec.buf = []
  recHint.value = ''
}

function onAnyKeyDown(ev: KeyboardEvent) {
  if (rec.row >= 0) { onRecKey(ev); return }
  if (recG.row >= 0) onRecGKey(ev)
}

function onRecKey(ev: KeyboardEvent) {
  if (rec.row < 0) return
  const tok = eventToken(ev)
  if (!tok) return // 单纯按 Ctrl/Alt/Shift 不算一个 token
  ev.preventDefault()
  ev.stopPropagation()
  if (tok === '<RET>') {
    if (rec.buf.length) { finishRec(); return }
    return
  }
  if (rec.buf.length >= 8) { finishRec(); return }
  rec.buf.push(tok)
  rows[rec.row].keys = rec.buf.join(' ')
}

function finishRec() {
  const i = rec.row
  const keys = rec.buf.join(' ')
  stopRec()
  if (i >= 0 && keys) rows[i].keys = keys
}

/* ---------------- 校验与冲突 ---------------- */

const bindingList = computed<api.KeyBinding[]>(() =>
  rows.map((r) => ({
    keys: r.keys.trim(),
    command: r.cmdArg ? `${r.cmdId}:${r.cmdArg}` : r.cmdId,
    enabled: r.enabled,
    note: r.note,
  })),
)

const issues = computed(() => analyzeBindings(bindingList.value))

function rowIssues(i: number) {
  return issues.value.get(i) || []
}

const syntaxErrors = computed(() =>
  rows.map((r, i) => ({ i, err: r.keys.trim() ? parseSeq(r.keys).error : '序列为空' })).filter((x) => x.err),
)

/* ---------------- 全局表的校验与保存 ---------------- */

const globalList = computed<api.GlobalKey[]>(() =>
  grows.map((r) => ({ keys: r.keys.trim(), action: r.action, enabled: r.enabled, note: r.note })),
)

const gactIds = computed(() => new Set(gacts.value.map((a) => a.id)))
const gissues = computed(() => analyzeGlobalKeys(globalList.value, gactIds.value))

function gRowIssue(i: number) {
  return gissues.value.get(i) || []
}

const gSyntaxErrors = computed(() =>
  grows.map((r, i) => ({ i, err: r.keys.trim() ? parseGlobalSeq(r.keys).error : '序列为空' })).filter((x) => x.err),
)

const busy = ref('')

async function doSave() {
  if (syntaxErrors.value.length) {
    busy.value = `应用内第 ${syntaxErrors.value.map((x) => x.i + 1).join('、')} 行的序列不合法，请修正后再保存`
    return
  }
  if (gSyntaxErrors.value.length) {
    busy.value = `全局热键第 ${gSyntaxErrors.value.map((x) => x.i + 1).join('、')} 行的写法不合法，请修正后再保存`
    return
  }
  emit('save', {
    enabled: cfg.enabled,
    timeoutMs: cfg.timeoutMs,
    echo: cfg.echo,
    bindings: bindingList.value,
  }, {
    enabled: gcfg.enabled,
    doubleTapMs: gcfg.doubleTapMs,
    bindings: globalList.value,
  })
}

async function restoreDefault() {
  // 出厂表由后端单一维护（含 M-x p → 站点栏），不在这里本地清空
  let km: api.KeymapConfig
  try {
    km = await api.DefaultKeymap()
  } catch (e) {
    busy.value = `读取出厂绑定表失败：${e}`
    return
  }
  cfg.enabled = km.enabled
  cfg.timeoutMs = km.timeoutMs
  cfg.echo = km.echo
  rows.splice(0, rows.length)
  for (const b of km.bindings || []) {
    const { id, arg } = splitCommand(b.command)
    rows.push({ enabled: b.enabled !== false, keys: b.keys, cmdId: id, cmdArg: arg, note: b.note || '' })
  }
  busy.value = `已恢复出厂绑定表（${rows.length} 条），点「保存」生效`
}

async function restoreDefaultG() {
  // 出厂全局表不补种（与 keybindings.json 的台账不同），「恢复出厂」是它唯一的复位入口
  let gk: api.GlobalKeymap
  try {
    gk = await api.DefaultGlobalKeys()
  } catch (e) {
    busy.value = `读取出厂全局热键表失败：${e}`
    return
  }
  gcfg.enabled = gk.enabled !== false
  gcfg.doubleTapMs = gk.doubleTapMs || 400
  grows.splice(0, grows.length)
  for (const b of gk.bindings || []) {
    grows.push({ enabled: b.enabled !== false, keys: b.keys, action: b.action, note: b.note || '' })
  }
  busy.value = `已恢复出厂全局热键表（${grows.length} 条），点「保存」生效`
}

onMounted(() => {
  savedEnabled = keyseq.enabled
  keyseq.enabled = false
  window.addEventListener('keydown', onAnyKeyDown, true)
  window.addEventListener('keyup', onRecGKeyUp, true)
  // 编辑器开着期间必须让系统级热键静默：否则录制时按下的 Alt+` 会把正在使用的
  // 窗口真的呼出/收起，卸载时才恢复。
  api.SetGlobalKeysPaused(true).catch(() => {})
  api.ListGlobalActions().then((v) => { gacts.value = v || [] }).catch((e) => { gactsErr.value = `读取全局动作清单失败：${e}` })
})

onBeforeUnmount(() => {
  window.removeEventListener('keydown', onAnyKeyDown, true)
  window.removeEventListener('keyup', onRecGKeyUp, true)
  keyseq.enabled = savedEnabled
  api.SetGlobalKeysPaused(false).catch(() => {})
})
</script>

<template>
  <div class="mask" @click.self="emit('close')">
    <div class="dlg">
      <div class="head">
        <span>键序列（Emacs 风格热键）</span>
        <button class="x" @click="emit('close')">&#10005;</button>
      </div>

      <div class="form">
        <fieldset>
          <legend>总控</legend>
          <div class="grid">
            <label>序列超时</label>
            <div class="row2">
              <input type="number" v-model.number="cfg.timeoutMs" min="200" max="10000" step="100" />
              <span class="unit">ms，前缀按下后等待续键的时长</span>
            </div>
          </div>
          <label class="chk"><input type="checkbox" v-model="cfg.enabled" /> 启用键序列（关闭时所有按键无条件透传远端）</label>
          <label class="chk"><input type="checkbox" v-model="cfg.echo" /> 在状态区回显已按下的序列</label>
        </fieldset>

        <fieldset>
          <legend>全局热键（系统级，本窗口没焦点也生效）</legend>
          <div class="grid">
            <label>双击判定</label>
            <div class="row2">
              <input type="number" v-model.number="gcfg.doubleTapMs" min="100" max="2000" step="50" />
              <span class="unit">ms，双击时两次按下之间的最长间隔</span>
            </div>
          </div>
          <label class="chk"><input type="checkbox" v-model="gcfg.enabled" /> 启用全局热键（走系统低级键盘钩子，与应用内键序列互不影响）</label>

          <div class="tbl">
            <div class="tr th">
              <span>用</span><span>按键序列</span><span>动作</span><span>备注</span><span></span>
            </div>
            <div class="tr" v-for="(r, i) in grows" :key="i">
              <span class="c-chk"><input type="checkbox" v-model="r.enabled" /></span>
              <span class="c-keys">
                <input v-model="r.keys" placeholder="M-`　或　&lt;C-M&gt; &lt;C-M&gt;" spellcheck="false"
                       :title="describeGlobalSeq(r.keys)"
                       :class="{ bad: !!r.keys.trim() && !!parseGlobalSeq(r.keys).error }" />
                <button class="rec" :class="{ on: recG.row === i }" @click="toggleRecG(i)">{{ recG.row === i ? '结束' : '录制' }}</button>
              </span>
              <span class="c-act">
                <select v-model="r.action">
                  <option v-for="a in gacts" :key="a.id" :value="a.id" :title="a.description">{{ a.label }}</option>
                </select>
              </span>
              <span class="c-note"><input v-model="r.note" placeholder="说明" spellcheck="false" /></span>
              <span class="c-del"><button class="mini" @click="grows.splice(i, 1)">✕</button></span>
            </div>
            <div v-if="!grows.length" class="empty">
              尚无全局热键 —— 点「＋ 新增热键」绑一个 Alt+` 呼出，或双击 Ctrl+Alt。
            </div>
          </div>

          <div class="issues" v-if="grows.length">
            <template v-for="(r, i) in grows" :key="'gis' + i">
              <p v-for="(it, k) in gRowIssue(i)" :key="k" :class="['lv-' + it.level]">
                全局第 {{ i + 1 }} 条 · {{ it.text }}
              </p>
            </template>
          </div>

          <div class="bar">
            <button @click="addGRow">＋ 新增热键</button>
            <button @click="restoreDefaultG">恢复出厂全局表</button>
            <span class="spacer" />
            <span class="unit">{{ grows.length }} 条</span>
          </div>
          <p class="hint" v-if="gactsErr">{{ gactsErr }}</p>
          <p class="hint recnote" v-if="recGHint">{{ recGHint }}</p>
          <p class="hint">
            写法：<code>M-`</code>　<code>C-M-t</code>　<code>S-&lt;F1&gt;</code> 是单段组合键，必须带修饰键（否则会把别的软件的打字吞掉）；
            <code>&lt;C-M&gt; &lt;C-M&gt;</code> 是「双击 Ctrl+Alt」—— 尖括号里只写修饰键表示按这一组键本身，写两遍表示双击。
            录制器能录组合键与修饰键双击；要录「连按两次某个组合键」请在框里把同一段写两遍。<br>
            鼠标悬停在全局序列输入框上可看这条写法到底代表什么手势。多段序列（如 <code>M-x p</code>）只能放到下面的应用内绑定。
          </p>
          <p class="hint">
            实现走 Windows 低级键盘钩子（WH_KEYBOARD_LL），因此命中时该按键不会传给当时前台的软件（双击不吞键）。
            改完点「保存」立即生效，无需重启；若被其它程序占用同一组键，钩子会照常触发本程序。
          </p>
        </fieldset>

        <fieldset>
          <legend>应用内绑定（本窗口有焦点就生效，无需焦点在终端里）</legend>
          <div class="tbl">
            <div class="tr th">
              <span>用</span><span>按键序列</span><span>命令</span><span>备注</span><span></span>
            </div>
            <div class="tr" v-for="(r, i) in rows" :key="i">
              <span class="c-chk"><input type="checkbox" v-model="r.enabled" /></span>
              <span class="c-keys">
                <input v-model="r.keys" placeholder="C-x C-f" spellcheck="false" :class="{ bad: !!r.keys.trim() && !!parseSeq(r.keys).error }" />
                <button class="rec" :class="{ on: rec.row === i }" @click="toggleRec(i)">{{ rec.row === i ? '结束' : '录制' }}</button>
              </span>
              <span class="c-cmd">
                <select v-model="r.cmdId">
                  <option v-for="c in cmds" :key="c.id" :value="c.id">{{ c.title }}</option>
                </select>
                <input v-model="r.cmdArg" placeholder="参数（可选）" spellcheck="false" />
              </span>
              <span class="c-note"><input v-model="r.note" placeholder="说明" spellcheck="false" /></span>
              <span class="c-del"><button class="mini" @click="rows.splice(i, 1)">✕</button></span>
            </div>
            <div v-if="!rows.length" class="empty">
              尚无绑定 —— 框架已就绪但不抢占任何按键。点「＋ 新增绑定」开始配置。
            </div>
          </div>

          <div class="issues" v-if="rows.length">
            <template v-for="(r, i) in rows" :key="'is' + i">
              <p v-for="(it, k) in rowIssues(i)" :key="k" :class="['lv-' + it.level]">
                第 {{ i + 1 }} 行 · {{ it.text }}
              </p>
            </template>
          </div>

          <div class="bar">
            <button @click="addRow">＋ 新增绑定</button>
            <button @click="restoreDefault">恢复出厂绑定表</button>
            <span class="spacer" />
            <span class="unit">{{ rows.length }} 条</span>
          </div>
          <p class="hint" v-if="recHint">{{ recHint }}</p>
        </fieldset>

        <fieldset>
          <legend>序列语法</legend>
          <p class="hint">
            <code>C-</code>=Ctrl <code>M-</code>=Alt <code>S-</code>=Shift，多个 token 用空格分隔成序列：<br>
            <code>C-x C-f</code>　<code>C-M-i</code>　<code>C-c &lt;ESC&gt; o</code>　<code>S-&lt;F1&gt;</code>　<code>&lt;SPC&gt; &lt;RET&gt; &lt;TAB&gt; &lt;F1&gt;…&lt;F24&gt;</code><br>
            <code>S-a</code> 与 <code>A</code> 等价（Shift 的效果已在字符里）；字母大小写不影响匹配（<code>M-x</code> ≡ <code>M-X</code>，CapsLock 开着也命中）。
            超键（Win）无法被应用捕获，故不支持。
          </p>
          <p class="hint">
            匹配采用 Emacs 语义：<b>最短完整匹配立即执行</b>，所以给 <code>C-x</code> 绑命令会让 <code>C-x C-f</code> 不可达（上方会警告）。
            只命中前缀时进入等待续键并吞下按键，超时或被取消都不会补发给远端 —— 因此请避免把 BBS 高频键（<code>&lt;ESC&gt;</code>、<code>C-c</code>）设为前缀。
          </p>
          <p class="hint">
            生效范围：本应用窗口拿到焦点就行，焦点在终端、工具栏还是站点栏都一样；只有正在
            往输入框里打字时不拦截。焦点在终端里时，没命中绑定的按键原样透传远端。
          </p>
        </fieldset>

        <p class="busy" v-if="busy">{{ busy }}</p>
      </div>

      <div class="foot">
        <button @click="emit('close')">取消</button>
        <button class="primary" @click="doSave">保存</button>
      </div>
    </div>
  </div>
</template>

<style scoped>
.mask { position: fixed; inset: 0; background: rgba(0, 0, 0, .55); display: flex; align-items: center; justify-content: center; z-index: 60; }
.dlg { width: 860px; max-width: 94vw; max-height: 88vh; overflow-y: auto; background: #24272e; border: 1px solid #3d424b; border-radius: 10px; box-shadow: 0 12px 40px rgba(0, 0, 0, .5); }
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
.hint { color: #6a7180; font-size: 12px; line-height: 1.6; margin: 6px 0 0; }
code { background: #1b1d22; border: 1px solid #33373f; border-radius: 3px; padding: 0 4px; color: #b6cf9a; font-size: 12px; }
.tbl { display: flex; flex-direction: column; gap: 6px; }
.tr { display: grid; grid-template-columns: 30px minmax(170px, 1.1fr) minmax(210px, 1.4fr) minmax(90px, 1fr) 34px; gap: 6px; align-items: center; }
.recnote { color: #7fb0e0; }
.th { color: #6a7180; font-size: 12px; }
.c-keys, .c-cmd, .c-act { display: flex; gap: 5px; align-items: center; }
.c-keys input { font-family: Consolas, monospace; }
.c-keys input.bad { border-color: #d2603f; }
.c-chk input { width: auto; }
.rec { padding: 5px 9px; font-size: 12px; white-space: nowrap; }
.rec.on { background: #2f74c0; border-color: #2f74c0; color: #fff; }
.mini { padding: 3px 8px; font-size: 12px; }
.empty { color: #6a7180; font-size: 12px; padding: 10px 0; }
.issues { margin-top: 8px; }
.issues p { margin: 2px 0; font-size: 12px; }
.lv-warn { color: #d9a441; }
.lv-error { color: #e06c5b; }
.bar { display: flex; align-items: center; gap: 8px; margin-top: 10px; }
.spacer { flex: 1; }
.busy { color: #e06c5b; font-size: 12px; margin: 10px 0 0; }
.foot { display: flex; justify-content: flex-end; gap: 8px; padding: 12px 16px; border-top: 1px solid #33373f; }
button { background: #2f333b; color: #d5d9e0; border: 1px solid #3d424b; border-radius: 5px; padding: 6px 16px; cursor: pointer; font-size: 13px; }
button.primary { background: #2f74c0; border-color: #2f74c0; color: #fff; }
button.primary:hover { background: #3a82cf; }
</style>
