package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
)

// Store persists sites, preferences, keymap and color schemes as JSON files
// under the user config directory (e.g. %APPDATA%/FTRW on Windows).
type Store struct {
	mu        sync.RWMutex
	dir       string
	Sites     []Site        `json:"sites"`
	Prefs     Preferences   `json:"prefs"`
	Schemes   []ColorScheme `json:"schemes"`
	Keymap    KeymapConfig  `json:"keymap"`
	GlobalKey GlobalKeymap  `json:"globalKey"`
	FMState   FMState       `json:"fmState"`
	loaded    bool
}

// FMTabState 一个文件管理器标签的会话快照：两栏路径 + 活动栏。
type FMTabState struct {
	Panes  []string `json:"panes"`
	Active int      `json:"active"`
}

// FMState 文件管理器会话：开了几个标签、每标签每栏路径。Active 是活动标签在
// 文件标签里的序号（-1 = 当时活动的不是文件标签），启动只还原文件标签，序号直接可用。
type FMState struct {
	Tabs   []FMTabState `json:"tabs"`
	Active int          `json:"active"`
}

func NewStore() (*Store, error) {
	dir := configRoot()
	base := filepath.Dir(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	migrateLegacyConfig(dir, filepath.Join(base, legacyDirName))
	s := &Store{dir: dir}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// configRoot 是本应用自己的数据目录（Windows 上为 %APPDATA%\FTRW）。日志目录也从
// 这里派生，所以它必须能在 Store 建起来之前就被调用。
func configRoot() string {
	base, err := os.UserConfigDir()
	if err != nil {
		base = "."
	}
	return filepath.Join(base, "FTRW")
}

// legacyDirName is the config folder used before the app was renamed FTRW. It was
// shared with the original C++ FQTerm, hence migration copies only our own JSON
// files and never deletes or touches anything else in there.
const legacyDirName = "FQTerm"

// ownConfigFiles are the files this rewrite owns; anything else in the legacy
// directory (fqterm.cfg, address.cfg, known_hosts, ...) belongs to the original
// client and must be left alone.
var ownConfigFiles = []string{"sites.json", "prefs.json", "schemes.json", "keybindings.json", "globalkeys.json"}

// migrateLegacyConfig seeds the new directory from the old one, but only when the
// new directory is still empty of our own files — otherwise a partially written
// new config would silently shadow (or be overwritten by) the old data.
func migrateLegacyConfig(dir, legacy string) {
	if dir == legacy {
		return
	}
	if _, err := os.Stat(legacy); err != nil {
		return
	}
	for _, name := range ownConfigFiles {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return // 新版已经自己跑过，尊重现有数据
		}
	}
	copied := 0
	for _, name := range ownConfigFiles {
		src := filepath.Join(legacy, name)
		b, err := os.ReadFile(src)
		if err != nil {
			continue // 旧版没写过这个文件（例如从没动过热键表）
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			log.Printf("migrate %s: %v", name, err)
			continue
		}
		copied++
	}
	if copied > 0 {
		log.Printf("配置已从 %s 迁移 %d 个文件到 %s", legacy, copied, dir)
	}
}

func (s *Store) file(name string) string { return filepath.Join(s.dir, name) }

func (s *Store) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Prefs = defaultPreferences()
	s.Sites = nil
	s.Schemes = defaultSchemes()
	s.Keymap = DefaultKeymap()
	s.GlobalKey = DefaultGlobalKeymap()

	// prefs：直接解码进 s.Prefs（已先填好出厂默认值），这样旧 prefs.json 里缺失的
	// 新增字段会自然保留默认——若解码进一个空结构再整体赋值，新字段会被静默清零。
	if b, err := os.ReadFile(s.file("prefs.json")); err == nil {
		if err := json.Unmarshal(b, &s.Prefs); err != nil {
			log.Printf("parse prefs.json: %v", err)
		}
	}
	// sites
	if b, err := os.ReadFile(s.file("sites.json")); err == nil {
		var sites []Site
		if json.Unmarshal(b, &sites) == nil {
			s.Sites = sites
		}
	}
	// schemes (merge over defaults by name)
	if b, err := os.ReadFile(s.file("schemes.json")); err == nil {
		var schemes []ColorScheme
		if json.Unmarshal(b, &schemes) == nil && len(schemes) > 0 {
			s.Schemes = schemes
		}
	}
	// keymap: keybindings.json holds the KeymapConfig object itself
	if b, err := os.ReadFile(s.file("keybindings.json")); err == nil {
		var km KeymapConfig
		if json.Unmarshal(b, &km) == nil {
			if km.TimeoutMs <= 0 {
				km.TimeoutMs = 1000
			}
			if km.Bindings == nil {
				km.Bindings = []KeyBinding{}
			}
			seedFactoryBindings(&km, DefaultKeymap())
			s.Keymap = km
		}
	}
	// globalkeys.json：跨应用热键表。与应用内键序表故意分开存：两张表的生效条件、
	// 语法形状、由谁匹配都不同，混在一个文件里会让 normalizeKeymap 那种「按形状钳制」
	// 变成两套分支。出厂行里的新增动作靠 Seeded 台账补种（见 seedFactoryGlobalBindings）。
	if b, err := os.ReadFile(s.file("globalkeys.json")); err == nil {
		var gk GlobalKeymap
		if json.Unmarshal(b, &gk) == nil {
			gk = normalizeGlobalKeymap(gk)
			seedFactoryGlobalBindings(&gk, DefaultGlobalKeymap())
			s.GlobalKey = gk
		}
	}
	// fmstate.json：文件管理器标签会话（标签数/每栏路径/活动栏），前端防抖落盘。
	if b, err := os.ReadFile(s.file("fmstate.json")); err == nil {
		var fm FMState
		if json.Unmarshal(b, &fm) == nil {
			s.FMState = fm
		}
	}
	s.loaded = true
	return nil
}

