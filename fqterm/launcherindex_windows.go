//go:build windows

package main

import (
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/go-ole/go-ole/oleutil"
)

// 启动器索引与启动的 Windows 侧实现。
//
// 扫描范围只覆盖「开始菜单 + 桌面」的 .lnk —— 这是 Windows 上应用可见性的
// 事实来源，UWP 的 App Execution Alias（WindowsApps 里的零字节 reparse 点）
// 首版不碰：解析形状完全不同，混进来只会拖累这条最短路径。
//
// TargetPath 走 WScript.Shell 的 CreateShortcut 而不是手搭 IShellLinkW vtable：
// 同一个 COM 对象、多两行属性读取，换掉一整张虚表声明。解析只服务匹配，
// 任何一个文件失败都只是少一路分数，绝不让索引整体失败。

// appShortcutRoots 给出四个扫描根目录（不存在的一律静默跳过）。
func appShortcutRoots() []string {
	home, _ := os.UserHomeDir()
	return []string{
		envOr("APPDATA", "") + `\Microsoft\Windows\Start Menu\Programs`,
		envOr("ProgramData", `C:\ProgramData`) + `\Microsoft\Windows\Start Menu\Programs`,
		filepath.Join(home, "Desktop"),
		envOr("PUBLIC", "") + `\Desktop`,
	}
}

// scanAppShortcuts 收集全部 .lnk、用户指定目录里的可执行文件，并解析目标 exe 路径。
func scanAppShortcuts(prefs Preferences) ([]LauncherEntry, error) {
	var paths []string
	for _, root := range appShortcutRoots() {
		if root == "" {
			continue
		}
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // 单目录进不去/被删都不该终止整轮扫描
			}
			if !d.IsDir() && strings.EqualFold(filepath.Ext(path), ".lnk") {
				paths = append(paths, path)
			}
			return nil
		})
	}

	targets := resolveLnkTargets(paths)
	entries := make([]LauncherEntry, 0, len(paths))
	seenPath := make(map[string]bool, len(paths))
	for _, p := range paths {
		name := strings.TrimSuffix(filepath.Base(p), filepath.Ext(p))
		if name == "" {
			continue
		}
		entries = append(entries, LauncherEntry{Name: name, Path: p, Target: targets[p]})
		seenPath[strings.ToLower(p)] = true
	}
	// 名称去重留到打分之后不做：用户开始菜单里同名不同目标的快捷方式真实存在，
	// 让他在列表里看到两条自己选，比替他猜一条好。

	// 用户指定的额外目录：按后缀收可执行文件。同一个文件可能被 .lnk 指到，
	// 按全路径去重收进同一张 entries，靠小写路径判重（NTFS 本来就不区分大小写）。
	for _, e := range scanDirFiles(prefs.LauncherDirs, prefs.LauncherExts, seenPath) {
		entries = append(entries, e)
	}

	// UWP 补种：「所有应用」里那批没有物理 .lnk 的应用（计算器、新版记事本、
	// 商店装的应用等）靠 Get-StartApps 拿。这是一次 powershell 冷启动，跑在
	// 后台建索引的 goroutine 里不碰启动路径；拿不到只少这批，不拖累主索引。
	if raw, err := startAppsCSVOutput(); err != nil {
		log.Printf("启动器 UWP 补种跳过：%v", err)
	} else {
		before := len(entries)
		entries = mergeUWPApps(entries, parseStartAppsCSV(raw))
		log.Printf("启动器 UWP 补种：%d/%d 个", len(entries)-before, len(parseStartAppsCSV(raw)))
	}

	// 拼音面在合并之后统一生成：.lnk、指定目录与 UWP 三批条目同一条路，不会漏半边。
	for i := range entries {
		entries[i].PinyinFull, entries[i].PinyinShort = pinyinFaces(entries[i].Name)
	}
	return entries, nil
}

// 目录文件扫描的闸门：指定目录可能盖住整个盘，递归深度、单目录条目数与
// 总收录数都钉死上限，超了就停 —— 宁可少收，不可把索引 goroutine 挂死。
const (
	dirScanMaxDepth   = 6
	dirScanMaxEntries = 2000 // 单目录一次 readdir 最多翻多少项
	dirScanMaxTotal   = 20000
)

