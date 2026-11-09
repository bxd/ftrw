package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"golang.org/x/text/transform"
)

// protoConn is the uniform byte-stream abstraction implemented by Telnet, SSH
// and local-shell connections.
type protoConn interface {
	io.ReadWriteCloser
	resize(cols, rows int) error
}

// Session statuses.
const (
	StatusConnecting = "connecting"
	StatusConnected  = "connected"
	StatusClosed     = "closed"
	StatusError      = "error"
	StatusZmodem     = "zmodem"
)

// TermData is emitted on event "term:data".
type TermData struct {
	ID   string `json:"id"`
	Data string `json:"data"` // UTF-8 decoded text
}

// TermStatus is emitted on event "term:status".
type TermStatus struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Message string `json:"message"`
	Title   string `json:"title"`
}

// SessionInfo is returned to the frontend to describe a live session.
type SessionInfo struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Title  string `json:"title"`
	Status string `json:"status"`
	Host   string `json:"host"`
}

// codec performs incremental legacy->UTF8 decoding, preserving partial
// multibyte sequences across chunk boundaries.
type codec struct {
	t     transform.Transformer
	carry []byte
}

func newCodec(encID int) *codec {
	c := &codec{}
	if enc := encodingFor(encID); enc != nil {
		c.t = enc.NewDecoder()
	}
	return c
}

func (c *codec) decode(src []byte) []byte {
	if c.t == nil {
		return src
	}
	if len(c.carry) > 0 {
		src = append(c.carry, src...)
		c.carry = nil
	}
	dst := make([]byte, len(src)*4+8)
	nDst, nSrc, err := c.t.Transform(dst, src, false)
	if err == transform.ErrShortDst || (err != nil && err != transform.ErrShortSrc) {
		// Best-effort: keep whatever we produced.
	}
	if nSrc < len(src) {
		c.carry = append(c.carry, src[nSrc:]...)
	}
	return dst[:nDst]
}

// Session is a single live terminal connection.
type Session struct {
	ID      string
	Site    Site
	Title   string
	Status  string
	encID   int
	conn    protoConn
	cdc     *codec
	writeMu sync.Mutex

	closed      chan struct{} // 会话终止信号（用户主动断开/标签关闭时关闭）：停掉防空闲、重连、登录推进协程
	closeOne    sync.Once
	uploadFiles []string     // pending ZMODEM upload (sender) files
	zmCancel    atomic.Bool  // ZMODEM 传输取消请求（进度弹窗的取消按钮置位，引擎在检查点响应）
	zmCooldown  atomic.Int64 // 取消冷却截止（UnixNano）：期内丢弃在途残留 ZMODEM 帧，防误启假会话

	// 空闲保活 / 自动重连 / 自动登录（对齐原版 idleTimer_、reconnectProcess、doAutoLogin 语义）
	lastAct      atomic.Int64 // 最近一次用户输入时刻（UnixNano）：防空闲据此判定静默时长
	zmActive     atomic.Bool  // ZMODEM 传输进行中：防空闲必须静默，否则注入字节会污染帧流
	userClosed   atomic.Bool  // 用户主动断开：不得触发自动重连
	reconnecting atomic.Bool  // 重连协程单一性
	lastCols     atomic.Int64 // 最近一次终端列数：自动重连后需重新协商，否则远端按默认尺寸输出
	lastRows     atomic.Int64 // 最近一次终端行数
	stopOnce     sync.Once    // 保证 closed 只关闭一次（Disconnect / 彻底放弃重连）

	loginPhase atomic.Int32 // loginPhase，自动登录阶段机当前态
	loginAt    atomic.Int64 // 进入当前阶段的时刻（UnixNano），超时兜底推进
	tailMu     sync.Mutex
	tail       []byte // 最近输出纯文本副本（已剔 ANSI），用于登录提示匹配
}

var sessionSeq int64

func newSessionID() string {
	n := atomic.AddInt64(&sessionSeq, 1)
	return strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + strconv.FormatInt(n, 36)
}

// TerminalService is the Wails-bound service managing terminal sessions.
type TerminalService struct {
	store    *Store
	mu       sync.RWMutex
	sessions map[string]*Session
	app      *application.App
	khosts   *KnownHosts
	dbgPath  string
}

func NewTerminalService(store *Store) *TerminalService {
	return &TerminalService{
		store:    store,
		sessions: make(map[string]*Session),
		khosts:   NewKnownHosts(store.Dir()),
		dbgPath:  filepath.Join(store.Dir(), "zmodem_debug.log"),
	}
}

func (ts *TerminalService) emitData(id, data string) {
	application.Get().Event.Emit("term:data", &TermData{ID: id, Data: data})
}

