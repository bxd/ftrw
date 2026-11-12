package main

import (
	"archive/tar"
	"archive/zip"
	"compress/bzip2"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/bodgit/sevenzip"
	"github.com/nwaples/rardecode/v2"
)

// 四种归档格式的 archiveReader 实现（见 filevfs.go 的接口约定）。
// zip/7z 有中央目录：OpenReader 一次、entries/open 都查内存表，句柄随 Close 关。
// tar/rar 是流式：entries 全扫一遍、open 重扫到目标，每次现开现关，
// 自身 Close 空操作（底层由 open 返回的流负责关）。

/* ---------- zip（标准库） ---------- */

type zipReader struct{ r *zip.ReadCloser }

func openZipReader(arc string) (archiveReader, error) {
	r, err := zip.OpenReader(arc)
	if err != nil {
		return nil, err
	}
	return &zipReader{r: r}, nil
}

func (z *zipReader) Close() error { return z.r.Close() }

func (z *zipReader) entries() ([]archiveEntry, error) {
	out := make([]archiveEntry, 0, len(z.r.File))
	for _, f := range z.r.File {
		name := normalizeInner(decodeArchiveName(f.Name))
		if name == "" {
			continue
		}
		out = append(out, archiveEntry{
			name:  name,
			size:  int64(f.UncompressedSize64),
			mtime: f.Modified,
			isDir: f.FileInfo().IsDir(),
		})
	}
	return out, nil
}

func (z *zipReader) open(name string) (io.ReadCloser, error) {
	for _, f := range z.r.File {
		if strings.EqualFold(normalizeInner(decodeArchiveName(f.Name)), name) {
			return f.Open()
		}
	}
	return nil, fmt.Errorf("压缩包内不存在该条目：%s", name)
}

/* ---------- 7z（bodgit/sevenzip，纯 Go） ---------- */

type sevenZipReader struct{ r *sevenzip.ReadCloser }

func open7zReader(arc string) (archiveReader, error) {
	r, err := sevenzip.OpenReader(arc)
	if err != nil {
		return nil, err
	}
	return &sevenZipReader{r: r}, nil
}

func (s *sevenZipReader) Close() error { return s.r.Close() }

func (s *sevenZipReader) entries() ([]archiveEntry, error) {
	out := make([]archiveEntry, 0, len(s.r.File))
	for _, f := range s.r.File {
		name := normalizeInner(f.Name)
		if name == "" {
			continue
		}
		out = append(out, archiveEntry{
			name:  name,
			size:  int64(f.UncompressedSize),
			mtime: f.Modified,
			isDir: f.FileHeader.FileInfo().IsDir() || strings.HasSuffix(f.Name, "/"),
		})
	}
	return out, nil
}

func (s *sevenZipReader) open(name string) (io.ReadCloser, error) {
	for _, f := range s.r.File {
		if strings.EqualFold(normalizeInner(f.Name), name) {
			return f.Open()
		}
	}
	return nil, fmt.Errorf("压缩包内不存在该条目：%s", name)
}

/* ---------- rar（nwaples/rardecode/v2，纯 Go，只读） ---------- */

// rarReader 不持句柄：RAR 是流式格式，entries/open 各自现开现扫。
// 加密包在 OpenReader/Next 时报 ErrArchiveEncrypted 一类，自然上抛。
type rarReader struct{ arc string }

func openRarReader(arc string) (archiveReader, error) {
	// 先探一次：坏包/加密包在这里就报错，不等到列举才炸。
	r, err := rardecode.OpenReader(arc)
	if err != nil {
		return nil, err
	}
	_ = r.Close()
	return &rarReader{arc: arc}, nil
}

func (rr *rarReader) Close() error { return nil }

func (rr *rarReader) entries() ([]archiveEntry, error) {
	r, err := rardecode.OpenReader(rr.arc)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var out []archiveEntry
	for {
		h, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		name := normalizeInner(decodeArchiveName(h.Name))
		if name == "" {
			continue
		}
		out = append(out, archiveEntry{name: name, size: h.UnPackedSize, mtime: h.ModificationTime, isDir: h.IsDir})
	}
	return out, nil
}

func (rr *rarReader) open(name string) (io.ReadCloser, error) {
	r, err := rardecode.OpenReader(rr.arc)
	if err != nil {
		return nil, err
	}
	for {
		h, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			_ = r.Close()
			return nil, err
		}
		if strings.EqualFold(normalizeInner(decodeArchiveName(h.Name)), name) {
			// Reader 本身就是当前文件的流；调用方 Close 即关整个归档。
			return r, nil
		}
	}
	_ = r.Close()
	return nil, fmt.Errorf("压缩包内不存在该条目：%s", name)
}

/* ---------- tar / tar.gz / tgz / tar.bz2 / tbz2（标准库） ---------- */

type tarReader struct{ arc string }

func openTarReader(arc string) (archiveReader, error) {
	// 探一次：坏包/压缩层损坏在这里报错。
	rc, err := tarOpenStream(arc)
	if err != nil {
		return nil, err
	}
	_ = rc.Close()
	return &tarReader{arc: arc}, nil
}

func (tr *tarReader) Close() error { return nil }

// tarOpenStream 开文件并按扩展名套解压层。multiCloser 逆序关各层。
func tarOpenStream(arc string) (io.ReadCloser, error) {
	f, err := os.Open(arc)
	if err != nil {
		return nil, err
	}
	ext := strings.ToLower(filepath.Ext(arc))
	var r io.Reader = f
	closers := []io.Closer{f}
	switch ext {
	case ".gz", ".tgz":
		gz, err := gzip.NewReader(f)
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		r = gz
		closers = append(closers, gz)
	case ".bz2", ".tbz2":
		r = bzip2.NewReader(f) // bzip2 解压层无 Close，关文件即可
	}
	return &multiCloser{Reader: r, closers: closers}, nil
}

type multiCloser struct {
	io.Reader
	closers []io.Closer
}

func (m *multiCloser) Close() error {
	var first error
	for i := len(m.closers) - 1; i >= 0; i-- {
		if err := m.closers[i].Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (tr *tarReader) entries() ([]archiveEntry, error) {
	rc, err := tarOpenStream(tr.arc)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	t := tar.NewReader(rc)
	var out []archiveEntry
	for {
		h, err := t.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		name := normalizeInner(decodeArchiveName(h.Name))
		if name == "" {
			continue
		}
		out = append(out, archiveEntry{name: name, size: h.Size, mtime: h.ModTime, isDir: h.Typeflag == tar.TypeDir})
	}
	return out, nil
}

func (tr *tarReader) open(name string) (io.ReadCloser, error) {
	rc, err := tarOpenStream(tr.arc)
	if err != nil {
		return nil, err
	}
	t := tar.NewReader(rc)
	for {
		h, err := t.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			_ = rc.Close()
			return nil, err
		}
		if strings.EqualFold(normalizeInner(decodeArchiveName(h.Name)), name) {
			// tar.Reader 已定位到当前文件数据；包一层让 Close 连带关底层流。
			return &multiCloser{Reader: t, closers: []io.Closer{rc}}, nil
		}
	}
	_ = rc.Close()
	return nil, fmt.Errorf("压缩包内不存在该条目：%s", name)
}
