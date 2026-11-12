// Bridge to the Go backend via the Wails v3 runtime (dynamic calls by name,
// avoiding a dependency on generated bindings during hand-written dev).
import { Call, Events } from '@wailsio/runtime'

// ---- Data models mirroring the Go structs ----

export interface Site {
  name: string
  host: string
  port: number
  hostType: number // 0=BBS 1=*NIX
  protocolType: number // 0=Telnet 1=SSH1 2=SSH2 3=Local
  autoLogin: boolean
  userName: string
  password: string
  // SSH 公钥认证：keyFile 非空即走私钥（passphrase 是私钥口令，可空），后端只看这个字段。
  keyFile: string
  passphrase: string
  preLogin: string
  postLogin: string
  serverEncodingID: number // 0=GBK 1=BIG5 2=UTF8 3=HKSCS 4=UAO
  colorScheme: string
  fontName: string
  fontSize: number
  fontNameCjk: string    // 中文/宽字符字体（CSS 字体回退链实现中英分字体）
  charSpacing: number    // 英文字符额外字距 px
  lineSpacing: number    // 额外行距 px
  antiAlias: boolean     // 抗锯齿
  fontAutoFit: number    // 0=随窗口改行列 1=随窗口改字号 2=固定行列
  useAnsiColor: boolean // 关掉则全画面退成黑白反相（原版 isAnsiColor_）
  alwaysHighlight: boolean // 所有前景色取亮色半区（原版 isAlwaysHighlight_）
  termType: string
  keyType: number
  numColumns: number
  numRows: number
  cursorType: number
  backspace: number
  maxIdleSeconds: number
  antiIdle: boolean
  antiIdleMessage: string
  autoReconnect: boolean
  reconnectInterval: number
  reconnectRetries: number // 重连重试次数，-1 = 无限
  enableZmodem: boolean
  group: string
  // 网络代理（D3）站点级覆盖：0=跟随全局设置（旧站点缺键即此） 1=本站点强制直连
  // 2=SOCKS4 3=SOCKS5 4=HTTP。选了类型但地址为空等于不用代理。
  proxyType: number
  proxyHost: string
  proxyPort: number
  proxyUser: string
  proxyPass: string
  // 本机 shell（protocolType===3）专用：shellID 是探测结果的稳定标识，shellArgs 是启动
  // 参数，auto 表示这条由扫描生成（允许被自动清理/更新）。
  shellID?: string
  shellArgs?: string[]
  auto?: boolean
}

export interface Preferences {
  defaultEncodingID: number
  defaultTermType: string
  defaultColumns: number
  defaultRows: number
  defaultScheme: string
  fontName: string
  fontSize: number
  zmodemDir: string
  imageViewerEnabled: boolean
  openWarnOnClose: boolean
  scrollbackLines: number
  confirmClose: boolean
  menuClickEnabled: boolean
  // 发帖分隔符（编辑工具栏）：上一次选用的那个字符，空串表示没选过
  postSepChar: string
  // 窗口形态：quake 下拉与普通窗口共用同一个原生窗口，尺寸按工作区（已扣除任务栏）百分比算
  quakeWidthPct: number
  quakeHeightPct: number
  quakeHideOnBlur: boolean
  trayEnabled: boolean
  closeToTray: boolean
  // IP 归属地（B8）：总开关 + 库位置 + 自定义下载地址，后两个留空即用默认
  ipLocationEnabled: boolean
  ipLocationDb: string
  ipLocationUrl: string
  // 网络代理（D3）：0=不用代理 2=SOCKS4 3=SOCKS5 4=HTTP；端口留空按类型补默认值。
  // 它同时管住终端连接、IP 库下载与链接预览三条出站路。
  proxyType: number
  proxyHost: string
  proxyPort: number
  proxyUser: string
  proxyPass: string
  // 启动器搜索范围：开始菜单与桌面永远在搜，launcherDirs 只填额外目录；
  // launcherExts 出厂 exe/lnk/bat/ps1/com，整列清空后端会退回出厂值。
  launcherDirs: string[]
  launcherExts: string[]
}

// 一份代理设置（与后端 ProxyConfig 同形）。测试连通时传的是界面上还没存盘的那一份。
export interface ProxyConfig {
  type: number
  host: string
  port: number
  user: string
  pass: string
}

export interface ColorScheme {
  name: string
  background: string
  foreground: string
  cursor: string
  palette: string[]
}

