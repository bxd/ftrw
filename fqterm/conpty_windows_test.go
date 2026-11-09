//go:build windows

package main

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// conptyStdinUsable 判断当前的运行环境能不能测 ConPTY。
//
// 真机跑下来的结论：同一个二进制，直接运行能测通，在 go test 里却必测失败。
// 差异在标准输入：go test 把子进程的 stdin 接成管道/NUL，此时候选子进程接不上
// 伪终端，它退回父控制台刷个 banner，然后因为 stdin 立即 EOF 而退 0。
// ftrw 本体是 windowsgui 子系统，根本没有控制台，反而是正常路径。所以这个环境下
// 只能跳过，报失败没有意义。
func conptyStdinUsable() bool {
	if _, err := windows.GetConsoleOutputCP(); err != nil {
		return true // 没有控制台（GUI 子系统），正常路径
	}
	h, err := windows.GetStdHandle(windows.STD_INPUT_HANDLE)
	if err != nil {
		return false
	}
	var mode uint32
	return windows.GetConsoleMode(h, &mode) == nil
}

// TestConptyShellRoundTrip 真的起一个 cmd.exe 接到伪终端上，敲一条 echo 再把输出读回来。
//
// 这一段是整套本地 shell 里最容易坏的部分：管道句柄的可继承性、交出去的那两端有没有
// 在本进程关掉、ClosePseudoConsole 的时机，错了会表现成「终端一片空白」或「关掉标签后
// 读循环永远不返回」，而这些在纯逻辑测试里都看不出来。所以这里宁可真的开进程。
//
// 用 -short 可以跳过（CI 或不希望弹窗的环境）。
func TestConptyShellRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("-short：跳过真起进程的 ConPTY 测试")
	}
	if !conptyStdinUsable() {
		t.Skip("本进程有控制台但 stdin 不是控制台输入（go test 就是这情形），ConPTY 在这种环境下接不上子进程")
	}
	comspec := os.Getenv("COMSPEC")
	if comspec == "" {
		t.Skip("环境里没有 COMSPEC")
	}
	c, err := newConptyShell(Site{Name: "conpty-test", Host: comspec, ProtocolType: 3, NumColumns: 80, NumRows: 25})
	if err != nil {
		t.Fatalf("起 ConPTY shell: %v", err)
	}
	defer c.Close()

	const marker = "FTRW_CONPTY_ROUNDTRIP"
	var mu sync.Mutex
	var collected strings.Builder
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			n, err := c.Read(buf)
			if n > 0 {
				mu.Lock()
				collected.Write(buf[:n])
				got := collected.String()
				mu.Unlock()
				if strings.Contains(got, marker) {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	if _, err := c.Write([]byte("echo " + marker + "\r")); err != nil {
		t.Fatalf("写入命令: %v", err)
	}
	select {
	case <-done:
	case <-time.After(15 * time.Second):
	}
	mu.Lock()
	got := collected.String()
	mu.Unlock()
	if !strings.Contains(got, marker) {
		// 退出码是定位问题的关键：0 = 子进程把输入当成了已结束的管道（stdin EOF）就退。
		if cc, ok := c.(*conptyConn); ok {
			var code uint32
			if err := windows.GetExitCodeProcess(cc.proc, &code); err == nil {
				t.Logf("子进程退出码 %#x（259=0x103 仍在运行）", code)
			}
		}
		t.Fatalf("输出里没看到回显（拿到 %d 字节）：\n%q", len(got), got)
	}

	// 回显到手后再改尺寸：resize 会让伪终端重画整屏，起进程之前改会把内容弄乱，
	// 不利于定位问题。
	if err := c.resize(120, 32); err != nil {
		t.Errorf("resize: %v", err)
	}
}
