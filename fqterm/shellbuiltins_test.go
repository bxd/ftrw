package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWriteBuiltinShims(t *testing.T) {
	dir := t.TempDir()
	// 用真实 PE（测试二进制自己）作源：Windows 上 writeBuiltinShims 要做控制台副本。
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := writeBuiltinShims(dir, exe); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ac.cmd", "am.cmd", "ac", "am", "ftrw-init.cmd", "ftrw-init.ps1", "ftrw-init.bash"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("缺 shim 文件 %s：%v", name, err)
		}
	}
	// shim 必须指向控制台副本而不是 GUI 本体（Windows 宿主）。
	target := exe
	if runtime.GOOS == "windows" {
		target = builtinCLIExe(dir)
		if _, err := os.Stat(target); err != nil {
			t.Fatalf("缺控制台副本：%v", err)
		}
	}
	acCmd, _ := os.ReadFile(filepath.Join(dir, "ac.cmd"))
	if !strings.Contains(string(acCmd), `"`+target+`" ac %*`) {
		t.Errorf("ac.cmd 内容不对：%q", acCmd)
	}
	initCmd, _ := os.ReadFile(filepath.Join(dir, "ftrw-init.cmd"))
	if !strings.Contains(string(initCmd), `doskey ac="`+target+`" ac $*`) {
		t.Errorf("ftrw-init.cmd 缺 doskey：%q", initCmd)
	}
	ps1, _ := os.ReadFile(filepath.Join(dir, "ftrw-init.ps1"))
	if !strings.Contains(string(ps1), "Set-Alias ac Invoke-FtrwAc -Force") || !strings.Contains(string(ps1), "Set-Alias am Invoke-FtrwAm -Force") {
		t.Errorf("ftrw-init.ps1 缺别名（需 -Force 覆盖只读同名别名）：%q", ps1)
	}
	if !strings.Contains(string(ps1), psSingleQuoted(target)+"' ac @args") {
		t.Errorf("ftrw-init.ps1 未指向控制台副本：%q", ps1)
	}
	bash, _ := os.ReadFile(filepath.Join(dir, "ftrw-init.bash"))
	if !strings.Contains(string(bash), ". ~/.bashrc") || !strings.Contains(string(bash), "alias ac=") {
		t.Errorf("ftrw-init.bash 形状不对：%q", bash)
	}
	sh, _ := os.ReadFile(filepath.Join(dir, "ac"))
	if !strings.HasPrefix(string(sh), "#!/bin/sh") || !strings.Contains(string(sh), ` ac "$@"`) {
		t.Errorf("POSIX shim 形状不对：%q", sh)
	}

	// 幂等：再写一遍不报错；exe 变了要全量重写
	if err := writeBuiltinShims(dir, exe); err != nil {
		t.Fatal(err)
	}
	exe2 := filepath.Join(dir, "elsewhere", "ftrw.exe")
	if err := os.MkdirAll(filepath.Dir(exe2), 0o755); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(exe); err == nil {
		_ = os.WriteFile(exe2, b, 0o755)
	}
	if err := writeBuiltinShims(dir, exe2); err != nil {
		t.Fatal(err)
	}
	acCmd, _ = os.ReadFile(filepath.Join(dir, "ac.cmd"))
	if strings.Contains(string(acCmd), exe+`"`) {
		t.Errorf("exe 变更后 shim 未重写：%q", acCmd)
	}
}

func TestMakeConsoleCopy(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("PE 子系统补丁只在 Windows 上有意义")
	}
	src, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "cli.exe")
	if err := makeConsoleCopy(src, dst); err != nil {
		t.Fatal(err)
	}
	readSubsystem := func(p string) uint16 {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		pe := int(binary.LittleEndian.Uint32(b[0x3C:]))
		return binary.LittleEndian.Uint16(b[pe+0x18+0x44:])
	}
	// go test 的二进制本身是控制台子系统，不预设源值；只断言补丁后必为 3。
	if got := readSubsystem(dst); got != 3 {
		t.Errorf("副本子系统应为控制台(3)，got %d", got)
	}
	if si, err := os.Stat(src); err == nil {
		if di, err := os.Stat(dst); err != nil || di.Size() != si.Size() {
			t.Errorf("副本大小应与源一致：src=%d dst=%d err=%v", si.Size(), di.Size(), err)
		}
	}
}

func TestPsSingleQuoted(t *testing.T) {
	if got := psSingleQuoted(`C:\a'b\ftrw.exe`); got != `C:\a''b\ftrw.exe` {
		t.Errorf("单引号转义不对：%q", got)
	}
}

func TestInjectBuiltinArgsCmd(t *testing.T) {
	dir := `C:\cfg\shbin`
	got := injectBuiltinArgs([]string{`C:\Windows\System32\cmd.exe`}, dir)
	want := []string{`C:\Windows\System32\cmd.exe`, "/K", filepath.Join(dir, "ftrw-init.cmd")}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("cmd 注入形状不对：%v", got)
	}
	// 用户自带 /K 或 /C：不碰
	for _, args := range [][]string{
		{`cmd.exe`, "/K", "mine.cmd"},
		{`cmd.exe`, "/C", "echo hi"},
	} {
		got := injectBuiltinArgs(args, dir)
		if strings.Join(got, "|") != strings.Join(args, "|") {
			t.Errorf("用户参数被覆盖：%v", got)
		}
	}
}

