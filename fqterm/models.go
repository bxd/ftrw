package main

// Site mirrors FQTerm's FQTermParam connection profile (see fqterm src/common/fqterm_param.h).
// It describes a saved connection in the site manager.
type Site struct {
	// General
	Name         string `json:"name"`         // display name
	Host         string `json:"host"`         // host address
	Port         int    `json:"port"`         // port
	HostType     int    `json:"hostType"`     // 0=BBS 1=*NIX
	ProtocolType int    `json:"protocolType"` // 0=Telnet 1=SSH1 2=SSH2 3=Local
	AutoLogin    bool   `json:"autoLogin"`
	UserName     string `json:"userName"`
	Password     string `json:"password"`
	// SSH 公钥认证：KeyFile 非空即走私钥（见 sshAuthFor），Passphrase 是私钥自己的口令，
	// 可空。原版靠 ssh-agent 转发公钥，没有这两个字段；这里填了私钥就不再尝试密码认证。
	KeyFile    string `json:"keyFile"`
	Passphrase string `json:"passphrase"`
	PreLogin   string `json:"preLogin"`
	PostLogin  string `json:"postLogin"`

	// Display / encoding
	ServerEncodingID int    `json:"serverEncodingID"` // 0=GBK 1=BIG5 2=UTF8 3=HKSCS 4=UAO
	ColorScheme      string `json:"colorScheme"`      // scheme name
	FontName         string `json:"fontName"`         // 英文字体（原版 englishFontName_）
	FontSize         int    `json:"fontSize"`
	FontNameCJK      string `json:"fontNameCjk"`     // 中文/宽字符字体（原版 otherFontName_）
	CharSpacing      int    `json:"charSpacing"`     // 英文字符额外字距 px（原版 charSpacing_）
	LineSpacing      int    `json:"lineSpacing"`     // 额外行距 px（原版 lineSpacing_）
	AntiAlias        bool   `json:"antiAlias"`       // 抗锯齿（原版 global/antialias）
	FontAutoFit      int    `json:"fontAutoFit"`     // 0=随窗口改行列 1=随窗口改字号（原版默认）2=固定行列
	UseANSIColor     bool   `json:"useAnsiColor"`    // 原版 isAnsiColor_：关掉则全画面退成黑白反相
	AlwaysHighlight  bool   `json:"alwaysHighlight"` // 原版 isAlwaysHighlight_：所有前景色取调色板亮色半区

	// Terminal
	TermType   string `json:"termType"` // vtnt / xterm / vt100
	KeyType    int    `json:"keyType"`  // 0=BBS 1=XTERM 2=VT100，生效处见 frontend/src/keymap.ts
	NumColumns int    `json:"numColumns"`
	NumRows    int    `json:"numRows"`
	CursorType int    `json:"cursorType"` // 0=Block 1=Underline 2=I-Beam
	Backspace  int    `json:"backspace"`  // 0=^H 1=^?，同由 keymap.ts 消费

	// Connection / misc
	MaxIdleSeconds   int    `json:"maxIdleSeconds"`
	AntiIdle         bool   `json:"antiIdle"`
	AntiIdleMessage  string `json:"antiIdleMessage"`
	AutoReconnect    bool   `json:"autoReconnect"`
	ReconnectInterva int    `json:"reconnectInterval"`
	ReconnectRetries int    `json:"reconnectRetries"` // 重连重试次数；-1 = 无限（对齐原版 retryTimes_）
	EnableZmodem     bool   `json:"enableZmodem"`
	Group            string `json:"group"` // site manager folder/group

	// 网络代理（D3）：站点级覆盖，编号沿用原版 fqterm.h 但含义多一档 ——
	// 0=跟随全局设置（旧 sites.json 缺键即此，行为与升级前一致）、1=本站点强制直连、
	// 2=SOCKS4 3=SOCKS5 4=HTTP（见 proxy.go）。地址为空等于不用代理。
	ProxyType int    `json:"proxyType"`
	ProxyHost string `json:"proxyHost"`
	ProxyPort int    `json:"proxyPort"`
	ProxyUser string `json:"proxyUser"`
	ProxyPass string `json:"proxyPass"`

	// Local shell（ProtocolType==3 专用）
	// ShellID 是探测结果的稳定标识（cmd / pwsh / wsl:Ubuntu），补种与清理都靠它认人，
	// 用户改显示名不会让同一条 shell 被再补一遍。ShellArgs 为启动参数；Auto 表示这条
	// 是由「扫描本机 Shell」自动生成、允许被清理的条目，用户手建的 Local 站点永远是 false。
	ShellID   string   `json:"shellID"`
	ShellArgs []string `json:"shellArgs"`
	Auto      bool     `json:"auto"`
}

