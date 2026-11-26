//go:build windows

package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestHDropRoundTripUnicodePaths(t *testing.T) {
	want := []string{`C:\临时目录\文章.txt`, `D:\second.bin`}
	if got := parseHDrop(buildHDrop(want)); !reflect.DeepEqual(got, want) {
		t.Fatalf("CF_HDROP 解析 = %q, want %q", got, want)
	}
}

func TestHDropRoundTripANSIPaths(t *testing.T) {
	want := []string{`C:\first.txt`, `D:\second.bin`}
	raw := make([]byte, 20)
	binary.LittleEndian.PutUint32(raw[0:4], 20)
	for _, path := range want {
		raw = append(raw, path...)
		raw = append(raw, 0)
	}
	raw = append(raw, 0)
	if got := parseHDrop(raw); !reflect.DeepEqual(got, want) {
		t.Fatalf("ANSI CF_HDROP 解析 = %q, want %q", got, want)
	}
}

func TestParseHDropRejectsBadHeader(t *testing.T) {
	if got := parseHDrop(make([]byte, 19)); got != nil {
		t.Fatalf("短头应返回空列表，得到 %q", got)
	}
	raw := buildHDrop([]string{`C:\a.txt`})
	raw[0] = 0xff
	if got := parseHDrop(raw); got != nil {
		t.Fatalf("越界文件偏移应返回空列表，得到 %q", got)
	}
}

func TestNormalizeClipboardFilePaths(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "附件.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := normalizeClipboardFilePaths([]string{file, strings.ToUpper(file)})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{file}) {
		t.Fatalf("规范化路径 = %q, want [%q]", got, file)
	}
	if _, err := normalizeClipboardFilePaths([]string{dir}); err == nil {
		t.Fatal("目录不能复制为附件")
	}
	if _, err := normalizeClipboardFilePaths([]string{file + "::inner"}); err == nil {
		t.Fatal("虚拟路径不能直接写 CF_HDROP")
	}
}
