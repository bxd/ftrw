package main

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ConfigService is the Wails-bound service exposing site manager, preferences
// and color scheme operations to the frontend.
type ConfigService struct {
	store   *Store
	draftMu sync.Mutex // 草稿保存/删除串行化，避免慢请求覆盖新快照
	// onProxyChanged 在代理设置真的改了之后被叫一次（异步）。存在的理由很具体：
	// IP 库下载失败过一轮就要等一天再去碰，而用户刚刚才把代理填对——这时候不能让他
	// 等到明天。见 main.go 里的接线。
	onProxyChanged func()
	// onPrefsChanged 在每次偏好保存后都会被叫一次（异步）：启动器拿它感知
	// 搜索目录/后缀的改动并立即重建索引。与 onProxyChanged 不同，这里不比旧值，
	// 叫一声没有副作用。见 main.go 里的接线。
	onPrefsChanged func(prefs Preferences)
}

func NewConfigService(store *Store) *ConfigService {
	return &ConfigService{store: store}
}

// OnProxyChanged 接上「代理改了」的回调（只能构造之后设：要通知的 IPService 比 ConfigService 晚建）。
func (cs *ConfigService) OnProxyChanged(fn func()) {
	cs.onProxyChanged = fn
}

// OnPrefsChanged 接上「偏好保存了」的回调，回调拿到的是归一化之后的那一份。
func (cs *ConfigService) OnPrefsChanged(fn func(prefs Preferences)) {
	cs.onPrefsChanged = fn
}

// --- Site manager ---