export interface SessionInfo {
  id: string
  name: string
  title: string
  status: string
  host: string
}

// ---- Keymap (Emacs 风格键序列) ----

// 整张表都是应用内绑定（本窗口有焦点即生效）。早先的 scope 字段已退役：旧配置里
// 残留的 "scope" 会被后端解码时忽略，下次保存就不再写回。
export interface KeyBinding {
  keys: string      // 序列表示法：“C-x C-f”、“C-c <ESC> o”
  command: string   // 命令 id（可用 "id:参数"）
  enabled: boolean
  note: string
}

export interface KeymapConfig {
  enabled: boolean
  timeoutMs: number
  echo: boolean
  bindings: KeyBinding[]
}

// ---- Config service ----

const cfg = 'main.ConfigService'
const term = 'main.TerminalService'
const appsvc = 'main.AppService'
const pvsvc = 'main.PreviewService'
const lan = 'main.LauncherService'
const ipsvc = 'main.IPService'
const fil = 'main.FileService'

export const ListSites = (): Promise<Site[]> => Call.ByName(`${cfg}.ListSites`)
export const SaveSite = (index: number, site: Site): Promise<void> => Call.ByName(`${cfg}.SaveSite`, index, site)
export const DeleteSite = (index: number): Promise<void> => Call.ByName(`${cfg}.DeleteSite`, index)
export const DefaultSite = (): Promise<Site> => Call.ByName(`${cfg}.DefaultSite`)
export const GetPreferences = (): Promise<Preferences> => Call.ByName(`${cfg}.GetPreferences`)
export const SetPreferences = (p: Preferences): Promise<void> => Call.ByName(`${cfg}.SetPreferences`, p)
// 文件管理器会话（fmstate.json）：标签数/每栏路径/活动栏，启动还原。
export interface FMTabState { panes: string[]; active: number }
export interface FMState { tabs: FMTabState[]; active: number }
export const GetFMState = (): Promise<FMState> => Call.ByName(`${cfg}.GetFMState`)
export const SetFMState = (st: FMState): Promise<void> => Call.ByName(`${cfg}.SetFMState`, st)
// TestProxy 经该代理建一条到 GitHub API 的隧道（只握手不下载），返一句成功文案，
// 失败时 reject 的原因里写清了卡在代理本身还是卡在建隧。
export const TestProxy = (c: ProxyConfig): Promise<string> => Call.ByName(`${cfg}.TestProxy`, c)
export const ListSchemes = (): Promise<ColorScheme[]> => Call.ByName(`${cfg}.ListSchemes`)
export const SaveScheme = (s: ColorScheme): Promise<void> => Call.ByName(`${cfg}.SaveScheme`, s)
export const DeleteScheme = (name: string): Promise<void> => Call.ByName(`${cfg}.DeleteScheme`, name)
export const ListSystemFonts = (): Promise<string[]> => Call.ByName(`${cfg}.ListSystemFonts`)
export const GetKeymap = (): Promise<KeymapConfig> => Call.ByName(`${cfg}.GetKeymap`)
export const DefaultKeymap = (): Promise<KeymapConfig> => Call.ByName(`${cfg}.DefaultKeymap`)
export const SaveKeymap = (km: KeymapConfig): Promise<void> => Call.ByName(`${cfg}.SaveKeymap`, km)

// ---- 本机 shell 扫描 ----

// 后端探测 cmd / PowerShell / pwsh / Git Bash / MSYS2 / Cygwin / WSL 各发行版，把它们落成
// protocolType===3、group==='shell' 的真实站点条目。计数为 0 表示列表没有任何变化。
export interface ShellSyncResult {
  added: number
  updated: number
  removed: number
  shells: string[]
}

export const SHELL_GROUP = 'shell'
export const SyncShells = (): Promise<ShellSyncResult> => Call.ByName(`${cfg}.SyncShells`)

// ---- 窗口形态 / 托盘 / 开机自启动 ----

export interface WindowModeState {
  mode: string    // 'window' | 'quake'
  visible: boolean
}

export const GetWindowMode = (): Promise<WindowModeState> => Call.ByName(`${appsvc}.GetWindowMode`)
export const ToggleWindowMode = (): Promise<WindowModeState> => Call.ByName(`${appsvc}.ToggleWindowMode`)
export const SetWindowMode = (mode: string): Promise<void> => Call.ByName(`${appsvc}.SetMode`, mode)
export const GetAutostart = (): Promise<boolean> => Call.ByName(`${appsvc}.GetAutostart`)
export const SetAutostart = (on: boolean): Promise<void> => Call.ByName(`${appsvc}.SetAutostart`, on)