// Preferences is the global application configuration (see FQTermPref).
type Preferences struct {
	DefaultEncodingID  int    `json:"defaultEncodingID"`
	DefaultTermType    string `json:"defaultTermType"`
	DefaultColumns     int    `json:"defaultColumns"`
	DefaultRows        int    `json:"defaultRows"`
	DefaultScheme      string `json:"defaultScheme"`
	FontName           string `json:"fontName"`
	FontSize           int    `json:"fontSize"`
	ZmodemDir          string `json:"zmodemDir"`
	ImageViewerEnabled bool   `json:"imageViewerEnabled"`
	OpenWarnOnClose    bool   `json:"openWarnOnClose"`
	ScrollbackLines    int    `json:"scrollbackLines"`
	ConfirmClose       bool   `json:"confirmClose"`
	MenuClickEnabled   bool   `json:"menuClickEnabled"` // 鼠标点 BBS 菜单/清单（原版 detectPageState + processLClick）

	// 发帖分隔符（编辑工具栏）：上一次选用的那个字符，热键 M-x s 直接复用。
	// 空串表示没选过，前端退回出厂默认 ^；只存能安全往返的可见字符。
	PostSepChar string `json:"postSepChar"`

	// IP 归属地（原版 openUrlCheck_ + qqwry.dat）：悬停命中屏幕上的 IPv4 就查本地纯真库。
	// IPLocationDb 空则用自动下载落点（配置目录）；IPLocationUrl 空则用内置镜像地址。
	IPLocationEnabled bool   `json:"ipLocationEnabled"` // 总开关，关掉就不再扫屏幕也不会下库
	IPLocationDb      string `json:"ipLocationDb"`      // qqwry.dat 路径或所在目录，可空
	IPLocationUrl     string `json:"ipLocationUrl"`     // 自定义库下载地址，可空

	// 网络代理（D3）：全局一份，站点可按 proxyForSite 覆盖。它同时管住终端连接
	// （telnet/ssh）与两条 HTTP 出路（IP 库下载、链接预览）。
	ProxyType int    `json:"proxyType"` // 0=不用代理 2=SOCKS4 3=SOCKS5 4=HTTP
	ProxyHost string `json:"proxyHost"`
	ProxyPort int    `json:"proxyPort"` // 0 时按类型补默认端口（见 proxyDefaultPort）
	ProxyUser string `json:"proxyUser"` // 留空即不做认证
	ProxyPass string `json:"proxyPass"`

	// 窗口形态：普通窗口与 quake 下拉共用同一个 HWND，只切换几何与置顶策略。
	// 下拉尺寸按工作区（已扣除任务栏）的百分比给，因此换显示器、改缩放比都自动适配。
	QuakeWidthPct   int  `json:"quakeWidthPct"`   // quake 宽度占工作区百分比，不足 100 时水平居中
	QuakeHeightPct  int  `json:"quakeHeightPct"`  // quake 高度占工作区百分比
	QuakeHideOnBlur bool `json:"quakeHideOnBlur"` // quake 失焦自动收起
	TrayEnabled     bool `json:"trayEnabled"`     // 显示系统托盘图标
	CloseToTray     bool `json:"closeToTray"`     // 点 ✕ 收起而非退出

	// 启动器搜索范围（launcher 分支）：开始菜单与桌面永远在搜，这里只填「额外目录」。
	// LauncherExts 出厂 exe,lnk,bat,ps1,com，用户可增减；目录或后缀改动在偏好保存后
	// 立即重建索引（见 main.go 的 OnPrefsChanged 接线）。
	LauncherDirs []string `json:"launcherDirs"`
	LauncherExts []string `json:"launcherExts"`
}