func TestInjectBuiltinArgsPwsh(t *testing.T) {
	dir := `C:\cfg\shbin`
	for _, base := range []string{`C:\Program Files\PowerShell\7\pwsh.exe`, `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`} {
		got := injectBuiltinArgs([]string{base, "-NoLogo"}, dir)
		if len(got) != 5 || got[2] != "-NoExit" || got[3] != "-Command" {
			t.Fatalf("pwsh 注入形状不对：%v", got)
		}
		if !strings.HasPrefix(got[4], ". '") || !strings.HasSuffix(got[4], "ftrw-init.ps1'") {
			t.Errorf("点源命令形状不对：%q", got[4])
		}
	}
	// 用户自带 -Command/-File/-EncodedCommand：不碰
	for _, flag := range []string{"-Command", "-c", "-File", "-EncodedCommand"} {
		args := []string{`pwsh.exe`, flag, "stuff"}
		got := injectBuiltinArgs(args, dir)
		if strings.Join(got, "|") != strings.Join(args, "|") {
			t.Errorf("用户 %s 被覆盖：%v", flag, got)
		}
	}
}

func TestInjectBuiltinArgsBash(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("POSIX 路径翻译只在 Windows 宿主上有意义")
	}
	dir := `C:\cfg\shbin`
	got := injectBuiltinArgs([]string{`C:\Program Files\Git\bin\bash.exe`, "--login", "-i"}, dir)
	if len(got) != 5 || got[3] != "--rcfile" || got[4] != "/c/cfg/shbin/ftrw-init.bash" {
		t.Errorf("gitbash 注入形状不对：%v", got)
	}
	got = injectBuiltinArgs([]string{`C:\cygwin64\bin\bash.exe`, "--login", "-i"}, dir)
	if got[4] != "/cygdrive/c/cfg/shbin/ftrw-init.bash" {
		t.Errorf("cygwin 前缀不对：%v", got)
	}
	// 用户自带 --rcfile 或 -c：不碰
	for _, args := range [][]string{
		{`bash.exe`, "--rcfile", "mine"},
		{`bash.exe`, "-c", "echo hi"},
	} {
		got := injectBuiltinArgs(args, dir)
		if strings.Join(got, "|") != strings.Join(args, "|") {
			t.Errorf("用户参数被覆盖：%v", got)
		}
	}
}

func TestInjectBuiltinArgsWslUntouched(t *testing.T) {
	args := []string{`C:\Windows\System32\wsl.exe`, "-d", "Ubuntu"}
	got := injectBuiltinArgs(args, `C:\cfg\shbin`)
	if strings.Join(got, "|") != strings.Join(args, "|") {
		t.Errorf("WSL 不应注入：%v", got)
	}
}

func TestPrependShimPath(t *testing.T) {
	env := []string{"TERM=xterm", "Path=" + `C:\Windows;C:\Tools`, "HOME=x"}
	got := prependShimPath(env, `C:\cfg\shbin`)
	var path string
	for _, kv := range got {
		if strings.HasPrefix(strings.ToUpper(kv), "PATH=") {
			path = kv
		}
	}
	if path == "" {
		t.Fatal("PATH 丢了")
	}
	if !strings.HasPrefix(path, "Path="+`C:\cfg\shbin`+string(os.PathListSeparator)) {
		t.Errorf("shim 目录未前置：%q", path)
	}
	if runtime.GOOS == "windows" && !strings.Contains(path, "/mnt/c/cfg/shbin") {
		t.Errorf("Windows 上应同时带 WSL 形式：%q", path)
	}
	if !strings.Contains(path, `C:\Windows`) {
		t.Errorf("原 PATH 被吞：%q", path)
	}
	// 原 env 不被就地修改
	if env[1] != "Path="+`C:\Windows;C:\Tools` {
		t.Error("prependShimPath 就地改了入参")
	}
	// 没有 PATH 时补一条
	got = prependShimPath([]string{"A=1"}, `C:\cfg\shbin`)
	if len(got) != 2 || !strings.HasPrefix(got[1], "PATH=") {
		t.Errorf("缺 PATH 时未补：%v", got)
	}
}

func TestEnvWithTermPrependsShim(t *testing.T) {
	t.Setenv("PATH", `C:\Windows`)
	env := envWithTerm()
	found := false
	for _, kv := range env {
		if strings.HasPrefix(strings.ToUpper(kv), "PATH=") && strings.Contains(kv, filepath.Join(configRoot(), "shbin")) {
			found = true
		}
	}
	if !found {
		t.Error("envWithTerm 未把 shim 目录前置到 PATH")
	}
}
