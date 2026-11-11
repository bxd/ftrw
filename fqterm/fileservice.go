package main

import (
	"crypto/rand"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// FileService 支撑文件管理器标签（launcher 分支）：目录列举、驱动器、基本文件操作。
// 全部走 Call.ByName('FileService.*') 暴露给前端，参数/返回只用本包 models 里的类型，
// 不引用其它服务的类型（否则绑定生成器会拖出 model 重名，见 launcher 的同款约定）。
// 平台专属那部分（驱动器枚举、隐藏属性判定）拆到 fileservice_windows.go /
// fileservice_other.go，这里只留跨平台骨架。
type FileService struct {
	// sessionSecret 是本进程随机生成的 HMAC 密钥（仅内存），用于给本机文件 URL 签名。
	sessionSecret []byte
	// py 是 Python 插件运行时；无 Python 时 Available=false，插件档自动禁用。
	py *pyRuntime
	// hwndProvider 由 main 在建好主窗后注入（见 shellmenu.go 的 Bind），供原生右键菜单
	// 取主窗 HWND 作为 InvokeCommand 的父窗；非 Windows 或注入前为 nil。
	hwndProvider func() uintptr
}

func NewFileService() *FileService {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		// crypto/rand 正常不会失败；真失败了给个定长兜底，签名照样自洽（只是弱）。
		copy(secret, []byte("ftrw-fallback-session-secret-please"))
	}
	return &FileService{sessionSecret: secret, py: newPyRuntime()}
}

// ListDrives 给驱动器栏。平台实现在 fileservice_*_.go。
func (s *FileService) ListDrives() []DriveInfo { return listDrives() }

// HomeDir 给用户目录，文件管理器与前端「回首页」共用一个真相来源。
func (s *FileService) HomeDir() string {
	home, _ := os.UserHomeDir()
	return home
}

// normalizeDir 把空路径兜底成用户主目录：文件管理器不该因为一个空串就报错开不了张。
func normalizeDir(path string) string {
	if strings.TrimSpace(path) == "" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
		return "."
	}
	return path
}

// ListDir 列举一个目录。不排序（前端按列排）、不跳过隐藏项（hidden 字段供前端过滤），
// 单条 stat 失败只是少一项、不影响整目录。"." 与 ".." 由前端导航行合成，这里只出真实条目。
func (s *FileService) ListDir(path string) ([]FileEntry, error) {
	dir := normalizeDir(path)
	raw, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("读取目录失败（%s）：%v", dir, err)
	}
	out := make([]FileEntry, 0, len(raw))
	for _, d := range raw {
		fi, err := d.Info()
		if err != nil {
			continue
		}
		full := filepath.Join(dir, d.Name())
		out = append(out, FileEntry{
			Name:    d.Name(),
			Path:    full,
			Size:    fi.Size(),
			ModTime: fi.ModTime().UnixNano(),
			IsDir:   d.IsDir(),
			Hidden:  isHiddenFile(full, d.Name()),
		})
	}
	return out, nil
}

// FileStat 取单个条目的信息（前端刷新某一行 / Enter 前进目录前判类型用）。
func (s *FileService) FileStat(path string) (FileEntry, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return FileEntry{}, fmt.Errorf("无法获取信息（%s）：%v", path, err)
	}
	name := filepath.Base(path)
	return FileEntry{
		Name:    name,
		Path:    path,
		Size:    fi.Size(),
		ModTime: fi.ModTime().UnixNano(),
		IsDir:   fi.IsDir(),
		Hidden:  isHiddenFile(path, name),
	}, nil
}

// MakeDir 新建目录（F7）。父级必须已存在，否则 os.Mkdir 直接报错回传，不悄悄 MkdirAll。
func (s *FileService) MakeDir(path string) error {
	if err := os.Mkdir(path, 0o755); err != nil {
		return fmt.Errorf("新建目录失败（%s）：%v", path, err)
	}
	return nil
}

// Rename 重命名或同卷移动（F2）。newPath 是带新名的全路径。
func (s *FileService) Rename(oldPath, newPath string) error {
	if err := os.Rename(oldPath, newPath); err != nil {
		return fmt.Errorf("重命名失败（%s → %s）：%v", oldPath, filepath.Base(newPath), err)
	}
	return nil
}

