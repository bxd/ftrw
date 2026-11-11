import { createApp, type App } from 'vue'
import FileManagerView from './components/FileManagerView.vue'

// FmTab 是主窗口里的「文件管理器」标签实例（launcher 分支，M-x e 呼出）。
//
// 它刻意鸭子类型地对齐 TermTab 里被 App.vue 通用标签逻辑摸到的那一小撮成员
// （container / fitNow / focus / dispose / id / status / title）：App.vue 沿用
// `insts: TermTab[]`，创建处把 FmTab 收窄 cast 进去，避免为一种新标签把整套终端
// 代码路径翻一遍。id 固定空串、status 固定 'ready'，让「有会话才断线/确认」那些分支
// （if(t.id)、t.status==='connected'、发帖 target 的 t&&t.id）自然跳过文件标签。
//
// 与启动器 / 预览窗一致走「独立 Vue 根」挂载：双栏状态全住在 FileManagerView 里，
// 不外溢到 App.vue，卸载只是 unmount。
export class FmTab {
  readonly kind = 'files' as const
  container: HTMLElement
  title = '文件管理器'
  status = 'ready'
  id = '' // 没有终端会话：空串让通用 closeTab / 发帖 target 一路自然跳过

  private app: App

  constructor(container: HTMLElement, initialPath?: string) {
    this.container = container
    this.app = createApp(FileManagerView, { initialPath: initialPath || '' })
    this.app.mount(container)
  }

  // setActive 抢焦点用：把焦点交给组件根 .fm，键盘分发才落在它身上。
  focus() {
    (this.container.querySelector('.fm') as HTMLElement | null)?.focus()
  }

  // 终端标签按窗口尺寸重排字体；文件标签是流式布局，跟着容器自适应，无需动作。
  fitNow() { /* 无操作 */ }

  dispose() {
    this.app.unmount()
  }
}
