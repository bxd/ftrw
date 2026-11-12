package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
)

// localConn 把交互 shell 接成一路 “Local” 会话：普通管道，没有 PTY。
// 这只用在非 Windows 系统上；Windows 侧必须走 ConPTY，见 conpty_windows.go。
type localConn struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	out   io.Reader
	one   sync.Once
}

// newLocalShell 起一个本地 shell 会话，具体怎么接终端由平台决定。
//
// Windows 交给 ConPTY：管道下 bash/pwsh 发现 stdin 不是 TTY 会关掉 readline（敲字不回显，
// 看着像卡死），cmd 也没有方向键和 Tab 补全；ConPTY 让 conhost 在中间充当终端驱动，
// 顺便把输出统一成 UTF-8。其它平台继续用管道。
func newLocalShell(site Site) (protoConn, error) {
	if runtime.GOOS == "windows" {
		return newConptyShell(site)
	}
	return newPipeShell(site)
}

// newPipeShell 按站点配置起 shell，两边接管道。
func newPipeShell(site Site) (protoConn, error) {
	argv := injectShellBuiltins(site, shellArgv(site))
	// 老配置里没有 shellArgs，而 POSIX 的 bash/zsh 不带 -i 会把_pipe_ 当脚本读到 EOF 就退，
	// 给它们补上交互开关。
	if len(argv) == 1 {
		argv = append(argv, interactiveArgs(exeBase(argv[0]))...)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	if dir := shellWorkingDir(); dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = envWithTerm()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &localConn{
		cmd:   cmd,
		stdin: stdin,
		out:   io.MultiReader(stdout, stderr),
	}, nil
}

func (c *localConn) Read(p []byte) (int, error)  { return c.out.Read(p) }
func (c *localConn) Write(p []byte) (int, error) { return c.stdin.Write(p) }
func (c *localConn) resize(cols, rows int) error { return nil }

func (c *localConn) Close() error {
	var err error
	c.one.Do(func() {
		c.stdin.Close()
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
		err = c.cmd.Wait()
	})
	return err
}

// uniquePath appends (n) until the path is unused.
func uniquePath(dir, name string) string {
	ext := filepath.Ext(name)
	base := name[:len(name)-len(ext)]
	path := filepath.Join(dir, name)
	for i := 1; ; i++ {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return path
		}
		path = filepath.Join(dir, base+"_"+itoa(i)+ext)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