// scanDirFiles 按后缀递归收用户指定目录里的可执行文件。seen 是已收录的全路径集
// （小写），命中则跳过 —— 开始菜单的 .lnk 指到安装目录时，同一个 exe 不列两遍。
// 不存在的目录静默跳过：用户卸了软件、改了盘符，不该让索引报错。
func scanDirFiles(dirs, exts []string, seen map[string]bool) []LauncherEntry {
	if len(dirs) == 0 || len(exts) == 0 {
		return nil
	}
	want := make(map[string]bool, len(exts))
	for _, ex := range exts {
		if ex = strings.ToLower(strings.Trim(ex, ".")); ex != "" {
			want[ex] = true
		}
	}
	st := dirScanState{want: want, seen: seen}
	for _, dir := range dirs {
		if len(st.out) >= dirScanMaxTotal {
			log.Printf("启动器目录扫描达到上限 %d 条，后面的目录不再扫", dirScanMaxTotal)
			break
		}
		st.walk(dir, 0)
	}
	return st.out
}

type dirScanState struct {
	want map[string]bool
	seen map[string]bool
	out  []LauncherEntry
}

// walk 扫单个目录并递归子目录。三个闸门各自独立：深度超了不往下走，
// 单目录项数超了后半截不翻，总收录数超了直接停。软链/重解析目录不特别
// 处理 —— 深度上限已经封死了环路与超深嵌套。
func (st *dirScanState) walk(dir string, depth int) {
	list, err := os.ReadDir(dir)
	if err != nil {
		return // 进不去/已不存在：静默跳过
	}
	if len(list) > dirScanMaxEntries {
		list = list[:dirScanMaxEntries]
	}
	for _, d := range list {
		if len(st.out) >= dirScanMaxTotal {
			return
		}
		path := filepath.Join(dir, d.Name())
		if d.IsDir() {
			// 系统/隐藏目录约定（$RECYCLE.BIN 一类）不进。
			if strings.HasPrefix(d.Name(), "$") {
				continue
			}
			if depth < dirScanMaxDepth {
				st.walk(path, depth+1)
			}
			continue
		}
		ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(path), "."))
		if !st.want[ext] {
			continue
		}
		key := strings.ToLower(path)
		if st.seen[key] {
			continue
		}
		name := strings.TrimSuffix(d.Name(), filepath.Ext(path))
		if name == "" {
			continue
		}
		st.seen[key] = true
		// Target 也填自身：打分面的 exe 名与 Path 名同源，不解析、不白送一个空面。
		st.out = append(st.out, LauncherEntry{Name: name, Path: path, Target: path})
	}
}

// startAppsCSVOutput 跑一次 Get-StartApps 拿两列 CSV。先把控制台输出编码拉成
// UTF-8：Windows PowerShell 5.1 默认按系统代码页写管道，不转的话「百度网盘」
// 这类中文名回来就是乱码字节。
func startAppsCSVOutput() (string, error) {
	raw, err := runHidden("powershell.exe", 8*time.Second,
		"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command",
		"[Console]::OutputEncoding=[Text.Encoding]::UTF8; Get-StartApps | Select-Object Name,AppID | ConvertTo-Csv -NoTypeInformation")
	if err != nil {
		return "", fmt.Errorf("Get-StartApps 失败：%v", err)
	}
	s := strings.TrimPrefix(string(raw), "\ufeff")
	if !strings.Contains(s, "!") {
		return "", fmt.Errorf("Get-StartApps 输出里没有 UWP 行，当成失败")
	}
	return s, nil
}

// resolveLnkTargets 在一个 STA 上下文里解析全部 .lnk 的 TargetPath。
// 失败的文件从结果里缺席（调用方按空 target 继续）。
func resolveLnkTargets(paths []string) map[string]string {
	out := make(map[string]string, len(paths))
	if len(paths) == 0 {
		return out
	}
	// WScript.Shell 是 STA 组件，整个解析循环必须钉死在同一 OS 线程上。
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED); err != nil {
		log.Printf("启动器索引 CoInitialize 失败，跳过目标解析：%v", err)
		return out
	}
	defer ole.CoUninitialize()

	unk, err := oleutil.CreateObject("WScript.Shell")
	if err != nil {
		log.Printf("启动器索引无法创建 WScript.Shell，跳过目标解析：%v", err)
		return out
	}
	defer unk.Release()
	shell, err := unk.QueryInterface(ole.IID_IDispatch)
	if err != nil {
		log.Printf("启动器索引 WScript.Shell 取 IDispatch 失败，跳过目标解析：%v", err)
		return out
	}
	defer shell.Release()

	for _, p := range paths {
		v, err := oleutil.CallMethod(shell, "CreateShortcut", p)
		if err != nil {
			continue
		}
		tpv, err := oleutil.GetProperty(v.ToIDispatch(), "TargetPath")
		if err == nil {
			if s, ok := tpv.Value().(string); ok && s != "" {
				out[p] = s
			}
			tpv.Clear()
		}
		v.Clear()
	}
	return out
}

