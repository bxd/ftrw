package main

// advcopy_cli.go：ftrw.exe ac/am 的 CLI 入口。内置命令作为 shell 的子进程跑，
// 天然继承 shell 的当前目录 —— 后端服务并不知道用户 cd 到了哪里，这是把 ac/am
// 做成 CLI 模式而不是后端方法的根本原因。shell 侧的别名注入见 shellbuiltins.go。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// advPlain 标记标准输出不是可重绘的控制台（纯重定向/无控制台）：
// 不画 ANSI 重绘块，只打最终结果。由 attachParentConsole 按平台判定。
var advPlain bool

// advWidth 是终端列数，供渲染器截断长行保证三行块不換行。默认 80，
// Windows 侧 attachParentConsole 会按真实控制台宽度覆盖。
var advWidth = 80

// runAdvancedCopyCLI 处理 ac/am 子命令，返回进程退出码。
func runAdvancedCopyCLI(cmd string, args []string) int {
	attachParentConsole() // 控制台副本自带控制台；GUI 本体直跑时借父进程（shell）的
	o, err := parseAdvArgs(args)
	if err == errAdvHelp {
		fmt.Println(advUsage)
		return 0
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	o.move = cmd == "am"
	// WSL 里跑的是 Windows exe，参数却是 POSIX 路径：翻译后再交给引擎。
	if distro := os.Getenv("WSL_DISTRO_NAME"); distro != "" {
		for i, s := range o.sources {
			o.sources[i] = wslToWindowsPath(s, distro)
		}
		o.dest = wslToWindowsPath(o.dest, distro)
	}
	var r advRenderer = &termRenderer{w: os.Stdout, width: advWidth}
	if advPlain {
		r = &plainRenderer{}
	}
	if err := runAdvanced(o, r); err != nil {
		r.finish()
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	r.finish()
	return 0
}

// plainRenderer 是非控制台输出的降级形态：不画重绘块，只在全部完成后打一行汇总。
type plainRenderer struct{ done bool }

func (p *plainRenderer) render(pr *advProgress) {
	if p.done || pr.filesDone < pr.filesTotal || pr.doneBytes < pr.totalBytes {
		return
	}
	p.done = true
	fmt.Printf("%d/%d files  %s / %s 100.0 %%\n",
		pr.filesDone, pr.filesTotal, humanBytes(pr.doneBytes), humanBytes(pr.totalBytes))
}

func (p *plainRenderer) note(line string) { fmt.Println(line) }
func (p *plainRenderer) finish()          {}

// wslToWindowsPath 把 WSL 里的 POSIX 路径翻成 Windows 路径：
// /mnt/X/... → X:\...（drvfs 挂载）；其余绝对路径 → \\wsl$\<distro>\...；
// 相对路径不动 —— 进程 cwd 已经是对应的 UNC 目录，相对解析天然正确。
func wslToWindowsPath(p, distro string) string {
	if !strings.HasPrefix(p, "/") {
		return p
	}
	if strings.HasPrefix(p, "/mnt/") && len(p) >= 6 && isDriveLetter(p[5]) {
		// p[6:] 以 / 开头，替换后自带前导 \，拼接前先去掉，免得和 :\ 叠成双反斜杠。
		rest := strings.TrimPrefix(strings.ReplaceAll(p[6:], "/", `\`), `\`)
		return strings.ToUpper(string(p[5])) + `:\` + rest
	}
	return `\\wsl$\` + distro + strings.ReplaceAll(p, "/", `\`)
}

func isDriveLetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// wslShimPath 把 Windows 路径翻成 WSL 的 /mnt/<盘符>/... 形式，
// 供 PATH 前置与 sh shim 脚本里的 exe 路径使用。
func wslShimPath(p string) string {
	v := filepath.ToSlash(p)
	if len(v) >= 2 && v[1] == ':' && isDriveLetter(v[0]) {
		return "/mnt/" + strings.ToLower(string(v[0])) + v[2:]
	}
	return v
}
