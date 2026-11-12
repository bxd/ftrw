package main

import (
	"path/filepath"
	"strings"
	"unicode"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// shellmenu.go：文件管理器原生右键菜单的跨平台骨架。
//
// 这里只放平台无关的部分：请求/结果的落地、目标路径的清洗、菜单顶部那段 FTRW 插件动作
// 的构建，以及把主窗 HWND 安全取出来的 Bind/nativeHWND。真正把菜单弹出来（Shell
// IContextMenu + TrackPopupMenu）在 shellmenu_windows.go，非 Windows 走 shellmenu_other.go 的桩。

const (
	// shellPluginCmdBase 是 FTRW 插件动作项的菜单命令 id 起点；原生项由 QueryContextMenu
	// 从 0 起分配，上限 shellPluginCmdBase-1，两段不撞。
	shellPluginCmdBase = 0xF000
	// shellMaxPaths 单次右键最多带多少个目标（与 Explorer 语义无关，纯防御：过多 PIDL
	// 会让某些外壳扩展卡死）。
	shellMaxPaths = 64
	// shellMaxTitleRunes 单条菜单文案的 rune 上限（插件回的 title 不可信）。
	shellMaxTitleRunes = 120
	// shellMaxPathLen 超过此长度的路径不送进 PIDL 解析（MAX_PATH 防御）。
	shellMaxPathLen = 260
)

// pluginMenuItem 是一条 FTRW 插件动作在原生菜单里的呈现。
type pluginMenuItem struct {
	cmd      uint32 // 菜单命令 id（shellPluginCmdBase + 序号）
	pluginID string
	actionID string
	title    string
}

// shellMenuOutcome 是平台层 showShellMenu 的回执（跨平台类型，Windows 侧填充）。
type shellMenuOutcome struct {
	picked   bool // 用户确实选了某项（非点空白/Esc 关掉）
	isPlugin bool
	pluginID string
	actionID string
	err      string // 供前端状态栏提示，空表示无
}

// buildPluginMenuItems 把各插件声明的 actions 摊平成菜单顶部那段 FTRW 项，命令 id 从
// shellPluginCmdBase 起连续分配。无 actions 的插件（纯预览）不产生任何项。
func buildPluginMenuItems(tools []PluginTool) []pluginMenuItem {
	var out []pluginMenuItem
	for _, t := range tools {
		for _, a := range t.Actions {
			out = append(out, pluginMenuItem{
				cmd:      uint32(shellPluginCmdBase) + uint32(len(out)),
				pluginID: t.ID,
				actionID: a.ID,
				title:    sanitizeMenuText(t.Title + " · " + a.Title),
			})
		}
	}
	return out
}

// lookupPluginMenuItem 按命令 id 反查对应的插件动作（id 落在 [base, base+len) 内才有效）。
func lookupPluginMenuItem(items []pluginMenuItem, cmd uint32) (pluginMenuItem, bool) {
	if cmd < shellPluginCmdBase {
		return pluginMenuItem{}, false
	}
	idx := int(cmd - shellPluginCmdBase)
	if idx < 0 || idx >= len(items) {
		return pluginMenuItem{}, false
	}
	return items[idx], true
}

// sanitizeMenuText 剥掉控制字符（插件/manifest 回的字符串不可信）并按 rune 截断，
// 保证喂给 AppendMenuW 的是一行干净文案。
func sanitizeMenuText(s string) string {
	var rs []rune
	for _, r := range s {
		if unicode.IsControl(r) {
			continue
		}
		rs = append(rs, r)
		if len(rs) >= shellMaxTitleRunes {
			break
		}
	}
	return strings.TrimSpace(string(rs))
}

// cleanShellPaths 清洗前端送来的目标路径：去空、只留绝对路径、大小写不敏感去重、剔除
// 超长项，总量截到 shellMaxPaths。activeDir 目前不参与（特殊项已由前端换成目录本身）。
func cleanShellPaths(raw []string) []string {
	seen := make(map[string]bool, len(raw))
	out := make([]string, 0, len(raw))
	for _, p := range raw {
		p = strings.TrimSpace(p)
		if p == "" || !filepath.IsAbs(p) {
			continue
		}
		if len(p) > shellMaxPathLen {
			continue
		}
		key := strings.ToLower(p)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, p)
		if len(out) >= shellMaxPaths {
			break
		}
	}
	return out
}

// Bind 在 main 建好主窗之后注入一个 HWND 取用器（不可导出到绑定，见 //wails:ignore）。
//
//wails:ignore
func (s *FileService) Bind(win *application.WebviewWindow) {
	s.bindWatch(win)
	s.taskEmit = func(t *FileTask) { win.EmitEvent(fileTaskEvent, t) }
	s.hwndProvider = func() uintptr {
		var h uintptr
		// NativeWindow 必须在主线程上取；服务方法跑在独立 goroutine，主循环空闲，InvokeSync 安全。
		application.InvokeSync(func() {
			if p := win.NativeWindow(); p != nil {
				h = uintptr(p)
			}
		})
		return h
	}
}

// nativeHWND 取主窗 HWND；未 Bind 或取不到时返回 0（菜单仍会弹，只是 InvokeCommand 无父窗）。
func (s *FileService) nativeHWND() uintptr {
	if s.hwndProvider == nil {
		return 0
	}
	return s.hwndProvider()
}

// ShellContextMenu 在文件管理器行上弹出 Windows 原生右键菜单，顶部拼一段 FTRW 插件动作。
// 命中原生动词就地执行；命中插件动作则把 pluginId/actionId 回传前端复用 runAction。
func (s *FileService) ShellContextMenu(req ShellMenuRequest) ShellMenuResult {
	paths := cleanShellPaths(req.Paths)
	items := buildPluginMenuItems(s.py.plugins())
	hwnd := s.nativeHWND()
	if len(paths) == 0 && len(items) == 0 {
		return ShellMenuResult{}
	}
	oc := showShellMenu(hwnd, req.Ctx.ActiveDir, paths, items)
	if !oc.picked {
		return ShellMenuResult{Error: oc.err}
	}
	if oc.isPlugin {
		return ShellMenuResult{Handled: true, IsPlugin: true, PluginID: oc.pluginID, ActionID: oc.actionID}
	}
	return ShellMenuResult{Handled: true, Error: oc.err}
}