// defaultLauncherExts 是启动器文件搜索的出厂后缀。lnk 也列其中：用户把软件安装
// 目录加进来时，那里常有现成的快捷方式，语义与开始菜单的 .lnk 完全一致。
var defaultLauncherExts = []string{"exe", "lnk", "bat", "ps1", "com"}

// FileEntry 是文件管理器（launcher 分支）的一条目录项：后端 ListDir 逐条直出，
// 排序与呈现都交给前端。Path 是全路径（一切后续操作的唯一句柄），ModTime 用
// UnixNano（按日期列排序时避免同目录里秒级精度打平）。
type FileEntry struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mtime"` // UnixNano
	IsDir   bool   `json:"isDir"`
	Hidden  bool   `json:"hidden"`
}

// DriveInfo 是文件管理器驱动器栏的一项。Root 是形如 "C:\" 的路径，Type 沿用
// Windows GetDriveType 的返回值（0 未知 / 2 可移动 / 3 本地 / 4 网络 / 5 光驱）。
type DriveInfo struct {
	Root  string `json:"root"`
	Label string `json:"label"`
	Type  int    `json:"type"`
	Ready bool   `json:"ready"`
}

// KeyBinding maps an Emacs-style key sequence to a command id. Parsing and
// matching happen entirely on the frontend (see frontend/src/keyseq.ts for the
// notation grammar); the backend only persists the table.
//
// 表里的每一条都是应用内绑定（本应用窗口有焦点即生效，不要求焦点在终端）。系统级
// 跨应用热键是另一套东西，由 AppService 直接注册，不占用这张表。
type KeyBinding struct {
	Keys    string `json:"keys"`    // 序列：“C-x C-f”、“C-c <ESC> o”
	Command string `json:"command"` // 命令 id
	Enabled bool   `json:"enabled"`
	Note    string `json:"note"` // 备注（可选，仅供绑定表展示）
}

// KeymapConfig is the whole keymap file (keybindings.json).
type KeymapConfig struct {
	Enabled   bool         `json:"enabled"`   // 总开关
	TimeoutMs int          `json:"timeoutMs"` // 前缀后等待续键的超时
	Echo      bool         `json:"echo"`      // 在状态区回显已按下的序列
	Bindings  []KeyBinding `json:"bindings"`
	// Seeded 记下“已尝试过补种的出厂命令 id”。它存在的意义是把“新增出厂绑定”
	// 和“用户手工解绑”区分开：没有这份名单，每次启动都会把用户删掉的绑定塞回去。
	Seeded []string `json:"seeded,omitempty"`
}

