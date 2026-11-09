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
  useAnsiColor: boolean
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

export interface KeyBinding {
  keys: string      // 序列表示法：“C-x C-f”、“C-c <ESC> o”
  command: string   // 命令 id（可用 "id:参数"）
  scope: string     // 'terminal' | 'global'
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

export const ListSites = (): Promise<Site[]> => Call.ByName(`${cfg}.ListSites`)
export const SaveSite = (index: number, site: Site): Promise<void> => Call.ByName(`${cfg}.SaveSite`, index, site)
export const DeleteSite = (index: number): Promise<void> => Call.ByName(`${cfg}.DeleteSite`, index)
export const DefaultSite = (): Promise<Site> => Call.ByName(`${cfg}.DefaultSite`)
export const GetPreferences = (): Promise<Preferences> => Call.ByName(`${cfg}.GetPreferences`)
export const SetPreferences = (p: Preferences): Promise<void> => Call.ByName(`${cfg}.SetPreferences`, p)
export const ListSchemes = (): Promise<ColorScheme[]> => Call.ByName(`${cfg}.ListSchemes`)
export const SaveScheme = (s: ColorScheme): Promise<void> => Call.ByName(`${cfg}.SaveScheme`, s)
export const DeleteScheme = (name: string): Promise<void> => Call.ByName(`${cfg}.DeleteScheme`, name)
export const ListSystemFonts = (): Promise<string[]> => Call.ByName(`${cfg}.ListSystemFonts`)
export const GetKeymap = (): Promise<KeymapConfig> => Call.ByName(`${cfg}.GetKeymap`)
export const DefaultKeymap = (): Promise<KeymapConfig> => Call.ByName(`${cfg}.DefaultKeymap`)
export const SaveKeymap = (km: KeymapConfig): Promise<void> => Call.ByName(`${cfg}.SaveKeymap`, km)

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

// ---- Encoding labels (mirror fqterm) ----

export const ENCODINGS = ['GBK', 'BIG5', 'UTF-8', 'HKSCS', 'UAO']
export const PROTOCOLS = ['Telnet', 'SSH1', 'SSH2', 'Local']
export const TERM_TYPES = ['vtnt', 'vt100', 'xterm', 'xterm-256color', 'ansi']
// 字体自适应三态，下标即 site.fontAutoFit（对齐原版 isFontAutoFit_）
export const FONT_ADJUST_MODES = ['行列随窗口（字号固定）', '字号随窗口（拉满）', '固定行列']
