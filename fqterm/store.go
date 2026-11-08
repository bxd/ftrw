package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// Store persists sites, preferences and color schemes as JSON files under the
// user config directory (e.g. %APPDATA%/FQTerm on Windows).
type Store struct {
	mu      sync.RWMutex
	dir     string
	Sites   []Site        `json:"sites"`
	Prefs   Preferences   `json:"prefs"`
	Schemes []ColorScheme `json:"schemes"`
	loaded  bool
}

func NewStore() (*Store, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		base = "."
	}
	dir := filepath.Join(base, "FQTerm")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{dir: dir}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) file(name string) string { return filepath.Join(s.dir, name) }

func (s *Store) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Prefs = defaultPreferences()
	s.Sites = nil
	s.Schemes = defaultSchemes()

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
