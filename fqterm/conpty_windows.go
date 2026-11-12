//go:build windows

package main

import (
	"fmt"
	"log"
	"os"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Win32 常量：x/sys/windows 没有全导出，这里按官方头文件的值本地定义，
// 命名保留 SDK 原样，方便和 MSDN 对照。
const (
	// PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE 就是 winbase.h 里
	// ProcThreadAttributeValue(22, FALSE, TRUE, FALSE) 的展开值。那个宏不是位移：
	// 低位是属性编号（22=0x16），0x00020000 是「Attribute is input only」标志位，
	// 合起来就成 0x00020016。按直觉写成 (22<<16)|(1<<14) 会得到 0x00164000，
	// UpdateProcThreadAttribute 直接报 ERROR_NOT_SUPPORTED。
	procThreadAttributePseudoConsole = 0x00020016
	extendedStartupinfoPresent       = 0x00080000 // EXTENDED_STARTUPINFO_PRESENT
	createUnicodeEnvironment         = 0x00000400 // CREATE_UNICODE_ENVIRONMENT
)

// 本地 shell 的初始尺寸。真正的行列由前端 fit 完调 Resize 覆盖，这里只要保证起进程
// 那一刻伪终端不是 0×0（那样 shell 会拿到一个荒谬的换行宽度）。
const (
	conptyDefaultCols = 80
	conptyDefaultRows = 25
	conptyMaxDim      = 32766

	// stillActive = STILL_ACTIVE，x/sys 没导出，按 winbase.h 的值本地定义。
	stillActive = 259
)

// conptyConn 把一个 Windows 控制台程序接在伪终端（ConPTY）上。
//
// 为什么不用 shell.go 里的管道：bash/zsh/pwsh 发现 stdin 不是 TTY 时会关掉 readline
// —— 不回显敲的字符、不刷提示符，界面看上去就是卡死；cmd.exe 勉强能用，但方向键、
// Tab 补全、Ctrl+C 全废。ConPTY 让 conhost 在中间充当终端驱动，子程序以为自己接到
// 的是真控制台，这些行为就都回来了。顺带一个好处：伪终端交给读端的是 UTF-8，
// 与系统代码页（GBK/CP437/CP932）无关，站点编码一律按 UTF-8 处理即可。
//
// 句柄归属按微软 mini-pty sample 的样式：
//   - 输入管道：读端交给伪终端，写端留在本进程（c.in）；
//   - 输出管道：写端交给伪终端，读端留在本进程（c.out）；
//   - 交出去的那两端必须在本进程 CloseHandle，否则输出读端永远等不到 EOF，
//     shell 退出后会话会一直挂在「已连接」上。
type conptyConn struct {
	in  *os.File
	out *os.File

	mu     sync.Mutex
	pty    *ptyThread
	proc   windows.Handle
	closed bool
}

// ptyThread 是伪终端句柄，外加一条为它 park 住的 OS 线程。
//
// CreatePseudoConsole 不能在随后要调 CreateProcess 的线程上跑（两边会互相等待），
// 微软文档同时要求创建线程在伪终端存活期间不要退出。所以线程发完结果就 park，
// 直到 ClosePseudoConsole 之后由 release 放行。每个本地会话固定占一条线程，结束归还。
type ptyThread struct {
	hpc     windows.Handle
	release chan struct{}
}

// conptyPacket 是 openConpty 的产物：伪终端 + 本进程两侧的可读写句柄。
type conptyPacket struct {
	pty *ptyThread
	in  *os.File // 往 shell 的输入端写
	out *os.File // 读 shell 的输出

	// given 已经交给伪终端、但本进程手里还握着副本的那两端。副本必须等 CreateProcess
	// 之后才关（微软 sample 与文档都是这个顺序：「Close these after CreateProcess of
	// child application with pseudoconsole object」）。在起子进程之前就关它们，管道
	// 的引用计数先掉一级，cmd 一接手就被判成断链 —— 现象是它刷了屏、设了标题，然后
	// 读到 EOF 秒退 0，界面停在提示符上再也敲不动。
	given [2]windows.Handle
}

func (pk *conptyPacket) releaseGivenHandles() {
	for _, h := range pk.given {
		if h != 0 {
			windows.CloseHandle(h)
		}
	}
	pk.given = [2]windows.Handle{}
}

// openConpty 建两条管道与一个伪终端。
func openConpty(cols, rows int) (*conptyPacket, error) {
	sa := windows.SecurityAttributes{
		Length:        uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		InheritHandle: 1, // 与微软 sample 一致：管道端要被 conhost 接手
	}
	// 输入：伪终端读 inputR，本进程写 inputW；输出：伪终端写 outputW，本进程读 outputR。
	var inputR, inputW, outputR, outputW windows.Handle
	if err := windows.CreatePipe(&inputR, &inputW, &sa, 0); err != nil {
		return nil, fmt.Errorf("建输入管道: %w", err)
	}
	if err := windows.CreatePipe(&outputR, &outputW, &sa, 0); err != nil {
		windows.CloseHandle(inputR)
		windows.CloseHandle(inputW)
		return nil, fmt.Errorf("建输出管道: %w", err)
	}

	pty, err := createPseudoConsoleOnThread(inputR, outputW, cols, rows)
	if err != nil {
		for _, h := range []windows.Handle{inputR, inputW, outputR, outputW} {
			windows.CloseHandle(h)
		}
		return nil, err
	}
	// 交出去的两端先留着副本，等 CreateProcess 之后再关；留下的两端包成 os.File，
	// 句柄从此归它们管。
	return &conptyPacket{
		pty:   pty,
		in:    os.NewFile(uintptr(inputW), "conpty-in"),
		out:   os.NewFile(uintptr(outputR), "conpty-out"),
		given: [2]windows.Handle{inputR, outputW},
	}, nil
}

// createPseudoConsoleOnThread 见 ptyThread 的注释：这条调用必须离开主调用线程，
// 而且那条线程要一直 park 到伪终端被关闭。发完结果就 return 会让 LockOSThread 失效、
// OS 线程被销毁，ConPTY 会话就没了落脚的线程 —— CreateProcess 不报错，但子
// 进程回退到父控制台（banner 打到日志里、stdin 一空就退），看起来像「能连但一帧也不输出」。
func createPseudoConsoleOnThread(inputRead, outputWrite windows.Handle, cols, rows int) (*ptyThread, error) {
	type result struct {
		pty *ptyThread
		err error
	}
	ch := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		pty := &ptyThread{release: make(chan struct{})}
		size := windows.Coord{X: int16(clampDim(cols)), Y: int16(clampDim(rows))}
		err := windows.CreatePseudoConsole(size, inputRead, outputWrite, 0, &pty.hpc)
		ch <- result{pty, err}
		if err != nil {
			return // 没建成会话，这条线程也没必要再留着
		}
		<-pty.release // 会话存活期间不得退出（releaseHandle 在 ClosePseudoConsole 之后放行）
	}()
	r := <-ch
	if r.err != nil {
		return nil, fmt.Errorf("CreatePseudoConsole %dx%d: %w", cols, rows, r.err)
	}
	return r.pty, nil
}