func (ts *TerminalService) emitStatus(s *Session) {
	application.Get().Event.Emit("term:status", &TermStatus{
		ID: s.ID, Status: s.Status, Title: s.Title, Message: s.Title,
	})
}

// Connect opens a new session for the given site and returns its id.
func (ts *TerminalService) Connect(site Site) (string, error) {
	if ts.app == nil {
		ts.app = application.Get()
	}
	id := newSessionID()
	s := &Session{
		ID:     id,
		Site:   site,
		Title:  site.Name,
		Status: StatusConnecting,
		encID:  site.ServerEncodingID,
		cdc:    newCodec(site.ServerEncodingID),
		closed: make(chan struct{}),
	}
	if s.Title == "" {
		if site.ProtocolType == 3 {
			// 本地 shell 没有主机端口，“host:0” 这种标题既难看又容易让人以为连错了机器。
			s.Title = exeBase(shellArgv(site)[0])
		} else {
			s.Title = fmt.Sprintf("%s:%d", site.Host, site.Port)
		}
	}
	ts.mu.Lock()
	ts.sessions[id] = s
	ts.mu.Unlock()
	ts.emitStatus(s)

	conn, err := ts.dial(site)
	if err != nil {
		s.Status = StatusError
		s.Title = err.Error()
		ts.emitStatus(s)
		ts.remove(id)
		return "", err
	}
	s.conn = conn
	s.Status = StatusConnected
	ts.emitStatus(s)

	go ts.readLoop(s)
	// 连接就绪：启动空闲保活，并按站点配置跑自动登录（preLogin→用户名→密码→postLogin）。
	now := time.Now().UnixNano()
	s.lastAct.Store(now)
	s.lastCols.Store(int64(portOr(site.NumColumns, 80)))
	s.lastRows.Store(int64(portOr(site.NumRows, 25)))
	go ts.idleGuard(s)
	ts.beginLogin(s, now)
	return id, nil
}

func (ts *TerminalService) dial(site Site) (protoConn, error) {
	switch site.ProtocolType {
	case 1, 2: // SSH1/SSH2 — we use SSH2 transport
		auth := AuthMethod{Kind: AuthPassword, Password: site.Password}
		return ConnectSSH(site.Host, portOr(site.Port, 22), site.UserName, auth, sshTermType(site), site.NumColumns, site.NumRows, ts.khosts.Callback())
	case 3: // Local shell
		return newLocalShell(site)
	default: // Telnet
		addr := net.JoinHostPort(site.Host, strconv.Itoa(portOr(site.Port, 23)))
		d := net.Dialer{Timeout: 15 * time.Second}
		c, err := d.Dial("tcp", addr)
		if err != nil {
			return nil, err
		}
		return NewTelnet(c, site.TermType), nil
	}
}

func sshTermType(site Site) string {
	if site.TermType == "" || site.TermType == "vtnt" {
		return "xterm-256color"
	}
	return site.TermType
}

func portOr(p, def int) int {
	if p <= 0 {
		return def
	}
	return p
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// readLoop decodes incoming bytes and streams UTF8 to the frontend, or hands
// the connection to the ZMODEM engine (blocking) when a transfer is detected.
func (ts *TerminalService) readLoop(s *Session) {
	buf := make([]byte, 32*1024)
	// 本轮循环固定使用同一个连接：自动重连会替换 s.conn 并另起新 readLoop，
	// 取局部变量避免与新循环读写同一连接。
	conn := s.conn
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			raw := buf[:n]
			if s.Site.EnableZmodem {
				ts.dbgChunk(raw)
				if detectZmodem(raw) {
					// 取消后的冷却期：服务器 rz 退出前在途回发的 ZACK/ZRPOS 残留在 TCP 缓冲里，
					// 若再次触发 runZmodem 会误启假会话并向 BBS 广播 ZRINIT，被主程序当用户输入
					// 读进命令行导致失去交互。冷却期内直接丢弃这类残留帧。
					if cd := s.zmCooldown.Load(); cd != 0 && time.Now().UnixNano() < cd {
						ts.dbg("readLoop: 冷却期内丢弃残留 ZMODEM 帧 len=%d", len(raw))
						continue
					}
					ts.runZmodem(s, raw)
					continue
				}
			}
			text := s.cdc.decode(raw)
			if len(text) > 0 {
				ts.emitData(s.ID, string(text))
				ts.feedLogin(s, text) // 自动登录：扇入输出流匹配登录提示
			}
		}
		if err != nil {
			reason := ""
			if !errors.Is(err, io.EOF) {
				reason = err.Error()
			}
			ts.handleClosed(s, reason)
			return
		}
	}
}

