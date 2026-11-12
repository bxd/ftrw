package main

// shellbuiltins.go：内置 ac/am 的 shell 侧接线。
//
// 优先级要求「内置最高」：别名/宏在命令解析里排在 PATH 查找之前，所以给每个
// 交互式 shell 注入启动脚本（cmd doskey / pwsh Set-Alias / bash --rcfile alias）。
// 注入不进去的场景（登录 shell、用户自带启动参数、WSL）靠 PATH 兜底：shim 目录
// 前置到 PATH，里面的 ac/am 脚本直接 exec ftrw.exe 子命令。
//
// 注入发生在 newConptyShell/newPipeShell 的调用点而不是 shellArgv —— 后者被
// TestShellArgv 断言成精确形状，站点配置还原必须保持纯净。

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// builtinShimDir 是 shim 与启动脚本的落点：配置目录下的 shbin。
func builtinShimDir() string {
	return filepath.Join(configRoot(), "shbin")
}

// builtinCLIExe 是内置命令实际执行的二进制：shbin 下的控制台子系统副本。
// GUI 子系统 exe 靠 AttachConsole 借控制台在 ConPTY 下不可靠（输出 invisible、
// Ctrl+C 收不到）；改成控制台子系统后 shell 原生等待、信号原生送达，不需要任何把戏。
func builtinCLIExe(dir string) string {
	return filepath.Join(dir, "ftrw-cli.exe")
}

// makeConsoleCopy 把 GUI 子系统 exe 复制一份并把 PE 头的 Subsystem 改成控制台(3)。
// 只改一个字段，代码段不动；main 的 ac/am 早分流保证副本永远不会初始化 GUI。
func makeConsoleCopy(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if len(b) < 0x40 || binary.LittleEndian.Uint16(b[0:]) != 0x5A4D {
		return fmt.Errorf("不是 MZ 映像：%s", src)
	}
	pe := int(binary.LittleEndian.Uint32(b[0x3C:]))
	if pe+0x5C > len(b) || binary.LittleEndian.Uint32(b[pe:]) != 0x4550 {
		return fmt.Errorf("PE 头损坏：%s", src)
	}
	sub := pe + 0x18 + 0x44                   // Optional Header 里的 Subsystem 字段（PE32/PE32+ 同偏移）
	binary.LittleEndian.PutUint16(b[sub:], 3) // IMAGE_SUBSYSTEM_WINDOWS_CUI
	return os.WriteFile(dst, b, 0o755)
}

// writeBuiltinShims 生成全部 shim/启动脚本。内容按 exe 路径算，重复写幂等；
// exe 路径变了（用户挪了 bin 目录）靠 .exe-path 标记发现并全量重写。
// 参数化 dir/exe 是为了测试能落到临时目录。
func writeBuiltinShims(dir, exe string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	marker := filepath.Join(dir, ".exe-path")
	if old, err := os.ReadFile(marker); err != nil || string(old) != exe {
		// 标记不符：清掉旧 shim，避免残留内容指向不存在的 exe。
		for _, name := range []string{"ac.cmd", "am.cmd", "ac", "am", "ftrw-init.cmd", "ftrw-init.ps1", "ftrw-init.bash", "ftrw-cli.exe"} {
			_ = os.Remove(filepath.Join(dir, name))
		}
		if err := os.WriteFile(marker, []byte(exe), 0o644); err != nil {
			return err
		}
	}
	// 控制台副本：所有 shim/别名都指向它，而不是 GUI 本体。
	// 源 exe 重建（大小/时间戳变了）要同步重做，否则用户测的还是旧代码。
	// 非 Windows 宿主上生成的 shim 仅供 WSL 互操作引用，不需要真副本。
	cliExe := exe
	if runtime.GOOS == "windows" {
		cliExe = builtinCLIExe(dir)
		need := true
		if si, err := os.Stat(exe); err == nil {
			if di, err := os.Stat(cliExe); err == nil && di.Size() == si.Size() && di.ModTime().Equal(si.ModTime()) {
				need = false
			}
		}
		if need {
			if err := makeConsoleCopy(exe, cliExe); err != nil {
				return err
			}
			if si, err := os.Stat(exe); err == nil {
				_ = os.Chtimes(cliExe, si.ModTime(), si.ModTime())
			}
		}
	}
	exeSlash := filepath.ToSlash(cliExe)
	files := map[string]string{
		// PATH 兜底（Windows 侧）：cmd 里敲 ac 命中 ac.cmd。控制台子系统 exe
		// 会被 cmd 原生等待，无需 start /WAIT。
		"ac.cmd": fmt.Sprintf("@\"%s\" ac %%*\r\n", cliExe),
		"am.cmd": fmt.Sprintf("@\"%s\" am %%*\r\n", cliExe),
		// cmd 注入：doskey 宏优先于 PATH 查找。
		"ftrw-init.cmd": fmt.Sprintf("@echo off\r\ndoskey ac=\"%s\" ac $*\r\ndoskey am=\"%s\" am $*\r\n", cliExe, cliExe),
		// pwsh/powershell 注入：别名优先于外部命令。控制台 exe 被 pwsh 原生等待，
		// 无需管道把戏；-Force 覆盖用户环境里可能存在的只读同名别名。
		"ftrw-init.ps1": fmt.Sprintf(
			"function Invoke-FtrwAc { & '%s' ac @args }\r\nfunction Invoke-FtrwAm { & '%s' am @args }\r\nSet-Alias ac Invoke-FtrwAc -Force\r\nSet-Alias am Invoke-FtrwAm -Force\r\n",
			psSingleQuoted(cliExe), psSingleQuoted(cliExe)),
		// bash 族注入：先补回被 --rcfile 顶掉的 ~/.bashrc，再压上别名。
		"ftrw-init.bash": fmt.Sprintf(
			"[ -f ~/.bashrc ] && . ~/.bashrc\nalias ac='\"%s\" ac'\nalias am='\"%s\" am'\n",
			exeSlash, exeSlash),
		// PATH 兜底（POSIX/WSL 侧）：无扩展名 sh 脚本。
		"ac": fmt.Sprintf("#!/bin/sh\nexec \"%s\" ac \"$@\"\n", wslShimPath(cliExe)),
		"am": fmt.Sprintf("#!/bin/sh\nexec \"%s\" am \"$@\"\n", wslShimPath(cliExe)),
	}
	for name, content := range files {
		p := filepath.Join(dir, name)
		if old, err := os.ReadFile(p); err == nil && string(old) == content {
			continue
		}
		if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
			return err
		}
	}
	return nil
}

