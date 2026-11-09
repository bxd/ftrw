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
//
// 本地 shell（ProtocolType==3）直接原样返回：它没有服务器，MaxIdleSeconds==0 是
// 「故意关掉」而不是「字段缺失」，一旦走进上面的分支就会被补成开保活 + ^@ 消息，
// 于是每隔两分钟往用户的 shell 里灌一个 NUL。
func normalizeSite(site Site) Site {
	if site.ProtocolType == 3 {
		return site
	}
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
	prefs = normalizePreferences(prefs)
	cs.store.mu.Lock()
	cs.store.Prefs = prefs
	cs.store.mu.Unlock()
	return cs.store.SavePrefs()
}

// normalizePreferences 守住 quake 宽/高的合法区间：前端传回的是整个 Preferences 对象，
// 旧版本页面或缺失字段（上一轮还没有 quakeWidthPct）会把它们带成 0，直接生效会把
// 窗口压成看不见的 0 尺寸条。
func normalizePreferences(p Preferences) Preferences {
	def := defaultPreferences()
	p.QuakeWidthPct = quakePct(p.QuakeWidthPct, def.QuakeWidthPct)
	p.QuakeHeightPct = quakePct(p.QuakeHeightPct, def.QuakeHeightPct)
	return p
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

// --- Keymap (Emacs-style key sequences) ---

// GetKeymap returns the persisted key table. Sequence syntax is parsed on the
// frontend, so the backend only guarantees sane numeric bounds and a non-nil slice.
func (cs *ConfigService) GetKeymap() KeymapConfig {
	cs.store.mu.RLock()
	defer cs.store.mu.RUnlock()
	return normalizeKeymap(cs.store.Keymap)
}

// DefaultKeymap exposes the factory table so the editor's「清空为出厂状态」button
// can rebuild it without duplicating the binding list on the frontend.
func (cs *ConfigService) DefaultKeymap() KeymapConfig { return normalizeKeymap(DefaultKeymap()) }

func (cs *ConfigService) SaveKeymap(km KeymapConfig) error {
	km = normalizeKeymap(km)
	cs.store.mu.Lock()
	// Seeded 是后端自己的补种台账，前端那个对象里根本没这个字段（见 api.ts 的
	// KeymapConfig）。不并回去的话，一次保存就能把台账清空，下次启动又把用户
	// 删掉的出厂绑定塞回来。
	km.Seeded = mergeSeeded(cs.store.Keymap.Seeded, km.Seeded)
	cs.store.Keymap = km
	cs.store.mu.Unlock()
	return cs.store.SaveKeymap()
}

// mergeSeeded 求两份补种记录的并集，保持先后顺序且去重。
func mergeSeeded(existing, incoming []string) []string {
	seen := make(map[string]bool, len(existing)+len(incoming))
	out := make([]string, 0, len(existing)+len(incoming))
	for _, id := range append(append([]string(nil), existing...), incoming...) {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func normalizeKeymap(km KeymapConfig) KeymapConfig {
	if km.TimeoutMs < 200 || km.TimeoutMs > 10000 {
		km.TimeoutMs = 1000
	}
	out := make([]KeyBinding, 0, len(km.Bindings))
	for _, b := range km.Bindings {
		if b.Keys == "" || b.Command == "" {
			continue // 残缺条目（手改文件）直接丢弃，不让它遮蔽其他序列
		}
		// 旧配置里的 "scope" 字段已退役：解码时自然被忽略，下次保存就不再写回。
		out = append(out, b)
	}
	km.Bindings = out
	return km
}