// ---- 跨应用热键（系统级） ----

// 与应用内键序列是两套东西：由后端 WH_KEYBOARD_LL 低级键盘钩子匹配，本窗口没焦点也
// 生效。形状只有「单段组合键」与「两段相同键的双击」，语法校验见 globalkey.ts。
export interface GlobalKey {
  keys: string      // "M-`"、"C-M-t"、"<C-M> <C-M>"
  action: string    // 动作 id，取自 ListGlobalActions
  enabled: boolean
  note: string
}

export interface GlobalKeymap {
  enabled: boolean
  doubleTapMs: number
  bindings: GlobalKey[]
}

export interface GlobalAction {
  id: string
  label: string
  description: string
}

export const GetGlobalKeys = (): Promise<GlobalKeymap> => Call.ByName(`${appsvc}.GetGlobalKeys`)
export const DefaultGlobalKeys = (): Promise<GlobalKeymap> => Call.ByName(`${appsvc}.DefaultGlobalKeys`)
export const SaveGlobalKeys = (gk: GlobalKeymap): Promise<void> => Call.ByName(`${appsvc}.SaveGlobalKeys`, gk)
export const ListGlobalActions = (): Promise<GlobalAction[]> => Call.ByName(`${appsvc}.ListGlobalActions`)
// 热键编辑器打开期间静音真热键，否则录制时按下的组合键会真的把窗口呼出/收起
export const SetGlobalKeysPaused = (on: boolean): Promise<void> => Call.ByName(`${appsvc}.SetGlobalKeysPaused`, on)
// 原生对话框会抢走主窗口焦点，quake 的「失焦即收起」需要这道护栏才不会误伤
export const SetDialogGuard = (on: boolean): Promise<void> => Call.ByName(`${appsvc}.SetDialogGuard`, on)
// SSH 私钥选择：弹原生选文件框，起始目录落在 ~/.ssh；取消返空串（不是错误）
export const PickKeyFile = (): Promise<string> => Call.ByName(`${appsvc}.PickKeyFile`)
// 偏好保存后重放与原生侧相关的设置（目前是托盘显隐）
export const ApplyAppPreferences = (): Promise<void> => Call.ByName(`${appsvc}.ApplyPreferences`)

// ---- 超链接预览窗口 ----

// 一条链接。kind 由前端按扩展名判完再送进来（见 linkscan.ts），后端只用它分流：
// 媒体→我们自己的预览页，网页→整窗顶层导航（frame 限制就是这么绕开的）。
export interface PreviewItem {
  url: string
  kind: 'image' | 'audio' | 'video' | 'page'
  name: string
}

export interface PreviewRequest {
  items: PreviewItem[]
  index: number
}

export const OpenPreview = (items: PreviewItem[], index: number): Promise<void> =>
  Call.ByName(`${pvsvc}.OpenPreview`, items, index)
export const TakePreviewRequest = (): Promise<PreviewRequest | null> =>
  Call.ByName(`${pvsvc}.TakePreviewRequest`)
export const ClosePreview = (): Promise<void> => Call.ByName(`${pvsvc}.ClosePreview`)
export const OpenExternalURL = (url: string): Promise<void> => Call.ByName(`${pvsvc}.OpenExternal`, url)
export const GetPreviewState = (): Promise<string> => Call.ByName(`${pvsvc}.GetPreviewState`)
// 探类型与真文件名：发一次 HEAD 拿 Content-Type 与 Content-Disposition。扩展名认不出来的
// 链接（BBS 的 att.php、纯附件号）靠它才不会被全判成网页，也不会整排图都叫 att.php。
// Resolve 成与入参一一对应的回执，探不到就是空字段。
export interface ProbeResult {
  kind?: string | null
  name?: string | null
}
export const ProbePreview = (url: string): Promise<ProbeResult> =>
  Call.ByName(`${pvsvc}.Probe`, url)
export const ProbePreviewMany = (urls: string[]): Promise<ProbeResult[]> =>
  Call.ByName(`${pvsvc}.ProbeMany`, urls)
