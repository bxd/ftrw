package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
)

// 压缩包虚拟路径（VFS）：让 zip/7z/rar/tar 族像普通文件夹一样浏览与拷出。
//
// 形状：<压缩包真实路径>::<包内路径>，包内路径用反斜杠、空 = 包根。
// 按第一个 "::" 拆分 —— Windows 真实路径不可能含 "::"，无歧义。路径对前端是
// 不透明句柄：ListDir/FileStat/Copy/OpenPath/PeekFile/Preview 六个 API 认它，
// 选择/排序/过滤/复制 UI 原样复用。首版只读：包内写操作与「复制进包」一律拒。
//
// 每次调用现开归档（zip/7z 有中央目录很快；tar/rar 全扫，典型体量可接受），
// 句柄缓存留后续。目录合成在 listVirtualDir：tar/7z 常无显式目录项，从文件
// 路径前缀集合成中间目录。

const vfsSep = "::"

// splitVirtualPath 拆虚拟路径。inner 归一成反斜杠、去首尾分隔符；非虚拟路径 ok=false。
func splitVirtualPath(p string) (arc, inner string, ok bool) {
	i := strings.Index(p, vfsSep)
	if i < 0 {
		return "", "", false
	}
	arc = p[:i]
	inner = strings.ReplaceAll(p[i+len(vfsSep):], "/", `\`)
	inner = strings.Trim(inner, `\`)
	return arc, inner, true
}

func isVirtualPath(p string) bool { return strings.Contains(p, vfsSep) }

func virtualPath(arc, inner string) string { return arc + vfsSep + inner }

// baseName 取包内路径的末段（反斜杠口径，不依赖 OS）。
func baseName(inner string) string {
	if i := strings.LastIndexByte(inner, '\\'); i >= 0 {
		return inner[i+1:]
	}
	return inner
}

// normalizeInner 把包内条目名归一成反斜杠相对路径。含 ".." 段或绝对路径的
// 返回空串 —— zip-slip 护栏在源头就挡掉，列举与解出都见不到恶意条目。
func normalizeInner(name string) string {
	n := strings.ReplaceAll(name, "/", `\`)
	// 首分隔符开头（Unix 绝对路径 / UNC）直接拒：先判再归一，不给 zip-slip 留缝。
	if strings.HasPrefix(n, `\`) {
		return ""
	}
	segs := make([]string, 0, 4)
	for _, s := range strings.Split(n, `\`) {
		if s == "" || s == "." {
			continue
		}
		if s == ".." {
			return ""
		}
		segs = append(segs, s)
	}
	if len(segs) == 0 {
		return ""
	}
	out := strings.Join(segs, `\`)
	if filepath.IsAbs(out) {
		return ""
	}
	return out
}

// decodeArchiveName 修条目名编码：合法 UTF-8 原样保留（兼容不置标志位的
// UTF-8 包）；否则按 GBK 解 —— 中文 Windows 造的压缩包常用 CP936 存文件名
// 且不置 UTF-8 标志，标准库按原始字节读，不修就是乱码。解不出再退回原串。
func decodeArchiveName(raw string) string {
	if utf8.ValidString(raw) {
		return raw
	}
	if s, err := simplifiedchinese.GBK.NewDecoder().String(raw); err == nil {
		return s
	}
	return raw
}

// archiveEntry 是包内一条条目：name 为包内全路径（反斜杠、无首尾分隔符）。
type archiveEntry struct {
	name  string
	size  int64
	mtime time.Time
	isDir bool
}

// archiveReader 是四种格式的统一读面。entries 回全量条目；open 按包内全路径
// 开流。Close 的语义分两档：zip/7z 持有文件句柄要真关；tar/rar 每次调用现开
// 现关、自身 Close 是空操作（open 返回的流负责关底层）。
type archiveReader interface {
	entries() ([]archiveEntry, error)
	open(name string) (io.ReadCloser, error)
	Close() error
}

// openArchive 按扩展名分派。加密/分卷包由各库自然报错上抛。
func openArchive(arc string) (archiveReader, error) {
	ext := strings.ToLower(filepath.Ext(arc))
	switch ext {
	case ".zip":
		return openZipReader(arc)
	case ".7z":
		return open7zReader(arc)
	case ".rar":
		return openRarReader(arc)
	case ".tar", ".tgz", ".tbz2":
		return openTarReader(arc)
	case ".gz", ".bz2":
		// 只认 tar.gz / tar.bz2：独立 .gz/.bz2 是单文件压缩，不是目录形状。
		stem := strings.TrimSuffix(arc, filepath.Ext(arc))
		if strings.EqualFold(filepath.Ext(stem), ".tar") {
			return openTarReader(arc)
		}
		return nil, fmt.Errorf("不支持的压缩格式（仅 tar.gz/tar.bz2，独立 .gz/.bz2 不认）：%s", arc)
	default:
		return nil, fmt.Errorf("不支持的压缩格式：%s", arc)
	}
}

// listVirtualDir 列举包内 inner 的直接子级。深层条目合成出直接子目录项；
// 显式目录项与合成目录按名去重。合成目录没有 mtime（给 0）。
func listVirtualDir(arc, inner string) ([]FileEntry, error) {
	r, err := openArchive(arc)
	if err != nil {
		return nil, fmt.Errorf("无法打开压缩包（%s）：%v", arc, err)
	}
	defer r.Close()
	es, err := r.entries()
	if err != nil {
		return nil, fmt.Errorf("读取压缩包失败（%s）：%v", arc, err)
	}
	prefix := ""
	if inner != "" {
		prefix = inner + `\`
	}
	seen := make(map[string]bool)
	out := make([]FileEntry, 0)
	for _, e := range es {
		if e.name == "" || !strings.HasPrefix(e.name, prefix) {
			continue
		}
		rest := e.name[len(prefix):]
		if rest == "" {
			continue
		}
		if i := strings.IndexByte(rest, '\\'); i >= 0 {
			dirName := rest[:i]
			if seen[dirName] {
				continue
			}
			seen[dirName] = true
			out = append(out, FileEntry{Name: dirName, Path: virtualPath(arc, prefix+dirName), IsDir: true})
			continue
		}
		if seen[rest] {
			continue
		}
		seen[rest] = true
		out = append(out, FileEntry{
			Name:    rest,
			Path:    virtualPath(arc, e.name),
			Size:    e.size,
			ModTime: e.mtime.UnixNano(),
			IsDir:   e.isDir,
		})
	}
	return out, nil
}

// statVirtual 取包内单条信息：精确命中（大小写不敏感）或前缀命中合成目录。
func statVirtual(arc, inner string) (FileEntry, error) {
	if inner == "" {
		return FileEntry{Name: filepath.Base(arc), Path: virtualPath(arc, ""), IsDir: true}, nil
	}
	r, err := openArchive(arc)
	if err != nil {
		return FileEntry{}, fmt.Errorf("无法打开压缩包（%s）：%v", arc, err)
	}
	defer r.Close()
	es, err := r.entries()
	if err != nil {
		return FileEntry{}, fmt.Errorf("读取压缩包失败（%s）：%v", arc, err)
	}
	for _, e := range es {
		if strings.EqualFold(e.name, inner) {
			return FileEntry{
				Name:    baseName(e.name),
				Path:    virtualPath(arc, e.name),
				Size:    e.size,
				ModTime: e.mtime.UnixNano(),
				IsDir:   e.isDir,
			}, nil
		}
	}
	prefix := inner + `\`
	for _, e := range es {
		if strings.HasPrefix(e.name, prefix) {
			return FileEntry{Name: baseName(inner), Path: virtualPath(arc, inner), IsDir: true}, nil
		}
	}
	return FileEntry{}, fmt.Errorf("压缩包内不存在该条目：%s", inner)
}

// extractTo 把虚拟源（单文件或子树）解到 dstDir，保留结构。inner 为空 = 包根
// 整体解出（条目直接落 dstDir）。条目名已在 normalizeInner 过了 zip-slip 闸。
func extractTo(arc, inner, dstDir string) error {
	r, err := openArchive(arc)
	if err != nil {
		return fmt.Errorf("无法打开压缩包（%s）：%v", arc, err)
	}
	defer r.Close()
	es, err := r.entries()
	if err != nil {
		return fmt.Errorf("读取压缩包失败（%s）：%v", arc, err)
	}
	// 精确命中非目录项 → 单文件；否则按前缀收子树。
	var single *archiveEntry
	prefix := ""
	root := dstDir
	if inner != "" {
		prefix = inner + `\`
		root = filepath.Join(dstDir, baseName(inner))
	}
	var subtree []archiveEntry
	for i := range es {
		e := &es[i]
		if inner != "" && strings.EqualFold(e.name, inner) {
			if !e.isDir {
				single = e
			}
			continue
		}
		if strings.HasPrefix(e.name, prefix) {
			subtree = append(subtree, *e)
		}
	}
	if single != nil {
		return extractOne(r, single.name, filepath.Join(dstDir, baseName(single.name)), single.mtime)
	}
	if len(subtree) == 0 {
		return fmt.Errorf("压缩包内不存在该条目：%s", inner)
	}
	for _, e := range subtree {
		dest := filepath.Join(root, e.name[len(prefix):])
		if e.isDir {
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := extractOne(r, e.name, dest, e.mtime); err != nil {
			return err
		}
	}
	return nil
}

// extractOne 解单个条目到 dest（父目录自动建），保留 mtime。
func extractOne(r archiveReader, name, dest string, mtime time.Time) error {
	rc, err := r.open(name)
	if err != nil {
		return err
	}
	defer rc.Close()
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, rc); err != nil {
		return err
	}
	if !mtime.IsZero() {
		_ = os.Chtimes(dest, mtime, mtime)
	}
	return nil
}

// extractTemp 把包内单文件解到临时目录（保留原名，扩展名不丢），供 OpenPath/
// Preview 交系统处理。ftrw-arc-* 由 NewFileService 启动时统一清理。
func extractTemp(arc, inner string) (string, error) {
	if inner == "" {
		return "", fmt.Errorf("包根不是文件，无法打开：%s", arc)
	}
	r, err := openArchive(arc)
	if err != nil {
		return "", fmt.Errorf("无法打开压缩包（%s）：%v", arc, err)
	}
	defer r.Close()
	rc, err := r.open(inner)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	dir, err := os.MkdirTemp("", "ftrw-arc-")
	if err != nil {
		return "", err
	}
	dest := filepath.Join(dir, baseName(inner))
	f, err := os.Create(dest)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := io.Copy(f, rc); err != nil {
		return "", err
	}
	return dest, nil
}

// openVirtualEntry 开包内条目流（PeekFile 用）。vfsEntry 把条目流与归档句柄
// 绑在一起关：zip/7z 两层都要关；tar/rar 的流自己就带底层关、arc.Close 空转。
func openVirtualEntry(arc, inner string) (io.ReadCloser, error) {
	r, err := openArchive(arc)
	if err != nil {
		return nil, fmt.Errorf("无法打开压缩包（%s）：%v", arc, err)
	}
	rc, err := r.open(inner)
	if err != nil {
		r.Close()
		return nil, err
	}
	return &vfsEntry{ReadCloser: rc, arc: r}, nil
}

type vfsEntry struct {
	io.ReadCloser
	arc archiveReader
}

func (v *vfsEntry) Close() error {
	err := v.ReadCloser.Close()
	if e := v.arc.Close(); e != nil && err == nil {
		err = e
	}
	return err
}

// rejectVirtual 是写操作的统一闸：任一路径落在包内就拒（首版只读）。
func rejectVirtual(paths ...string) error {
	for _, p := range paths {
		if isVirtualPath(p) {
			return fmt.Errorf("压缩包内暂只读（可浏览 / 拷出）：%s", p)
		}
	}
	return nil
}

// cleanArcTemp 清上一轮运行留下的 ftrw-arc-* 临时目录（best-effort）：
// 解出预览/打开的临时文件不该在系统 TEMP 里越积越多。
func cleanArcTemp() {
	tmp := os.TempDir()
	entries, err := os.ReadDir(tmp)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "ftrw-arc-") {
			_ = os.RemoveAll(filepath.Join(tmp, e.Name()))
		}
	}
}
