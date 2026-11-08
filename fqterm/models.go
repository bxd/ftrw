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
	PreLogin     string `json:"preLogin"`
	PostLogin    string `json:"postLogin"`

	// Display / encoding
	ServerEncodingID int    `json:"serverEncodingID"` // 0=GBK 1=BIG5 2=UTF8 3=HKSCS 4=UAO
	ColorScheme      string `json:"colorScheme"`      // scheme name
	FontName         string `json:"fontName"`         // 英文字体（原版 englishFontName_）
	FontSize         int    `json:"fontSize"`
	FontNameCJK      string `json:"fontNameCjk"` // 中文/宽字符字体（原版 otherFontName_）
	CharSpacing      int    `json:"charSpacing"` // 英文字符额外字距 px（原版 charSpacing_）
	LineSpacing      int    `json:"lineSpacing"` // 额外行距 px（原版 lineSpacing_）
	AntiAlias        bool   `json:"antiAlias"`   // 抗锯齿（原版 global/antialias）
	FontAutoFit      int    `json:"fontAutoFit"` // 0=随窗口改行列 1=随窗口改字号（原版默认）2=固定行列
	UseANSIColor     bool   `json:"useAnsiColor"`

	// Terminal
	TermType   string `json:"termType"` // vtnt / xterm / vt100
	KeyType    int    `json:"keyType"`  // 0=BBS 1=XTERM 2=VT100
	NumColumns int    `json:"numColumns"`
	NumRows    int    `json:"numRows"`
	CursorType int    `json:"cursorType"` // 0=Block 1=Underline 2=I-Beam
	Backspace  int    `json:"backspace"`  // 0=^H 1=^?

	// Connection / misc
	MaxIdleSeconds   int    `json:"maxIdleSeconds"`
	AntiIdle         bool   `json:"antiIdle"`
	AntiIdleMessage  string `json:"antiIdleMessage"`
	AutoReconnect    bool   `json:"autoReconnect"`
	ReconnectInterva int    `json:"reconnectInterval"`
	ReconnectRetries int    `json:"reconnectRetries"` // 重连重试次数；-1 = 无限（对齐原版 retryTimes_）
	EnableZmodem     bool   `json:"enableZmodem"`
	Group            string `json:"group"` // site manager folder/group
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
		OpenWarnOnClose:   false,
		ScrollbackLines:   2000,
		ConfirmClose:      true,
	}
}
