package main

import (
	"io"
	"net"
	"sync"
	"time"
)

// Telnet IAC protocol constants (RFC 854).
const (
	iac  = 255
	dont = 254
	do   = 253
	wont = 252
	will = 251
	sb   = 250
	se   = 240
	ga   = 249

	optEcho       = 1
	optSGA        = 3
	optStatus     = 5
	optTiming     = 6
	optLinemode   = 34
	optXDisplay   = 24
	optEnvVar     = 36
	optNewEnviron = 39
	optTType      = 24
)

// Telnet wraps a net.Conn and implements the Telnet client side of option
// negotiation. Its Read method yields only data bytes (IAC sequences are
// consumed internally), so callers can treat it like a transparent byte stream.
type Telnet struct {
	conn     net.Conn
	termType string

	mu  sync.Mutex
	buf []byte // pending decoded data

	closeOnce sync.Once
}

func NewTelnet(conn net.Conn, termType string) *Telnet {
	if termType == "" {
		termType = "vtnt"
	}
	return &Telnet{conn: conn, termType: termType}
}

// Write sends application bytes, escaping any 0xFF as IAC IAC.
func (t *Telnet) Write(p []byte) (int, error) {
	out := make([]byte, 0, len(p)+8)
	for _, b := range p {
		if b == iac {
			out = append(out, iac, iac)
		} else {
			out = append(out, b)
		}
	}
	if _, err := t.conn.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Read implements io.Reader returning only telnet data bytes.
func (t *Telnet) Read(p []byte) (int, error) {
	for len(t.buf) == 0 {
		tmp := make([]byte, 4096)
		n, err := t.conn.Read(tmp)
		if n > 0 {
			t.feed(tmp[:n])
		}
		if err != nil {
			if len(t.buf) > 0 {
				// Return buffered data first; report error next call.
				break
			}
			return 0, err
		}
	}
	n := copy(p, t.buf)
	t.buf = t.buf[n:]
	return n, nil
}

func (t *Telnet) Close() error {
	var err error
	t.closeOnce.Do(func() { err = t.conn.Close() })
	return err
}

func (t *Telnet) SetDeadline(ts time.Time) error { return t.conn.SetDeadline(ts) }

// SetReadDeadline 只约束读，不影响写。ZMODEM 发送期间的后台 ZACK 排空协程需要
// 用短读超时轮询，若误用 SetDeadline 会连带给写加期限、在大文件传输中途触发写超时。
func (t *Telnet) SetReadDeadline(ts time.Time) error  { return t.conn.SetReadDeadline(ts) }
func (t *Telnet) SetWriteDeadline(ts time.Time) error { return t.conn.SetWriteDeadline(ts) }

func (t *Telnet) LocalAddr() net.Addr  { return t.conn.LocalAddr() }
func (t *Telnet) RemoteAddr() net.Addr { return t.conn.RemoteAddr() }

// resize is a no-op for Telnet; window size changes are not tracked dynamically.
func (t *Telnet) resize(cols, rows int) error { return nil }

var _ io.Closer = (*Telnet)(nil)

// feed parses raw bytes, executing negotiation and collecting data into t.buf.
func (t *Telnet) feed(raw []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	i := 0
	for i < len(raw) {
		b := raw[i]
		if b != iac {
			t.buf = append(t.buf, b)
			i++
			continue
		}
		// Need at least IAC + command.
		if i+1 >= len(raw) {
			// Store IAC? Cannot reliably; drop trailing IAC.
			i++
			continue
		}
		cmd := raw[i+1]
		switch cmd {
		case iac: // escaped 0xFF data byte
			t.buf = append(t.buf, iac)
			i += 2
		case will, wont, do, dont:
			if i+2 >= len(raw) {
				return
			}
			opt := raw[i+2]
			t.handleNeg(cmd, opt)
			i += 3
		case sb:
			end := findSE(raw, i+2)
			if end < 0 {
				return
			}
			t.handleSub(raw[i+2 : end])
			i = end + 2 // skip IAC SE
		default:
			// GA, EL, EC etc. — ignore single-byte commands.
			i += 2
		}
	}
}

func findSE(raw []byte, from int) int {
	for j := from; j+1 < len(raw); j++ {
		if raw[j] == iac && raw[j+1] == se {
			return j
		}
	}
	return -1
}

// sendCommand writes an IAC command to the peer.
func (t *Telnet) sendCommand(cmd, opt byte) {
	t.conn.Write([]byte{iac, cmd, opt})
}

func (t *Telnet) handleNeg(cmd, opt byte) {
	switch cmd {
	case do:
		switch opt {
		case optSGA, optEcho:
			t.sendCommand(will, opt)
		case optTType:
			t.sendCommand(will, opt)
		default:
			t.sendCommand(wont, opt)
		}
	case dont:
		t.sendCommand(wont, opt)
	case will:
		switch opt {
		case optEcho, optSGA:
			t.sendCommand(do, opt)
		default:
			t.sendCommand(dont, opt)
		}
	case wont:
		t.sendCommand(dont, opt)
	}
}

func (t *Telnet) handleSub(sub []byte) {
	if len(sub) == 0 {
		return
	}
	switch sub[0] {
	case optTType:
		// Respond with our terminal type when requested (SEND=1).
		if len(sub) > 1 && sub[1] == 1 {
			payload := []byte{iac, sb, optTType, 0} // IS=0
			payload = append(payload, t.termType...)
			payload = append(payload, iac, se)
			t.conn.Write(payload)
		}
	case optNewEnviron, optEnvVar:
		// Refuse environment negotiation to avoid hangs.
		t.sendCommand(wont, sub[0])
	}
}