// 另存为：必须走后端 —— 跨域图片的响应体 webview fetch 不到，download 属性也会被忽略。
// Resolve 成选定的完整路径，取消则是空字符串。
export const SavePreviewItem = (url: string, name: string): Promise<string> =>
  Call.ByName(`${pvsvc}.SaveItem`, url, name)
// 最大化/还原预览窗口，Resolve 成切换后的状态（以后端 IsMaximised 为准）。
export const TogglePreviewMaximise = (): Promise<boolean> =>
  Call.ByName(`${pvsvc}.ToggleMaximise`)

// ---- IP 归属地（B8） ----

// 一次查询的回执。detail 是后端已经拼好的一行字，界面直接显示即可，
// 不需要自己拼「国家 城市」，也不需要知道错误是怎么分级的。
export interface IPLocationResult {
  query: string
  ip: string
  country: string
  city: string
  detail: string
  found: boolean
  error: string
}

// 更新过程的事件载荷。state: downloading | done | error；message 就是该显示的那
// 一行字（下载时带百分比，结束时是结果说明），前端不需要自己拼进度。
export interface IPUpdateProgress {
  state: string
  message: string
  error: string
}

export const IPLocation = (query: string): Promise<IPLocationResult> =>
  Call.ByName(`${ipsvc}.Lookup`, query)
// 前端诊断行写进后端日志（Alt+单击那一格看到了什么）。界面上什么都看不到的时候，
// 这是唯一能分清「没识别 / 没坐标 / 没显示」的取证通道。
export const IPNote = (msg: string): Promise<void> => Call.ByName(`${ipsvc}.Note`, msg)
export const StartIPUpdate = (): Promise<number> => Call.ByName(`${ipsvc}.StartUpdate`)
export const IPStatus = (): Promise<string> => Call.ByName(`${ipsvc}.Status`)
// 改完库路径/地址后叫一次：后端会重新找库、重新读盘。
export const IPReload = (): Promise<void> => Call.ByName(`${ipsvc}.Reload`)

// ---- 启动器（?view=launcher 隐藏常驻窗）----

// 一条命中。path 是 .lnk 全路径（invoke 用它），target 是解析出的 exe 路径，
// 可能为空（只用于展示与匹配）。
export interface LauncherResult {
  name: string
  path: string
  target: string
  score: number
}

export interface LauncherInfo {
  ready: boolean
  count: number
  indexing: boolean
}

export const LauncherSearch = (query: string): Promise<LauncherResult[]> =>
  Call.ByName(`${lan}.LauncherSearch`, query)
// elevate=true 走 runas 动词（Ctrl+Enter 以管理员身份启动，UAC 由系统弹）。
export const LauncherInvoke = (path: string, elevate = false): Promise<void> => Call.ByName(`${lan}.LauncherInvoke`, path, elevate)
export const LauncherHide = (): Promise<void> => Call.ByName(`${lan}.LauncherHide`)
export const LauncherStatus = (): Promise<LauncherInfo> => Call.ByName(`${lan}.LauncherInfo`)
export const LauncherRebuild = (): Promise<void> => Call.ByName(`${lan}.LauncherRebuild`)
// LauncherReveal：把当前命中的路径交给主窗口的文件管理器标签（Shift+Enter 定位、
// Alt+Enter 额外预览）。path 必须是索引项里的（后端白名单校验）。preview=true 走右栏预览。
export const LauncherReveal = (path: string, preview: boolean): Promise<void> =>
  Call.ByName(`${lan}.LauncherReveal`, path, preview)
// 设置页「添加目录」：弹原生选目录框，取消返空串（不是错误）
export const PickLauncherDir = (): Promise<string> => Call.ByName(`${lan}.PickLauncherDir`)
// 窗口已被预建常驻，每次呼出后端推一条：页面拿它清空上次输入并重新聚焦。
export const onLauncherShow = (cb: () => void) =>
  Events.On('launcher:show', () => cb())

// LauncherReveal 后端把主窗提到前台后推给主窗口的定位事件。事件走全局总线（每个
// 窗口都会收到），但只有主窗口注册了监听者（App.vue），启动器窗没人听这一条。
export interface FileManagerReveal {
  path: string
  preview: boolean
}
export const onFileManagerReveal = (cb: (d: FileManagerReveal) => void) =>
  Events.On('fm:reveal', (e: any) => cb(e.data))

// 目录外部改动（explorer 复制/删除等）后端 watch防抖后推；文件管理器据此自动刷新对应栏。
export const onFileManagerDirChanged = (cb: (path: string) => void) =>
  Events.On('fm:dir-changed', (e: any) => cb(String(e.data)))

