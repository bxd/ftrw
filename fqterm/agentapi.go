package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// Agent API：给本地 AI agent 用的 127.0.0.1 HTTP 接口（能力对齐一个坐在屏幕前的
// 操作员：读屏、发键、发文字、等待画面、跑应用命令、管标签）。
//
// 设计要点：
//   - 只听 127.0.0.1。屏上有密码回显、操作上花的是用户的 BBS 账号，任何本机进程
//     都能驱动这件事必须用户显式开启（偏好默认关）并带 token。
//   - 屏的真值在渲染端（xterm.js 缓冲区），BBS 排版语义只有它知道，所以读屏/发键
//     一律经 Wails 事件往返前端执行（见 frontend/src/agentbridge.ts），后端不做第二
//     套终端解析器，避免"agent 看到的"和"用户看到的"不一致。
//   - 开发通路：环境变量 FTRW_AGENT_API="端口" 或 "端口:token" 可在不开设置页的
//     情况下临时开启，方便脚本化验收；正式使用仍走设置页。
//   - Shell 标签（protocolType==3）可读回滚区；BBS 标签只返回当前可见屏幕 —— 这是
//     前端 screen 处理器按模式分流的语义，HTTP 层如实透传。

const defaultAgentApiPort = 37837 // 手机键盘拼 FQTERM，好记且躲开常见开发端口

// agentAPIEnv 非空即临时强制开启：值形如 "37837" 或 "37837:abcdef…"。
const agentAPIEnv = "FTRW_AGENT_API"

// AgentCmd 是 "agent:cmd" 事件的载荷：后端 HTTP 线程 → 前端的往返请求。
// Payload 用 JSON 字符串而不是 any，省掉泛型事件注册与前端二次解码的口径问题。
type AgentCmd struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Payload string `json:"payload"`
}

type agentResp struct {
	payload string
	errMsg  string
}

type agentConfig struct {
	enabled bool
	port    int
	token   string
}

// agentConfigFromPrefs 把偏好折成一份可比对的生效配置（改动检测用整体相等）。
func agentConfigFromPrefs(p Preferences) agentConfig {
	c := agentConfig{enabled: p.AgentApiEnabled, port: p.AgentApiPort, token: p.AgentApiToken}
	if c.port <= 0 {
		c.port = defaultAgentApiPort
	}
	return c
}

// envAgentConfig 解析开发覆盖变量；ok=false 表示没设或设得不合法（继续用偏好）。
func envAgentConfig() (c agentConfig, ok bool) {
	v := strings.TrimSpace(os.Getenv(agentAPIEnv))
	if v == "" {
		return agentConfig{}, false
	}
	portStr, tokenStr, hasToken := strings.Cut(v, ":")
	port, err := strconv.Atoi(strings.TrimSpace(portStr))
	if err != nil || port < 1 || port > 65535 {
		log.Printf("agentapi: 忽略 %s=%q（端口不合法）", agentAPIEnv, v)
		return agentConfig{}, false
	}
	tokenStr = strings.TrimSpace(tokenStr)
	if !hasToken || tokenStr == "" {
		tokenStr = "ftrw-dev" // 开发覆盖：短 token 也总比没有强（只听回环）
	}
	return agentConfig{enabled: true, port: port, token: tokenStr}, true
}

func newAgentToken() string {
	var b [20]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 挂了就退到时间戳异或：宁可难看也别生成空 token。
		n := time.Now().UnixNano()
		for i := range b {
			b[i] = byte(n >> (8 * i))
		}
	}
	return hex.EncodeToString(b[:])
}

// AgentAPI 持有监听器与"偏好 → 监听"的对账状态。生命周期由 main 接线：
// 启动时 Sync 一次，之后每次 SetPreferences 变化再 Sync（见 configservice.OnAgentChanged）。
type AgentAPI struct {
	store *Store
	ts    *TerminalService

	mu     sync.Mutex
	cfg    agentConfig
	server *http.Server
	ln     net.Listener
}

func NewAgentAPI(store *Store, ts *TerminalService) *AgentAPI {
	return &AgentAPI{store: store, ts: ts}
}

// prefs 现取一份偏好快照（与 ConfigService.GetPreferences 同一把锁，读的是同一内存）。
func (a *AgentAPI) prefs() Preferences {
	a.store.mu.RLock()
	defer a.store.mu.RUnlock()
	return a.store.Prefs
}