// newConptyShell 按站点配置起一个本地 shell 并接到伪终端上。
func newConptyShell(site Site) (protoConn, error) {
	argv := injectShellBuiltins(site, shellArgv(site))
	cols, rows := initialShellSize(site)

	pk, err := openConpty(cols, rows)
	if err != nil {
		return nil, err
	}
	pi, err := spawnUnderPseudoConsole(argv, pk.pty.hpc, shellWorkingDir())
	// 无论成败都得关掉交出去那两端的副本：成功时子进程已经接手，失败时更没必要留着。
	pk.releaseGivenHandles()
	if err != nil {
		pk.in.Close()
		pk.out.Close()
		pk.pty.releaseHandle()
		return nil, err
	}
	windows.CloseHandle(pi.Thread) // 只要进程句柄；线程句柄用不上
	c := &conptyConn{in: pk.in, out: pk.out, pty: pk.pty, proc: pi.Process}
	go c.watchExit()
	log.Printf("本地 shell 已接入 ConPTY：%s（初始 %dx%d，目录 %q）", windowsCommandLine(argv), cols, rows, shellWorkingDir())
	return c, nil
}

// spawnUnderPseudoConsole 用 CreateProcessW + STARTUPINFOEX 的属性表把子进程挂到
// 伪终端上。exec.Cmd 做不到这件事：它没有地方塞 PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE。
func spawnUnderPseudoConsole(argv []string, hpc windows.Handle, dir string) (windows.ProcessInformation, error) {
	var pi windows.ProcessInformation
	al, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return pi, fmt.Errorf("初始化进程线程属性表: %w", err)
	}
	defer al.Delete()
	// 注意属性值的形状：微软的 ConPTY sample 传的是 hPC 这个值本身（配 sizeof(hPC)），
	// 不是它的地址。传 &hpc 时内核把一个栈地址当成了伪终端句柄，CreateProcess 依旧成功，
	// 但子进程初始化时拿着个无效句柄去找控制台 —— 官方文档把这叫成 0xC0000142，
	// 现象是终端一片空白。
	// 这里不用 unsafe.Pointer(hpc)：那是 uintptr→指针的直接转换，vet 会报 possible misuse，
	// GC 语义上也不成立；先把值存在变量里再按指针读回来是等价且合法的写法。
	pseudo := hpc
	if err := al.Update(procThreadAttributePseudoConsole, *(*unsafe.Pointer)(unsafe.Pointer(&pseudo)), unsafe.Sizeof(pseudo)); err != nil {
		return pi, fmt.Errorf("登记伪终端属性: %w", err)
	}
	siex := windows.StartupInfoEx{}
	siex.Cb = uint32(unsafe.Sizeof(siex))
	siex.ProcThreadAttributeList = al.List()

	cmdLine, err := syscall.UTF16PtrFromString(windowsCommandLine(argv))
	if err != nil {
		return pi, fmt.Errorf("命令行编码: %w", err)
	}
	env := utf16EnvBlock(envWithTerm())
	var dirPtr *uint16
	if dir != "" {
		// 起始目录不合法就留给系统决定，别为了一个目录把 shell 整个起不来。
		if d, err := syscall.UTF16PtrFromString(dir); err == nil {
			dirPtr = d
		}
	}
	var envPtr *uint16
	if len(env) > 0 {
		envPtr = &env[0]
	}
	// bInheritHandles 必须是 FALSE：交出去的那两端管道已经附了可继承标记，靠
	// CreatePseudoConsole 自己传给 conhost；子进程这边只认伪终端属性，不能让它
	// 再继承到别句柄，否则它会同时看到一个真控制台和一个伪终端，行为不可预测。
	if err := windows.CreateProcess(nil, cmdLine, nil, nil, false,
		extendedStartupinfoPresent|createUnicodeEnvironment, envPtr, dirPtr, &siex.StartupInfo, &pi); err != nil {
		return pi, fmt.Errorf("启动 %q: %w", argv[0], err)
	}
	// 这三块内存在 CreateProcess 期间必须原地不动，显式续命到调用返回之后。
	runtime.KeepAlive(cmdLine)
	runtime.KeepAlive(env)
	runtime.KeepAlive(pseudo)
	return pi, nil
}