// seedFactoryBindings 把新版本新增的出厂绑定补种进用户已有的绑定表。
//
// 判据是 Seeded 里「已尝试过补种的命令 id」而不是「表里有没有这条 Keys」：用户
// 可能改键或干脆解绑，只要某个命令的出厂绑定被尝试过一次就不再强塞回去，否则
// 「删掉 M-x M-q」会在每次启动时复活。bound 那一跳是为了区分「用户手工解绑」与
// 「用户把这条改成了别的键」——后者命令 id 还在，不该再加一条重复绑定。
func seedFactoryBindings(cur *KeymapConfig, factory KeymapConfig) {
	seeded := make(map[string]bool, len(cur.Seeded))
	for _, id := range cur.Seeded {
		seeded[id] = true
	}
	bound := make(map[string]bool, len(cur.Bindings))
	for _, b := range cur.Bindings {
		bound[b.Command] = true
	}
	for _, b := range factory.Bindings {
		if seeded[b.Command] {
			continue
		}
		seeded[b.Command] = true
		cur.Seeded = append(cur.Seeded, b.Command)
		if bound[b.Command] {
			continue
		}
		cur.Bindings = append(cur.Bindings, b)
	}
}

// seedFactoryGlobalBindings 把新版本新增的出厂跨应用热键补种进用户已有的表。
// 判据与 seedFactoryBindings 完全同一套：台账记「已尝试过补种的动作 id」，
// bound 那一跳区分「用户手工解绑」与「用户把这条改成了别的键」。
// 与表里的台账只在内存，不单独落盘：用户下次在界面保存时自然写回。
func seedFactoryGlobalBindings(cur *GlobalKeymap, factory GlobalKeymap) {
	seeded := make(map[string]bool, len(cur.Seeded))
	for _, id := range cur.Seeded {
		seeded[id] = true
	}
	bound := make(map[string]bool, len(cur.Bindings))
	for _, b := range cur.Bindings {
		bound[b.Action] = true
	}
	for _, b := range factory.Bindings {
		if seeded[b.Action] {
			continue
		}
		seeded[b.Action] = true
		cur.Seeded = append(cur.Seeded, b.Action)
		if bound[b.Action] {
			continue
		}
		cur.Bindings = append(cur.Bindings, b)
	}
}

func (s *Store) saveJSON(name string, v interface{}) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.file(name), b, 0o644)
}

func (s *Store) SaveSites() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.saveJSON("sites.json", s.Sites)
}

func (s *Store) SavePrefs() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.saveJSON("prefs.json", s.Prefs)
}

func (s *Store) SaveSchemes() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.saveJSON("schemes.json", s.Schemes)
}

func (s *Store) SaveKeymap() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.saveJSON("keybindings.json", s.Keymap)
}

// SaveGlobalKey 落盘跨应用热键表（globalkeys.json）。
func (s *Store) SaveGlobalKey() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.saveJSON("globalkeys.json", s.GlobalKey)
}

// SaveFMState 落盘文件管理器会话（fmstate.json）。
func (s *Store) SaveFMState() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.saveJSON("fmstate.json", s.FMState)
}

// Dir returns the config directory (used for default zmodem download path).
func (s *Store) Dir() string { return s.dir }

func defaultSchemes() []ColorScheme {
	green := ColorScheme{
		Name:       "Default",
		Background: "#000000",
		Foreground: "#AAAAAA",
		Cursor:     "#AAAAAA",
		Palette: [16]string{
			"#000000", "#AA0000", "#00AA00", "#AA5500",
			"#0000AA", "#AA00AA", "#00AAAA", "#AAAAAA",
			"#555555", "#FF5555", "#55FF55", "#FFFF55",
			"#5555FF", "#FF55FF", "#55FFFF", "#FFFFFF",
		},
	}
	blue := ColorScheme{
		Name:       "BBS Blue",
		Background: "#1a2b4a",
		Foreground: "#c7d5f0",
		Cursor:     "#ffffff",
		Palette:    green.Palette,
	}
	dos := ColorScheme{
		Name:       "Classic DOS",
		Background: "#0000a8",
		Foreground: "#ffffff",
		Cursor:     "#aaaaaa",
		Palette: [16]string{
			"#000000", "#a80000", "#00a800", "#a85400",
			"#0000a8", "#a800a8", "#00a8a8", "#a8a8a8",
			"#545454", "#fc5454", "#54fc54", "#fcfc54",
			"#5454fc", "#fc54fc", "#54fcfc", "#fcfcfc",
		},
	}
	amber := ColorScheme{
		Name:       "Amber Terminal",
		Background: "#1c1300",
		Foreground: "#ffb000",
		Cursor:     "#ffd870",
		Palette:    green.Palette,
	}
	return []ColorScheme{green, blue, dos, amber}
}