// ---- 文件管理器（launcher 分支：主窗口 M-x e 开的 Total Commander 式标签）----

// 一条目录项。后端 ListDir 逐条直出，不排序、不跳过隐藏（hidden 供前端过滤）。
// path 是全路径，一切后续操作的唯一句柄；mtime 是 UnixNano。
export interface FileEntry {
  name: string
  path: string
  size: number
  mtime: number
  isDir: boolean
  hidden: boolean
}

export interface DriveInfo {
  root: string
  label: string
  type: number // 0 未知 / 2 可移动 / 3 本地 / 4 网络 / 5 光驱
  ready: boolean
}

export interface PluginTool {
  id: string
  title: string
  kind: string
  needsSelection: boolean
  extensions?: string[]
  actions?: PluginAction[]
}

// PluginAction：插件声明的一条有副作用动作（解压/压缩等），在「扩展」菜单里点。
export interface PluginAction {
  id: string
  title: string
}

// PreviewRef：后端 FileService.Preview 回传——已签名的可渲染 URL + 种类。
export interface PreviewRef {
  url: string
  kind: string // image / text / html / audio / video / pdf / other
  mimeType: string
  needPlugin: boolean
  size: number
}

// PluginArtifact：插件把任意文件转成的产物（path 经 Go 签成 url）。
export interface PluginArtifact {
  kind: string
  path: string
  url: string
  html: string
  text: string
  title: string
}

export interface PluginContext {
  activeDir: string
  selected: string[]
  targetDir: string
}

export interface PluginResult {
  message: string
}

// ShellMenuRequest：原生右键菜单一次弹出的入参（目标路径集 + 随行 ctx）。
export interface ShellMenuRequest {
  paths: string[]
  ctx: PluginContext
}

// ShellMenuResult：右键菜单一次选择的结果。isPlugin 时前端据 pluginId/actionId 复用 runAction。
export interface ShellMenuResult {
  handled: boolean
  pluginId: string
  actionId: string
  isPlugin: boolean
  error: string
}

// path 传空串后端会回主目录（ListDir/FileHome 都兜底）。
export const FileHome = (): Promise<string> => Call.ByName(`${fil}.HomeDir`)
export const FileDrives = (): Promise<DriveInfo[]> => Call.ByName(`${fil}.ListDrives`)
export const FileListDir = (path: string): Promise<FileEntry[]> => Call.ByName(`${fil}.ListDir`, path)
export const FileWatchDir = (path: string): Promise<void> => Call.ByName(`${fil}.WatchDir`, path)
export const FileUnwatchDir = (path: string): Promise<void> => Call.ByName(`${fil}.UnwatchDir`, path)
export const FileStat = (path: string): Promise<FileEntry> => Call.ByName(`${fil}.FileStat`, path)
export const FileMkdir = (path: string): Promise<void> => Call.ByName(`${fil}.MakeDir`, path)
export const FileRename = (oldPath: string, newPath: string): Promise<void> =>
  Call.ByName(`${fil}.Rename`, oldPath, newPath)
export const FileCopy = (src: string[], dstDir: string): Promise<void> => Call.ByName(`${fil}.Copy`, src, dstDir)
export const FileMove = (src: string[], dstDir: string): Promise<void> => Call.ByName(`${fil}.Move`, src, dstDir)
export const FileDragPaths = (paths: string[]): Promise<void> => Call.ByName(`${fil}.DragPaths`, paths)
export const FileDelete = (paths: string[]): Promise<void> => Call.ByName(`${fil}.Delete`, paths)
// 删除到回收站（Del）；Shift+Del / F8 走上面的 FileDelete 永久删除。
export const FileRecycle = (paths: string[]): Promise<void> => Call.ByName(`${fil}.Recycle`, paths)
// 用系统默认方式打开一条路径（Enter）。
export const FileOpenPath = (path: string): Promise<void> => Call.ByName(`${fil}.OpenPath`, path)
// 读文件开头一段做文本预览（F3），默认 64KB。
export const FilePeek = (path: string, maxBytes = 65536): Promise<string> => Call.ByName(`${fil}.PeekFile`, path, maxBytes)
// Python 扩展预览：无 Python 时 Plugins 为空、PythonAvailable 为假；Ctrl+Q 走 FilePreview，
// kind=other 再交 FilePluginPreview。
export const FilePlugins = (): Promise<PluginTool[]> => Call.ByName(`${fil}.Plugins`)
export const FilePythonAvailable = (): Promise<boolean> => Call.ByName(`${fil}.PythonAvailable`)
// 插件目录绝对路径（程序目录\plugins）。
export const FilePluginsDir = (): Promise<string> => Call.ByName(`${fil}.PluginsDir`)
// 触发插件的一条动作（解压/压缩）。path 为当前光标项（可空），ctx 携选中集与对侧目录。
export const FileRunPluginAction = (pluginID: string, action: string, path: string, ctx: PluginContext): Promise<PluginResult> =>
  Call.ByName(`${fil}.RunPluginAction`, pluginID, action, path, ctx)