func (cs *ConfigService) ListSites() []Site {
	cs.store.mu.Lock()
	defer cs.store.mu.Unlock()
	out := make([]Site, len(cs.store.Sites))
	for i, s := range cs.store.Sites {
		out[i] = normalizeSite(s)
		cs.store.Sites[i] = out[i]
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
	if site.ProfileID == "" {
		site.ProfileID = newProfileID()
	}
	if site.ProtocolType == 3 {
		return site
	}
	if site.HostType == 0 && !site.autoEditorSet {
		site.AutoEditor = true
	}
	if site.MaxIdleSeconds == 0 {
		site.MaxIdleSeconds = 120
		site.AntiIdle = true
		// 字体字段里 FontAutoFit==0 与 AntiAlias==false 都是合法含义，无法靠零值区分，
		// 只能在“旧站点”分支里按原版默认补齐（字号自适应 + 开抗锯齿）。
		site.FontAutoFit = 1
		site.AntiAlias = true
		// UseANSIColor 同理：出厂是开（原版 isAnsiColor_=1），老文件缺键拿到 false 会让整个
		// 版面默默变黑白。AlwaysHighlight 出厂为关，缺键正好，不需要补。
		site.UseANSIColor = true
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
	site = normalizeSite(site)
	cs.store.mu.Lock()
	if index >= 0 && index < len(cs.store.Sites) {
		// profileId 是不可变身份：前端改名、改账号或换主机都不能把它换掉。
		site.ProfileID = cs.store.Sites[index].ProfileID
		if site.ProfileID == "" {
			site.ProfileID = newProfileID()
		}
		cs.store.Sites[index] = site
	} else {
		for _, existing := range cs.store.Sites {
			if site.ProfileID != "" && existing.ProfileID == site.ProfileID {
				site.ProfileID = newProfileID()
				break
			}
		}
		if site.ProfileID == "" {
			site.ProfileID = newProfileID()
		}
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
	old := cs.GetPreferences()
	prefs = normalizePreferences(prefs)
	cs.store.mu.Lock()
	cs.store.Prefs = prefs
	cs.store.mu.Unlock()
	// 代理住在两个 http.Transport 的 DialContext 回调里（那里拿不到 store），所以改完
	// 必须把内存里那一份同步掉，否则改设置要重启才生效。
	setGlobalProxy(proxyFromPrefs(prefs))
	if proxyFromPrefs(old) != proxyFromPrefs(prefs) && cs.onProxyChanged != nil {
		go cs.onProxyChanged()
	}
	if cs.onPrefsChanged != nil {
		go cs.onPrefsChanged(prefs)
	}
	return cs.store.SavePrefs()
}

// TestProxy 拿一份还没存盘的代理设置去试连：界面上改完就能测，不必先保存、再等下一次
// 启动才看出效果。成功返一句给人看的话，失败时 error 里写清了卡在代理本身还是卡在建隧。
func (cs *ConfigService) TestProxy(pc ProxyConfig) (string, error) {
	return proxyTest(pc)
}

// GetFMState 回文件管理器会话快照（标签数/每栏路径/活动栏），启动还原用。
func (cs *ConfigService) GetFMState() FMState {
	cs.store.mu.RLock()
	defer cs.store.mu.RUnlock()
	return cs.store.FMState
}

// SetFMState 落盘文件管理器会话。前端在导航/切栏/开关标签后防抖调用。
func (cs *ConfigService) SetFMState(st FMState) error {
	if st.Active < -1 {
		st.Active = -1
	}
	cs.store.mu.Lock()
	cs.store.FMState = st
	cs.store.mu.Unlock()
	return cs.store.SaveFMState()
}

// ListEditorDrafts 返回按最近修改时间倒序排列的本地文章草稿。
func (cs *ConfigService) ListEditorDrafts() []EditorDraft {
	cs.store.mu.RLock()
	defer cs.store.mu.RUnlock()
	out := make([]EditorDraft, len(cs.store.Drafts))
	copy(out, cs.store.Drafts)
	sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt > out[j].UpdatedAt })
	return out
}

// SaveEditorDraft 新增或覆盖一篇草稿。revision 必须等于当前版本，旧快照不能覆盖新内容。
func (cs *ConfigService) SaveEditorDraft(draft EditorDraft) (EditorDraft, error) {
	draft = normalizeEditorDraft(draft)
	if draft.ID == "" {
		return EditorDraft{}, errors.New("草稿缺少 ID")
	}
	cs.draftMu.Lock()
	defer cs.draftMu.Unlock()

	draft.UpdatedAt = time.Now().UnixMilli()
	cs.store.mu.Lock()
	next := append([]EditorDraft(nil), cs.store.Drafts...)
	found := false
	for i := range next {
		if next[i].ID == draft.ID {
			if draft.Revision != next[i].Revision {
				cs.store.mu.Unlock()
				return EditorDraft{}, errors.New("草稿已在别处更新，请重新打开后再保存")
			}
			draft.CreatedAt = next[i].CreatedAt
			draft.Revision++
			next[i] = draft
			found = true
			break
		}
	}
	if !found {
		if draft.Revision != 0 {
			cs.store.mu.Unlock()
			return EditorDraft{}, errors.New("草稿已被删除或已在别处更新")
		}
		draft.Revision = 1
		next = append(next, draft)
	}
	if len(next) > 200 {
		sort.SliceStable(next, func(i, j int) bool {
			return next[i].UpdatedAt > next[j].UpdatedAt
		})
		next = next[:200]
	}
	cs.store.mu.Unlock()
	// 先写成功再提交内存；落盘失败时调用方仍持旧 revision，可以直接重试。
	if err := cs.store.saveJSON("drafts.json", next); err != nil {
		return EditorDraft{}, err
	}
	cs.store.mu.Lock()
	cs.store.Drafts = next
	cs.store.mu.Unlock()
	return draft, nil
}

// DeleteEditorDraft 删除本地草稿；已经上传到 BBS 的正文不受影响。
func (cs *ConfigService) DeleteEditorDraft(id string) error {
	return cs.DeleteEditorDrafts([]string{id})
}

// DeleteEditorDrafts 一次写入批量删除结果，避免逐条删除留下半成品。
func (cs *ConfigService) DeleteEditorDrafts(ids []string) error {
	wanted := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			wanted[id] = struct{}{}
		}
	}
	if len(wanted) == 0 {
		return nil
	}

	cs.draftMu.Lock()
	defer cs.draftMu.Unlock()
	cs.store.mu.Lock()
	next := make([]EditorDraft, 0, len(cs.store.Drafts))
	changed := false
	for _, draft := range cs.store.Drafts {
		if _, ok := wanted[draft.ID]; ok {
			changed = true
			continue
		}
		next = append(next, draft)
	}
	cs.store.mu.Unlock()
	if !changed {
		return nil
	}
	if err := cs.store.saveJSON("drafts.json", next); err != nil {
		return err
	}
	cs.store.mu.Lock()
	cs.store.Drafts = next
	cs.store.mu.Unlock()
	return nil
}

