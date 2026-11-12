package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf16"
)

// 本机 shell 在站点表里统一放这个组，站点列表栏靠它把「系统 shell」和用户自建站点分开。
const ShellSiteGroup = "shell"

// ShellSpec 是本机探测到的一个交互式 shell。它最后会变成一条 Local 站点
// （ProtocolType==3），所以用户能像改任何站点一样改它的字体、配色、编码、参数。
//
// ID 是稳定标识而不是显示名：补种与清理都靠它认人（「WSL · Ubuntu」这种名字用户
// 会改，改了就不该被当成另一条 shell 再补一遍）。带 ':' 的 ID 表示同一来源下的多个
// 条目，冒号前的部分才是探测源 —— 见 shellSource，它决定「这次探测到底可不可信」。
type ShellSpec struct {
	ID   string   // 稳定标识：cmd / powershell / pwsh / gitbash / wsl / wsl:Ubuntu
	Name string   // 列表显示名
	Path string   // 可执行文件绝对路径（落进 Site.Host）
	Args []string // 启动参数
}

// shellSource 取探测源：wsl:Ubuntu → wsl；cmd → cmd。
func shellSource(id string) string {
	if i := strings.IndexByte(id, ':'); i > 0 {
		return id[:i]
	}
	return id
}

// siteFromShell 把探测结果映成一条站点配置。
//
// 关键取舍：本地 shell 没有服务器，所以空闲保活、自动重连、ZMODEM 全部关掉 ——
// 留着它们不但没意义，还会让保活字节往 shell 里灌 NUL。编码固定 UTF-8：
// Windows 侧走 ConPTY，控制台输出在伪终端这一层已经统一转成 UTF-8，与系统代码页
// （GBK/CP437/CP932）无关；POSIX 侧本来就是 UTF-8。
func siteFromShell(sh ShellSpec) Site {
	s := DefaultSite()
	s.Name = sh.Name
	s.Host = sh.Path
	s.Port = 0
	s.HostType = 1 // *NIX：按终端实际尺寸走，不做 BBS 的 80×24 下限
	s.ProtocolType = 3
	s.AutoLogin = false
	s.UserName = ""
	s.Password = ""
	s.PreLogin = ""
	s.PostLogin = ""
	s.ServerEncodingID = 2 // UTF-8
	s.TermType = "xterm-256color"
	s.KeyType = 1
	s.AntiIdle = false
	s.AntiIdleMessage = ""
	s.AutoReconnect = false
	s.MaxIdleSeconds = 0
	s.EnableZmodem = false
	s.Group = ShellSiteGroup
	s.ShellID = sh.ID
	s.ShellArgs = append([]string(nil), sh.Args...)
	s.Auto = true
	return s
}

// shellArgv 还原一条 Local 站点的启动命令。Host 留空时退回系统默认 shell，
// 这样「Local 协议但不填路径」的老配置依旧能用（等价于改名前的行为）。
func shellArgv(site Site) []string {
	prog := strings.TrimSpace(site.Host)
	if prog == "" {
		prog = defaultShellPath()
	}
	argv := []string{prog}
	for _, a := range site.ShellArgs {
		if a != "" {
			argv = append(argv, a)
		}
	}
	return argv
}

func defaultShellPath() string {
	if runtime.GOOS == "windows" {
		if c := os.Getenv("COMSPEC"); c != "" {
			return c
		}
		return "cmd.exe"
	}
	if sh := os.Getenv("SHELL"); sh != "" {
		return sh
	}
	return "/bin/sh"
}

// shellWorkingDir 是本地 shell 的起始目录。不显式设的话子进程会继承本程序的当前
// 目录 —— 双击 ftrw.exe 时那通常就是 bin\，用户一进来就站在一个莫名其妙的地方。
func shellWorkingDir() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h
	}
	return ""
}

