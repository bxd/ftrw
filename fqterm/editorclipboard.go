package main

import (
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

const editorDropTarget = "editor-attachments"

// EditorClipboardData 把系统剪贴板里可转成附件的两种内容一次读回，避免前端
// 分两次读取期间剪贴板被其它程序换掉。Files 优先于 ImageBase64。
type EditorClipboardData struct {
	Files       []string `json:"files"`
	ImageBase64 string   `json:"imageBase64"`
}

// EditorFilesDropped 是 Wails 原生文件拖放事件的前端载荷。只有落在编辑区
// data-file-drop-target 上的文件才会广播，终端和文件管理器不受影响。
type EditorFilesDropped struct {
	Files []string `json:"files"`
}

// CopyFilesToClipboard 写入标准 CF_HDROP，让文件管理器 Ctrl+C 复制的文件既能
// 贴进本编辑器，也能照常粘到 Explorer 或其它识别文件列表的应用。
func (s *AppService) CopyFilesToClipboard(paths []string) error {
	return copyFilesToClipboard(paths)
}

// ReadEditorClipboard 返回 CF_HDROP 文件列表；没有文件时再尝试把剪贴板图片
// 转成 PNG base64。普通文字不属于附件，前端收到空结果后继续原生文字粘贴。
func (s *AppService) ReadEditorClipboard() (EditorClipboardData, error) {
	data, err := readEditorClipboard()
	if data.Files == nil {
		data.Files = []string{}
	}
	return data, err
}

func editorDropFiles(details application.DropTargetDetails, files []string) []string {
	if details.Attributes["data-file-drop-target"] != editorDropTarget || len(files) == 0 {
		return nil
	}
	return files
}

func (s *AppService) installEditorDropHook() {
	s.win.OnWindowEvent(events.Common.WindowFilesDropped, func(event *application.WindowEvent) {
		details := event.Context().DropTargetDetails()
		if details == nil {
			return
		}
		files := editorDropFiles(*details, event.Context().DroppedFiles())
		if len(files) == 0 {
			return
		}
		s.app.Event.Emit("editor:files-dropped", &EditorFilesDropped{Files: files})
	})
}