// Sync 让监听状态追上当前配置（幂等；env 覆盖优先于偏好）。
func (a *AgentAPI) Sync() {
	cfg := agentConfigFromPrefs(a.prefs())
	if env, ok := envAgentConfig(); ok {
		cfg = env
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if cfg == a.cfg && (cfg.enabled || a.server == nil) {
		return
	}
	if a.server != nil {
		a.server.Close()
		a.server, a.ln = nil, nil
		a.cfg = agentConfig{}
	}
	if !cfg.enabled {
		log.Printf("agentapi: 已停用")
		return
	}
	// 只听回环：这台机器上的远程主机谁也够不着它。
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", cfg.port))
	if err != nil {
		log.Printf("agentapi: 端口 %d 监听失败（API 不生效）：%v", cfg.port, err)
		a.cfg = cfg
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ftrw/status", a.handleStatus)
	// 文档端点免鉴权：让刚探测到端口的 agent 无需先有 token 就能自助读懂全部接口。
	mux.HandleFunc("GET /ftrw/doc", a.handleDoc)
	mux.HandleFunc("GET /ftrw/sites", a.auth(a.handleSites))
	mux.HandleFunc("GET /ftrw/tabs", a.auth(a.handleTabs))
	mux.HandleFunc("GET /ftrw/screen", a.auth(a.handleScreen))
	mux.HandleFunc("POST /ftrw/send", a.auth(a.handleSend))
	mux.HandleFunc("POST /ftrw/wait", a.auth(a.handleWait))
	mux.HandleFunc("POST /ftrw/command", a.auth(a.handleCommand))
	mux.HandleFunc("POST /ftrw/tab/activate", a.auth(a.handleActivate))
	mux.HandleFunc("POST /ftrw/tab/close", a.auth(a.handleCloseTab))
	mux.HandleFunc("POST /ftrw/connect", a.auth(a.handleConnect))
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	a.server, a.ln, a.cfg = srv, ln, cfg
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("agentapi: 服务循环退出：%v", err)
		}
	}()
	log.Printf("agentapi: 已启用 http://127.0.0.1:%d/ftrw/（token 鉴权）", cfg.port)
}

// --- 鉴权与响应helper ---

func (a *AgentAPI) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		token := a.cfg.token
		a.mu.Unlock()
		got := strings.TrimSpace(r.Header.Get("X-FTRW-Token"))
		if got == "" {
			got = strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		}
		if token == "" || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			writeAgentJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "token 缺失或不正"})
			return
		}
		next(w, r)
	}
}

func writeAgentJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// ok/err 的统一出口：HTTP 层尽量把前端的错误原文带给 agent（agent 靠字面排错）。
func (a *AgentAPI) reply(w http.ResponseWriter, data json.RawMessage, err error) {
	if err != nil {
		writeAgentJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeAgentJSON(w, http.StatusOK, map[string]any{"ok": true, "data": json.RawMessage(data)})
}

// --- 路由 ---

// handleStatus 不需要 token：agent 用它探测 API 开没开、该说哪个端口。
func (a *AgentAPI) handleStatus(w http.ResponseWriter, _ *http.Request) {
	a.mu.Lock()
	cfg := a.cfg
	a.mu.Unlock()
	writeAgentJSON(w, http.StatusOK, map[string]any{"ok": true, "data": map[string]any{
		"enabled": cfg.enabled, "port": cfg.port,
	}})
}

func (a *AgentAPI) handleTabs(w http.ResponseWriter, _ *http.Request) {
	// 标签的活跃态/尺寸/协议模式都住在前端，统一走往返拿一份全的。
	data, err := a.request("tabs", map[string]any{}, 10*time.Second)
	a.reply(w, data, err)
}

// handleSites 列存站名：connect 得知道名字才能连，而名字住在配置里，后端直接答，不走往返。
func (a *AgentAPI) handleSites(w http.ResponseWriter, _ *http.Request) {
	a.store.mu.RLock()
	out := make([]map[string]any, 0, len(a.store.Sites))
	for _, s := range a.store.Sites {
		out = append(out, map[string]any{
			"name": s.Name, "host": s.Host, "port": s.Port,
			"protocolType": s.ProtocolType, "group": s.Group,
		})
	}
	a.store.mu.RUnlock()
	writeAgentJSON(w, http.StatusOK, map[string]any{"ok": true, "data": map[string]any{"sites": out}})
}

func (a *AgentAPI) handleScreen(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	payload := map[string]any{"tab": q.Get("tab")}
	if v := q.Get("from"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			payload["from"] = n
		}
	}
	if v := q.Get("count"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			payload["count"] = n
		}
	}
	data, err := a.request("screen", payload, 10*time.Second)
	a.reply(w, data, err)
}