// DefaultKeymap is the factory table: the framework ships enabled but claims
// only the sequences whose features actually expect a floating, overlay UI
// (the site sidebar), so ordinary typing still passes straight through to the BBS.
// Users can rebind or unbind these in the keymap editor (⌨ 热键).
func DefaultKeymap() KeymapConfig {
	bindings := []KeyBinding{
		{
			Keys:    "M-x p",
			Command: "app.toggle-sidebar",
			Enabled: true,
			Note:    "显示/隐藏浮动态站点栏（也可把鼠标移到窗口左缘唤出）",
		},
		{
			Keys:    "M-x M-q",
			Command: "app.toggle-quake-mode",
			Enabled: true,
			Note:    "普通窗口 / quake 下拉模式互切（跨应用呼出走系统热键 Alt+`）",
		},
		// 全屏。键位取 f = fullscreen，还在 M-x 一族里；与 tab.close(M-x k)、
		// 发帖一族(M-x s/r/m/l/v/i/t)、站点栏(M-x p) 都不撞（重键由 TestDefaultKeymapHasNoDuplicateKeys 扫）。
		{
			Keys:    "M-x f",
			Command: "app.toggle-fullscreen",
			Enabled: true,
			Note:    "全屏 / 退出全屏（全屏时工具条沉到屏幕底缘，鼠标停 1 秒浮出；焦点不在终端时 ESC 也退出）",
		},
		// 标签循环切换。记法里尖括号是特殊键名，S- 前缀不会被字符大小写吸掉
		// （不同于 S-a ≡ A），所以写 S-<LEFT> 就是“按住 Shift 按左方向键”。
		// 代价：绑定生效期间终端里 shift+方向键 扩展选区会被吞掉，想两者兼得
		// 就在这张表里改掉或停用它们。
		{
			Keys:    "S-<LEFT>",
			Command: "tab.prev",
			Enabled: true,
			Note:    "切到上一个标签（循环；只有一个标签时无动作）",
		},
		{
			Keys:    "S-<RIGHT>",
			Command: "tab.next",
			Enabled: true,
			Note:    "切到下一个标签（循环；只有一个标签时无动作）",
		},
		// 关闭当前标签。键位收在 M-x 一族里（k = close/kill），与发帖那几条不撞。
		{
			Keys:    "M-x k",
			Command: "tab.close",
			Enabled: true,
			Note:    "关闭当前标签（连着线时按偏好可能弹确认；断开态下还可直接按空格关）",
		},
		// 发帖动作一族：除 post.insert 外都是「读鼠标选区 → 变换 → 原地替换」。
		// 源头不用剪贴板：水木那类站的编辑器跑在服务器上，剪贴板里躺着什么客户端猜不到，
		// 而用户要改的是他刚在编辑区敲下去的那段字 —— 选区同时供了内容和位置。
		// 键位收在 M-x 前缀的同一族里，避开已被占用的 M-x p 与 M-x M-q。
		{
			Keys:    "M-x s",
			Command: "post.sep",
			Enabled: true,
			Note:    "选中文字字间插入上次选用的分隔符（可写成 post.sep:符号 固定某个）",
		},
		{
			Keys:    "M-x r",
			Command: "post.sep-random",
			Enabled: true,
			Note:    "从预置集合里随机挑一种符号，把选中那段插字间",
		},
		{
			Keys:    "M-x m",
			Command: "post.sep-mixed",
			Enabled: true,
			Note:    "逐字随机：选中那段里每两个字之间抽一个不同的预置符号",
		},
		{
			Keys:    "M-x l",
			Command: "post.sep-pick",
			Enabled: true,
			Note:    "打开分隔符选择面板（预置符号 / 自定义 / 随机都在里头）",
		},
		{
			Keys:    "M-x v",
			Command: "post.vertical",
			Enabled: true,
			Note:    "选中文字转竖排后原地替换（栅格按当前终端列数算）",
		},
		{
			Keys:    "M-x i",
			Command: "post.insert",
			Enabled: true,
			Note:    "剪贴板原样逐行灌入光标处（不删任何东西，也是替换失败时的后路）",
		},
		{
			Keys:    "M-x t",
			Command: "post.toolbar-toggle",
			Enabled: true,
			Note:    "钉住 / 收起终端顶部的编辑工具栏（平时把鼠标移到顶缘唤出）",
		},
		// 文件管理器（launcher 分支）：键位取 e = explorer，还在 M-x 一族里；
		// 与已占用的 p/f/k/s/r/m/l/v/i/t 都不撞（重键由 TestDefaultKeymapHasNoDuplicateKeys 扫）。
		{
			Keys:    "M-x e",
			Command: "files.open-manager",
			Enabled: true,
			Note:    "新建一个 Total Commander 式双栏文件管理器标签",
		},
		// 编辑文件管理器活动栏的路径（d = directory）。只在有可见文件管理器标签时生效，
		// 落在终端标签上时命令自己回一句提示。键位与已占用的 p/f/k/s/r/m/l/v/i/t/e 都不撞。
		{
			Keys:    "M-x d",
			Command: "files.edit-path",
			Enabled: true,
			Note:    "编辑文件管理器当前栏的路径（回车跳转 · Esc 取消）",
		},
		// 复制当前高亮行的全路径（c = copy）。与编辑路径同一套广播模式，只在有可见
		// 文件管理器标签时生效；键位与已占用的 p/f/k/s/r/m/l/v/i/t/e/d/b 都不撞。
		{
			Keys:    "M-x c",
			Command: "files.copy-path",
			Enabled: true,
			Note:    "复制文件管理器当前栏高亮文件的全路径到剪贴板",
		},
		// 按序号启动站点（b = BBS）。序号是侧栏从上往下的视觉顺序（含本机 shell），
		// 前端按分组展开序数；键位与已占用的 p/f/k/s/r/m/l/v/i/t/e/d 都不撞。
		{
			Keys:    "M-x b",
			Command: "site.launch-by-number",
			Enabled: true,
			Note:    "弹小输入框，输数字回车启动侧栏从上往下第 N 个站点（含本机 shell；Esc 取消）",
		},
		// 终端复制/粘贴：所有终端标签（本机 shell / SSH / Telnet）都生效，命令自己判
		// 活动标签是不是连着线的终端。键位不用 Ctrl+Shift+C/V：序列引擎对字母做大小写
		// 折叠（S-a ≡ A），C-S-c 与 C-c 归一后是同一个键，而 Ctrl+C 是 shell 的中断键，
		// 绝不能占。取经典控制台键 Ctrl+Insert / Shift+Insert：特殊键名不受折叠影响，
		// 与已有绑定都不撞（重键由 TestDefaultKeymapHasNoDuplicateKeys 扫）。
		{
			Keys:    "C-<INS>",
			Command: "term.copy",
			Enabled: true,
			Note:    "复制终端选区到剪贴板（shell / SSH / Telnet 通用；先鼠标框选文字）",
		},
		{
			Keys:    "S-<INS>",
			Command: "term.paste",
			Enabled: true,
			Note:    "把剪贴板贴进终端（shell / SSH / Telnet 通用；逐行限速送入，末尾不自动回车）",
		},
	}
	return KeymapConfig{
		Enabled:   true,
		TimeoutMs: 1000,
		Echo:      true,
		Bindings:  bindings,
		Seeded:    seededIDs(bindings),
	}
}

