package main

// Agent API 自述文档。单一数据源 buildAgentApiDoc()，既能渲染成 Markdown
// （GET /ftrw/doc，默认，方便 LLM/人直接读并 curl），也能输出结构化 JSON
// （GET /ftrw/doc?format=json，方便程序解析）。文档随二进制编译、不依赖外部
// 文件；运行时把真实监听端口注入示例，agent 拿到即可直接照抄调用。
//
// /ftrw/doc 与 /ftrw/status 一样免 token：让刚探测到端口的 agent 无需先有凭据
// 就能读懂「怎么鉴权、有哪些接口、怎么发帖回帖」，这是本端点存在的意义。

import (
	"fmt"
	"net/http"
	"strings"
)

const (
	agentDocVersion = "v1"
	agentDocFence   = "```"
)

// docField 描述一个查询参数或请求体字段。
type docField struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Required bool   `json:"required,omitempty"`
	Desc     string `json:"desc"`
}

// docEndpoint 描述一个 HTTP 端点。
type docEndpoint struct {
	Method   string     `json:"method"`
	Path     string     `json:"path"`
	Auth     bool       `json:"auth"`
	Summary  string     `json:"summary"`
	Query    []docField `json:"query,omitempty"`
	Body     []docField `json:"body,omitempty"`
	Response string     `json:"response"`
	Example  string     `json:"example"`
}

type agentApiDocModel struct {
	Title       string        `json:"title"`
	Version     string        `json:"version"`
	Description string        `json:"description"`
	BaseURL     string        `json:"baseUrl"`
	Auth        string        `json:"auth"`
	Envelope    string        `json:"envelope"`
	Conventions []string      `json:"conventions"`
	KeyNotation string        `json:"keyNotation"`
	Quickstart  []string      `json:"quickstart"`
	Endpoints   []docEndpoint `json:"endpoints"`
}