// utf16EnvBlock 按 CreateProcess 的要求拼环境块：每项以 NUL 结尾、整块再补一个 NUL。
// 值里带 NUL 的条目直接跳过，免得把整块环境废掉。
func utf16EnvBlock(kv []string) []uint16 {
	out := make([]uint16, 0, 4096)
	for _, s := range kv {
		u, err := syscall.UTF16FromString(s)
		if err != nil {
			continue
		}
		out = append(out, u...)
	}
	return append(out, 0)
}

func initialShellSize(site Site) (int, int) {
	cols, rows := site.NumColumns, site.NumRows
	if cols < 2 {
		cols = conptyDefaultCols
	}
	if rows < 2 {
		rows = conptyDefaultRows
	}
	return clampDim(cols), clampDim(rows)
}

func clampDim(n int) int {
	if n < 1 {
		return 1
	}
	if n > conptyMaxDim {
		return conptyMaxDim
	}
	return n
}

func (c *conptyConn) Read(p []byte) (int, error)  { return c.out.Read(p) }
func (c *conptyConn) Write(p []byte) (int, error) { return c.in.Write(p) }

// resize 把前端的行列变化转给伪终端。ConPTY 会合成窗口大小改变事件，shell 里的
// vim/less 才知道该重排。
func (c *conptyConn) resize(cols, rows int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.pty == nil {
		return nil // 会话正在拆，尺寸变化晚一步到是正常现象，不算错误
	}
	size := windows.Coord{X: int16(clampDim(cols)), Y: int16(clampDim(rows))}
	if err := windows.ResizePseudoConsole(c.pty.hpc, size); err != nil {
		return fmt.Errorf("ResizePseudoConsole %dx%d: %w", cols, rows, err)
	}
	return nil
}

