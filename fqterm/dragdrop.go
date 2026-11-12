package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// dragdrop.go：把一批文件/目录拖给外部应用（Explorer 等）的跨平台入口。
// 原生 OLE DoDragDrop 在 dragdrop_windows.go；非 Windows 报不支持。
// 虚拟路径（压缩包内）先解到临时目录再拖，与拷出语义一致；临时目录沿用
// ftrw-arc-* 约定，下次启动统一清理。包根不整包拖（进包选条目或 F5 拷出）。

// DragPaths 发起出站拖拽：阻塞到用户放下/取消（DoDragDrop 模态循环）。
func (s *FileService) DragPaths(paths []string) error {
	real, err := materializeDragPaths(paths)
	if err != nil {
		return err
	}
	if len(real) == 0 {
		return fmt.Errorf("没有可拖拽的目标")
	}
	return dragPathsNative(real)
}

// materializeDragPaths 把虚拟源解出、真实源验存在，回最终要拖的全路径列表。
func materializeDragPaths(paths []string) ([]string, error) {
	var out []string
	var tmp string
	for _, p := range paths {
		if arc, inner, ok := splitVirtualPath(p); ok {
			if inner == "" {
				return nil, fmt.Errorf("包根不整包拖拽，请进包选条目或 F5 拷出：%s", arc)
			}
			if tmp == "" {
				t, err := os.MkdirTemp("", "ftrw-arc-")
				if err != nil {
					return nil, err
				}
				tmp = t
			}
			if err := extractTo(arc, inner, tmp); err != nil {
				return nil, err
			}
			out = append(out, filepath.Join(tmp, baseName(inner)))
			continue
		}
		if _, err := os.Stat(p); err != nil {
			continue // 已不存在的跳过
		}
		out = append(out, p)
	}
	return out, nil
}