// buildAgentApiDoc 用真实端口组装一份完整文档模型（示例里的 base 已代入端口）。
func buildAgentApiDoc(port int) agentApiDocModel {
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	hdr := `-H "X-FTRW-Token: <TOKEN>"`
	jct := `-H "Content-Type: application/json"`

	return agentApiDocModel{
		Title:   "FTRW Agent API",
		Version: agentDocVersion,
		Description: "FTRW 内置的本地 HTTP 接口，让 AI agent 像坐在屏幕前的操作员一样驱动终端里的" +
			"远程会话（读屏 / 发键 / 发文字 / 等屏 / 跑应用命令 / 管标签 / 连接站点）。" +
			"只监听 127.0.0.1；屏幕真值取自渲染端 xterm.js，BBS 模式下读屏只返回当前可见区域。",
		BaseURL: base,
		Auth: "除 GET /ftrw/status 与 GET /ftrw/doc 外，所有端点都要鉴权。在请求头带 " +
			"`X-FTRW-Token: <token>` 或 `Authorization: Bearer <token>`。token 来自设置页或环境变量 " +
			"FTRW_AGENT_API=\"端口:token\"。缺 token 返回 401 {\"ok\":false,\"error\":\"token 缺失或不正\"}。",
		Envelope: "统一信封。成功：HTTP 200 `{\"ok\":true,\"data\":<结果>}`。" +
			"失败：HTTP 4xx/5xx `{\"ok\":false,\"error\":\"<中文原因>}`（请求体非法 400，前端往返失败/超时 502）。",
		Conventions: []string{
			"需先在设置页开启 Agent API，或用环境变量 FTRW_AGENT_API=\"37837:你的token\" 临时强制开启（仅本机回环可达）。",
			"冷启动顺序：先 GET /ftrw/status 探测端口是否开着 → GET /ftrw/tabs 拿 tab id → 再操作。",
			"几乎所有带 tab 的端点都可省略 tab：省略即对「当前活动标签」操作。tab id 从 /ftrw/tabs 取。",
			"POST /ftrw/send 是同步往返：返回 {\"sent\":N} 时画面已更新，可立刻读屏或 wait，无需额外轮询。",
			"逐键发送在键之间留 keyDelayMs（默认 150ms）给远端刷新；操作多级菜单/提示时建议「一步一停」，读屏确认后再发下一步。",
			"protocolType==3 为 shell 标签（可读回滚区）；其余为 BBS 标签（只返回可见屏）。",
			"pwsh/Windows 下内联 JSON 请用「单引号」包裹 -d '{...}'；中文正文建议写进 UTF-8 文件后 --data-binary @file，避免控制台编码乱码。",
		},
		KeyNotation: "keys 字段是 Emacs 风格记法：用单个空格分隔多个 token，如 \"Up Down RET\"、\"C-x\"、\"M-x k\"。" +
			"命名键：RET(=Enter) / ESC / TAB / BS / DEL / INS / SPC / HOME / END / PGUP / PGDN / UP / DOWN / LEFT / RIGHT / F1…F24。" +
			"修饰前缀 C-(Ctrl) M-(Alt) S-(Shift)，可组合如 C-M-x；字母大小写不敏感；可打印字符直接写。" +
			"空格本身是分隔符，需要发空格写 <SPC>。特殊键也可用尖括号写法（<RET> 等价 RET）。" +
			"若某个 token 命中了用户设置的热键绑定，会被当作应用命令执行（与真人按键同一路径），否则编码后发往远端。",
		Quickstart: []string{
			"GET /ftrw/status —— 确认 API 已启用、拿到端口。",
			"GET /ftrw/tabs —— 列出标签，取一个 tab id（记下 protocolType 判断 BBS/shell）。",
			"GET /ftrw/screen?tab=<id> —— 读当前画面（lines 数组，每行含 y 绝对行号与 text）。",
			"POST /ftrw/send {tab,keys} —— 发导航键；例如 {\"keys\":\"Down Down RET\"}。",
			"POST /ftrw/send {tab,text} —— 写入一段文字（走站点编码，适合 ASCII；中文见上）。",
			"POST /ftrw/wait {tab,pattern} —— 阻塞等某行文字出现（如等提示 \"站内\"），超时返回 matched:false。",
			"重复 screen→send→wait 直到完成；驱动 BBS 回帖见文档末尾「典型流程」。",
		},
		Endpoints: []docEndpoint{
			{
				Method: "GET", Path: "/ftrw/status", Auth: false,
				Summary:  "探测 Agent API 是否开启及监听端口（无需 token，agent 冷启动第一站）。",
				Response: `{"ok":true,"data":{"enabled":true,"port":` + fmt.Sprint(port) + `}}`,
				Example:  `curl -s ` + base + `/ftrw/status`,
			},
			{
				Method: "GET", Path: "/ftrw/sites", Auth: true,
				Summary:  "列出已保存的站点（connect 需要站名，站名来自这里）。",
				Response: `{"ok":true,"data":{"sites":[{"name":"水木","host":"bbs.mysmth.net","port":23,"protocolType":0,"group":"BBS"}]}}`,
				Example:  `curl -s ` + hdr + ` ` + base + `/ftrw/sites`,
			},
			{
				Method: "GET", Path: "/ftrw/tabs", Auth: true,
				Summary:  "列出所有标签：id、标题、连接状态、是否活动、尺寸、协议模式、站点名。",
				Response: `{"ok":true,"data":{"tabs":[{"index":0,"id":"<TABID>","title":"水木","status":"connected","active":true,"cols":90,"rows":24,"protocolType":0,"siteName":"水木"}]}}`,
				Example:  `curl -s ` + hdr + ` ` + base + `/ftrw/tabs`,
			},
			{
				Method: "GET", Path: "/ftrw/screen", Auth: true,
				Summary: "读取某标签的终端画面。BBS 只返回可见区；shell 可用 from/count 读回滚区。",
				Query: []docField{
					{Name: "tab", Type: "string", Desc: "标签 id；省略=当前活动标签"},
					{Name: "from", Type: "int", Desc: "仅 shell：起始绝对行号（读回滚区）"},
					{Name: "count", Type: "int", Desc: "仅 shell：读取的行数"},
				},
				Response: `{"ok":true,"data":{"mode":"bbs","cols":90,"rows":24,"totalLines":29,"viewport":{"start":5,"end":29},"cursor":{"x":72,"y":28},"status":"connected","lines":[{"y":5,"text":"...","wrapped":false}]}}`,
				Example:  `curl -s ` + hdr + ` "` + base + `/ftrw/screen?tab=<TABID>"`,
			},
			{
				Method: "POST", Path: "/ftrw/send", Auth: true,
				Summary: "向远端发送文字和/或键序列（同步，返回即已生效）。text 与 keys 至少给一个。",
				Body: []docField{
					{Name: "tab", Type: "string", Desc: "标签 id；省略=当前活动标签"},
					{Name: "text", Type: "string", Desc: "原样写入的文本（如 \"hello\\r\"）；段间空行用 \\r\\r"},
					{Name: "keys", Type: "string", Desc: "空格分隔的键序列，如 \"Up Down RET\"、\"C-x\"（见键记法）"},
					{Name: "keyDelayMs", Type: "int", Desc: "每键间隔 0-5000ms，默认 150"},
					{Name: "timeoutMs", Type: "int", Desc: "本次操作等待上限（可选）"},
				},
				Response: `{"ok":true,"data":{"sent":3}}  // sent = 写入字符数 + 键数`,
				Example: "curl -s -X POST \"" + base + `/ftrw/send" ` + hdr + ` ` + jct + ` \
  -d '{"tab":"<TABID>","keys":"Up Down RET","keyDelayMs":300}'`,
			},
			{
				Method: "POST", Path: "/ftrw/wait", Auth: true,
				Summary: "阻塞等待画面上出现某段文本（子串或正则），用于确认提示/菜单已刷新。",
				Body: []docField{
					{Name: "tab", Type: "string", Desc: "标签 id；省略=当前活动标签"},
					{Name: "pattern", Type: "string", Required: true, Desc: "要等的文本；regex=true 时按正则"},
					{Name: "regex", Type: "bool", Desc: "true 时把 pattern 当 JS 正则"},
					{Name: "timeoutMs", Type: "int", Desc: "等待上限，默认 5000，上限 60000"},
					{Name: "full", Type: "bool", Desc: "仅 shell：连回滚区一起搜"},
				},
				Response: `命中 {"ok":true,"data":{"matched":true,"y":25,"text":"...","waitedMs":812}}；超时 {"matched":false,"waitedMs":5001}`,
				Example: "curl -s -X POST \"" + base + `/ftrw/wait" ` + hdr + ` ` + jct + ` \
  -d '{"tab":"<TABID>","pattern":"站内","timeoutMs":4000}'`,
			},
			{
				Method: "POST", Path: "/ftrw/command", Auth: true,
				Summary: "执行一条应用内命令（与热键绑定同口径），如清屏、发送预置串等。",
				Body: []docField{
					{Name: "spec", Type: "string", Required: true, Desc: `"命令id" 或 "命令id:参数"（同用户键位绑定表）`},
				},
				Response: `{"ok":true,"data":{"ran":"term.send:ls\r"}}`,
				Example:  `curl -s -X POST "` + base + `/ftrw/command" ` + hdr + ` ` + jct + ` -d '{"spec":"<命令id[:参数]>"}'`,
			},
			{
				Method: "POST", Path: "/ftrw/tab/activate", Auth: true,
				Summary:  "把指定标签切为当前活动标签。",
				Body:     []docField{{Name: "tab", Type: "string", Required: true, Desc: "标签 id"}},
				Response: `{"ok":true,"data":{"index":0}}`,
				Example:  `curl -s -X POST "` + base + `/ftrw/tab/activate" ` + hdr + ` ` + jct + ` -d '{"tab":"<TABID>"}'`,
			},
			{
				Method: "POST", Path: "/ftrw/tab/close", Auth: true,
				Summary:  "关闭指定标签。",
				Body:     []docField{{Name: "tab", Type: "string", Required: true, Desc: "标签 id"}},
				Response: `{"ok":true,"data":{}}`,
				Example:  `curl -s -X POST "` + base + `/ftrw/tab/close" ` + hdr + ` ` + jct + ` -d '{"tab":"<TABID>"}'`,
			},
			{
				Method: "POST", Path: "/ftrw/connect", Auth: true,
				Summary:  "按站点名新开一个标签并连接（等连上才返回，失败给原文；超时给足 45s）。",
				Body:     []docField{{Name: "site", Type: "string", Required: true, Desc: "存站名，见 /ftrw/sites"}},
				Response: `{"ok":true,"data":{ ...连接结果... }}`,
				Example:  `curl -s -X POST "` + base + `/ftrw/connect" ` + hdr + ` ` + jct + ` -d '{"site":"水木"}'`,
			},
			{
				Method: "GET", Path: "/ftrw/doc", Auth: false,
				Summary:  "返回本文档本身（默认 Markdown，一个请求读全）。加 ?format=json 取结构化版本。",
				Query:    []docField{{Name: "format", Type: "string", Desc: "\"markdown\"(默认) 或 \"json\""}},
				Response: "Markdown 文本（Content-Type: text/markdown）或 {\"ok\":true,\"data\":{...端点数组...}}",
				Example:  `curl -s ` + base + `/ftrw/doc` + "\ncurl -s \"" + base + `/ftrw/doc?format=json"`,
			},
		},
	}
}