var (
	modShell32     = syscall.NewLazyDLL("shell32.dll")
	procShellExecW = modShell32.NewProc("ShellExecuteW")
	modShlwapi     = syscall.NewLazyDLL("shlwapi.dll")
	procAssocQuery = modShlwapi.NewProc("AssocQueryStringW")
	swShowNormal   = uintptr(1)
)

// seErrNoAssoc：ShellExecuteW 返回码 31（SE_ERR_NOASSOC）—— 该文件类型对给定
// 动词没有关联处理程序。文档只注册了 open、没注册 runas 时直接 runas 就拿这个。
const seErrNoAssoc = 31

// shellExecuteRaw 调一次 ShellExecuteW，verb 传 "runas" 即「以管理员身份运行」
// （UAC 弹由系统负责），空串走默认；params 非空则作为命令行参数带上。
// 返回原始返回值：>32 是成功句柄（err 为 nil），<=32 是 SE_ERR_* 错误码。
func shellExecuteRaw(path, params, verb string) (uintptr, error) {
	wide, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, fmt.Errorf("路径无法转宽字符：%s", path)
	}
	var verbPtr, paramsPtr *uint16
	if verb != "" {
		if verbPtr, err = syscall.UTF16PtrFromString(verb); err != nil {
			return 0, fmt.Errorf("动词无法转宽字符：%s", verb)
		}
	}
	if params != "" {
		if paramsPtr, err = syscall.UTF16PtrFromString(params); err != nil {
			return 0, fmt.Errorf("参数无法转宽字符：%s", params)
		}
	}
	r, _, e := procShellExecW.Call(0, uintptr(unsafe.Pointer(verbPtr)), uintptr(unsafe.Pointer(wide)), uintptr(unsafe.Pointer(paramsPtr)), 0, swShowNormal)
	if r <= 32 {
		return r, fmt.Errorf("ShellExecuteW(%s) 失败，错误码 %d（%v）", path, r, e)
	}
	return r, nil
}

// launcherShellExecute 用系统默认方式打开一条路径：参数、工作目录、提权弹窗
// 全部由 shell 处理，这正是「启动一个快捷方式」该有的语义，不自己起进程。
func launcherShellExecute(path string, verb string) error {
	_, err := shellExecuteRaw(path, "", verb)
	return err
}

// assocExecutable 用 shlwapi 解析某扩展名（如 ".png"）「open」动词对应的处理程序
// exe 全路径。走系统关联表（含 UserChoice），比手拆注册表 Unicode 安全。拿不到返空串。
func assocExecutable(ext string) string {
	if ext == "" {
		return ""
	}
	assocPtr, err := syscall.UTF16PtrFromString(ext)
	if err != nil {
		return ""
	}
	verbPtr, err := syscall.UTF16PtrFromString("open")
	if err != nil {
		return ""
	}
	buf := make([]uint16, 2048)
	cch := uint32(len(buf))
	// ASSOCF_NONE=0，ASSOCSTR_EXECUTABLE=2；S_OK==0 才算成功。
	ret, _, _ := procAssocQuery.Call(
		uintptr(0),
		uintptr(2),
		uintptr(unsafe.Pointer(assocPtr)),
		uintptr(unsafe.Pointer(verbPtr)),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&cch)),
	)
	if ret != 0 {
		return ""
	}
	return syscall.UTF16ToString(buf)
}

// launcherShellExecuteElevated 以管理员身份启动一条路径。先直接对目标走 runas：
// exe / 脚本 / 自带 runas 动词的 .lnk 与文档会在此弹 UAC 并成功。若拿回 NOASSOC
// （该文档类型只注册了 open、没注册 runas —— nomacs 这类第三方处理程序很常见），
// 退一步用 shlwapi 解析出处理程序 exe，再对该 exe 走 runas、把文档当参数带上。
// 其它错误码（UAC 被取消 = ACCESSDENIED 等）不重试，原样回传。
func launcherShellExecuteElevated(path string) error {
	r, err := shellExecuteRaw(path, "", "runas")
	if err == nil {
		return nil
	}
	if r != seErrNoAssoc {
		return err
	}
	exe := assocExecutable(strings.ToLower(filepath.Ext(path)))
	if exe == "" {
		return err // 解析不出处理程序，只能回传原始的 NOASSOC
	}
	if _, err2 := shellExecuteRaw(exe, `"`+path+`"`, "runas"); err2 != nil {
		return fmt.Errorf("%v（回退到处理程序 %s 提权也失败：%v）", err, exe, err2)
	}
	return nil
}
