package main

import (
	"errors"
)

// ConfigService is the Wails-bound service exposing site manager, preferences
// and color scheme operations to the frontend.
type ConfigService struct {
	store *Store
}

func NewConfigService(store *Store) *ConfigService {
	return &ConfigService{store: store}
}

// --- Site manager ---

func (cs *ConfigService) ListSites() []Site {
	cs.store.mu.RLock()
	defer cs.store.mu.RUnlock()
	out := make([]Site, len(cs.store.Sites))
	for i, s := range cs.store.Sites {
		out[i] = normalizeSite(s)
	}
	return out
}

// normalizeSite 给旧配置补上后来新增的空闲/重连默认值。以 maxIdleSeconds==0 判定
// “字段缺失的旧站点”（0 秒本身无意义），按原版默认开启防空闲；避免升级后老站点
// 默默丢掉保活能力。重连保持默认关闭，仅补齐间隔/次数。
func normalizeSite(site Site) Site {
	if site.MaxIdleSeconds == 0 {
		site.MaxIdleSeconds = 120
		site.AntiIdle = true
		// 字体字段里 FontAutoFit==0 与 AntiAlias==false 都是合法含义，无法靠零值区分，
		// 只能在“旧站点”分支里按原版默认补齐（字号自适应 + 开抗锯齿）。
		site.FontAutoFit = 1
		site.AntiAlias = true
		if site.FontNameCJK == "" {
			site.FontNameCJK = "SimSun"
		}
		if site.FontSize == 0 {
			site.FontSize = 14
		}
	}
	if site.AntiIdleMessage == "" {
		site.AntiIdleMessage = "^@"
	}
	if site.ReconnectInterva == 0 {
		site.ReconnectInterva = 3
	}
	if site.ReconnectRetries == 0 {
		site.ReconnectRetries = -1
	}
	return site
}

func (cs *ConfigService) SaveSite(index int, site Site) error {
	cs.store.mu.Lock()
	if index >= 0 && index < len(cs.store.Sites) {
		cs.store.Sites[index] = site
	} else {
		cs.store.Sites = append(cs.store.Sites, site)
	}
	sites := append([]Site(nil), cs.store.Sites...)
	cs.store.mu.Unlock()
	return cs.store.saveJSON("sites.json", sites)
}

func (cs *ConfigService) DeleteSite(index int) error {
	cs.store.mu.Lock()
	if index < 0 || index >= len(cs.store.Sites) {
		cs.store.mu.Unlock()
		return errors.New("index out of range")
	}
	cs.store.Sites = append(cs.store.Sites[:index], cs.store.Sites[index+1:]...)
	sites := append([]Site(nil), cs.store.Sites...)
	cs.store.mu.Unlock()
	return cs.store.saveJSON("sites.json", sites)
}

func (cs *ConfigService) DefaultSite() Site { return DefaultSite() }

// ListSystemFonts returns the installed font family names for the font picker.
func (cs *ConfigService) ListSystemFonts() []string { return listSystemFonts() }

// --- Preferences ---

func (cs *ConfigService) GetPreferences() Preferences {
	cs.store.mu.RLock()
	defer cs.store.mu.RUnlock()
	return cs.store.Prefs
}

func (cs *ConfigService) SetPreferences(prefs Preferences) error {
	cs.store.mu.Lock()
	cs.store.Prefs = prefs
	cs.store.mu.Unlock()
	return cs.store.SavePrefs()
}

// --- Color schemes ---

func (cs *ConfigService) ListSchemes() []ColorScheme {
	cs.store.mu.RLock()
	defer cs.store.mu.RUnlock()
	out := make([]ColorScheme, len(cs.store.Schemes))
	copy(out, cs.store.Schemes)
	return out
}

func (cs *ConfigService) SaveScheme(scheme ColorScheme) error {
	cs.store.mu.Lock()
	replaced := false
	for i := range cs.store.Schemes {
		if cs.store.Schemes[i].Name == scheme.Name {
			cs.store.Schemes[i] = scheme
			replaced = true
			break
		}
	}
	if !replaced {
		cs.store.Schemes = append(cs.store.Schemes, scheme)
	}
	out := append([]ColorScheme(nil), cs.store.Schemes...)
	cs.store.mu.Unlock()
	return cs.store.saveJSON("schemes.json", out)
}

func (cs *ConfigService) DeleteScheme(name string) error {
	cs.store.mu.Lock()
	filtered := cs.store.Schemes[:0]
	for _, s := range cs.store.Schemes {
		if s.Name != name {
			filtered = append(filtered, s)
		}
	}
	cs.store.Schemes = filtered
	out := append([]ColorScheme(nil), cs.store.Schemes...)
	cs.store.mu.Unlock()
	return cs.store.saveJSON("schemes.json", out)
}