// renderAgentApiMarkdown 把文档模型渲染成人类/LLM 友好的 Markdown。
func renderAgentApiMarkdown(port int) string {
	m := buildAgentApiDoc(port)
	var b strings.Builder
	addf := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }

	addf("# %s 文档（%s）\n\n", m.Title, m.Version)
	addf("%s\n\n", m.Description)
	addf("**Base URL**：`%s`\n\n", m.BaseURL)

	addf("## 快速开始\n\n")
	for i, s := range m.Quickstart {
		addf("%d. %s\n", i+1, s)
	}
	addf("\n")

	addf("## 约定\n\n")
	for _, s := range m.Conventions {
		addf("- %s\n", s)
	}
	addf("\n")

	addf("## 鉴权\n\n%s\n\n", m.Auth)
	addf("## 统一响应信封\n\n%s\n\n", m.Envelope)
	addf("## 键记法（`keys`）\n\n%s\n\n", m.KeyNotation)

	addf("## 端点\n\n")
	for _, e := range m.Endpoints {
		auth := "免 token"
		if e.Auth {
			auth = "需 token"
		}
		addf("### `%s %s` — %s\n\n", e.Method, e.Path, e.Summary)
		addf("- 鉴权：%s\n", auth)
		if len(e.Query) > 0 {
			addf("- 查询参数：\n")
			for _, q := range e.Query {
				addf("    - `%s`%s（%s）%s\n", q.Name, reqMark(q.Required), q.Type, q.Desc)
			}
		}
		if len(e.Body) > 0 {
			addf("- 请求体（JSON）：\n")
			for _, f := range e.Body {
				addf("    - `%s`%s（%s）%s\n", f.Name, reqMark(f.Required), f.Type, f.Desc)
			}
		}
		addf("- 响应：%s\n", code(e.Response))
		addf("- 示例：\n\n%sbash\n%s\n%s\n\n", agentDocFence, e.Example, agentDocFence)
	}

	addf("## 典型流程：驱动 BBS 回帖\n\n")
	addf("1. `POST /ftrw/send` 发 `{\"keys\":\"r\"}`（阅读页回信），再发 `{\"keys\":\"RET\"}` 进编辑器。\n")
	addf("2. 编辑器里先 `GET /ftrw/screen` 数清引言（quote）占几行，用 `Down` 把光标移到引言**下方空行**再写正文，避免插进引言中间。\n")
	addf("3. 正文用 `POST /ftrw/send` 的 `text`（中文走 UTF-8 文件 --data-binary）；段间空行写 `\\r\\r`。\n")
	addf("4. 发 `{\"keys\":\"C-x\"}` → `POST /ftrw/wait` 等 `\"站内\"` 出现发表提示。\n")
	addf("5. 关键时序：提示 `(L)站内…[L]:` 收到后，把 `L` 与 `RET` 当两次独立 send 分开发（连发 `\"L RET\"` 常吞掉 RET）。\n\n")
	addf("> 端点语义以本文件为准；如与实现漂移，请以 `GET /ftrw/doc?format=json` 的机器可读版本为最新。\n")

	return b.String()
}

func reqMark(required bool) string {
	if required {
		return " *（必填）*"
	}
	return ""
}

// code 把响应示例包成行内代码；含换行的用围栏代码块。
func code(s string) string {
	if strings.Contains(s, "\n") {
		return "\n\n" + agentDocFence + "json\n" + s + "\n" + agentDocFence + "\n"
	}
	return "`" + strings.ReplaceAll(s, "`", "'") + "`"
}

// handleDoc 服务文档本身：默认 Markdown，?format=json 输出结构化 JSON。免鉴权。
func (a *AgentAPI) handleDoc(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	port := a.cfg.port
	a.mu.Unlock()
	if port <= 0 {
		port = defaultAgentApiPort
	}
	if strings.EqualFold(r.URL.Query().Get("format"), "json") {
		writeAgentJSON(w, http.StatusOK, map[string]any{"ok": true, "data": buildAgentApiDoc(port)})
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(renderAgentApiMarkdown(port)))
}
