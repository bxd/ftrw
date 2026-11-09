//go:build !windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// discoverShells 在 POSIX 侧以 /etc/shells 为准，外加 $SHELL 兜底。
//
// 没有注册表那种猜路径的麻烦，但也不能把 /etc/shells 里所有条目当真：它们只是
// 「允许登录的 shell」清单，其中 /usr/sbin/nologin、/bin/false 不是交互 shell，
// 所以只接受在 /bin、/usr/bin、/usr/local/bin 下的常见解释器。
func discoverShells() ([]ShellSpec, map[string]bool) {
	probed := map[string]bool{"posix": true}
	var specs []ShellSpec
	seen := map[string]bool{}

	add := func(path string) {
		if path == "" || seen[path] || !progExists(path) {
			return
		}
		seen[path] = true
		base := filepath.Base(path)
		specs = append(specs, ShellSpec{
			ID:   "posix:" + base,
			Name: base,
			Path: path,
			Args: interactiveArgs(base),
		})
	}

	if sh := os.Getenv("SHELL"); sh != "" {
		add(sh)
	}
	if b, err := os.ReadFile("/etc/shells"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			p := strings.TrimSpace(strings.TrimSuffix(line, "\r"))
			if p == "" || strings.HasPrefix(p, "#") {
				continue
			}
			if !shellDirAllowed(p) {
				continue
			}
			add(p)
		}
	}

	// 常见的装在不规则前缀下的：Homebrew、Nix、conda。存在就收，不存在一句 Stat 而已。
	for _, p := range []string{
		"/opt/homebrew/bin/fish",
		"/usr/local/bin/fish",
		"/run/current-system/sw/bin/bash",
	} {
		add(p)
	}
	return specs, probed
}

// shellDirAllowed 挑出「真的是给人用的 shell」所在的目录。
func shellDirAllowed(path string) bool {
	switch filepath.Dir(path) {
	case "/bin", "/usr/bin", "/usr/local/bin", "/opt/homebrew/bin", "/run/current-system/sw/bin":
		return true
	}
	return false
}

// newConptyShell 只存在于 Windows。其它平台不需要它：要么本就没有伪终端这一层
// （本地 shell 直接走 shell.go 的管道实现），要么 PTY 在内核里，不在这里补。
func newConptyShell(site Site) (protoConn, error) {
	return nil, errors.New(runtime.GOOS + " 平台没有 ConPTY 实现")
}