func (ts *TerminalService) handleClosed(s *Session, reason string) {
	happened := false
	s.closeOne.Do(func() {
		happened = true
		s.writeMu.Lock()
		if s.conn != nil {
			s.conn.Close()
		}
		s.writeMu.Unlock()
		s.Status = StatusClosed
		if reason != "" {
			s.Status = StatusError
			ts.emitData(s.ID, fmt.Sprintf("\r\n\x1b[31m[连接断开: %s]\x1b[0m\r\n", reason))
		} else {
			ts.emitData(s.ID, "\r\n\x1b[90m[连接已关闭]\x1b[0m\r\n")
		}
		ts.emitStatus(s)
	})
	if !happened {
		return
	}
	// 自动重连（对齐原版 reconnectProcess）：仅非用户主动断开且站点启用时重试。
	if s.Site.AutoReconnect && !s.userClosed.Load() {
		go ts.reconnectLoop(s)
	}
}

func (ts *TerminalService) remove(id string) {
	ts.mu.Lock()
	delete(ts.sessions, id)
	ts.mu.Unlock()
}

// Write sends UTF8 text from the frontend to the connection (encoded to site charset).
func (ts *TerminalService) Write(id string, text string) error {
	s, ok := ts.get(id)
	if !ok {
		return errors.New("session not found")
	}
	b := encodeBytes(text, s.encID)
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.conn == nil {
		return errors.New("not connected")
	}
	s.lastAct.Store(time.Now().UnixNano()) // 用户有输入即重新计时（等价原版 leaveIdle）
	_, err := s.conn.Write(b)
	return err
}

// WriteRaw sends bytes untouched (used by ZMODEM).
func (ts *TerminalService) WriteRaw(id string, data []byte) error {
	s, ok := ts.get(id)
	if !ok {
		return errors.New("session not found")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.conn == nil {
		return errors.New("not connected")
	}
	_, err := s.conn.Write(data)
	return err
}

// Resize updates the terminal window dimensions.
func (ts *TerminalService) Resize(id string, cols, rows int) error {
	s, ok := ts.get(id)
	if !ok {
		return errors.New("session not found")
	}
	if s.conn == nil {
		return nil
	}
	s.lastCols.Store(int64(cols))
	s.lastRows.Store(int64(rows))
	return s.conn.resize(cols, rows)
}

// Disconnect closes a session.
func (ts *TerminalService) Disconnect(id string) error {
	s, ok := ts.get(id)
	if !ok {
		return nil
	}
	s.userClosed.Store(true)                  // 主动断开：不得自动重连
	s.stopOnce.Do(func() { close(s.closed) }) // 停掉防空闲/重连/登录后台协程
	s.writeMu.Lock()
	if s.conn != nil {
		s.conn.Close()
	}
	s.writeMu.Unlock()
	ts.remove(id)
	return nil
}

// SendFile starts a ZMODEM upload of the given local files by invoking `rz`
// on the remote side; the ensuing ZMODEM header triggers sender mode.
func (ts *TerminalService) SendFile(id string, paths []string) error {
	s, ok := ts.get(id)
	if !ok {
		return errors.New("session not found")
	}
	if !s.Site.EnableZmodem {
		return errors.New("zmodem disabled for this site")
	}
	if len(paths) == 0 {
		return errors.New("no files selected")
	}
	s.zmCooldown.Store(0) // 用户主动发起新上传，解除取消后的残留帧冷却，保证 rz 的 ZRINIT 能被识别
	s.uploadFiles = paths
	return ts.Write(id, "rz\r")
}

// CancelTransfer 请求中止当前 ZMODEM 传输（上传/下载均适用）。
// 置位原子标志后立即打断引擎可能阻塞的读，使其在 ~100ms 内响应；
// 引擎随后发送 FQTerm 标准的 CAN 中止序列并结束会话。
func (ts *TerminalService) CancelTransfer(id string) error {
	s, ok := ts.get(id)
	if !ok {
		return errors.New("session not found")
	}
	s.zmCancel.Store(true)
	s.zmCooldown.Store(time.Now().Add(3 * time.Second).UnixNano())
	if s.conn != nil {
		if rd, ok := s.conn.(interface{ SetReadDeadline(time.Time) error }); ok {
			_ = rd.SetReadDeadline(time.Now())
		} else if sd, ok := s.conn.(interface{ SetDeadline(time.Time) error }); ok {
			_ = sd.SetDeadline(time.Now())
		}
	}
	return nil
}

// ListSessions returns a snapshot of active sessions.
func (ts *TerminalService) ListSessions() []SessionInfo {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	out := make([]SessionInfo, 0, len(ts.sessions))
	for _, s := range ts.sessions {
		out = append(out, SessionInfo{ID: s.ID, Name: s.Site.Name, Title: s.Title, Status: s.Status, Host: s.Site.Host})
	}
	return out
}

func (ts *TerminalService) get(id string) (*Session, bool) {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	s, ok := ts.sessions[id]
	return s, ok
}
