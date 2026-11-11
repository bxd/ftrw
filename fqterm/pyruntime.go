package main

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

//go:embed pyrunner/runner.py
var runnerPySrc []byte

// pyruntime.go：文件管理器预览的 Python 插件运行时（替换 plugins.go 的桩）。
//
// 模型对齐 Total Commander 的「委派给渲染器」，但适配 WebView2：插件不画 UI，只把任意
// 文件转成前端已会显示的东西（image/audio/video/pdf/html/text）。每个插件跑在自己的
// venv 里、由一个独立的 JSON-RPC 子进程承载（解释器不同，无法共用一个进程）。
//
// 目录约定：<程序目录(exe 所在)>\plugins\<name>\  { plugin.json, main.py, requirements.txt?, .venv/, _work/ }

// PluginArtifact 是插件回传的预览产物。path 是插件写到 _work 的本机产物文件，Go 会签成 URL。
type PluginArtifact struct {
	Kind  string `json:"kind"`
	Path  string `json:"path"`
	URL   string `json:"url"`
	HTML  string `json:"html"`
	Text  string `json:"text"`
	Title string `json:"title"`
}

// pluginAction 是插件声明的一条有副作用的动作（解压/压缩等），在「扩展」菜单里列出。
type pluginAction struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type pluginManifest struct {
	ID           string         `json:"id"`
	Title        string         `json:"title"`
	Extensions   []string       `json:"extensions"`
	Entry        string         `json:"entry"`
	Func         string         `json:"func"`
	ActionFunc   string         `json:"actionFunc"`
	Actions      []pluginAction `json:"actions"`
	Requirements string         `json:"requirements"`
}

type pyLine struct {
	data string
	err  error
}

type pyWorker struct {
	id, dir, entry, fn, af string
	cmd                    *exec.Cmd
	stdin                  io.WriteCloser
	lines                  chan pyLine
	mu                     sync.Mutex
	seq                    int
	dead                   bool
	restarts               int
	lastUsed               time.Time
}

type pyRuntime struct {
	mu        sync.Mutex
	root      string
	pythonExe string
	available bool
	probed    bool

	wmu     sync.Mutex
	workers map[string]*pyWorker
}

func newPyRuntime() *pyRuntime {
	r := &pyRuntime{
		// 插件放在程序目录（exe 所在目录）下的 plugins\<name>，便携、随应用一起拷贝。
		root:    filepath.Join(executableDir(), "plugins"),
		workers: map[string]*pyWorker{},
	}
	go r.reaper()
	return r
}

// probe 检测系统 Python（一次性、缓存）。没有就 available=false，插件档静默禁用。
func (r *pyRuntime) probe() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.probeLocked()
}

func (r *pyRuntime) probeLocked() bool {
	if r.probed {
		return r.available
	}
	r.probed = true
	// 逐个实测 PATH 上所有 python 可执行，第一个真能跑的胜出。
	// 不能只靠 exec.LookPath("python")：Windows 上 Microsoft Store 的 python 别名常排在 PATH 前，
	// LookPath 只回第一个命中（那个跑不起来的别名），就会误报「无 Python」而漏掉后面真正的解释器（如 miniconda）。
	for _, p := range pythonCandidates() {
		if out, err := runLimited(p, 15*time.Second, "-c", "import sys;print(sys.version)"); err == nil && strings.Contains(out, ".") {
			r.pythonExe = p
			r.available = true
			log.Printf("预览插件运行时：检测到 Python %s（%s）", p, firstLine(out))
			return true
		}
		log.Printf("预览插件运行时：%s 存在但不可用，跳过", p)
	}
	log.Printf("预览插件运行时：未检测到可用 Python，扩展预览禁用（原生媒体预览不受影响）")
	return false
}

