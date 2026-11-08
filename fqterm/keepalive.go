package main

// 本文件实现三件"挂机可用性"能力，语义对齐原版 FQTerm C++：
//   1) 防空闲 idleGuard        ← fqterm_session.cpp 的 idleTimer_/onIdle/leaveIdle
//   2) 自动重连 reconnectLoop  ← fqterm_session.cpp 的 reconnectProcess/reconnect
//   3) 自动登录 阶段机          ← fqterm_session.cpp 的 doAutoLogin（原版靠扫描 "guest"
//      一次性灌入且注释点名"水木忽略连续输入需 sleep 1s"；此处改为按提示分阶段推进 +
//      超时兜底，并在自动重连后重新登录——重连不登录等于白连）

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ---- FQTerm 风格转义串解析 ----

// parseEscStr 对齐原版 parseString + FQTERM_CTRL(c)=((c)&0x1f)：
// "^X" → 控制字符（"^@"→0x00 NUL、"^M"→0x0D CR、"^["→0x1B ESC）；
// "\n"→CR、"\r"→LF、"\t"→TAB（原版 \n/\r 映射与直觉相反，此处保持一致以兼容既有配置文件）；
// 其余字节原样保留（UTF-8 高位字节不会被 '^'/'\' 命中）。
func parseEscStr(s string) []byte {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '^':
			if i+1 < len(s) {
				i++
				out = append(out, s[i]&0x1f)
			} else {
				out = append(out, c)
			}
		case '\\':
			if i+1 < len(s) {
				i++
				switch s[i] {
				case 'n':
					out = append(out, '\r')
				case 'r':
					out = append(out, '\n')
				case 't':
					out = append(out, '\t')
				default:
					out = append(out, '\\', s[i])
				}
			} else {
				out = append(out, c)
			}
		default:
			out = append(out, c)
		}
	}
	return out
}

// stripAnsi 去掉 CSI/OSC 控制序列，只留可读文本，供登录提示匹配。
func stripAnsi(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		c := b[i]
		if c != 0x1b {
			out = append(out, c)
			continue
		}
		if i+1 >= len(b) {
			break
		}
		switch b[i+1] {
		case '[': // CSI: 参数字节直到 final(0x40-0x7e)
			i += 2
			for i < len(b) && (b[i] < 0x40 || b[i] > 0x7e) {
				i++
			}
		case ']': // OSC: 直到 BEL 或下一个 ESC
			i += 2
			for i < len(b) && b[i] != 0x07 && b[i] != 0x1b {
				i++
			}
		default:
			i++
		}
	}
	return out
}

// ---- 防空闲 ----

// idleGuard 在用户静默满 maxIdleSeconds 后发送 antiIdleMessage（默认 "^@" = NUL），
// 使 BBS 的空闲踢人计时器归零；用户一有输入即重新计时（等价原版 leaveIdle）。
// ZMODEM 传输期间与重连期间必须静默：注入的字节会打乱帧流或写到已关闭的连接。
func (ts *TerminalService) idleGuard(s *Session) {
	iv := s.Site.MaxIdleSeconds
	if iv <= 0 {
		iv = 120 // 原版 fqterm_param.cpp: maxIdleSeconds_ = 120
	}
	msg := parseEscStr(s.Site.AntiIdleMessage)
	if len(msg) == 0 {
		msg = []byte{0x00}
	}
	idleDur := time.Duration(iv) * time.Second
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.closed:
			return
		case now := <-t.C:
			if !s.Site.AntiIdle || s.zmActive.Load() || s.reconnecting.Load() || s.userClosed.Load() {
				continue
			}
			if now.Sub(time.Unix(0, s.lastAct.Load())) < idleDur {
				continue
			}
			if err := ts.WriteRaw(s.ID, msg); err != nil {
				return // 连接异常交由 readLoop/重连处理
			}
			s.lastAct.Store(now.UnixNano()) // 发送后重新计时（等价原版重复定时器周期触发）
			ts.dbg("idleGuard: 静默 %d 秒，已发送防空闲字节(%d B)", iv, len(msg))
		}
	}
}

// ---- 自动重连 ----