// sendReq 的 text 与 keys 二选一（也允许都有：先段文字再一串键，正好一次填表提交）。
type sendReq struct {
	Tab        string `json:"tab"`
	Text       string `json:"text"`
	Keys       string `json:"keys"`
	KeyDelayMs int    `json:"keyDelayMs"`
	TimeoutMs  int    `json:"timeoutMs"`
}

func (a *AgentAPI) handleSend(w http.ResponseWriter, r *http.Request) {
	var req sendReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAgentJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "请求体不是合法 JSON: " + err.Error()})
		return
	}
	if req.Text == "" && req.Keys == "" {
		writeAgentJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "text 与 keys 至少要给一个"})
		return
	}
	// 前端逐键要等画面反应（默认 150ms/键），HTTP 侧超时给足再加减余量。
	timeout := 15 * time.Second
	if req.TimeoutMs > 0 && req.TimeoutMs < 120000 {
		timeout = time.Duration(req.TimeoutMs+10000) * time.Millisecond
	}
	data, err := a.request("send", map[string]any{
		"tab": req.Tab, "text": req.Text, "keys": req.Keys, "keyDelayMs": req.KeyDelayMs,
	}, timeout)
	a.reply(w, data, err)
}

type waitReq struct {
	Tab       string `json:"tab"`
	Pattern   string `json:"pattern"`
	Regex     bool   `json:"regex"`
	TimeoutMs int    `json:"timeoutMs"`
	Full      bool   `json:"full"` // shell 标签连回滚区一起找
}

func (a *AgentAPI) handleWait(w http.ResponseWriter, r *http.Request) {
	var req waitReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAgentJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "请求体不是合法 JSON: " + err.Error()})
		return
	}
	if req.Pattern == "" {
		writeAgentJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "pattern 不能为空"})
		return
	}
	if req.TimeoutMs <= 0 {
		req.TimeoutMs = 5000
	}
	if req.TimeoutMs > 60000 {
		req.TimeoutMs = 60000
	}
	// 前端真的等满 timeoutMs 才回，HTTP 侧留 5 秒缓冲。
	data, err := a.request("wait", map[string]any{
		"tab": req.Tab, "pattern": req.Pattern, "regex": req.Regex,
		"timeoutMs": req.TimeoutMs, "full": req.Full,
	}, time.Duration(req.TimeoutMs)*time.Millisecond+5*time.Second)
	a.reply(w, data, err)
}

func (a *AgentAPI) handleCommand(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Spec string `json:"spec"` // "id" 或 "id:参数"，与热键绑定表同一口径
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Spec == "" {
		writeAgentJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "需要 {\"spec\":\"命令id[:参数]\"}"})
		return
	}
	data, err := a.request("command", map[string]any{"spec": body.Spec}, 15*time.Second)
	a.reply(w, data, err)
}

func (a *AgentAPI) handleActivate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Tab string `json:"tab"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAgentJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "请求体不是合法 JSON"})
		return
	}
	data, err := a.request("activate", map[string]any{"tab": body.Tab}, 10*time.Second)
	a.reply(w, data, err)
}

func (a *AgentAPI) handleCloseTab(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Tab string `json:"tab"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAgentJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "请求体不是合法 JSON"})
		return
	}
	data, err := a.request("close", map[string]any{"tab": body.Tab}, 10*time.Second)
	a.reply(w, data, err)
}

// handleConnect 按站点名开新标签：拨号在前端那侧慢慢磨（代理、自动登录），超时给足。
func (a *AgentAPI) handleConnect(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Site string `json:"site"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Site == "" {
		writeAgentJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "需要 {\"site\":\"存站名\"}"})
		return
	}
	data, err := a.request("connect", map[string]any{"site": body.Site}, 45*time.Second)
	a.reply(w, data, err)
}

// --- 前端往返 ---

// request 把一条命令发给渲染端并等回执。pending 的登记表住在 TerminalService：
// 前端完成时调 main.TerminalService.AgentReply（动态方法调用，无需新生成绑定）。
func (a *AgentAPI) request(kind string, payload map[string]any, timeout time.Duration) (json.RawMessage, error) {
	app := application.Get()
	if app == nil {
		return nil, errors.New("应用尚未就绪")
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	reqID := newSessionID()
	ch := a.ts.trackAgent(reqID)
	defer a.ts.untrackAgent(reqID)
	app.Event.Emit("agent:cmd", &AgentCmd{ID: reqID, Kind: kind, Payload: string(buf)})
	select {
	case r := <-ch:
		if r.errMsg != "" {
			return nil, errors.New(r.errMsg)
		}
		if len(r.payload) == 0 {
			return json.RawMessage("{}"), nil
		}
		return json.RawMessage(r.payload), nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("前端应答超时（%v），渲染端可能卡住", timeout)
	}
}
