package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
)

// localConn runs the user's interactive shell as a "Local" session. It uses
// plain pipes (no PTY), which is adequate for cmd.exe on Windows and a basic
// shell on POSIX systems.
type localConn struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	out   io.Reader
	one   sync.Once
}

func newLocalShell() (protoConn, error) {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		shell := os.Getenv("COMSPEC")
		if shell == "" {
			shell = "cmd.exe"
		}
		cmd = exec.Command(shell)
	} else {
		shell := os.Getenv("SHELL")
		if shell == "" {
			shell = "/bin/sh"
		}
		cmd = exec.Command(shell, "-i")
	}
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
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
