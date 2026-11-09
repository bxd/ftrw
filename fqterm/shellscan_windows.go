//go:build windows

package main

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// discoverShells 探测本机的交互 shell。
//
// 返回的第二个值是给清理逻辑用的护栏：只有「这次确实探到了」的来源才允许删掉它名下
// 消失的条目。wsl.exe 探针超时或列出发行版失败时把用户的 WSL 站点当成「已卸载」删掉，
// 是最让人恼火的一种自动行为。
//
// 全部判断只看文件存在与否和一次轻量进程调用，不写注册表：注册表视图在 32/64 位与
// 用户级安装之间的差异比省下的这几行代码麻烦得多。
func discoverShells() ([]ShellSpec, map[string]bool) {
	root := systemRoot()
	pf := envOr("ProgramFiles", `C:\Program Files`)
	pfx86 := envOr("ProgramFiles(x86)", `C:\Program Files (x86)`)
	localAppData := os.Getenv("LOCALAPPDATA")

	var specs []ShellSpec
	probed := map[string]bool{}

	add := func(sp ShellSpec) {
		if !progExists(sp.Path) {
			return
		}
		if strings.TrimSpace(sp.Name) == "" {
			sp.Name = exeBase(sp.Path)
		}
		specs = append(specs, sp)
	}

	// 命令提示符：COMSPEC 是唯一权威来源，它跟着系统区域设置走。
	probed["cmd"] = true
	add(ShellSpec{
		ID:   "cmd",
		Name: "命令提示符 CMD",
		Path: envOr("COMSPEC", filepath.Join(root, "System32", "cmd.exe")),
	})

	// Windows PowerShell 5.1：随系统装，路径固定。
	probed["powershell"] = true
	add(ShellSpec{
		ID:   "powershell",
		Name: "Windows PowerShell",
		Path: filepath.Join(root, "System32", "WindowsPowerShell", "v1.0", "powershell.exe"),
		Args: []string{"-NoLogo"},
	})

	// PowerShell 7（pwsh）：MSI 装到 ProgramFiles\PowerShell\7，商店/用户级装到 LOCALAPPDATA。
	// PATH 放最前：绿色版、 scoop 之类只出现在 PATH 里。
	var pwshCands []string
	if p := lookPath("pwsh.exe"); p != "" {
		pwshCands = append(pwshCands, p)
	}
	pwshCands = append(pwshCands,
		filepath.Join(pf, "PowerShell", "7", "pwsh.exe"),
		filepath.Join(pfx86, "PowerShell", "7", "pwsh.exe"),
	)
	if localAppData != "" {
		pwshCands = append(pwshCands, filepath.Join(localAppData, "Programs", "PowerShell", "7", "pwsh.exe"))
	}
	probed["pwsh"] = true
	add(ShellSpec{
		ID:   "pwsh",
		Name: "PowerShell 7",
		Path: firstExisting(pwshCands...),
		Args: []string{"-NoLogo"},
	})

	// Git Bash：git.exe 在 <Git>\cmd\，bash.exe 在隔壁的 <Git>\bin\，所以从 PATH 反推一级。
	var bashCands []string
	if git := lookPath("git.exe"); git != "" {
		bashCands = append(bashCands, filepath.Join(filepath.Dir(filepath.Dir(git)), "bin", "bash.exe"))
	}
	bashCands = append(bashCands,
		filepath.Join(pf, "Git", "bin", "bash.exe"),
		filepath.Join(pfx86, "Git", "bin", "bash.exe"),
	)
	if localAppData != "" {
		bashCands = append(bashCands, filepath.Join(localAppData, "Programs", "Git", "bin", "bash.exe"))
	}
	probed["gitbash"] = true
	add(ShellSpec{
		ID:   "gitbash",
		Name: "Git Bash",
		Path: firstExisting(bashCands...),
		// --login 读 /etc/profile 与 ~/.bash_profile（Git Bash 的 PATH 拼装靠它），
		// -i 保证交互式提示符；ConPTY 已经给了 TTY，这两个只是让 rc 流程走全。
		Args: []string{"--login", "-i"},
	})

	// MSYS2 / Cygwin：装到哪基本就是那几个默认目录，用户自定义安装的可以在站点里改路径。
	probed["msys2"] = true
	add(ShellSpec{ID: "msys2", Name: "MSYS2", Path: filepath.Join(`C:\`, "msys64", "usr", "bin", "bash.exe"), Args: []string{"--login", "-i"}})
	probed["cygwin"] = true
	add(ShellSpec{ID: "cygwin", Name: "Cygwin", Path: firstExisting(
		filepath.Join(`C:\`, "cygwin64", "bin", "bash.exe"),
		filepath.Join(`C:\`, "cygwin", "bin", "bash.exe"),
	), Args: []string{"--login", "-i"}})

	// WSL：System32\wsl.exe 是内置的桥（商店版也在它后面），PATH 兜底防止非常规安装。
	wslPath := firstExisting(filepath.Join(root, "System32", "wsl.exe"), lookPath("wsl.exe"))
	if wslPath != "" {
		// 默认发行版一条：跟随 `wsl -s` 的结果，列不出发行版时它是唯一入口。
		add(ShellSpec{ID: "wsl", Name: "WSL（默认发行版）", Path: wslPath})
		distros, err := wslDistros(wslPath)
		if err != nil {
			// 探不到列表就不置位，让已存在的 wsl:xxx 条目留在表里。
			log.Printf("列出 WSL 发行版失败，保留现有条目：%v", err)
			return dedupeByID(specs), probed
		}
		probed["wsl"] = true
		for _, d := range distros {
			// docker-desktop 这类后台发行版不是给人用的交互 shell，列出来只会碍事。
			if strings.EqualFold(d, "docker-desktop") || strings.EqualFold(d, "docker-desktop-data") {
				continue
			}
			add(ShellSpec{ID: "wsl:" + d, Name: "WSL · " + d, Path: wslPath, Args: []string{"-d", d}})
		}
	}

	return dedupeByID(specs), probed
}

// wslDistros 跑 `wsl -l -q` 拿发行版名列表。
//
// 这里的坑是编码：老 wsl.exe 往管道里写 UTF-16LE，新版写 UTF-8，判据见
// decodeWslOutput。发行版名可以含空格（"Ubuntu 24.04 LTS"），所以只能按行切。
func wslDistros(wslPath string) ([]string, error) {
	out, err := runHidden(wslPath, 3*time.Second, "-l", "-q")
	if err != nil {
		return nil, err
	}
	var names []string
	seen := map[string]bool{}
	for _, line := range strings.Split(decodeWslOutput(out), "\n") {
		n := distroName(line)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		names = append(names, n)
	}
	if len(names) == 0 {
		return nil, errors.New("wsl -l -q 没有列出任何发行版")
	}
	return names, nil
}

// runHidden 跑一个控制台小程序并收它的 stdout。
//
// HideWindow 是这里的关键：本程序是 GUI 子系统（-H windowsgui）启动的，没有控制台，
// Windows 会给每个控制台子进程新建一个窗口。不压住它，启动时就会连着闪几个黑框 ——
// 正是第一轮被点名过的那个问题。超时用 context：wsl.exe 在 WSL 服务没起来的时候
// 可以挂很久，绝不能把站点列表的刷新卡住。
func runHidden(name string, timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf // 错误文案也要能进日志，否则只剩一个 exit status
	if err := cmd.Run(); err != nil {
		return buf.Bytes(), err
	}
	return buf.Bytes(), nil
}

// firstExisting 返回第一个真实存在的候选路径，全都不存在返回 ""。
func firstExisting(cands ...string) string {
	for _, c := range cands {
		if progExists(c) {
			return c
		}
	}
	return ""
}

// dedupeByID 同 ID 只留第一个：PATH 里可能有两个 pwsh（机器级 + 用户级），
// 但它们的 ShellSpec.ID 都是 pwsh，补两种子就会在同一路径上打架。
func dedupeByID(specs []ShellSpec) []ShellSpec {
	seen := map[string]bool{}
	out := specs[:0]
	for _, sp := range specs {
		if sp.ID == "" || seen[sp.ID] {
			continue
		}
		seen[sp.ID] = true
		out = append(out, sp)
	}
	return out
}

func systemRoot() string {
	return envOr("SystemRoot", `C:\Windows`)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