// progExists 判断一个候选程序路径是不是真的一个文件。跟 logging.go 里的 fileExists
// 差一点：那个把目录也算存在，而探测靠它筛候选，指到一个目录上就会起进程失败。
// 探测源里有十几个候选路径，用 Stat 比试跑一次进程便宜得多，而且不会弹出任何窗口。
func progExists(path string) bool {
	if path == "" {
		return false
	}
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

// lookPath 把命令名解析成绝对路径，找不到返回 ""。
// 探测 Git 这类「装在 PATH 里」的工具要用它，但拿到的是 PATH 目录，
// 兄弟目录才是真实目标（git.exe 在 <Git>\cmd\，bash.exe 在 <Git>\bin\）。
func lookPath(name string) string {
	p, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	return p
}

// windowsQuoteArg 按 MSVC/CreateProcess 的规则给一个参数加引号。
//
// 这里必须自己拼命令行：CreateProcessW 只吃一条字符串，而 lpCommandLine 里的反斜杠
// 只有在紧邻引号时才算转义 —— 直接 Join 会让 "C:\Program Files\Git\bin\bash.exe"
// 之外的路径（尤其结尾带反斜杠的）解析错位。含空格的发行版名（如 "Ubuntu 24.04"）
// 也靠它保住成一个参数。
func windowsQuoteArg(arg string) string {
	if arg == "" {
		return `""`
	}
	if !strings.ContainsAny(arg, " \t\"") {
		return arg
	}
	var b strings.Builder
	b.WriteByte('"')
	backslashes := 0
	for i := 0; i < len(arg); i++ {
		switch c := arg[i]; c {
		case '\\':
			backslashes++
		case '"':
			// 引号前先把手里的反斜杠翻倍（每个反斜杠要两个才能保住自己），再把引号本身转义
			b.WriteString(strings.Repeat("\\", backslashes*2+1))
			b.WriteByte('"')
			backslashes = 0
		default:
			b.WriteString(strings.Repeat("\\", backslashes))
			backslashes = 0
			b.WriteByte(c)
		}
	}
	// 结尾的反斜杠必须翻倍，否则会吃掉闭合引号
	b.WriteString(strings.Repeat("\\", backslashes*2))
	b.WriteByte('"')
	return b.String()
}

// windowsCommandLine 把 argv 拼成 CreateProcessW 的 lpCommandLine。
func windowsCommandLine(argv []string) string {
	parts := make([]string, 0, len(argv))
	for _, a := range argv {
		parts = append(parts, windowsQuoteArg(a))
	}
	return strings.Join(parts, " ")
}

// joinShellArgs 给日志和界面用：参数按 Windows 规则加引号，POSIX 侧留空格分隔。
func joinShellArgs(args []string) string {
	if len(args) == 0 {
		return ""
	}
	if runtime.GOOS == "windows" {
		return windowsCommandLine(args)
	}
	return strings.Join(args, " ")
}

// envWithTerm 在继承本进程环境的基础上补 TERM，并把内置命令 shim 目录前置到
// PATH（ac/am 的兜底通道：注入不进去的 shell 靠它命中 shim）。ConPTY 起的
// 子进程不会自己设 TERM，而 bash/vim/less 都靠它判断能力集。
func envWithTerm() []string {
	env := os.Environ()
	hasTerm := false
	for _, kv := range env {
		if strings.HasPrefix(kv, "TERM=") {
			hasTerm = true
			break
		}
	}
	if !hasTerm {
		env = append(append([]string(nil), env...), "TERM=xterm-256color")
	}
	return prependShimPath(env, builtinShimDir())
}

// distroName 清理 `wsl -l -q` 输出里的一行：去掉 BOM、空引号、尾随空白。
// 发行版名可以含空格（"Ubuntu 24.04 LTS"），所以这里绝不按空白切。
func distroName(line string) string {
	s := strings.TrimSpace(line)
	s = strings.TrimPrefix(s, "\ufeff")
	s = strings.Trim(s, "\"")
	return strings.TrimSpace(s)
}

// decodeWslOutput 处理 wsl.exe 的输出编码坑：老版本直接把 UTF-16LE 写进管道，
// 新版本写 UTF-8。判据用「ASCII 文本每两个字节夹一个 NUL」，足够稳且不会误伤
// UTF-8（UTF-8 文本里不会出现成对的位置模式，也不含 NUL）。
func decodeWslOutput(b []byte) string {
	if len(b) >= 2 && len(b)%2 == 0 {
		utf16le := true
		for i := 1; i < len(b); i += 2 {
			if b[i] != 0 {
				utf16le = false
				break
			}
		}
		if utf16le {
			if s, err := decodeUTF16LE(b); err == nil {
				return s
			}
		}
	}
	return string(b)
}

// decodeUTF16LE 把老版 wsl.exe 写的 UTF-16LE 字节转成 UTF-8 字符串。
func decodeUTF16LE(b []byte) (string, error) {
	if len(b)%2 != 0 {
		return "", errors.New("UTF-16 长度不是偶数")
	}
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		u = append(u, uint16(b[i])|uint16(b[i+1])<<8)
	}
	return string(utf16.Decode(u)), nil
}

// interactiveArgs 只给确实需要显式开关的 shell 加参数：bash/zsh 不带 -i 时从管道
// 读命令会当成脚本执行完就退；fish 与 csh 默认就是交互。
//
// 只有管道实现（非 Windows）会用到它。Windows 走 ConPTY，参数完全由站点配置决定，
// 这里返回 nil 免得给 cmd.exe 塞一个它不认识的东西。
func interactiveArgs(base string) []string {
	if runtime.GOOS == "windows" {
		return nil
	}
	switch base {
	case "bash", "zsh", "dash", "ksh", "sh":
		return []string{"-i"}
	}
	return nil
}

// exeBase 是 filepath.Base 的小写形式，用于同一来源里的候选去重
// （Windows 路径大小写不定，PATH 里可能有多个 pwsh）。
func exeBase(path string) string {
	return strings.ToLower(filepath.Base(path))
}
