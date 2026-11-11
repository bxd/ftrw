package main

import "errors"

// 文件管理器的 Python 扩展层契约（launcher 分支）。本轮只钉死接口与目录/协议约定，
// 不接运行时：Plugins 恒返回空，RunPlugin 恒报错。UI 与服务层现在就按这套形状挂，
// 未来接上 Python 只替换下面两个方法的实现，不改签名、不改前端。
//
// 运行时计划（后续实现）：
//   - 起 `python -u` 子进程，用 stdio 行分隔 JSON-RPC 通信（一行一条请求/响应）。
//   - 插件发现目录：%APPDATA%\FTRW\plugins\*.py，每个 .py 暴露约定的元数据与 run()。
//   - 插件可见的最小 API（listDir / read / write / stat / prompt）全部映射回本包的
//     FileService 方法，Python 侧不直接碰文件系统，权限与路径校验集中在 Go 一侧。

// PluginTool 是一条扩展工具对文件管理器暴露的命令入口。
type PluginTool struct {
	ID             string `json:"id"`
	Title          string `json:"title"`
	Kind           string `json:"kind"` // archive / rename / checksum / thumbnail 等类别，供 UI 分组
	NeedsSelection bool   `json:"needsSelection"`
}

// PluginContext 是触发插件时随行的字段：活动目录、选中项、对侧目录（复制/移动的落点）。
type PluginContext struct {
	ActiveDir string   `json:"activeDir"`
	Selected  []string `json:"selected"`
	TargetDir string   `json:"targetDir"`
}

// PluginResult 是插件回传的结果，本轮只有文本消息。
type PluginResult struct {
	Message string `json:"message"`
}

// Plugins 列出可用扩展工具。运行时尚未接入，恒空。
func (s *FileService) Plugins() []PluginTool {
	return []PluginTool{}
}

// RunPlugin 触发一条扩展工具。本轮运行时未接入，固定拒绝。
func (s *FileService) RunPlugin(toolID string, ctx PluginContext) (PluginResult, error) {
	return PluginResult{}, errors.New("Python 扩展运行时尚未接入")
}