// psSingleQuoted 把路径放进 PowerShell 单引号字符串：内部单引号翻倍。
func psSingleQuoted(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// posixShimPath 把 Windows 路径翻成 bash 族认的 POSIX 形式：
// cygwin 要 /cygdrive/<d>/...，msys2/gitbash 要 /<d>/...。
func posixShimPath(p string, cygwin bool) string {
	v := filepath.ToSlash(p)
	if len(v) >= 2 && v[1] == ':' && isDriveLetter(v[0]) {
		d := strings.ToLower(string(v[0]))
		if cygwin {
			return "/cygdrive/" + d + v[2:]
		}
		return "/" + d + v[2:]
	}
	return v
}

// injectShellBuiltins 给交互式 shell 追加内置命令注入参数，返回新 argv。
// 用户自带启动脚本/命令参数时一律不碰 —— 宁可退回 PATH 兜底，也不覆盖用户意图。
func injectShellBuiltins(site Site, argv []string) []string {
	if runtime.GOOS != "windows" || len(argv) == 0 {
		return argv
	}
	// shim 内容里的 exe 路径必须是 ftrw.exe 自己，不是 shell 路径。
	exe, err := os.Executable()
	if err != nil {
		return argv
	}
	dir := builtinShimDir()
	if err := writeBuiltinShims(dir, exe); err != nil {
		return argv // 写不出 shim 就不注入，PATH 兜底也一并失效但 shell 照常起
	}
	return injectBuiltinArgs(argv, dir)
}

// injectBuiltinArgs 是注入的纯逻辑部分：dir 参数化，测试不碰真实配置目录。
func injectBuiltinArgs(argv []string, dir string) []string {
	hasArg := func(pred func(string) bool) bool {
		for _, a := range argv[1:] {
			if pred(a) {
				return true
			}
		}
		return false
	}
	switch exeBase(argv[0]) {
	case "cmd.exe":
		if hasArg(func(a string) bool {
			u := strings.ToUpper(a)
			return strings.HasPrefix(u, "/C") || strings.HasPrefix(u, "/K")
		}) {
			return argv
		}
		return append(argv, "/K", filepath.Join(dir, "ftrw-init.cmd"))
	case "pwsh.exe", "powershell.exe":
		if hasArg(func(a string) bool {
			u := strings.ToLower(a)
			return u == "-command" || u == "-c" || u == "-file" || u == "-f" ||
				u == "-encodedcommand" || u == "-e"
		}) {
			return argv
		}
		ps1 := filepath.Join(dir, "ftrw-init.ps1")
		return append(argv, "-NoExit", "-Command", ". '"+psSingleQuoted(ps1)+"'")
	case "bash.exe":
		if hasArg(func(a string) bool { return a == "--rcfile" || a == "-c" }) {
			return argv
		}
		cygwin := strings.Contains(strings.ToLower(argv[0]), "cygwin")
		rc := posixShimPath(filepath.Join(dir, "ftrw-init.bash"), cygwin)
		return append(argv, "--rcfile", rc)
	}
	// wsl.exe 等其余 shell：不注入，靠 envWithTerm 的 PATH 前置兜底。
	return argv
}

// prependShimPath 把 shim 目录前置到 PATH。Windows 下环境变量名大小写不定
// （Path/PATH 都可能），按不区分大小写找。Windows 上同时前置两种形式：
// 原生路径给 cmd/pwsh/bash 找 ac.cmd，/mnt/<d>/... 给 WSL 找无扩展名 sh shim；
// 各自只认自己那种，另一种条目无害。
func prependShimPath(env []string, dir string) []string {
	entries := []string{dir}
	if runtime.GOOS == "windows" {
		entries = append(entries, wslShimPath(dir))
	}
	prefix := strings.Join(entries, string(os.PathListSeparator))
	out := append([]string(nil), env...)
	for i, kv := range out {
		eq := strings.IndexByte(kv, '=')
		if eq > 0 && strings.EqualFold(kv[:eq], "PATH") {
			out[i] = kv[:eq+1] + prefix + string(os.PathListSeparator) + kv[eq+1:]
			return out
		}
	}
	return append(out, "PATH="+prefix)
}
