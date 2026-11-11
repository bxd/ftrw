package main

// 文件管理器的 Python 扩展层契约（launcher 分支）。类型与目录/协议约定留在这里；
// 真正的运行时（探测 Python、发现插件、每插件 venv、JSON-RPC worker）在 pyruntime.go。
//
// 插件布局：<程序目录(exe 所在)>\plugins\<name>\
//   - plugin.json：{"id","title","extensions":[...],"entry":"main.py","func":"preview","actionFunc":"action","actions":[{"id","title"}],"requirements":"requirements.txt"}
//   - main.py：def preview(path, ctx) -> {"kind","path"|"html"|"text","title"}（Ctrl+Q 预览）
//             def action(name, path, ctx) -> {"message":..}（解压/压缩等，菜单触发）
//   - 产物（转成的 png/pdf/html 等）写到 <插件目录>/_work/ 并回传 path，Go 会签成可渲染 URL。

// PluginTool 是一条扩展工具对文件管理器暴露的命令入口。
type PluginTool struct {
	ID             string         `json:"id"`
	Title          string         `json:"title"`
	Kind           string         `json:"kind"` // preview 等类别，供 UI 分组
	NeedsSelection bool           `json:"needsSelection"`
	Extensions     []string       `json:"extensions"` // 该插件认领的扩展名（小写含点）
	Actions        []PluginAction `json:"actions"`    // 有副作用的动作（解压/压缩等），列在「扩展」菜单
}

// PluginAction 是插件声明的一条动作，前端点击后携当前项 + 选中集 + 对侧目录触发。
type PluginAction struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// PluginContext 是触发插件时随行的字段：活动目录、选中项、对侧目录（复制/移动的落点）。
type PluginContext struct {
	ActiveDir string   `json:"activeDir"`
	Selected  []string `json:"selected"`
	TargetDir string   `json:"targetDir"`
}

// PluginResult 是插件回传的文本结果（菜单类触发用；预览走 PluginArtifact）。
type PluginResult struct {
	Message string `json:"message"`
}

// ShellMenuRequest 是原生右键菜单一次弹出的入参：目标路径集（绝对、去重、限长）随行 ctx。
// ctx 供菜单顶部的 FTRW 插件动作段使用（activeDir/selected/targetDir）。
type ShellMenuRequest struct {
	Paths []string      `json:"paths"`
	Ctx   PluginContext `json:"ctx"`
}

// ShellMenuResult 是原生右键菜单一次选择的结果。handled=false 表示未弹/失败/点了空白，
// 前端不动作；isPlugin=true 时前端据 pluginId/actionId 复用既有 runAction。
type ShellMenuResult struct {
	Handled  bool   `json:"handled"`
	PluginID string `json:"pluginId"`
	ActionID string `json:"actionId"`
	IsPlugin bool   `json:"isPlugin"`
	Error    string `json:"error"` // 仅供前端状态栏提示，空表示无
}

// Plugins 列出可用扩展工具（无 Python 时返回空，前端不显示扩展但原生预览照常）。
func (s *FileService) Plugins() []PluginTool {
	return s.py.plugins()
}

// PythonAvailable 供前端提示「未检测到 Python，插件预览不可用」。
func (s *FileService) PythonAvailable() bool {
	return s.py.probe()
}

// PluginsDir 回插件目录的绝对路径（程序目录\plugins），供前端把真实位置显示给用户。
func (s *FileService) PluginsDir() string {
	return s.py.pluginsDir()
}

// PluginPreview 用指定插件把 path 转成可渲染产物；产物文件已签成 URL。
func (s *FileService) PluginPreview(pluginID string, path string, ctx PluginContext) (PluginArtifact, error) {
	return s.py.preview(s, pluginID, path, ctx)
}

// RunPluginAction 触发插件的一条有副作用动作（如解压/压缩）。path 为当前光标项（可空）。
func (s *FileService) RunPluginAction(pluginID, action, path string, ctx PluginContext) (PluginResult, error) {
	return s.py.runAction(pluginID, action, path, ctx)
}
