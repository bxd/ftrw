package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// DownloadEntry 是下载管理器的一行：一个自动建立的下载目录，不暴露目录内部导航。
// Size / FileCount 统计目录里的直接文件，供列表判断内容量；预览另走 Preview。
type DownloadEntry struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	FileCount int    `json:"fileCount"`
	ModTime   int64  `json:"mtime"` // UnixNano
}

// DownloadPreviewItem 与 PreviewItem 同形，独立命名避免绑定生成器把两个服务的模型撞名。
type DownloadPreviewItem struct {
	URL  string `json:"url"`
	Kind string `json:"kind"`
	Name string `json:"name"`
	Path string `json:"path,omitempty"`
}

// DownloadManagerService 只管理 ZMODEM 自动建立的下载目录。路径句柄由前端原样传回，
// 但每次写操作都在后端重新校验，不能把下载根目录本身或深层文件交给回收站。
type DownloadManagerService struct {
	store   *Store
	files   *FileService
	recycle func([]string) error
}

func NewDownloadManagerService(store *Store, files *FileService) *DownloadManagerService {
	return &DownloadManagerService{store: store, files: files, recycle: files.Recycle}
}

// RootDir 返回当前偏好里的下载根目录；目录不存在时建立，首次使用也能看到空列表。
func (s *DownloadManagerService) RootDir() (string, error) {
	if s == nil || s.store == nil {
		return "", errors.New("下载管理器尚未初始化")
	}
	root, err := filepath.Abs(s.store.DownloadDir())
	if err != nil {
		return "", fmt.Errorf("解析下载目录失败：%w", err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("建立下载目录失败（%s）：%w", root, err)
	}
	fi, err := os.Stat(root)
	if err != nil {
		return "", fmt.Errorf("读取下载目录失败（%s）：%w", root, err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("下载路径不是目录：%s", root)
	}
	return root, nil
}

// List 只列直接子目录，按修改时间从新到旧；根目录里的散文件和子目录内部都不出现。
func (s *DownloadManagerService) List() ([]DownloadEntry, error) {
	root, err := s.RootDir()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("读取下载目录失败（%s）：%w", root, err)
	}
	out := make([]DownloadEntry, 0, len(raw))
	for _, d := range raw {
		if !d.IsDir() {
			continue
		}
		path := filepath.Join(root, d.Name())
		fi, err := os.Lstat(path)
		if err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			continue
		}
		children, err := os.ReadDir(path)
		if err != nil {
			continue
		}
		entry := DownloadEntry{Name: d.Name(), Path: path, ModTime: fi.ModTime().UnixNano()}
		for _, child := range children {
			if child.IsDir() {
				continue
			}
			info, err := child.Info()
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			entry.FileCount++
			entry.Size += info.Size()
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ModTime != out[j].ModTime {
			return out[i].ModTime > out[j].ModTime
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

// Delete 把整个下载目录移入回收站。非 Windows 平台沿用 FileService 的降级行为（永久删除）。
func (s *DownloadManagerService) Delete(path string) error {
	return s.DeleteMany([]string{path})
}

// DeleteMany 批量移入回收站。先校验全部目标，再发起一次删除，避免误带一个越界路径。
func (s *DownloadManagerService) DeleteMany(paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	dirs := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		dir, err := s.downloadDir(path)
		if err != nil {
			return fmt.Errorf("批量删除校验失败：%w", err)
		}
		if _, ok := seen[dir]; ok {
			continue
		}
		seen[dir] = struct{}{}
		dirs = append(dirs, dir)
	}
	if s.recycle == nil {
		return errors.New("回收站功能尚未初始化")
	}
	if err := s.recycle(dirs); err != nil {
		return fmt.Errorf("删除 %d 个下载项失败：%w", len(dirs), err)
	}
	return nil
}

// Preview 重建下载项的正文/附件顺序。拆分后的原始容器仍在磁盘上，但不重复加入预览；
// 普通下载没有 *_正文.txt，于是原文件就是唯一预览项。
func (s *DownloadManagerService) Preview(path string) ([]DownloadPreviewItem, error) {
	dir, err := s.downloadDir(path)
	if err != nil {
		return nil, err
	}
	if s.files == nil {
		return nil, errors.New("文件预览服务尚未初始化")
	}
	files, err := readDownloadFiles(dir)
	if err != nil {
		return nil, err
	}
	files = orderDownloadFiles(files)
	containers := splitContainerFiles(dir, files)
	items := make([]DownloadPreviewItem, 0, len(files)-len(containers))
	for _, file := range files {
		if containers[file.Name()] {
			continue
		}
		path := filepath.Join(dir, file.Name())
		ref, err := s.files.Preview(path)
		if err != nil || ref.URL == "" {
			continue // 单个附件失效不应挡住正文和其余附件
		}
		name := file.Name()
		if strings.HasSuffix(name, "_正文.txt") {
			name = "正文"
		}
		items = append(items, DownloadPreviewItem{
			URL: ref.URL, Kind: downloadPreviewKind(ref.Kind), Name: name, Path: path,
		})
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("下载项里没有可预览的文件：%s", filepath.Base(dir))
	}
	return items, nil
}

// downloadDir 强制目标为下载根目录的直接子目录。filepath.Rel 同时拦住外部路径、
// 根目录本身、.. 穿越和深层文件；Lstat 再排除符号链接/目录联接指向别处的情况。
func (s *DownloadManagerService) downloadDir(path string) (string, error) {
	root, err := s.RootDir()
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(path) == "" {
		return "", errors.New("下载项路径为空")
	}
	dir, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("解析下载项路径失败：%w", err)
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == "." || filepath.IsAbs(rel) || filepath.Dir(rel) != "." {
		return "", fmt.Errorf("只能操作下载根目录的直接子目录：%s", path)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return "", fmt.Errorf("下载项不存在或无法读取（%s）：%w", filepath.Base(dir), err)
	}
	if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("下载项不是普通目录：%s", path)
	}
	return dir, nil
}

func readDownloadFiles(dir string) ([]os.FileInfo, error) {
	raw, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("读取下载项失败（%s）：%w", filepath.Base(dir), err)
	}
	files := make([]os.FileInfo, 0, len(raw))
	for _, entry := range raw {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		files = append(files, info)
	}
	return files, nil
}

func orderDownloadFiles(files []os.FileInfo) []os.FileInfo {
	sort.SliceStable(files, func(i, j int) bool {
		ib := strings.HasSuffix(files[i].Name(), "_正文.txt")
		jb := strings.HasSuffix(files[j].Name(), "_正文.txt")
		if ib != jb {
			return ib
		}
		return strings.ToLower(files[i].Name()) < strings.ToLower(files[j].Name())
	})
	return files
}

// splitContainerFiles 用现有附件记录解析器确认原始容器：普通附件即使解析失败，
// 也不会被误排除。只检查与正文同主名的候选，避免逐个读取无关的大附件。
func splitContainerFiles(dir string, files []os.FileInfo) map[string]bool {
	out := make(map[string]bool)
	for _, body := range files {
		name := body.Name()
		if !strings.HasSuffix(name, "_正文.txt") {
			continue
		}
		stem := strings.TrimSuffix(name, "_正文.txt")
		for _, candidate := range files {
			if candidate.Name() == name || !strings.EqualFold(strings.TrimSuffix(candidate.Name(), filepath.Ext(candidate.Name())), stem) {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, candidate.Name()))
			if err != nil {
				continue
			}
			if _, _, ok := parseArticleAttachments(data, 0); ok {
				out[candidate.Name()] = true
			}
		}
	}
	return out
}

func downloadPreviewKind(kind string) string {
	switch kind {
	case "image", "audio", "video", "pdf":
		return kind
	case "text", "html":
		return "text" // 本地 HTML 按源码显示，不在带后端绑定的窗口里执行
	default:
		return "file"
	}
}