// reconnectLoop 断线后按 reconnectInterval 间隔重试，直到成功或达到 reconnectRetries
// （-1 无限）。成功即复用同一 Session 换上新连接并重启读取循环与自动登录，前端标签、
// 滚动缓冲、窗口尺寸都不受影响。
func (ts *TerminalService) reconnectLoop(s *Session) {
	if !s.reconnecting.CompareAndSwap(false, true) {
		return
	}
	defer s.reconnecting.Store(false)

	site := s.Site
	interval := site.ReconnectInterva
	if interval <= 0 {
		interval = 3
	}
	retries := site.ReconnectRetries
	if retries == 0 {
		retries = -1 // 旧配置只有开关没有次数：既然勾选了重连就按无限处理
	}
	ts.emitData(s.ID, fmt.Sprintf("\x1b[90m[将在 %d 秒后自动重连…]\x1b[0m\r\n", interval))

	for attempt := 1; ; attempt++ {
		select {
		case <-s.closed:
			return
		case <-time.After(time.Duration(interval) * time.Second):
		}
		if s.userClosed.Load() {
			return
		}
		conn, err := ts.dial(site)
		if err == nil {
			s.writeMu.Lock()
			s.conn = conn
			s.writeMu.Unlock()
			s.cdc = newCodec(site.ServerEncodingID) // 解码状态复位
			s.closeOne = sync.Once{}                // 允许后续再次关闭
			s.Status = StatusConnected
			s.lastAct.Store(time.Now().UnixNano())
			// 重新上报窗口尺寸（Telnet 靠 NAWS 协商、SSH 靠 pty 请求）：不重发则远端
			// 会按默认 80x24 排版，BBS 菜单/分栏会错位。
			if c, r := int(s.lastCols.Load()), int(s.lastRows.Load()); c > 0 && r > 0 {
				_ = conn.resize(c, r)
			}
			ts.emitStatus(s)
			ts.emitData(s.ID, fmt.Sprintf("\r\n\x1b[32m[已自动重连（第 %d 次尝试）]\x1b[0m\r\n", attempt))
			ts.dbg("reconnect: 第 %d 次尝试成功", attempt)
			ts.beginLogin(s, time.Now().UnixNano())
			go ts.readLoop(s)
			return
		}
		ts.dbg("reconnect: 第 %d 次失败: %v", attempt, err)
		if retries >= 0 && attempt >= retries {
			ts.emitData(s.ID, fmt.Sprintf("\r\n\x1b[31m[自动重连失败（已尝试 %d 次），请手动连接]\x1b[0m\r\n", attempt))
			s.stopOnce.Do(func() { close(s.closed) })
			return
		}
		ts.emitData(s.ID, fmt.Sprintf("\x1b[90m[重连失败: %v，%d 秒后继续…]\x1b[0m\r\n", err, interval))
	}
}

// ---- 自动登录阶段机 ----

type loginPhase int32

const (
	phaseOff   loginPhase = 0 // 未启用或已完成
	phaseGreet loginPhase = 1 // 等登录菜单/首屏 → 发 preLogin
	phaseUser  loginPhase = 2 // 等用户名提示 → 发 userName
	phasePass  loginPhase = 3 // 等密码提示 → 发 password
	phasePost  loginPhase = 4 // 凭据已发 → 1 秒后发 postLogin（水木忽略连续输入）
)

var (
	reGreet = regexp.MustCompile(`(?i)(guest|new\s*user|login\s*name|user\s*name|帐号|账号|用户名|password|密码|选择|selection)`)
	reUser  = regexp.MustCompile(`(?i)(login\s*name|user\s*name|account\s*name|帐号|账号|用户名|login\s*[:>])`)
	rePass  = regexp.MustCompile(`(?i)(password|密码|passphrase)`)
)

// wantCreds 返回是否应把用户名/密码当作输入灌进会话：SSH 的凭据已在握手阶段用于认证，
// 再灌一次会污染远端 shell。
func (s *Session) wantCreds() bool {
	return s.Site.ProtocolType != 1 && s.Site.ProtocolType != 2
}

// beginLogin 启动（或重启）自动登录流程。
func (ts *TerminalService) beginLogin(s *Session, now int64) {
	site := s.Site
	if !site.AutoLogin || (site.PreLogin == "" && site.UserName == "" && site.Password == "" && site.PostLogin == "") {
		s.loginPhase.Store(int32(phaseOff))
		return
	}
	s.tailMu.Lock()
	s.tail = s.tail[:0]
	s.tailMu.Unlock()

	creds := s.wantCreds()
	var p loginPhase
	switch {
	case site.PreLogin != "":
		p = phaseGreet
	case creds && site.UserName != "":
		p = phaseUser
	case creds && site.Password != "":
		p = phasePass
	default:
		p = phasePost
	}
	s.loginPhase.Store(int32(p))
	s.loginAt.Store(now)
	ts.dbg("beginLogin: 自动登录启动 phase=%d preLogin=%q user=%q postLogin=%q creds=%v", p, site.PreLogin, site.UserName, site.PostLogin, creds)
	go ts.loginTick(s)
}