// SaveEditorImage 把浏览器 Canvas 生成的 PNG 落到配置目录，返回可交给 ZMODEM 的路径。
// 只接受 PNG 且限制 8 MiB：既防止把任意文件伪装成图片，也避免误传巨大临时对象。
func (cs *ConfigService) SaveEditorImage(name, dataBase64 string) (string, error) {
	if i := strings.Index(dataBase64, ","); i >= 0 && strings.HasPrefix(dataBase64, "data:") {
		dataBase64 = dataBase64[i+1:]
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(dataBase64))
	if err != nil {
		return "", errors.New("图片数据不是有效的 base64")
	}
	if len(data) < 8 || len(data) > 8<<20 || string(data[:8]) != "\x89PNG\r\n\x1a\n" {
		return "", errors.New("文字图片必须是小于 8 MiB 的 PNG")
	}
	name = sanitizeName(strings.TrimSpace(name))
	if !strings.HasSuffix(strings.ToLower(name), ".png") {
		name = strings.TrimSuffix(name, filepath.Ext(name)) + ".png"
	}
	dir := filepath.Join(cs.store.Dir(), "editor-images")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := uniquePath(dir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// normalizePreferences 守住 quake 宽/高的合法区间：前端传回的是整个 Preferences 对象，
// 旧版本页面或缺失字段（上一轮还没有 quakeWidthPct）会把它们带成 0，直接生效会把
// 窗口压成看不见的 0 尺寸条。
func normalizePreferences(p Preferences) Preferences {
	def := defaultPreferences()
	p.QuakeWidthPct = quakePct(p.QuakeWidthPct, def.QuakeWidthPct)
	p.QuakeHeightPct = quakePct(p.QuakeHeightPct, def.QuakeHeightPct)
	// 代理字段归一（端口越界补默认值、认不出的类型当没配），存盘与生效用同一份，
	// 免得界面里看着是 SOCKS5、实际拨号时走了另一套解释。
	pc := proxyFromPrefs(p).normalized()
	p.ProxyType, p.ProxyHost, p.ProxyPort, p.ProxyUser, p.ProxyPass = pc.Type, pc.Host, pc.Port, pc.User, pc.Pass
	// 启动器搜索范围：剪空白、去重；后缀列表整列被剪空时退回出厂值。
	// 「一个后缀都不搜」不是能成立的状态（那等于把功能弄坏了），想只搜开始菜单
	// 把目录清空就行；旧 prefs.json 没这个键（解码后 nil）也从这里拿到出厂后缀。
	p.LauncherDirs = normalizeLauncherList(p.LauncherDirs, false)
	if ex := normalizeLauncherList(p.LauncherExts, true); len(ex) > 0 {
		p.LauncherExts = ex
	} else {
		p.LauncherExts = defaultLauncherExts
	}
	return p
}

// normalizeLauncherList 逐项剪空白、小写（withLower 时）、丢弃空项并按小写去重。
// 剪完只剩标点的项（纯逗号、纯点）也丢：后缀框里多打的逗号会被前端拆成这种碎片，
// 留着它们就会顶掉「整列已空 → 回出厂值」的判断。
func normalizeLauncherList(items []string, withLower bool) []string {
	out := make([]string, 0, len(items))
	seen := make(map[string]bool, len(items))
	for _, it := range items {
		v := strings.TrimSpace(it)
		if strings.Trim(v, ".,;、；， ") == "" {
			continue
		}
		key := strings.ToLower(v)
		if withLower {
			v = key
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, v)
	}
	return out
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