// seededIDs 把绑定列表映成命令 id 名单（补种记录用，重复 id 只记一次）。
func seededIDs(bindings []KeyBinding) []string {
	out := make([]string, 0, len(bindings))
	seen := map[string]bool{}
	for _, b := range bindings {
		if seen[b.Command] {
			continue
		}
		seen[b.Command] = true
		out = append(out, b.Command)
	}
	return out
}

// GlobalKey 是一条系统级（跨应用）热键绑定。它与应用内 KeyBinding 是两套东西：
// 由后端低级键盘钩子（kbdhook_windows.go）匹配，本窗口没焦点也生效，语法只有两种
// 形状 —— 单段组合键（M-`）或两段相同键的双击（<C-M> <C-M>），详见 globalkey.go。
type GlobalKey struct {
	Keys    string `json:"keys"`
	Action  string `json:"action"` // 动作 id，见 AppService.ListGlobalActions
	Enabled bool   `json:"enabled"`
	Note    string `json:"note"`
}

// GlobalKeymap 是 globalkeys.json 的全部内容。
type GlobalKeymap struct {
	Enabled bool `json:"enabled"`
	// DoubleTapMs 是双击判定窗口：全部松开后再按下同一组合键，超过这个时长就不算双击。
	DoubleTapMs int         `json:"doubleTapMs"`
	Bindings    []GlobalKey `json:"bindings"`
	// Seeded 记下「已尝试过补种的出厂动作 id」，形状与 KeymapConfig.Seeded 同一套
	// 语义：新版本新增的出厂热键只塞一次，用户删掉就不会每次启动又复活。
	Seeded []string `json:"seeded,omitempty"`
}

// DefaultGlobalKeymap 是出厂表。出厂行靠 store 加载时的台账补种（见
// seedFactoryGlobalBindings）送达已有配置的老用户：新版增了动作（launcher.toggle）
// 时补一次，用户删过就不再强塞；手工解绑后也能用界面上的「恢复出厂」按钮复位。
func DefaultGlobalKeymap() GlobalKeymap {
	bindings := []GlobalKey{
		{
			Keys:    "M-`",
			Action:  "quake.toggle",
			Enabled: true,
			Note:    "Alt+反引号；波浪号是同一个物理键，也认（绑定没要求 Shift 时多按的 Shift 不算数）",
		},
		{
			Keys:    "<C-M> <C-M>",
			Action:  "launcher.toggle",
			Enabled: true,
			Note:    "双击 Ctrl+Alt 呼出/收起启动器（模糊搜索开始菜单与桌面快捷方式，回车启动）",
		},
		{
			Keys:    "<C-M> <C-M>",
			Action:  "quake.toggle",
			Enabled: false,
			Note:    "示例：双击 Ctrl+Alt 呼出 quake，出厂默认停用（同键位已给启动器）",
		},
	}
	return GlobalKeymap{
		Enabled:     true,
		DoubleTapMs: defaultDoubleTapMs,
		Bindings:    bindings,
		Seeded:      seededActionIDs(bindings),
	}
}