// pythonCandidates 枚举 PATH 各目录里的 python/python3 可执行（去重），再补 py 启动器。
// 保留 PATH 顺序，但配合 probeLocked 的逐个实测，能跳过商店别名这类跑不起来的命中。
func pythonCandidates() []string {
	var out []string
	seen := map[string]bool{}
	tryAdd := func(p string) {
		if p == "" {
			return
		}
		if fi, err := os.Stat(p); err != nil || fi.IsDir() {
			return
		}
		key := strings.ToLower(p)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, p)
	}
	names := []string{"python", "python3"}
	if runtime.GOOS == "windows" {
		names = []string{"python.exe", "python3.exe"}
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		for _, n := range names {
			tryAdd(filepath.Join(dir, n))
		}
	}
	if p, err := exec.LookPath("py"); err == nil {
		tryAdd(p)
	}
	return out
}

func (r *pyRuntime) pluginsDir() string { return r.root }

// ensureRunner 把内嵌的 runner.py 落到 root/_runtime/runner.py（内容变了就覆写）。
func (r *pyRuntime) ensureRunner() (string, error) {
	dir := filepath.Join(r.root, "_runtime")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "runner.py")
	if cur, err := os.ReadFile(path); err == nil && string(cur) == string(runnerPySrc) {
		return path, nil
	}
	if err := os.WriteFile(path, runnerPySrc, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// plugins 列出可用预览插件（只读 plugin.json，绝不启动解释器）。无 Python 返回空。
func (r *pyRuntime) plugins() []PluginTool {
	if !r.probe() {
		return nil
	}
	entries, err := os.ReadDir(r.root)
	if err != nil {
		return nil // 目录还不存在，正常
	}
	var out []PluginTool
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), "_") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		m, err := readManifest(filepath.Join(r.root, e.Name()))
		if err != nil || len(m.Extensions) == 0 {
			continue
		}
		id := m.ID
		if id == "" {
			id = e.Name()
		}
		title := m.Title
		if title == "" {
			title = e.Name()
		}
		out = append(out, PluginTool{
			ID:             id,
			Title:          title,
			Kind:           "preview",
			NeedsSelection: true,
			Extensions:     normalizeExts(m.Extensions),
			Actions:        publicActions(m.Actions),
		})
	}
	return out
}

func readManifest(dir string) (pluginManifest, error) {
	var m pluginManifest
	b, err := os.ReadFile(filepath.Join(dir, "plugin.json"))
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("plugin.json 解析失败：%v", err)
	}
	if m.Entry == "" {
		m.Entry = "main.py"
	}
	if m.Func == "" {
		m.Func = "preview"
	}
	if m.ActionFunc == "" {
		m.ActionFunc = "action"
	}
	if m.Requirements == "" {
		m.Requirements = "requirements.txt"
	}
	return m, nil
}