// 在文件行上弹系统 Explorer 原生右键菜单（顶部拼 FTRW 插件动作）。菜单坐标后端自取鼠标位置。
export const FileShellContextMenu = (req: ShellMenuRequest): Promise<ShellMenuResult> =>
  Call.ByName(`${fil}.ShellContextMenu`, req)
// 拿一条本地文件的渲染元信息（签名 URL + 种类）。Ctrl+Q 预览第一步。
export const FilePreview = (path: string): Promise<PreviewRef> => Call.ByName(`${fil}.Preview`, path)
// 未知类型交给认领该扩展名的插件转产物。
export const FilePluginPreview = (pluginID: string, path: string, ctx: PluginContext): Promise<PluginArtifact> =>
  Call.ByName(`${fil}.PluginPreview`, pluginID, path, ctx)

// ---- Terminal service ----

export const Connect = (site: Site): Promise<string> => Call.ByName(`${term}.Connect`, site)
export const Write = (id: string, data: string): Promise<void> => Call.ByName(`${term}.Write`, id, data)
export const Resize = (id: string, cols: number, rows: number): Promise<void> => Call.ByName(`${term}.Resize`, id, cols, rows)
export const Disconnect = (id: string): Promise<void> => Call.ByName(`${term}.Disconnect`, id)
export const SendFile = (id: string, paths: string[]): Promise<void> => Call.ByName(`${term}.SendFile`, id, paths)
export const CancelTransfer = (id: string): Promise<void> => Call.ByName(`${term}.CancelTransfer`, id)
export const ListSessions = (): Promise<SessionInfo[]> => Call.ByName(`${term}.ListSessions`)

// ---- Events ----

export interface TermData { id: string; data: string }
export interface TermStatus { id: string; status: string; message: string; title: string }
export interface ZmodemProgress {
  id: string; direction: string; filename: string
  size: number; done: number; state: string; message: string
}

export const onTermData = (cb: (d: TermData) => void) =>
  Events.On('term:data', (e: any) => cb(e.data))
export const onTermStatus = (cb: (d: TermStatus) => void) =>
  Events.On('term:status', (e: any) => cb(e.data))
export const onZmodem = (cb: (d: ZmodemProgress) => void) =>
  Events.On('zmodem:progress', (e: any) => cb(e.data))
export const onWindowMode = (cb: (d: WindowModeState) => void) =>
  Events.On('app:mode', (e: any) => cb(e.data))
// 预览窗口已开着时换内容。事件走全局总线（每个窗口都会收到），但只有预览页注册了
// 监听者，主窗口那边没人听这一条，所以不必再按 sender 过滤。
export const onPreviewLoad = (cb: (d: PreviewRequest) => void) =>
  Events.On('preview:load', (e: any) => cb(e.data))
// IP 库自动更新（启动后一天一轮）与手动「立即更新」都往这里报。
export const onIPUpdate = (cb: (d: IPUpdateProgress) => void) =>
  Events.On('ip:update', (e: any) => cb(e.data))

// ---- Encoding labels (mirror fqterm) ----

export const ENCODINGS = ['GBK', 'BIG5', 'UTF-8', 'HKSCS', 'UAO']
export const PROTOCOLS = ['Telnet', 'SSH1', 'SSH2', 'Local']
export const TERM_TYPES = ['vtnt', 'vt100', 'xterm', 'xterm-256color', 'ansi']
// 字体自适应三态，下标即 site.fontAutoFit（对齐原版 isFontAutoFit_）
export const FONT_ADJUST_MODES = ['行列随窗口（字号固定）', '字号随窗口（拉满）', '固定行列']