// seededActionIDs 把出厂行映成动作 id 名单（补种台账用，重复 id 只记一次）。
func seededActionIDs(bindings []GlobalKey) []string {
	out := make([]string, 0, len(bindings))
	seen := map[string]bool{}
	for _, b := range bindings {
		if seen[b.Action] {
			continue
		}
		seen[b.Action] = true
		out = append(out, b.Action)
	}
	return out
}

// ColorScheme defines terminal palette (16 ANSI colors + defaults).
type ColorScheme struct {
	Name       string     `json:"name"`
	Background string     `json:"background"`
	Foreground string     `json:"foreground"`
	Cursor     string     `json:"cursor"`
	Palette    [16]string `json:"palette"`
}

// DefaultSite returns a sensible BBS-oriented default profile.
func DefaultSite() Site {
	return Site{
		Host:             "bbs.example.com",
		Port:             23,
		HostType:         0,
		ProtocolType:     0,
		ServerEncodingID: 0,
		TermType:         "vtnt",
		KeyType:          0,
		NumColumns:       80,
		NumRows:          25,
		CursorType:       0,
		Backspace:        1,
		EnableZmodem:     true,
		// 空闲/重连默认值对齐原版 fqterm_param.cpp：maxIdle=120、antiidlestring="^@"、isantiidle=true。
		MaxIdleSeconds:   120,
		AntiIdle:         true,
		AntiIdleMessage:  "^@",
		AutoReconnect:    false,
		ReconnectInterva: 3,
		ReconnectRetries: -1,
		ColorScheme:      "Default",
		FontName:         "Courier New",
		FontSize:         14,
		// 字体相关默认值对齐原版：中英分字体、字号自适应（isFontAutoFit_=1）、开抗锯齿。
		FontNameCJK:  "SimSun",
		CharSpacing:  0,
		LineSpacing:  0,
		AntiAlias:    true,
		FontAutoFit:  1,
		UseANSIColor: true,
		// 总是高亮默认关（原版 fqterm_param.cpp:22 isAlwaysHighlight_=0）：开着会把整屏前景
		// 抬到亮色半区，水木那种已经用亮色的版面会过曝，所以留给用户自己选。
		AlwaysHighlight: false,
	}
}

func defaultPreferences() Preferences {
	return Preferences{
		DefaultEncodingID: 0,
		DefaultTermType:   "vtnt",
		DefaultColumns:    80,
		DefaultRows:       25,
		DefaultScheme:     "Default",
		FontName:          "Courier New",
		FontSize:          14,
		ZmodemDir:         "",
		// 超链接预览窗口默认开启：BBS 阅读里点链接是最常见的动作，关掉之后
		// 同一次点击只会把链接丢给系统浏览器。
		ImageViewerEnabled: true,
		OpenWarnOnClose:    false,
		ScrollbackLines:    2000,
		ConfirmClose:       true,
		// 点菜单默认开着：判定只在悬停已命中（手型光标 + 整行高亮）时才响应点击，
		// 判错的表现只是「这一点没反应」，不会把按键误发给 BBS。
		MenuClickEnabled: true,
		// IP 归属地默认开着：没库时只会静默地去下一次，不会往屏幕上弹东西。
		IPLocationEnabled: true,
		QuakeWidthPct:     100,
		QuakeHeightPct:    40,
		QuakeHideOnBlur:   false,
		TrayEnabled:       true,
		CloseToTray:       false,
		LauncherExts:      defaultLauncherExts,
	}
}
