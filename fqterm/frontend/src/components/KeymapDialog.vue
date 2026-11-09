<script setup lang="ts">
import { reactive, ref, computed, onMounted, onBeforeUnmount } from 'vue'
import * as api from '../api'
import { analyzeBindings, eventToken, keyseq, listCommands, parseSeq } from '../keyseq'

const props = defineProps<{ km: api.KeymapConfig }>()
const emit = defineEmits<{
  (e: 'save', km: api.KeymapConfig): void
  (e: 'close'): void
}>()

/** 编辑态用拆开的 cmdId/cmdArg，保存时再拼回 "id:arg"，避免用户手写冒号。 */
interface Row {
  enabled: boolean
  keys: string
  cmdId: string
  cmdArg: string
  scope: string
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
    return { enabled: b.enabled !== false, keys: b.keys, cmdId: id, cmdArg: arg, scope: b.scope || 'terminal', note: b.note || '' }
  }),
)

function addRow() {
  rows.push({ enabled: true, keys: '', cmdId: cmds[0]?.id || 'demo.echo', cmdArg: '', scope: 'terminal', note: '' })
}

/* ---------------- 按键录制 ---------------- */

const rec = reactive({ row: -1, buf: [] as string[] })
const recHint = ref('')
// 编辑期间需要让引擎静默，否则录制时按下的 C-x 会被真的当作热键执行。
let savedEnabled = true

function toggleRec(i: number) {
  if (rec.row === i) { stopRec(); return }
  const cur = rows[i]?.keys || ''
  const { tokens, error } = parseSeq(cur)
  rec.buf = error ? [] : [...tokens]
  rec.row = i
  recHint.value = '按下按键组成序列，回车结束'
}

function stopRec() {
  rec.row = -1
  rec.buf = []
  recHint.value = ''
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
    scope: r.scope,
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

const busy = ref('')

async function doSave() {
  if (syntaxErrors.value.length) {
    busy.value = `第 ${syntaxErrors.value.map((x) => x.i + 1).join('、')} 行的序列不合法，请修正后再保存`
    return
  }
  emit('save', {
    enabled: cfg.enabled,
    timeoutMs: cfg.timeoutMs,
    echo: cfg.echo,
    bindings: bindingList.value,
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
    rows.push({ enabled: b.enabled !== false, keys: b.keys, cmdId: id, cmdArg: arg, scope: b.scope || 'terminal', note: b.note || '' })
  }
  busy.value = `已恢复出厂绑定表（${rows.length} 条），点「保存」生效`
}

onMounted(() => {
  savedEnabled = keyseq.enabled
  keyseq.enabled = false
  window.addEventListener('keydown', onRecKey, true)
})

onBeforeUnmount(() => {
  window.removeEventListener('keydown', onRecKey, true)
  keyseq.enabled = savedEnabled
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
          <legend>绑定表</legend>
          <div class="tbl">
            <div class="tr th">
              <span>用</span><span>按键序列</span><span>命令</span><span>作用域</span><span>备注</span><span></span>
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
              <span class="c-scope">
                <select v-model="r.scope">
                  <option value="terminal">终端</option>
                  <option value="global">全局</option>
                </select>
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
            作用域：<b>终端</b> = 仅终端有焦点时拦截（由 xterm 的按键回调实现）；<b>全局</b> = 无论焦点在哪都在 window 捕获阶段拦截。
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
.tr { display: grid; grid-template-columns: 30px minmax(170px, 1.1fr) minmax(210px, 1.4fr) 78px minmax(90px, 1fr) 34px; gap: 6px; align-items: center; }
.th { color: #6a7180; font-size: 12px; }
.c-keys, .c-cmd { display: flex; gap: 5px; align-items: center; }
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
