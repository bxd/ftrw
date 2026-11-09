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
	mu      sync.RWMutex
	dir     string
	Sites   []Site        `json:"sites"`
	Prefs   Preferences   `json:"prefs"`
	Schemes []ColorScheme `json:"schemes"`
	Keymap  KeymapConfig  `json:"keymap"`
	loaded  bool
}

func NewStore() (*Store, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		base = "."
	}
	dir := filepath.Join(base, "FTRW")
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

// legacyDirName is the config folder used before the app was renamed FTRW. It was
// shared with the original C++ FQTerm, hence migration copies only our own JSON
// files and never deletes or touches anything else in there.
const legacyDirName = "FQTerm"

// ownConfigFiles are the files this rewrite owns; anything else in the legacy
// directory (fqterm.cfg, address.cfg, known_hosts, ...) belongs to the original
// client and must be left alone.
var ownConfigFiles = []string{"sites.json", "prefs.json", "schemes.json", "keybindings.json"}

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

	// prefs
	if b, err := os.ReadFile(s.file("prefs.json")); err == nil {
		var p Preferences
		if json.Unmarshal(b, &p) == nil {
			s.Prefs = p
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
			s.Keymap = km
		}
	}
	s.loaded = true
	return nil
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