// Delete 永久删除（Shift+Del / F8，不走回收站）。逐个 RemoveAll，任一失败即中断并把已删的留在原地。
func (s *FileService) Delete(paths []string) error {
	for _, p := range paths {
		if err := os.RemoveAll(p); err != nil {
			return fmt.Errorf("删除失败（%s）：%v", p, err)
		}
	}
	return nil
}

// Recycle 删除到回收站（Del）。平台实现在 fileservice_*_.go：Windows 走 shell 的
// FOF_ALLOWUNDO，非 Windows 平台无标准回收站，退化为永久删除。逐个处理，失败即中断。
func (s *FileService) Recycle(paths []string) error {
	for _, p := range paths {
		if err := recyclePath(p); err != nil {
			return fmt.Errorf("移入回收站失败（%s）：%v", p, err)
		}
	}
	return nil
}

// Copy 把 src 里每条复制到 dstDir（F5）。目标同名且源就是目标时拒绝，避免自己套自己。
func (s *FileService) Copy(src []string, dstDir string) error {
	for _, from := range src {
		to := filepath.Join(dstDir, filepath.Base(from))
		if samePath(from, to) {
			return fmt.Errorf("源与目标为同一处，无法复制：%s", from)
		}
		if err := copyTree(from, to); err != nil {
			return fmt.Errorf("复制失败（%s）：%v", from, err)
		}
	}
	return nil
}

// Move 把 src 里每条移到 dstDir（F6）。先试 os.Rename（同卷瞬时完成），
// 失败（跨卷 EXDEV 等）退化为复制+删源。
func (s *FileService) Move(src []string, dstDir string) error {
	for _, from := range src {
		to := filepath.Join(dstDir, filepath.Base(from))
		if samePath(from, to) {
			continue
		}
		if err := os.Rename(from, to); err != nil {
			if err := copyTree(from, to); err != nil {
				return fmt.Errorf("移动失败（%s）：%v", from, err)
			}
			if err := os.RemoveAll(from); err != nil {
				return fmt.Errorf("已复制到 %s 但删除源失败（%s）：%v", to, from, err)
			}
		}
	}
	return nil
}

// OpenPath 用系统默认方式打开一条路径（Enter）：直接复用启动器的 ShellExecuteW 通路，
// 参数、工作目录、关联处理程序全部由 shell 负责，不自己起进程。
func (s *FileService) OpenPath(path string) error {
	return launcherShellExecute(path, "")
}

// PeekFile 读文件开头若干字节做文本预览（F3 内置查看器）：默认 64KB、上限 1MB。
// 裁掉尾部半个多字节字符避免前端乱码；二进制不判型、原样给（观感由前端兜）。
func (s *FileService) PeekFile(path string, maxBytes int) (string, error) {
	if maxBytes <= 0 || maxBytes > 1<<20 {
		maxBytes = 64 << 10
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("打开文件失败（%s）：%v", path, err)
	}
	defer f.Close()
	buf := make([]byte, maxBytes)
	n, _ := io.ReadFull(f, buf) // EOF / ErrUnexpectedEOF 都算读到了开头那截
	buf = buf[:n]
	for n > 0 && !utf8.Valid(buf[:n]) {
		n--
	}
	return string(buf[:n]), nil
}

// copyTree 递归复制一个文件或目录树，保留目录结构与文件权限位、mtime。
func copyTree(from, to string) error {
	fi, err := os.Lstat(from)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		if err := os.MkdirAll(to, 0o755); err != nil {
			return err
		}
		children, err := os.ReadDir(from)
		if err != nil {
			return err
		}
		for _, c := range children {
			if err := copyTree(filepath.Join(from, c.Name()), filepath.Join(to, c.Name())); err != nil {
				return err
			}
		}
		return nil
	}
	return copyFile(from, to, fi)
}

func copyFile(from, to string, fi os.FileInfo) error {
	src, err := os.Open(from)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(to, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, fi.Mode().Perm())
	if err != nil {
		return err
	}
	defer dst.Close()
	if _, err := io.Copy(dst, src); err != nil {
		return err
	}
	_ = os.Chtimes(to, fi.ModTime(), fi.ModTime())
	return nil
}

/* ---------- 预览（Ctrl+Q）：分类 + 签名 URL ---------- */