// feedLogin 由 readLoop 在每块解码输出后调用：匹配登录提示即立即推进阶段（不等超时）。
func (ts *TerminalService) feedLogin(s *Session, text []byte) {
	p := loginPhase(s.loginPhase.Load())
	if p == phaseOff || p == phasePost {
		return
	}
	plain := strings.ToLower(string(stripAnsi(text)))
	if len(plain) > 4000 {
		plain = plain[len(plain)-4000:]
	}
	s.tailMu.Lock()
	combined := string(s.tail) + plain
	if len(combined) > 4000 {
		combined = combined[len(combined)-4000:]
	}
	s.tail = []byte(combined)
	s.tailMu.Unlock()

	switch p {
	case phaseGreet:
		if reGreet.MatchString(combined) {
			ts.loginAdvance(s, phaseGreet, "首屏提示匹配")
		}
	case phaseUser:
		if reUser.MatchString(combined) {
			ts.loginAdvance(s, phaseUser, "用户名提示匹配")
		}
	case phasePass:
		if rePass.MatchString(combined) {
			ts.loginAdvance(s, phasePass, "密码提示匹配")
		}
	}
}

// loginAdvance 执行当前阶段的发送动作并推进到下一阶段（CAS 保证只推进一次、不重发）。
func (ts *TerminalService) loginAdvance(s *Session, from loginPhase, why string) {
	site := s.Site
	var next loginPhase
	var payload string
	switch from {
	case phaseGreet:
		payload = site.PreLogin
		switch {
		case s.wantCreds() && site.UserName != "":
			next = phaseUser
		case s.wantCreds() && site.Password != "":
			next = phasePass
		default:
			next = phasePost
		}
	case phaseUser:
		if !s.wantCreds() || site.UserName == "" {
			next = phasePass
			if !s.wantCreds() || site.Password == "" {
				next = phasePost
			}
			break
		}
		payload = site.UserName
		if s.wantCreds() && site.Password != "" {
			next = phasePass
		} else {
			next = phasePost
		}
	case phasePass:
		if !s.wantCreds() || site.Password == "" {
			next = phasePost
			break
		}
		payload = site.Password
		next = phasePost
	default:
		return
	}
	if !s.loginPhase.CompareAndSwap(int32(from), int32(next)) {
		return // 已被其它路径推进
	}
	s.loginAt.Store(time.Now().UnixNano())
	s.tailMu.Lock()
	s.tail = s.tail[:0]
	s.tailMu.Unlock()
	if payload != "" {
		ts.sendLoginBytes(s, payload)
	}
	ts.dbg("loginAdvance: %s → phase %d（发送 %q）", why, next, payload)
}

// sendLoginBytes 发送一条登录串：解析 FQTerm 转义，末尾无换行则自动补 CR
// （界面上填 "new"/"l" 这类单条命令，用户预期发送后立即回车）。
func (ts *TerminalService) sendLoginBytes(s *Session, text string) {
	b := parseEscStr(text)
	if len(b) == 0 {
		return
	}
	if last := b[len(b)-1]; last != '\r' && last != '\n' {
		b = append(b, '\r')
	}
	_ = ts.WriteRaw(s.ID, b)
}

// loginTick 负责超时兜底推进与 phasePost 的 1 秒延时（对齐原版 sleep(1) 后才发
// 登录后命令），并在完成/会话终止时退出。
func (ts *TerminalService) loginTick(s *Session) {
	posted := false
	t := time.NewTicker(300 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-s.closed:
			return
		case now := <-t.C:
			p := loginPhase(s.loginPhase.Load())
			if p == phaseOff {
				return
			}
			elapsed := now.Sub(time.Unix(0, s.loginAt.Load()))
			switch p {
			case phaseGreet:
				if elapsed > 12*time.Second {
					ts.loginAdvance(s, phaseGreet, "首屏超时兜底")
				}
			case phaseUser, phasePass:
				if elapsed > 20*time.Second {
					ts.loginAdvance(s, p, "提示超时兜底")
				}
			case phasePost:
				if !posted && elapsed >= time.Second {
					posted = true
					if site := s.Site; site.PostLogin != "" {
						ts.sendLoginBytes(s, site.PostLogin)
						ts.dbg("loginTick: 已发送登录后命令 %q", site.PostLogin)
					}
					s.loginPhase.Store(int32(phaseOff))
					return
				}
			}
		}
	}
}
