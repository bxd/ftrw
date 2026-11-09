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
}

// DefaultGlobalKeymap 是出厂表。这张表不做「补种」：出厂行只有两条、且带一个停用的
// 示例，用户删掉就是删掉了，需要恢复用界面上的「恢复出厂」按钮，比 keybindings.json
// 那套 Seeded 台账更省事，也不会出现「解绑后每次启动又复活」。
func DefaultGlobalKeymap() GlobalKeymap {
	return GlobalKeymap{
		Enabled:     true,
		DoubleTapMs: defaultDoubleTapMs,
		Bindings: []GlobalKey{
			{
				Keys:    "M-`",
				Action:  "quake.toggle",
				Enabled: true,
				Note:    "Alt+反引号；波浪号是同一个物理键，也认（绑定没要求 Shift 时多按的 Shift 不算数）",
			},
			{
				Keys:    "<C-M> <C-M>",
				Action:  "quake.toggle",
				Enabled: false,
				Note:    "示例：双击 Ctrl+Alt 呼出，出厂默认停用",
			},
		},
	}
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
		QuakeHideOnBlur:   true,
		TrayEnabled:       true,
		CloseToTray:       false,
	}
}