// PreviewRef 是前端渲染一条本地文件所需的元信息。
type PreviewRef struct {
	URL        string `json:"url"`        // 已签名的 /__ftrwfile 地址（媒体/img/iframe 直接用）
	Kind       string `json:"kind"`       // image / text / html / audio / video / pdf / other
	MimeType   string `json:"mimeType"`   // 供前端参考（可空）
	NeedPlugin bool   `json:"needPlugin"` // kind=other 时置真：交给 Python 插件认领转换
	Size       int64  `json:"size"`
}

// previewKindByExt：扩展名（小写、含点）→ 预览种类。不在表内的落 other，交给插件档。
var previewKindByExt = map[string]string{
	".png": "image", ".jpg": "image", ".jpeg": "image", ".gif": "image", ".webp": "image",
	".bmp": "image", ".dib": "image", ".ico": "image", ".avif": "image", ".svg": "image", ".tif": "image", ".tiff": "image", ".jfif": "image",
	".mp3": "audio", ".wav": "audio", ".ogg": "audio", ".oga": "audio", ".m4a": "audio", ".aac": "audio", ".flac": "audio", ".opus": "audio", ".wma": "audio",
	".mp4": "video", ".webm": "video", ".mov": "video", ".m4v": "video", ".mkv": "video", ".avi": "video", ".wmv": "video", ".mpg": "video", ".mpeg": "video",
	".html": "html", ".htm": "html", ".xhtml": "html", ".mhtml": "html",
	".pdf": "pdf",
	".txt": "text", ".md": "text", ".markdown": "text", ".log": "text", ".json": "text", ".csv": "text", ".tsv": "text",
	".ini": "text", ".conf": "text", ".cfg": "text", ".yaml": "text", ".yml": "text", ".xml": "text", ".toml": "text",
	".go": "text", ".py": "text", ".js": "text", ".ts": "text", ".jsx": "text", ".tsx": "text", ".vue": "text", ".c": "text", ".h": "text",
	".cpp": "text", ".hpp": "text", ".rs": "text", ".java": "text", ".rb": "text", ".php": "text", ".sh": "text", ".bat": "text", ".ps1": "text",
	".css": "text", ".sql": "text", ".lua": "text", ".r": "text", ".swift": "text", ".kt": "text", ".gradle": "text",
}

func previewKindFor(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	if k, ok := previewKindByExt[ext]; ok {
		return k
	}
	// 点开头且无扩展（如 Makefile）当作文本兜底，其余交插件。
	if ext == "" {
		return "text"
	}
	return "other"
}

// Preview 校验文件存在并给出渲染元信息 + 签名 URL。未知扩展名不报错，只置 NeedPlugin。
func (s *FileService) Preview(path string) (PreviewRef, error) {
	abs := canonicalPath(path)
	fi, err := os.Stat(abs)
	if err != nil {
		return PreviewRef{}, fmt.Errorf("无法预览（%s）：%v", path, err)
	}
	if fi.IsDir() {
		return PreviewRef{}, fmt.Errorf("目录不可预览：%s", path)
	}
	kind := previewKindFor(abs)
	ref := PreviewRef{
		URL:        s.buildFileURL(abs),
		Kind:       kind,
		MimeType:   mime.TypeByExtension(strings.ToLower(filepath.Ext(abs))),
		NeedPlugin: kind == "other",
		Size:       fi.Size(),
	}
	return ref, nil
}

// SignPath 给一条本机路径签发可渲染 URL（插件转换产物用）；校验存在且为普通文件。
func (s *FileService) SignPath(path string) (string, error) {
	abs := canonicalPath(path)
	fi, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("签名失败（%s）：%v", path, err)
	}
	if fi.IsDir() {
		return "", fmt.Errorf("不能签目录：%s", path)
	}
	return s.buildFileURL(abs), nil
}

// samePath 判两条路径是否指向同一处：取绝对路径后 EqualFold（NTFS 不区分大小写；
// 类 Unix 上大小写不敏感只会把「换个大小写同名」误判成同源，代价只是少一次自拷贝）。
func samePath(a, b string) bool {
	aa, err1 := filepath.Abs(filepath.Clean(a))
	bb, err2 := filepath.Abs(filepath.Clean(b))
	if err1 != nil || err2 != nil {
		return a == b
	}
	return strings.EqualFold(aa, bb)
}
