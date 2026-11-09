/**
 * 键序列框架自带的内置命令 —— 用来证明「注册表 + 绑定」这条链路完整可用。
 *
 * 业务命令（上传、切标签、搜索、发帖…）不需要改引擎：各自模块在初始化时调用
 * keyseq.registerCommand({ id, title, handler }) 注册即可出现在绑定表里。
 *
 * 注意：本模块只注册命令，不预置任何按键绑定。默认绑定表为空，意味着框架在场
 * 但不抢占任何键，所有按键照常透传给远端 BBS。
 */
import * as api from './api'
import { keyseq, registerCommand } from './keyseq'

export interface KeymapDeps {
  /** 打开绑定表编辑器 */
  openEditor: () => void
  /** 状态区回显（对应 Emacs 的 echo area） */
  echo: (msg: string, kind?: 'info' | 'warn') => void
  /** 当前活动会话 id，无会话时返回空串 */
  activeSessionId: () => string
  /** 内存里的当前配置（用于原地翻转开关） */
  getConfig: () => api.KeymapConfig
  /** 配置变更后同步给引擎 */
  applyConfig: (km: api.KeymapConfig) => void
}

export function registerBuiltinCommands(deps: KeymapDeps) {
  registerCommand({
    id: 'keymap.describe',
    title: '热键：打开绑定表编辑器',
    description: '列出全部序列与命令，可现场修改',
    handler: () => deps.openEditor(),
  })

  registerCommand({
    id: 'keymap.describe-key',
    title: '热键：查询下一个按键绑定了什么',
    description: '同 Emacs 的 describe-key：只查询不执行',
    handler: () => keyseq.startDescribe(),
  })

  registerCommand({
    id: 'keymap.cancel',
    title: '热键：取消正在等待续键的序列',
    handler: () => keyseq.cancel('序列已取消'),
  })

  registerCommand({
    id: 'keymap.reload',
    title: '热键：从配置文件重新载入绑定表',
    description: '手改 keybindings.json 后无需重启',
    handler: async () => {
      const km = await api.GetKeymap()
      deps.applyConfig(km)
      deps.echo(`绑定表已重新载入（${km.bindings.length} 条）`)
    },
  })

  registerCommand({
    id: 'keymap.toggle-enabled',
    title: '热键：启用 / 停用整个键序列功能',
    description: '停用后所有按键立即恢复透传远端',
    handler: async () => {
      const km = deps.getConfig()
      km.enabled = !km.enabled
      await api.SaveKeymap(km)
      deps.applyConfig(km)
      deps.echo(km.enabled ? '键序列已启用' : '键序列已停用（所有按键恢复透传远端）', 'warn')
    },
  })

  registerCommand({
    id: 'term.send',
    title: '终端：发送固定文本（命令写成 term.send:内容）',
    description: '参数里的 \\n 转换回车、\\t 转换制表符',
    handler: (arg) => {
      const id = deps.activeSessionId()
      if (!id) { deps.echo('没有活动会话，无法发送', 'warn'); return }
      if (!arg) { deps.echo('term.send 缺少文本参数', 'warn'); return }
      void api.Write(id, arg.replace(/\\n/g, '\r').replace(/\\t/g, '\t'))
    },
  })

  registerCommand({
    id: 'demo.echo',
    title: '演示：把参数显示在状态区',
    description: '验证命令链路，不含业务含义',
    handler: (arg) => deps.echo(arg || 'demo.echo 已触发（无参数）'),
  })
}