// watchExit 等子进程结束并记下退出码。输出读端的 EOF 由 conhost 拆伪终端时给出，
// 这里不去关句柄 —— 在别的线程上强关正在被 readLoop 阻塞读的管道，只会把错误类型
// 变成「句柄已关闭」，把真正的退出原因盖掉。
func (c *conptyConn) watchExit() {
	c.mu.Lock()
	proc := c.proc
	c.mu.Unlock()
	if proc == 0 {
		return
	}
	windows.WaitForSingleObject(proc, windows.INFINITE)
	var code uint32
	if err := windows.GetExitCodeProcess(proc, &code); err == nil {
		log.Printf("本地 shell 进程结束，退出码 %d", code)
	}
}

func (c *conptyConn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	pty, proc := c.pty, c.proc
	c.pty, c.proc = nil, 0
	c.mu.Unlock()

	// 先终止子进程：伪终端在进程还活着的时候不会被真正收掉。
	if proc != 0 {
		// 已经退出的进程不去碰 TerminateProcess —— 拿到的报错是「Access is denied」，
		// 看着像权限问题，其实是对已结束进程要终止权的正常反应，会污染日志。
		var code uint32
		if err := windows.GetExitCodeProcess(proc, &code); err == nil && code == stillActive {
			if err := windows.TerminateProcess(proc, 0); err != nil {
				log.Printf("终止本地 shell 进程: %v", err)
			}
		}
		windows.WaitForSingleObject(proc, 2000)
		windows.CloseHandle(proc)
	}
	// 输入端先关，免得 shell 在被杀的过程中又吞到一个半截命令。
	c.in.Close()
	if pty != nil {
		pty.releaseHandle()
	}
	// 读端最后关。它是 os.NewFile 包起来的管道句柄，在 Windows 上走 IOCP，
	// Close 会把阻塞在 Read 上的 readLoop 顶回来（报 file already closed）。
	c.out.Close()
	return nil
}

// releaseHandle 关掉伪终端并放行那条 park 住的创建线程。
//
// ClosePseudoConsole 会阻塞到 conhost 收摊。子进程已经被终止的情况下它应当在毫秒级
// 返回，但万一被卡住也不该把会话拆除的流程拖死，所以放到 goroutine 里等最多 2 秒。
func (p *ptyThread) releaseHandle() {
	done := make(chan struct{})
	go func() {
		windows.ClosePseudoConsole(p.hpc)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		log.Printf("ClosePseudoConsole 超过 2 秒未返回，留给系统回收")
	}
	close(p.release)
}