// publicActions 把清单里的动作映成给前端的列表（过滤掉无 id 的）。
func publicActions(in []pluginAction) []PluginAction {
	if len(in) == 0 {
		return nil
	}
	out := make([]PluginAction, 0, len(in))
	for _, a := range in {
		if a.ID == "" {
			continue
		}
		title := a.Title
		if title == "" {
			title = a.ID
		}
		out = append(out, PluginAction{ID: a.ID, Title: title})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func normalizeExts(in []string) []string {
	out := make([]string, 0, len(in))
	for _, e := range in {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if !strings.HasPrefix(e, ".") {
			e = "." + e
		}
		out = append(out, e)
	}
	return out
}

func (r *pyRuntime) manifestByID(id string) (pluginManifest, string, bool) {
	entries, err := os.ReadDir(r.root)
	if err != nil {
		return pluginManifest{}, "", false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(r.root, e.Name())
		m, err := readManifest(dir)
		if err != nil {
			continue
		}
		mid := m.ID
		if mid == "" {
			mid = e.Name()
		}
		if mid == id {
			return m, dir, true
		}
	}
	return pluginManifest{}, "", false
}

func venvPythonExe(dir string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(dir, ".venv", "Scripts", "python.exe")
	}
	return filepath.Join(dir, ".venv", "bin", "python")
}

// ensureVenv 保证插件有可用解释器：缺 .venv 就建，有 requirements.txt 就 pip 装。
// 任一步失败退化成系统 Python（best-effort，让插件至少有机会用 stdlib 跑）。
func (r *pyRuntime) ensureVenv(dir, requirements string) string {
	py := venvPythonExe(dir)
	if _, err := os.Stat(py); err == nil {
		return py
	}
	// 建 venv（首运行较慢）。
	if out, err := runLimited(r.pythonExe, 180*time.Second, "-m", "venv", filepath.Join(dir, ".venv")); err != nil {
		log.Printf("建 venv 失败（%s）：%v —— 退回系统 Python", dir, firstLine(out))
		return r.pythonExe
	}
	reqPath := filepath.Join(dir, requirements)
	if fi, err := os.Stat(reqPath); err == nil && !fi.IsDir() {
		if out, err := runLimited(py, 600*time.Second, "-m", "pip", "install", "--disable-pip-version-check", "-r", reqPath); err != nil {
			log.Printf("pip 安装依赖失败（%s）：%v", dir, firstLine(out))
			// 仍返回 venv python：可能依赖已在、或插件只需 stdlib。
		}
	}
	return py
}

// runLimited 跑一次性命令、限时，返回合并输出。
func runLimited(name string, timeout time.Duration, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	hideConsoleWindow(cmd)
	done := make(chan struct{})
	var out []byte
	var err error
	go func() {
		out, err = cmd.CombinedOutput()
		close(done)
	}()
	select {
	case <-done:
		return string(out), err
	case <-time.After(timeout):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return string(out), fmt.Errorf("命令超时（%v）", timeout)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func (r *pyRuntime) getWorker(id, dir, entry, fn, af, pythonExe, runnerPath string) (*pyWorker, error) {
	r.wmu.Lock()
	defer r.wmu.Unlock()
	if w, ok := r.workers[id]; ok && !w.dead {
		return w, nil
	}
	w := &pyWorker{id: id, dir: dir, entry: entry, fn: fn, af: af, lastUsed: time.Now()}
	if err := w.spawn(pythonExe, runnerPath); err != nil {
		return nil, err
	}
	r.workers[id] = w
	return w, nil
}

func (w *pyWorker) spawn(pythonExe, runnerPath string) error {
	cmd := exec.Command(pythonExe, "-u", runnerPath, w.dir, w.entry, w.fn, w.af)
	hideConsoleWindow(cmd)
	cmd.Env = append(os.Environ(), "PYTHONIOENCODING=utf-8", "PYTHONUNBUFFERED=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	w.cmd = cmd
	w.stdin = stdin
	w.lines = make(chan pyLine, 1)
	w.dead = false
	w.lastUsed = time.Now()

	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
		for sc.Scan() {
			w.lines <- pyLine{data: sc.Text()}
		}
		w.lines <- pyLine{err: io.EOF}
	}()
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			log.Printf("[py:%s] %s", w.id, sc.Text())
		}
	}()
	return nil
}

func (w *pyWorker) kill() {
	if w.cmd != nil && w.cmd.Process != nil {
		_ = w.cmd.Process.Kill()
	}
	if w.stdin != nil {
		_ = w.stdin.Close()
	}
	w.dead = true
}

type rpcReq struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      int            `json:"id"`
	Method  string         `json:"method"`
	Params  map[string]any `json:"params"`
}

type rpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// request 发一条 RPC 并等回应（method 可为 preview/action）；worker 已死则按需重 spawn，超过重启上限拒绝。
func (r *pyRuntime) request(w *pyWorker, pythonExe, runnerPath, method string, params map[string]any, timeout time.Duration) (json.RawMessage, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.dead {
		if w.restarts >= 2 {
			return nil, fmt.Errorf("插件 %s 反复崩溃，已暂停使用", w.id)
		}
		w.restarts++
		if err := w.spawn(pythonExe, runnerPath); err != nil {
			return nil, fmt.Errorf("重启插件进程失败：%v", err)
		}
	}

	w.seq++
	id := w.seq
	payload, _ := json.Marshal(rpcReq{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if _, err := w.stdin.Write(append(payload, '\n')); err != nil {
		w.kill()
		return nil, fmt.Errorf("写入请求失败：%v", err)
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case l := <-w.lines:
		if l.err != nil {
			w.kill()
			return nil, fmt.Errorf("插件进程已退出")
		}
		var resp rpcResp
		if err := json.Unmarshal([]byte(l.data), &resp); err != nil {
			return nil, fmt.Errorf("响应解析失败：%v（原文：%s）", err, l.data)
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("插件报错：%s", resp.Error.Message)
		}
		w.lastUsed = time.Now()
		return resp.Result, nil
	case <-timer.C:
		w.kill()
		return nil, fmt.Errorf("插件响应超时（%v）", timeout)
	}
}

// prepare 探测 → 定位清单 → 落 runner → 备 venv → 起/复用 worker。preview 与 runAction 共用。
func (r *pyRuntime) prepare(id string) (pythonExe, runnerPath string, w *pyWorker, err error) {
	if !r.probe() {
		return "", "", nil, fmt.Errorf("未检测到 Python，无法使用插件")
	}
	m, dir, ok := r.manifestByID(id)
	if !ok {
		return "", "", nil, fmt.Errorf("找不到插件：%s", id)
	}
	runnerPath, err = r.ensureRunner()
	if err != nil {
		return "", "", nil, fmt.Errorf("准备 runner 失败：%v", err)
	}
	pythonExe = r.ensureVenv(dir, m.Requirements)
	w, err = r.getWorker(id, dir, m.Entry, m.Func, m.ActionFunc, pythonExe, runnerPath)
	if err != nil {
		return "", "", nil, fmt.Errorf("启动插件失败：%v", err)
	}
	return pythonExe, runnerPath, w, nil
}

// preview 定位插件 → 一次 preview RPC → 给产物文件签名。
func (r *pyRuntime) preview(s *FileService, id, path string, ctx PluginContext) (PluginArtifact, error) {
	py, runnerPath, w, err := r.prepare(id)
	if err != nil {
		return PluginArtifact{}, err
	}
	raw, err := r.request(w, py, runnerPath, "preview", map[string]any{"path": path, "ctx": ctx}, 120*time.Second)
	if err != nil {
		return PluginArtifact{}, err
	}
	var art PluginArtifact
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &art); err != nil {
			return art, fmt.Errorf("产物解析失败：%v", err)
		}
	}
	if art.Path != "" {
		if url, err := s.SignPath(art.Path); err == nil {
			art.URL = url
		}
	}
	return art, nil
}

// runAction 定位插件 → 一次 action RPC（解压/压缩等有副作用），回文本结果。给足超时因为大压缩包耗时。
func (r *pyRuntime) runAction(id, action, path string, ctx PluginContext) (PluginResult, error) {
	py, runnerPath, w, err := r.prepare(id)
	if err != nil {
		return PluginResult{}, err
	}
	raw, err := r.request(w, py, runnerPath, "action", map[string]any{"name": action, "path": path, "ctx": ctx}, 600*time.Second)
	if err != nil {
		return PluginResult{}, err
	}
	var res struct {
		Message string `json:"message"`
	}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &res)
	}
	if res.Message == "" {
		res.Message = "完成"
	}
	return PluginResult{Message: res.Message}, nil
}

// reaper 定期回收空闲 worker，控制常驻 python 进程数量。
func (r *pyRuntime) reaper() {
	tick := time.NewTicker(2 * time.Minute)
	defer tick.Stop()
	for range tick.C {
		r.wmu.Lock()
		for id, w := range r.workers {
			if time.Since(w.lastUsed) > 5*time.Minute {
				w.mu.Lock()
				w.kill()
				w.mu.Unlock()
				delete(r.workers, id)
			}
		}
		r.wmu.Unlock()
	}
}
