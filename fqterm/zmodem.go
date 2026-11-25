package main

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wailsapp/wails/v3/pkg/application"
	"golang.org/x/text/transform"
)

// ---- ZMODEM protocol constants (fqterm zmodem.h / Oden spec) ----
const (
	zpad   = '*' // 0x2A
	zdle   = 0x18
	zbin   = 'A'
	zhex   = 'B'
	zbin32 = 'C'

	zrqinit = 0
	zrinit  = 1
	zsinit  = 2
	zack    = 3
	zfile   = 4
	zskip   = 5
	znak    = 6
	zabort  = 7
	zfin    = 8
	zrpos   = 9
	zdata   = 10
	zeof    = 11
	zferr   = 12
	zcrc    = 13
	zcompl  = 15

	zcRCE = 'h' // end of file
	zcRCG = 'i' // continue, no ack
	zcRCQ = 'j' // continue, with ZACK
	zcRCW = 'k' // end frame, with ZACK
)

func crc16XMODEM(b []byte) uint16 {
	crc := uint16(0)
	for _, c := range b {
		crc ^= uint16(c) << 8
		for i := 0; i < 8; i++ {
			if crc&0x8000 != 0 {
				crc = (crc << 1) ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// headerBytes builds the 5-byte ZMODEM header: TYPE + 32-bit field.
// 与 FQTerm ZEnc4 / lrzsz 一致：4 个数据字节为 LSB first（小端）。
func headerBytes(typ byte, pos uint32) [5]byte {
	return [5]byte{typ, byte(pos), byte(pos >> 8), byte(pos >> 16), byte(pos >> 24)}
}

func headerPos(h [5]byte) uint32 {
	return uint32(h[1]) | uint32(h[2])<<8 | uint32(h[3])<<16 | uint32(h[4])<<24
}

// zcrc32 是 ZMODEM 的 CRC32（反射式，init/终异或 0xffffffff），即标准 zlib/PKZIP CRC32。
// ⚠️ 不能用 ^crc32.Update(0xFFFFFFFF,...)：Go 的 Update 首参是「已 finalize 的校验和」，
// 内部会先 ^ 还原寄存器，传 0xFFFFFFFF 等于从寄存器 0 起算，CRC 完全错误——服务器收到
// ZBIN32 头 CRC 校验失败，每帧都被立即 ZRPOS(0) 拒绝（offset 卡 0、巨慢的根因）。
func zcrc32(b []byte) uint32 { return crc32.ChecksumIEEE(b) }

func zEscape(in []byte) []byte {
	out := make([]byte, 0, len(in)+8)
	for _, c := range in {
		// 转义集逐字对齐 FQTerm ZXmitData(行 2182-2197)：
		// c==ZDLE 或 (c&0x7f)∈{0x10,0x11,0x13,0x0d,0x0a,0x1b,0x1d,0x7f} 均需转义；
		// ⚠️ ZRUB0/ZRUB1 用 FQTerm 定义(zmodem.h 行 302-303)：0x7f→'l'(0x6c)、0xff→'m'(0x6d)，
		// **不是** lrzsz 的 0x72/0x7d——水木 rz 按 0x6c/0x6d 解码，错用 0x72/0x7d 会让每个含
		// 0xff/0x7f 的子包 CRC 校验失败、整帧被拒（旧版一直 ZRPOS(0) 的根因之一）。
		if c == zdle {
			out = append(out, zdle, c^0x40)
			continue
		}
		c2 := c & 0x7f
		switch c2 {
		case 0x10, 0x11, 0x13, 0x0d, 0x0a, 0x1b, 0x1d, 0x7f:
			switch c {
			case 0x7f:
				out = append(out, zdle, 0x6c) // 'l' ZRUB0
			case 0xff:
				out = append(out, zdle, 0x6d) // 'm' ZRUB1
			default:
				out = append(out, zdle, c^0x40)
			}
		default:
			out = append(out, c)
		}
	}
	return out
}

// zEscapeHeader 是二进制头(ZBIN32)载荷的转义：逐字对齐 FQTerm putZdle(行 1316-1335)。
// 与数据转义 zEscape 的差别：**不**转义 0x0a/0x1b。类型字节(如 ZDATA=0x0A)必须裸发——
// 水木 rz 的 bin32 头解析把类型当裸字节读，转义成 18 4A 会被判坏头狂刷 ZRPOS(0)。
// 转义集：ZDLE(0x18)或(c&0x7f)∈{0x10,0x11,0x13,0x0d,0x1d,0x7f}；0x7f→'l'(0x6c)、0xff→'m'(0x6d)，其余 ZDLE+(c^0x40)。
func zEscapeHeader(in []byte) []byte {
	out := make([]byte, 0, len(in)+4)
	for _, c := range in {
		if c == zdle {
			out = append(out, zdle, c^0x40)
			continue
		}
		c2 := c & 0x7f
		switch c2 {
		case 0x10, 0x11, 0x13, 0x0d, 0x1d, 0x7f:
			switch c {
			case 0x7f:
				out = append(out, zdle, 0x6c) // ZRUB0
			case 0xff:
				out = append(out, zdle, 0x6d) // ZRUB1
			default:
				out = append(out, zdle, c^0x40)
			}
		default:
			out = append(out, c)
		}
	}
	return out
}

func unescape(c byte) byte {
	switch c {
	case 0x6c, 0x72: // 'l'(FQTerm/zmodem2) / 'r'(lrzsz) → ZRUB0
		return 0x7f
	case 0x6d, 0x7d: // 'm'(FQTerm/zmodem2) / '}'(lrzsz) → ZRUB1
		return 0xff
	default:
		return c ^ 0x40
	}
}

// ZmodemProgress is emitted on event "zmodem:progress".
type ZmodemProgress struct {
	ID        string `json:"id"`
	Direction string `json:"direction"`
	Filename  string `json:"filename"`
	Size      int64  `json:"size"`
	Done      int64  `json:"done"`
	State     string `json:"state"`
	Message   string `json:"message"`
}

// ZmodemDownloaded is emitted on event "zmodem:downloaded" after a receive has
// been saved and, when possible, split into article body and attachments.
// Paths is in preview order: body first, then attachments in their original order.
// For an ordinary file or a failed split it contains only the original file.
type ZmodemDownloaded struct {
	ID        string   `json:"id"`
	Direction string   `json:"direction"`
	Filename  string   `json:"filename"`
	Directory string   `json:"directory"`
	Paths     []string `json:"paths"`
	Split     bool     `json:"split"`
	Size      int64    `json:"size"`
}

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// detectZmodem 只接受能通过帧头 CRC 校验的 ZMODEM 头。
// 仅匹配 `* ZDLE` 会把 JPEG 等二进制数据里的随机字节误判为新会话；
// 会话异常退出后，这些假会话会分块吞掉数据，间隙又把二进制泄漏到终端。
func detectZmodem(raw []byte) bool {
	for i := 0; i+2 < len(raw); i++ {
		if raw[i] != zpad || raw[i+1] != zdle {
			continue
		}
		switch raw[i+2] {
		case zhex:
			if validZHexHeader(raw[i+3:]) {
				return true
			}
		case zbin, zbin32:
			if validZBinHeader(raw[i+3:], raw[i+2] == zbin32) {
				return true
			}
		}
	}
	return false
}

func validZHexHeader(raw []byte) bool {
	var hx []byte
	for _, c := range raw {
		if len(hx) == 16 {
			break
		}
		if !isHexDigit(c) {
			break
		}
		hx = append(hx, c)
	}
	if len(hx) != 14 && len(hx) != 16 {
		return false
	}
	b := make([]byte, len(hx)/2)
	for i := range b {
		b[i] = byte(hexVal(hx[i*2])<<4 | hexVal(hx[i*2+1]))
	}
	want := binary.BigEndian.Uint16(b[5:7])
	got := crc16XMODEM(b[:5])
	return got == want || crc16XMODEM(append(append([]byte{}, b[:5]...), 0, 0)) == want
}

func readZWireByte(raw []byte, idx *int) (byte, bool) {
	if *idx >= len(raw) {
		return 0, false
	}
	c := raw[*idx]
	*idx++
	if c == '\r' && *idx < len(raw) && raw[*idx] == 0 {
		*idx++
	}
	return c, true
}

func readZBinHeaderByte(raw []byte, idx *int) (byte, bool) {
	c, ok := readZWireByte(raw, idx)
	if !ok || c != zdle {
		return c, ok
	}
	escaped, ok := readZWireByte(raw, idx)
	if !ok {
		return 0, false
	}
	return unescape(escaped), true
}

func validZBinHeader(raw []byte, crc32f bool) bool {
	idx := 0
	var h [5]byte
	for i := range h {
		c, ok := readZBinHeaderByte(raw, &idx)
		if !ok {
			return false
		}
		h[i] = c
	}
	crcLen := 2
	if crc32f {
		crcLen = 4
	}
	b := make([]byte, crcLen)
	for i := range b {
		c, ok := readZBinHeaderByte(raw, &idx)
		if !ok {
			return false
		}
		b[i] = c
	}
	if crc32f {
		return zcrc32(h[:]) == binary.LittleEndian.Uint32(b)
	}
	want := binary.BigEndian.Uint16(b)
	got := crc16XMODEM(h[:])
	return got == want || crc16XMODEM(append(append([]byte{}, h[:]...), 0, 0)) == want
}

// zmodemEngine drives one ZMODEM session on top of a protoConn.
type zmodemEngine struct {
	sess        *Session
	rwc         protoConn
	dlDir       string
	upload      []string
	direction   string
	prebuf      []byte // bytes already consumed by detection, replayed first
	ts          *TerminalService
	dbgOn       bool
	rxRaw       []byte // raw bytes read from the socket (diagnostics only)
	lastSkipped bool   // sendFile 收到 ZSKIP 的标记（sender 统计批次结果用）
}

// dbg appends a timestamped line to the zmodem debug log (best-effort).
func (ts *TerminalService) dbg(format string, args ...any) {
	if ts.dbgPath == "" {
		return
	}
	f, err := os.OpenFile(ts.dbgPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "[%s] %s\n", time.Now().Format("15:04:05.000"), fmt.Sprintf(format, args...))
}

// dbgChunk logs raw chunks that contain ZDLE so we can see what the peer sends.
func (ts *TerminalService) dbgChunk(raw []byte) {
	if ts.dbgPath == "" || !bytes.ContainsRune(raw, zdle) {
		return
	}
	ts.dbg("chunk len=%d hex=%s", len(raw), hex.EncodeToString(raw))
}

// frame is a parsed ZMODEM header + optional data payload.
type frame struct {
	typ    byte
	pos    uint32
	data   []byte
	subch  byte
	crc32f bool // 帧头为 ZBIN32：帧内后续裸子包的 CRC 也是 CRC32（LE）
	crcOK  bool
}

// runZmodem 驱动一次协议会话，并返回同一 TCP 块中协议结束后尚未消费的字节。
// readLoop 必须把这些尾部交给终端解码；否则 ZFIN/OO 后紧跟的 BBS 重绘会被整体吞掉。
func (ts *TerminalService) runZmodem(s *Session, initial []byte) []byte {
	defer func() {
		if r := recover(); r != nil {
			ts.emitData(s.ID, fmt.Sprintf("\r\n\x1b[31m[ZMODEM 异常: %v]\x1b[0m\r\n", r))
		}
	}()
	dlDir := ts.store.DownloadDir()
	os.MkdirAll(dlDir, 0o755)
	_ = initial
	s.zmCancel.Store(false) // 新一轮传输，清除上一轮可能残留的取消标志
	s.zmCooldown.Store(0)   // 会话已正式启动，解除残留帧冷却
	s.zmDrain.Store(false)  // 新会话开始时恢复终端输出
	s.zmActive.Store(true)  // 传输期间防空闲必须静默（注入的 NUL 会打乱帧流）
	defer s.zmActive.Store(false)
	e := &zmodemEngine{sess: s, rwc: s.conn, dlDir: dlDir, direction: "recv", prebuf: initial, ts: ts, dbgOn: ts.dbgPath != ""}
	if len(s.uploadFiles) > 0 {
		e.upload = s.uploadFiles
		e.direction = "send"
		s.uploadFiles = nil
	}
	err := e.run()
	if errors.Is(err, errNotZmodem) {
		// 上一轮批末的 ZFIN/ZACK 等残留帧不是新会话：不能消费下一份预选附件，
		// 也不能进入冷却或丢弃后续真正的 ZRINIT，更不能往 BBS 命令行注入换行。
		e.setReadDeadline(time.Time{})
		return e.prebuf
	}
	if err != nil {
		if !errors.Is(err, errCancelled) && !errors.Is(err, errNotZmodem) {
			state := "error"
			if e.direction == "send" {
				state = "batch-error" // 会话级失败；单文件 error 仍会继续传后续文件
			}
			e.progress(state, "", 0, 0, "ZMODEM: "+err.Error())
		}
		// 异常/误判退出后，TCP 缓冲里可能仍有大量裸二进制子包。
		// readLoop 在静默窗口内继续丢弃，避免重新解码成终端乱码。
		s.zmDrain.Store(true)
		s.zmLastRaw.Store(time.Now().UnixNano())
		s.zmCooldown.Store(time.Now().Add(3 * time.Second).UnixNano())
	}
	// 接收循环会设逐帧读超时，退出时一律清除：陈旧 deadline 若留给 readLoop，
	// 下一次正常 Read 会被误杀成超时、连接被平白判死断开。
	e.setReadDeadline(time.Time{})
	ts.emitData(s.ID, "\r\n")
	if err != nil {
		return nil // 异常/取消后尾部可能仍是二进制，不能回放给终端。
	}
	return e.prebuf
}

func (e *zmodemEngine) rxByte() (byte, error) {
	if len(e.prebuf) > 0 {
		c := e.prebuf[0]
		e.prebuf = e.prebuf[1:]
		return c, nil
	}
	var b [1]byte
	for {
		n, err := e.rwc.Read(b[:])
		if n == 1 {
			if e.dbgOn {
				e.rxRaw = append(e.rxRaw, b[0])
			}
			return b[0], nil
		}
		if err != nil {
			return 0, err
		}
	}
}

// unread 把一个多读到的字节退回读流头部。
func (e *zmodemEngine) unread(c byte) {
	e.prebuf = append([]byte{c}, e.prebuf...)
}

// rxBinaryByte 读取 ZMODEM 二进制段，并还原 Telnet 对 CR 的 NUL 展开。
// 水木的 sz 会直接把数据中的 0x0d 发给 Telnet 层，链路随后插入 0x00；
// 这个 NUL 不属于文件内容，也不算 ZMODEM CRC，必须在转义解码前消费掉。
func (e *zmodemEngine) rxBinaryByte() (byte, error) {
	c, err := e.rxByte()
	if err != nil || c != '\r' {
		return c, err
	}
	next, err := e.rxByte()
	if err != nil {
		return 0, err
	}
	if next == 0 {
		return c, nil
	}
	e.unread(next)
	return c, nil
}

func (e *zmodemEngine) writeRaw(b []byte) error {
	e.sess.writeMu.Lock()
	defer e.sess.writeMu.Unlock()
	_, err := e.rwc.Write(b)
	return err
}

// setReadDeadline 设读超时（传零时间清除）。优先用只作用于读的 SetReadDeadline，
// 避免 Go net.Conn.SetDeadline 连带约束写、在大文件数据循环里因 Write 阻塞而误触写超时。
// 若连接只实现了 SetDeadline（如某些包装），退而用之。
func (e *zmodemEngine) setReadDeadline(d time.Time) {
	if rd, ok := e.rwc.(interface{ SetReadDeadline(time.Time) error }); ok {
		_ = rd.SetReadDeadline(d)
		return
	}
	if sd, ok := e.rwc.(interface{ SetDeadline(time.Time) error }); ok {
		_ = sd.SetDeadline(d)
	}
}

func (e *zmodemEngine) progress(state, name string, size, done int64, msg string) {
	app := application.Get()
	if app == nil { // 单元测试不启动 Wails 应用，事件静默丢弃
		return
	}
	app.Event.Emit("zmodem:progress", &ZmodemProgress{
		ID: e.sess.ID, Direction: e.direction, Filename: name,
		Size: size, Done: done, State: state, Message: msg,
	})
}

func (e *zmodemEngine) emitDownloaded(name, dir string, paths []string, split bool, size int64) {
	app := application.Get()
	if app == nil {
		return
	}
	app.Event.Emit("zmodem:downloaded", &ZmodemDownloaded{
		ID: e.sess.ID, Direction: e.direction, Filename: name, Directory: dir,
		Paths: paths, Split: split, Size: size,
	})
}

// errCancelled 表示用户通过进度弹窗取消了传输，区别于真实协议错误。
var errCancelled = errors.New("zmodem: 已取消")

// errRemoteAbort 表示远端明确中止/拒绝当前协议会话，不能继续发送后续文件。
var errRemoteAbort = errors.New("zmodem: 远端中止传输")

// errSessionFinished 表示 receiveFile 已替外层完成 ZFIN 握手，receiver 不应再等待下一帧。
var errSessionFinished = errors.New("zmodem: session finished")

// errNotZmodem 表示自动检测命中的字节没有合法开场帧；不报传输错误，也不进入冷却或丢弃窗口。
var errNotZmodem = errors.New("zmodem: no valid opening frame")

// abortSeq 是 FQTerm ZmodemAbort(行 748-750) 的中止序列：CAN×8 + BS×10。
// lrzsz 类服务器 rz/sz 收到连续 CAN 会立即退出并回到命令提示符。
var abortSeq = []byte{0x18, 0x18, 0x18, 0x18, 0x18, 0x18, 0x18, 0x18, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8}

func (e *zmodemEngine) cancelled() bool {
	return e.sess != nil && e.sess.zmCancel.Load()
}

// checkCancel 在传输检查点调用：已取消则发送 CAN 中止序列、上报 cancelled 事件并返回 errCancelled。
func (e *zmodemEngine) checkCancel() error {
	if !e.cancelled() {
		return nil
	}
	e.setReadDeadline(time.Time{})
	_ = e.writeRaw(abortSeq)
	// 取消后设 3s 冷却：服务器 rz 退出前在途回发的 ZACK/ZRPOS 残留在 TCP 缓冲里，
	// 若不拦截会被 readLoop 误判为新会话，导致向 BBS 广播 ZRINIT 污染命令行。
	if e.sess != nil {
		e.sess.zmCooldown.Store(time.Now().Add(3 * time.Second).UnixNano())
	}
	e.dbg("checkCancel: 用户取消传输，已发送 CAN 中止序列并进入冷却期")
	e.progress("cancelled", "", 0, 0, "已取消传输")
	return errCancelled
}

// readFrame scans for a header and reads its optional data + CRC.
// 扫描时必须先看到 ZPAD('*') 再看 ZDLE（与 FQTerm 一致），且对畸形帧重新
// 同步（continue）而非直接报错中止，避免屏幕数据里的零星字节把整个会话带崩。
func (e *zmodemEngine) readFrame() (frame, error) {
	for {
		c, err := e.rxByte()
		if err != nil {
			return frame{}, err
		}
		if c != zpad {
			continue
		}
		// 已见一个 '*'：可能还有更多 pad（hex 帧有两个），直到遇到非 '*'。
		var nxt byte
		for {
			nxt, err = e.rxByte()
			if err != nil {
				return frame{}, err
			}
			if nxt != zpad {
				break
			}
		}
		if nxt != zdle {
			continue // '*' 后不是 ZDLE，是普通文本
		}
		f, err := e.rxByte()
		if err != nil {
			return frame{}, err
		}
		switch f {
		case zhex:
			fr, ferr := e.readHexFrame()
			if ferr != nil {
				return frame{}, ferr // 流级错误（EOF/超时）必须上报，只有帧前噪声在上方扫描循环里重新同步
			}
			e.dbg("recv frame: typ=%d pos=%d data=%d subch=%c crc32=%v crcOK=%v", fr.typ, fr.pos, len(fr.data), fr.subch, fr.crc32f, fr.crcOK)
			return fr, nil
		case zbin, zbin32:
			fr, ferr := e.readBinFrame(f == zbin32)
			if ferr != nil {
				return frame{}, ferr
			}
			e.dbg("recv frame: typ=%d pos=%d data=%d subch=%c crc32=%v crcOK=%v", fr.typ, fr.pos, len(fr.data), fr.subch, fr.crc32f, fr.crcOK)
			return fr, nil
		default:
			continue
		}
	}
}

func (e *zmodemEngine) readHexFrame() (frame, error) {
	// 长度容错：本服务器发 14 位 hex（7 字节），标准 lrzsz 发 16 位；
	// 读到非 hex 字符（行尾 CR）即结束，最多取 16 位。
	var hx []byte
	var end byte
	for len(hx) < 16 {
		c, err := e.rxByte()
		if err != nil {
			return frame{}, err
		}
		if !isHexDigit(c) {
			end = c // 行尾分隔符已消费，不要再从流里读一次
			break
		}
		hx = append(hx, c)
	}
	if len(hx) < 2 {
		return frame{}, fmt.Errorf("bad hex header")
	}
	n := len(hx) / 2
	b := make([]byte, n)
	for i := 0; i < n; i++ {
		b[i] = byte(hexVal(hx[i*2])<<4 | hexVal(hx[i*2+1]))
	}
	var h [5]byte
	copy(h[:], b)
	typ := b[0]

	// 行尾兼容 CRLF 与水木的 CR NUL：CR 后只消费一个字节；若不是 LF/NUL 就退回。
	// 关键：无数据帧（尤其 ZRQINIT）必须在此立即返回，绝不能前瞻等下一帧——
	// 水木发完 ZRQINIT 就停住等我们回 ZRINIT，前瞻会把应答拖到读超时，服务端已放弃握手。
	switch end {
	case '\r':
		c, err := e.rxByte()
		if err != nil {
			return frame{}, err
		}
		if c != '\n' && c != 0 {
			e.unread(c)
		}
	case '\n', 0:
		// 已完整消费行尾
	default:
		e.unread(end)
	}

	if typ == zfile || typ == zsinit || typ == zdata {
		// hex 头后紧跟二进制数据（与水木自己 sendHexHdrData 同构：描述符/子包 + CRC16）。
		// 旧版不读这段 —— ZFILE 到来时 fr.data 为空、parseFileDesc 失败回 ZSKIP，
		// 正是水木 sz 下载不了的根因。hex 头行尾的 XON 不属于数据段，仅在这里兼容。
		if c, err := e.rxByte(); err != nil {
			return frame{}, err
		} else if c != 0x11 {
			e.unread(c)
		}
		data, subch, ok, derr := e.readDataSection(false)
		if derr != nil {
			return frame{}, derr
		}
		return frame{typ: typ, pos: headerPos(h), data: data, subch: subch, crcOK: ok}, nil
	}
	return frame{typ: typ, pos: headerPos(h), crcOK: true}, nil
}

func (e *zmodemEngine) readBinFrame(crc32f bool) (frame, error) {
	h := make([]byte, 5)
	for i := 0; i < 5; i++ {
		c, err := e.rxBinaryByte()
		if err != nil {
			return frame{}, err
		}
		if c == zdle {
			c2, err := e.rxByte()
			if err != nil {
				return frame{}, err
			}
			c = unescape(c2)
		}
		h[i] = c
	}
	var hdr [5]byte
	copy(hdr[:], h)
	typ := hdr[0]
	pos := headerPos(hdr)

	// 头部帧自带一个独立 CRC（bin16=2 字节，bin32=4 字节），先消费掉；CRC 字节可能被 ZDLE 转义。
	crcLen := 2
	if crc32f {
		crcLen = 4
	}
	for i := 0; i < crcLen; {
		c, err := e.rxBinaryByte()
		if err != nil {
			return frame{}, err
		}
		if c == zdle {
			if _, err := e.rxByte(); err != nil {
				return frame{}, err
			}
		}
		i++
	}

	hasData := typ == zfile || typ == zsinit || typ == zdata || typ == zcommand
	if !hasData {
		return frame{typ: typ, pos: pos, crc32f: crc32f, crcOK: true}, nil
	}
	data, subch, ok, derr := e.readDataSection(crc32f)
	if derr != nil {
		return frame{}, derr
	}
	return frame{typ: typ, pos: pos, data: data, subch: subch, crc32f: crc32f, crcOK: ok}, nil
}

// readDataSection 读取帧头的二进制数据段：转义数据直到 ZDLE+子包类型字节，再读尾部 CRC。
// crc32f 时子包 CRC 为 CRC32（小端）；否则 CRC16（大端）。CRC 字节同样经过转义，必须去转义——
// 旧版裸读，遇到 0x18 就错位。CRC16 同时接受「不终算 XMODEM」（水木自家变体，上传端由
// 真实 ZRINIT 帧反推确认）与「补两 0 终算」（标准 lrzsz）——接收端没得选，命中其一即算好。
func (e *zmodemEngine) readDataSection(crc32f bool) (data []byte, subch byte, crcOK bool, err error) {
	if e.dbgOn {
		e.rxRaw = e.rxRaw[:0] // 只保留当前子包的线上原始字节，失败时可直接取证
	}
	data = make([]byte, 0, 1024)
	// Telnet 对 CR NUL 的扩展可能与文件原有的 CR NUL 重合。主候选删除所有
	// 二义 NUL；fullData 保留全部，并记录每个可选的 NUL 位置。同一个包里若
	// 既有真扩展又有原始 NUL，必须逐点组合，不能只在“全删/全留”两个端点里选。
	var fullData []byte
	var nulPositions []int
	for {
		c, err := e.rxByte()
		if err != nil {
			return nil, 0, false, err
		}
		if c == zdle {
			c2, err := e.rxByte()
			if err != nil {
				return nil, 0, false, err
			}
			switch c2 {
			case zcRCE, zcRCG, zcRCQ, zcRCW:
				subch = c2
				goto readcrc
			default:
				c = unescape(c2)
			}
			data = append(data, c)
			if fullData != nil {
				fullData = append(fullData, c)
			}
			continue
		}
		if c == '\r' {
			next, err := e.rxByte()
			if err != nil {
				return nil, 0, false, err
			}
			if next == 0 {
				if fullData == nil {
					fullData = append([]byte(nil), data...)
				}
				nulPositions = append(nulPositions, len(fullData)+1)
				data = append(data, c)
				fullData = append(fullData, c, 0)
				continue
			}
			e.unread(next)
		}
		data = append(data, c)
		if fullData != nil {
			fullData = append(fullData, c)
		}
	}
readcrc:
	crcLen := 2
	if crc32f {
		crcLen = 4
	}
	b := make([]byte, crcLen)
	for i := 0; i < crcLen; i++ {
		c, err := e.rxBinaryByte()
		if err != nil {
			return nil, 0, false, err
		}
		if c == zdle {
			if c, err = e.rxByte(); err != nil {
				return nil, 0, false, err
			}
			c = unescape(c)
		}
		b[i] = c
	}
	var want, got uint64
	if crc32f {
		want = uint64(binary.LittleEndian.Uint32(b))
		got = uint64(zcrc32(dataSectionCover(data, subch)))
		crcOK = got == want
	} else {
		want = uint64(binary.BigEndian.Uint16(b))
		cover := dataSectionCover(data, subch)
		got = uint64(crc16XMODEM(cover))
		crcOK = got == want || uint64(crc16XMODEM(append(cover, 0, 0))) == want
	}
	if !crcOK && len(nulPositions) > 0 {
		candidate, ok, kept := resolveAmbiguousNULs(fullData, nulPositions, subch, crc32f, want)
		if ok {
			data = candidate
			crcOK = true
			e.dbg("readDataSection: CRC 二义 CR NUL 组合还原成功 ambiguous=%d kept=%d len=%d", len(nulPositions), kept, len(data))
		}
	}
	if !crcOK && crc32f {
		// 水木的 Telnet 链路偶尔把文件中的 FF 00 当 IAC NUL 吞掉，但发送方仍按
		// 原始数据计算 CRC32。实测三个坏包都在补回一个 FF 00 后完全吻合。
		base := data
		if fullData != nil {
			base = fullData
		}
		if candidate, ok, kept, insertAt := repairDroppedIACNUL(base, nulPositions, subch, uint32(want)); ok {
			data = candidate
			crcOK = true
			e.dbg("readDataSection: CRC32 IAC NUL 丢失还原成功 ambiguous=%d kept=%d insert=%d len=%d", len(nulPositions), kept, insertAt, len(data))
		}
	}
	if !crcOK {
		rawHex := ""
		if e.dbgOn && len(e.rxRaw) > 0 {
			raw := e.rxRaw
			if len(raw) > 16*1024 {
				raw = raw[len(raw)-16*1024:]
			}
			rawHex = hex.EncodeToString(raw)
		}
		if crc32f {
			e.dbg("readDataSection: CRC32 失败 subch=%c len=%d/%d ambiguous=%d got=%08x want=%08x raw=%s", subch, len(data), len(fullData), len(nulPositions), got, want, rawHex)
		} else {
			e.dbg("readDataSection: CRC16 失败 subch=%c len=%d/%d ambiguous=%d got=%04x want=%04x raw=%s", subch, len(data), len(fullData), len(nulPositions), got, want, rawHex)
		}
	}
	return data, subch, crcOK, nil
}

func dataSectionCover(data []byte, subch byte) []byte {
	cover := make([]byte, len(data)+1)
	copy(cover, data)
	cover[len(data)] = subch
	return cover
}

func dataSectionCRC(data []byte, subch byte, crc32f, finalized bool) uint64 {
	cover := dataSectionCover(data, subch)
	if crc32f {
		return uint64(zcrc32(cover))
	}
	if finalized {
		cover = append(cover, 0, 0)
	}
	return uint64(crc16XMODEM(cover))
}

func applyNULChoices(full []byte, positions []int, keep []bool) []byte {
	if len(positions) == 0 {
		return full
	}
	keepAt := make([]bool, len(full))
	for i, pos := range positions {
		if i < len(keep) && keep[i] {
			keepAt[pos] = true
		}
	}
	out := make([]byte, 0, len(full)-len(positions))
	for i, c := range full {
		if !keepAt[i] {
			out = append(out, c)
		}
	}
	return out
}

// resolveAmbiguousNULs 枚举每个二义 NUL 的保留/删除组合，并由线上 CRC 选定。
// 1024 字节子包里出现多个二义点仍很少见；限制 16 个，最坏 65536 次短 CRC，
// 避免异常数据造成指数级计算失控。
func resolveAmbiguousNULs(full []byte, positions []int, subch byte, crc32f bool, want uint64) ([]byte, bool, int) {
	if len(positions) > 16 {
		return nil, false, 0
	}
	for _, finalized := range []bool{false, true} {
		if crc32f && finalized {
			continue
		}
		for mask := 0; mask < 1<<len(positions); mask++ {
			keep := make([]bool, len(positions))
			kept := 0
			for i := range keep {
				if mask&(1<<i) != 0 {
					keep[i] = true
					kept++
				}
			}
			candidate := applyNULChoices(full, positions, keep)
			if dataSectionCRC(candidate, subch, crc32f, finalized) == want {
				return candidate, true, kept
			}
		}
	}
	return nil, false, 0
}

// repairDroppedIACNUL 反查被 Telnet 吞掉的一个 FF 00 数据对。CRC32 会随插入位置
// 明显变化，因此可用来精确定位；误命中概率约 1/2^32。与 CR NUL 二义组合时，
// 枚举上限降到 8，避免异常包造成过大的 CPU 开销。
func repairDroppedIACNUL(full []byte, positions []int, subch byte, want uint32) ([]byte, bool, int, int) {
	if len(positions) > 8 {
		return nil, false, 0, 0
	}
	for mask := 0; mask < 1<<len(positions); mask++ {
		keep := make([]bool, len(positions))
		kept := 0
		for i := range keep {
			if mask&(1<<i) != 0 {
				keep[i] = true
				kept++
			}
		}
		base := applyNULChoices(full, positions, keep)
		for pos := 0; pos <= len(base); pos++ {
			candidate := make([]byte, 0, len(base)+2)
			candidate = append(candidate, base[:pos]...)
			candidate = append(candidate, 0xff, 0)
			candidate = append(candidate, base[pos:]...)
			if zcrc32(dataSectionCover(candidate, subch)) == want {
				return candidate, true, kept, pos
			}
		}
	}
	return nil, false, 0, 0
}

func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	default:
		return int(c-'A') + 10
	}
}

const zcommand = 18

func (e *zmodemEngine) sendHexHeader(typ byte, pos uint32) error {
	h := headerBytes(typ, pos)
	// 与 FQTerm ZXmitHdrHex 对齐：hex 头固定用双 ZPAD 引入（`**B`），单 ZPAD 会被 lrzsz 类
	// 服务器当作噪声丢弃，导致 ZRQINIT 不被识别。
	out := []byte{zpad, zpad, zdle, zhex}
	const hexs = "0123456789abcdef"
	for _, c := range h {
		out = append(out, hexs[c>>4], hexs[c&0xf])
	}
	// CRC16 over the 5 header bytes. 水木 rz 用不终算的 XMODEM CRC16（不补两个 0 字节），
	// 已用服务器真实 ZRINIT（数据 01 00 00 00 23 → CRC be50）反推确认；标准 lrzsz 的补零终算会算成 67f7。
	crc := crc16XMODEM(h[:])
	out = append(out, hexs[crc>>12], hexs[(crc>>8)&0xf], hexs[(crc>>4)&0xf], hexs[crc&0xf])
	out = append(out, '\r', '\n')
	// FQTerm 行 1253：非 ZACK/ZFIN 额外附 XON(0x11) 作为接收方流控推动。
	if typ != zack && typ != zfin {
		out = append(out, 0x11)
	}
	e.dbg("sendHexHeader: typ=%d pos=%d out(%d)=%s", typ, pos, len(out), hex.EncodeToString(out))
	return e.writeRaw(out)
}

// sendHexHdrData 发送 hex 头 + 二进制数据块（Oden 允许的混合模式，很多 BBS 的
// 极简 rz 实现只解析 hex 头、不处理 bin/bin32 头）。
// header 部分：ZPAD ZPAD ZDLE 'B' + 14 hex chars + CRLF [+ XON]（CRC16）；
// data 部分：直接接上转义数据 + ZDLE subch + 转义 CRC16（MSB-first）。
func (e *zmodemEngine) sendHexHdrData(typ byte, pos uint32, data []byte, subch byte) error {
	h := headerBytes(typ, pos)
	const hexs = "0123456789abcdef"
	out := []byte{zpad, zpad, zdle, zhex}
	for _, c := range h {
		out = append(out, hexs[c>>4], hexs[c&0xf])
	}
	hcrc := crc16XMODEM(h[:])
	out = append(out, hexs[hcrc>>12], hexs[(hcrc>>8)&0xf], hexs[(hcrc>>4)&0xf], hexs[hcrc&0xf])
	out = append(out, '\r', '\n')
	if typ != zack && typ != zfin {
		out = append(out, 0x11)
	}
	out = append(out, zEscape(data)...)
	out = append(out, zdle, subch)
	dcover := append(append([]byte{}, data...), subch)
	dcrc := crc16XMODEM(dcover) // hex 头帧(ZSINIT/ZFILE)数据子包 CRC16 不终算，与头一致；实测终算会被水木 ZNAK
	out = append(out, zEscape([]byte{byte(dcrc >> 8), byte(dcrc & 0xff)})...)
	e.dbg("sendHexHdrData: typ=%d pos=%d out(%d)=%s", typ, pos, len(out), hex.EncodeToString(out))
	return e.writeRaw(out)
}

func (e *zmodemEngine) sendBinHeader(typ byte, pos uint32) error {
	h := headerBytes(typ, pos)
	out := []byte{zpad, zdle, zbin32}
	out = append(out, zEscapeHeader(h[:])...)
	crc := zcrc32(h[:])
	var cb [4]byte
	binary.LittleEndian.PutUint32(cb[:], crc)
	out = append(out, zEscapeHeader(cb[:])...)
	e.dbg("sendBinHeader: typ=%d pos=%d out(%d)=%s", typ, pos, len(out), hex.EncodeToString(out))
	return e.writeRaw(out)
}

func (e *zmodemEngine) sendDataFrame(pos uint32, data []byte, subch byte) error {
	// 水木 ZRINIT 广告 CANFC32(0x20)：数据阶段必须 ZBIN32 二进制头 + CRC32 子包。
	// FQTerm 行 2733(info->crc32 = zrinitflags & CANFC32)与 zmodem2(ZDATA_HEADER=Encoding::ZBIN32)
	// 都如此；hex 头 + CRC16 的数据子包会被服务器判定校验失败、狂刷 ZRPOS(0) 永不在 offset 前进。
	if derr := e.sendBinHeader(zdata, pos); derr != nil {
		return derr
	}
	return e.sendSubpacket(data, subch)
}

// sendSubpacket 发送一个 ZBIN32 数据子包（不含 ZDATA 头）：转义数据 + ZDLE + 子包类型 + CRC32(4 字节 LE，转义)。
// CRC32 覆盖 data+subch，ISO-HDLC（反射、init/终异或 0xffffffff），与 zmodem2 write_subpacket(wire.rs:430-466)
// 及 FQTerm ZXmitData 的 crc32 分支完全一致；子包类型字节仍为 'h'/'i'/'j'/'k'，CRC 宽度由 ZBIN32 头决定。
func (e *zmodemEngine) sendSubpacket(data []byte, subch byte) error {
	out := zEscape(data)
	out = append(out, zdle, subch)
	dcover := append(append([]byte{}, data...), subch)
	dcrc := zcrc32(dcover)
	var cb [4]byte
	binary.LittleEndian.PutUint32(cb[:], dcrc)
	out = append(out, zEscape(cb[:])...)
	return e.writeRaw(out)
}

// rxFrameTimeout 是下载会话中等待下一帧的上限。水木 sz 在要求重传后可能等待约
// 30 秒才重发，因此超时后先补发 ZRPOS 续收，不能立刻把连接还给终端输出。
const rxFrameTimeout = 30 * time.Second

// zrinitFlagsField 是 ZRINIT 的 32 位字段：flags 放线上第 4 字节，与水木 rz 自己发的
// ZRINIT「01 00 00 00 23」完全同构 —— CANFDX|CANOVIO|CANFC32，广告全双工、流式接收与
// CRC32 能力，让 sz 以 ZBIN32 数据帧高速连发（接收端对两种 CRC 都已验证兼容）。
const zrinitFlagsField = uint32(0x23) << 24

// run decides the role and drives the transfer.
func (e *zmodemEngine) run() error {
	if e.direction == "send" {
		return e.sender()
	}
	// 自动判定角色：先读取对端的开场帧（不像旧版那样盲目当接收方）。
	//   ZRINIT  -> 对端是接收方（BBS 端跑了 rz 等待收文件，即发帖按 U 的情形）
	//             于是弹出系统文件对话框，选定后作为发送方上传。
	//   ZRQINIT -> 对端是发送方（BBS 端跑了 sz），我们作为接收方下载。
	// 读不到帧（EOF/非 ZMODEM）则退回接收方广播，兼容老式发起。
	e.setReadDeadline(time.Now().Add(rxFrameTimeout))
	fr, err := e.readFrame()
	if err != nil {
		// 残留噪声/半帧：不回应、不启动会话。旧版在此广播 ZRINIT 会把垃圾注入 BBS 命令行。
		e.dbg("run: readFrame err=%v -> 不启动会话（避免向 BBS 误发 ZRINIT）", err)
		return errNotZmodem
	}
	if e.ts != nil {
		e.ts.dbg("run: first frame typ=%d pos=%d", fr.typ, fr.pos)
	}
	if fr.typ != zrinit && fr.typ != zrqinit {
		// 只有 ZRINIT（对端 rz 等收）/ ZRQINIT（对端 sz 待发）才是合法开场帧；
		// 取消后在途的 ZACK/ZRPOS/ZEOF 等一律视为残留噪声，不启动会话、不回应。
		e.dbg("run: 非开场帧 typ=%d，视为残留噪声，不启动会话", fr.typ)
		return errNotZmodem
	}
	if fr.typ == zrinit {
		// 只有对端确认以 rz 接收时才消费本地编辑器预选文件。残留 ZFIN/ZACK
		// 走到这里之前已返回，不会把下一份附件提前吞掉。
		if len(e.upload) == 0 && e.sess != nil {
			e.upload = e.sess.takeStagedUpload()
		}
		paths := e.upload
		if len(paths) == 0 {
			var perr error
			paths, perr = e.promptUploadFiles()
			if perr != nil || len(paths) == 0 {
				// 用户取消：发 ZABORT 让远端 rz 退出，否则它会持续重发 ZRINIT 反复弹框。
				e.sendHexHeader(zabort, 0)
				e.progress("cancelled", "", 0, 0, "已取消上传")
				return nil
			}
		}
		e.upload = paths
		e.direction = "send"
		// 对齐 FQTerm ZmodemTInit 发送方开场：先“nudge”发 ZRQINIT，再等对方重新回 ZRINIT，
		// GotRinit 里因 AlwaysSinit=1 发 ZSINIT，最后才 ZFILE。少了 ZRQINIT/ZSINIT，SMTH
		// 服务器的 rz 会不认识发送方、对 ZFILE 视而不见。
		if err := e.sendHexHeader(zrqinit, 0); err != nil {
			return err
		}
		for {
			f2, err2 := e.readFrame()
			if err2 != nil {
				e.dbg("run: post-ZRQINIT readFrame err=%v", err2)
				break
			}
			e.dbg("run: post-ZRQINIT frame typ=%d", f2.typ)
			if f2.typ == zrinit {
				break
			}
		}
		if err := e.sendZSInit(); err != nil {
			return err
		}
		return e.sender()
	}
	// ZRQINIT 或其它：作为接收方应答并进入接收循环。
	if err := e.sendHexHeader(zrinit, zrinitFlagsField); err != nil {
		return err
	}
	// 让下载卡片立即出现：水木 Ctrl+Y 会先显示“请输入文件名”再发 ZFILE，
	// 用户此时需在 BBS 提示处按回车；没有这条等待事件会误以为客户端卡死。
	e.progress("waiting", "", 0, 0, "已应答下载，等待 BBS 发送文件信息…")
	return e.receiver()
}

// promptUploadFiles pops the native open-file dialog (from the backend) so the
// user can pick local files to send to the remote side, mirroring FQTerm's
// "Choose a file to upload" behaviour when the peer initiates a receive.
func (e *zmodemEngine) promptUploadFiles() ([]string, error) {
	d := application.Get().Dialog.OpenFileWithOptions(&application.OpenFileDialogOptions{
		Title:                   "选择要上传的文件",
		Message:                 "选择要发送的文件（ZMODEM 上传）",
		CanChooseFiles:          true,
		CanChooseDirectories:    false,
		AllowsMultipleSelection: true,
	})
	return d.PromptForMultipleSelection()
}

// dbg 转发到后端的 zmodem 取证日志。
func (e *zmodemEngine) dbg(format string, args ...any) {
	if e.ts != nil {
		e.ts.dbg(format, args...)
	}
}

// ---- receiver (download) ----

func (e *zmodemEngine) receiver() error {
	for {
		if cerr := e.checkCancel(); cerr != nil {
			return cerr
		}
		e.setReadDeadline(time.Now().Add(rxFrameTimeout))
		fr, err := e.readFrame()
		if err != nil {
			if cerr := e.checkCancel(); cerr != nil {
				return cerr
			}
			return fmt.Errorf("等待发送帧失败: %w", err)
		}
		switch fr.typ {
		case zsinit:
			// 发送方初始化帧携带 attention 字符串；FQTerm GotSinitData 回 ZACK(SerialNo)。
			// 水木会停在这里等确认，旧版直接 continue 导致后续 ZFILE 永不到来。
			if fr.crcOK {
				e.sendHexHeader(zack, 0)
			} else {
				e.sendHexHeader(znak, 0)
			}
			continue
		case zfile:
			if err := e.receiveFile(fr); err != nil {
				if errors.Is(err, errSessionFinished) {
					return nil
				}
				return err
			}
		case zack, zrinit:
			continue
		case zrqinit:
			// 服务端没及时看到上一发 ZRINIT 时会重发 ZRQINIT，补答一次；
			// 旧版只 continue，错过握手窗口后就一直等不到 ZFILE。
			e.sendHexHeader(zrinit, zrinitFlagsField)
			continue
		case zcompl:
			// 发送方要求确认 CRC32 能力：回带 CANFC32 的 ZRINIT 继续。
			e.sendHexHeader(zrinit, zrinitFlagsField)
			continue
		case zfin:
			// 水木对应的 FQTerm 接收状态机 GotFin：回 ZFIN 让发送方退出，不能回 ZRINIT。
			// 也不能发「**OO」—— sz 退出后 BBS 主程序会把多余字节当命令行输入。
			e.sendHexHeader(zfin, 0)
			return nil
		case zabort:
			return nil
		default:
			// Unexpected header at batch end.
			return nil
		}
	}
}

func (e *zmodemEngine) receiveFile(fileHdr frame) error {
	name, size, descErr := parseFileDesc(fileHdr.data, e.sess.encID)
	if descErr != nil {
		e.sendHexHeader(zskip, 0)
		return nil
	}
	safeName := sanitizeName(name)
	dir, err := e.createDownloadDir(safeName)
	if err != nil {
		e.sendHexHeader(zferr, 0)
		return fmt.Errorf("create download directory for %s: %w", name, err)
	}
	path := filepath.Join(dir, safeName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		e.sendHexHeader(zferr, 0)
		return fmt.Errorf("create %s: %w", name, err)
	}
	defer f.Close()
	e.progress("start", name, size, 0, "接收 "+name)
	e.sendHexHeader(zrpos, 0)

	var offset int64
	frameTimeouts := 0
	for {
		if cerr := e.checkCancel(); cerr != nil {
			return cerr
		}
		e.setReadDeadline(time.Now().Add(rxFrameTimeout))
		fr, err := e.readFrame()
		if err != nil {
			if cerr := e.checkCancel(); cerr != nil {
				return cerr
			}
			// 水木 sz 的重传/继续发送可能超过 30 秒。会话中一律补发 ZRPOS
			// 并继续等，不能按固定次数退出；否则后续裸二进制子包会污染终端。
			if os.IsTimeout(err) {
				frameTimeouts++
				e.dbg("receiveFile: %s 等待帧超时，补发 ZRPOS(%d) 续收（第 %d 次）", name, offset, frameTimeouts)
				e.sendHexHeader(zrpos, uint32(offset))
				continue
			}
			return fmt.Errorf("等待数据帧失败: %w", err)
		}
		frameTimeouts = 0
		switch fr.typ {
		case zdata:
			if uint64(fr.pos) != uint64(offset) {
				e.dbg("receiveFile: %s ZDATA 偏移不符 pos=%d actual=%d，请求重发", name, fr.pos, offset)
				e.sendHexHeader(zrpos, uint32(offset))
				continue
			}
			// 一个 ZDATA 帧可含多个背靠背的裸子包（发送方流式，帧首才带头），
			// 必须连读到 ZCRCW/ZCRCE；旧版只读一个子包，其余字节被当噪声丢弃——数据丢失。
			for {
				e.setReadDeadline(time.Now().Add(rxFrameTimeout))
				if !fr.crcOK {
					// 坏子包：要求从已确认的 offset 重发（发送方会重开一个新 ZDATA 帧）。
					e.sendHexHeader(zrpos, uint32(offset))
					break
				}
				if len(fr.data) > 0 {
					if _, werr := f.Write(fr.data); werr != nil {
						e.sendHexHeader(zferr, 0)
						return werr
					}
					offset += int64(len(fr.data))
					e.progress("transfer", name, size, offset, "")
					e.dbg("receiveFile: %s offset=%d len=%d subch=%c crcOK=%v", name, offset, len(fr.data), fr.subch, fr.crcOK)
				}
				switch fr.subch {
				case zcRCW, zcRCQ:
					e.sendHexHeader(zack, uint32(offset))
				}
				if fr.subch == zcRCG || fr.subch == zcRCQ {
					// 同帧内无头续子包，CRC 宽度跟随帧头格式。
					data, subch, ok, rerr := e.readDataSection(fr.crc32f)
					if rerr != nil {
						if os.IsTimeout(rerr) {
							e.dbg("receiveFile: %s 子包读取超时，补发 ZRPOS(%d) 续收", name, offset)
							e.sendHexHeader(zrpos, uint32(offset))
							break
						}
						return rerr
					}
					fr = frame{typ: zdata, data: data, subch: subch, crc32f: fr.crc32f, crcOK: ok}
					continue
				}
				break // ZCRCW/ZCRCE：本数据帧结束，等下一个帧头（ZEOF/ZACK/重发 ZDATA）
			}
		case zeof:
			if uint64(fr.pos) != uint64(offset) || (size > 0 && offset != size) {
				e.dbg("receiveFile: %s ZEOF 提前或偏移不符 pos=%d declared=%d actual=%d，请求从 actual 续传", name, fr.pos, size, offset)
				e.sendHexHeader(zrpos, uint32(offset))
				continue
			}
			f.Close()
			if size <= 0 {
				// 水木文章常在 ZFILE 中声明 0，直到 ZEOF 才知道实际长度。
				size = offset
				e.progress("transfer", name, size, offset, "")
			}
			e.finishDownload(path, name, size, offset)
			e.sendHexHeader(zrinit, zrinitFlagsField)
			return nil
		case zfin:
			f.Close()
			if size > 0 && offset != size {
				// 发送方在文件完成前断会：必须回 ZFIN 让它退出，但不能假报下载成功。
				e.sendHexHeader(zfin, 0)
				return fmt.Errorf("传输提前结束: %s 声明 %d 字节，实际仅收到 %d 字节", name, size, offset)
			}
			if size <= 0 {
				size = offset
			}
			e.finishDownload(path, name, size, offset)
			if err := e.sendHexHeader(zfin, 0); err != nil {
				return err
			}
			return errSessionFinished
		case zabort, zferr:
			return fmt.Errorf("发送方中止/错误(typ=%d)", fr.typ)
		default:
			// ZACK/ZRINIT/ZRQINIT 等陈旧或保活帧：忽略，继续等数据。
			continue
		}
	}
}

func parseFileDesc(data []byte, encID int) (string, int64, error) {
	s := string(data)
	s = strings.ReplaceAll(s, "\x00", " ")
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return "", 0, fmt.Errorf("empty descriptor")
	}
	// ZFILE 的文件名是原始字节，按站点编码解；GBK 字节偶然合法 UTF-8 时也不能误判。
	name := filepath.Base(decodeZmodemName(fields[0], encID))
	var size int64
	if len(fields) >= 2 {
		size, _ = strconv.ParseInt(fields[1], 10, 64)
	}
	return name, size, nil
}

func decodeZmodemName(raw string, encID int) string {
	if enc := encodingFor(encID); enc != nil {
		if s, _, err := transform.String(enc.NewDecoder(), raw); err == nil && !strings.ContainsRune(s, utf8.RuneError) {
			return s
		}
	}
	return decodeArchiveName(raw)
}

func sanitizeName(name string) string {
	name = sanitizeFileName(filepath.Base(strings.ReplaceAll(name, "\\", "/")))
	if name == "" || name == "." {
		return "zmodem_file"
	}
	if reservedWindowsName(name) {
		return "zmodem_file" + filepath.Ext(name)
	}
	return name
}

func reservedWindowsName(name string) bool {
	stem := strings.TrimSuffix(name, filepath.Ext(name))
	stem = strings.ToUpper(strings.TrimRight(stem, " ."))
	switch stem {
	case "CON", "PRN", "AUX", "NUL", "CLOCK$":
		return true
	}
	if len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && stem[3] >= '1' && stem[3] <= '9' {
		return true
	}
	return false
}

// createDownloadDir atomically reserves a fresh per-download directory. Mkdir
// rather than Stat+Create avoids two simultaneous downloads choosing the same name.
func (e *zmodemEngine) createDownloadDir(name string) (string, error) {
	root, err := filepath.Abs(e.dlDir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	ext := filepath.Ext(name)
	base := sanitizeFileName(strings.TrimSuffix(name, ext))
	if base == "" || base == "." {
		base = "zmodem_file"
	}
	for i := 1; ; i++ {
		dirName := base
		if i > 1 {
			dirName += "_" + itoa(i)
		}
		dir := filepath.Join(root, dirName)
		if err := os.Mkdir(dir, 0o755); err == nil {
			return dir, nil
		} else if !os.IsExist(err) {
			return "", err
		}
	}
}

// finishDownload keeps the original file untouched and adds a preview-friendly
// split when the payload is a waterwood article with appended attachment records.
func (e *zmodemEngine) finishDownload(path, name string, size, done int64) {
	paths, split, err := splitDownloadedArticle(path, e.sess.encID)
	message := "已保存到 " + path
	switch {
	case err != nil:
		message += "（附件拆分失败：" + err.Error() + "）"
	case split:
		message += fmt.Sprintf("（已拆出正文和 %d 个附件）", len(paths)-1)
	}
	e.progress("done", name, size, done, message)
	e.emitDownloaded(name, filepath.Dir(path), paths, split, size)
}

var attachmentPad = []byte{0, 0, 0, 0, 0, 0, 0, 0}

type articleAttachment struct {
	name string
	data []byte
}

// parseArticleAttachments parses the deterministic record format written by
// KBS_BBS/upload_post_append: 8 NUL, NUL-terminated name, big-endian uint32 size,
// then exactly that many payload bytes. Records are contiguous through EOF.
func parseArticleAttachments(data []byte, encID int) ([]byte, []articleAttachment, bool) {
	marker := bytes.Index(data, attachmentPad)
	if marker < 0 {
		return nil, nil, false
	}
	body := data[:marker]
	if bytes.HasSuffix(body, []byte("\r\n")) {
		body = body[:len(body)-2]
	} else if bytes.HasSuffix(body, []byte("\n")) {
		body = body[:len(body)-1]
	}

	pos := marker
	attachments := make([]articleAttachment, 0, 4)
	for pos < len(data) {
		if len(data)-pos < len(attachmentPad) || !bytes.Equal(data[pos:pos+len(attachmentPad)], attachmentPad) {
			return nil, nil, false
		}
		pos += len(attachmentPad)
		nameEnd := bytes.IndexByte(data[pos:], 0)
		if nameEnd <= 0 || nameEnd > 1024 {
			return nil, nil, false
		}
		rawName := string(data[pos : pos+nameEnd])
		pos += nameEnd + 1
		if len(data)-pos < 4 {
			return nil, nil, false
		}
		size := binary.BigEndian.Uint32(data[pos : pos+4])
		pos += 4
		if uint64(size) > uint64(len(data)-pos) {
			return nil, nil, false
		}
		end := pos + int(size)
		attachments = append(attachments, articleAttachment{name: sanitizeName(decodeZmodemName(rawName, encID)), data: data[pos:end]})
		pos = end
	}
	if len(attachments) == 0 {
		return nil, nil, false
	}
	return body, attachments, true
}

func splitDownloadedArticle(path string, encID int) ([]string, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return []string{path}, false, err
	}
	body, attachments, ok := parseArticleAttachments(data, encID)
	if !ok {
		return []string{path}, false, nil
	}
	paths, err := writeArticleParts(path, body, attachments)
	if err != nil {
		return []string{path}, false, err
	}
	return paths, true, nil
}

func writeArticleParts(original string, body []byte, attachments []articleAttachment) (paths []string, err error) {
	created := make([]string, 0, len(attachments)+1)
	defer func() {
		if err != nil {
			for _, path := range created {
				_ = os.Remove(path)
			}
		}
	}()
	write := func(name string, data []byte) error {
		path := uniquePath(filepath.Dir(original), name)
		if werr := os.WriteFile(path, data, 0o644); werr != nil {
			return werr
		}
		created = append(created, path)
		paths = append(paths, path)
		return nil
	}

	ext := filepath.Ext(original)
	stem := strings.TrimSuffix(filepath.Base(original), ext)
	if err = write(sanitizeName(stem+"_正文.txt"), body); err != nil {
		return nil, err
	}
	for _, attachment := range attachments {
		if err = write(attachment.name, attachment.data); err != nil {
			return nil, err
		}
	}
	return paths, nil
}

// ---- sender (upload) ----

func (e *zmodemEngine) sender() error {
	okCount, skipCount, failCount := 0, 0, 0
	for _, path := range e.upload {
		if cerr := e.checkCancel(); cerr != nil {
			return cerr
		}
		fi, err := os.Stat(path)
		if err != nil {
			e.progress("error", filepath.Base(path), 0, 0, err.Error())
			failCount++
			continue
		}
		e.lastSkipped = false
		if err := e.sendFile(path, fi); err != nil {
			if errors.Is(err, errCancelled) {
				return err // 取消：不再发 ZFIN/**OO，中止序列已发出
			}
			e.progress("error", filepath.Base(path), 0, 0, err.Error())
			// 本地缺文件可在发送前跳过；协议/网络错误后继续发可能交错帧，必须整批失败。
			return err
		}
		if e.lastSkipped {
			skipCount++
		} else {
			okCount++
		}
	}
	// End of batch（FQTerm ZmodemTFinish/TFinishOps）：发 ZFIN，对端 ZRINIT/ZNAK→重发 ZFIN，
	// 对端 ZFIN→发 "OO" 退出 ZMODEM 回命令模式。不干净断会，服务器 rz 会残留、
	// 后续字节泄到 BBS 导致行为异常（如误发无附件文章）。
	// 用户实测：100% 后要发两个空命令——水木 rz 结算附件时会吞掉第一发 ZFIN（取证：
	// ZFIN 后沉默 10s、服务器自己发 ZFIN 收尾），故第一发 ZFIN 500ms 无回应就主动补
	// 第二发空 ZFIN；对端 ZFIN 也立即退出，不再傻等 ZRINIT 到超时。
	e.sendHexHeader(zfin, 0)
	resent := false
	finDeadline := time.Now().Add(500 * time.Millisecond)
	for {
		e.setReadDeadline(finDeadline)
		fr, rerr := e.readFrame()
		if rerr != nil {
			if !resent {
				resent = true
				e.sendHexHeader(zfin, 0) // 补第二发空命令
				finDeadline = time.Now().Add(10 * time.Second)
				continue
			}
			break // 超时兜底：仍发 OO 尝试退出
		}
		e.dbg("sender: TFinish frame typ=%d pos=%d", fr.typ, fr.pos)
		if fr.typ == zfin {
			break // 对端确认断会
		}
		if fr.typ == zrinit || fr.typ == znak {
			resent = true
			e.sendHexHeader(zfin, 0) // FQTerm TFinishOps：ZRINIT/ZNAK→重发 ZFIN
			finDeadline = time.Now().Add(10 * time.Second)
		}
	}
	e.setReadDeadline(time.Time{})
	e.writeRaw([]byte{'O', 'O'}) // FQTerm OverAndOut：裸 "OO"，不带 ZPAD
	// 只有整批 ZFIN/OO 收尾后才发批次终态。单文件 error/skipped 不是终态，
	// 前端必须等 batch-done，避免后台还在传时再次 StageUpload 交错协议。
	message := uploadBatchMessage(okCount, skipCount, failCount)
	e.progress("batch-done", "", 0, 0, message)
	return nil
}

func uploadBatchMessage(okCount, skipCount, failCount int) string {
	failed := skipCount + failCount
	switch {
	case okCount > 0 && failed == 0:
		return "上传完成"
	case okCount > 0:
		return fmt.Sprintf("上传完成（%d 个文件被跳过/失败）", failed)
	case failed > 0:
		return fmt.Sprintf("上传失败（%d 个文件被跳过/失败）", failed)
	default:
		return "上传完成"
	}
}

func (e *zmodemEngine) sendFile(path string, fi os.FileInfo) error {
	name := filepath.Base(path)
	size := fi.Size()
	date := fi.ModTime().Unix()
	// FQTerm/lrzsz 兼容描述符：文件名以 NUL 结尾，随后是字段行，再以 NUL 结尾。
	// 字段顺序对齐 FQTerm sendFilename："size date mode 0 filesRem bytesRem 0"。
	fields := fmt.Sprintf("%d %o %o 0 %d %d 0", size, date, 0644, 1, size)
	desc := make([]byte, 0, len(name)+len(fields)+4)
	desc = append(desc, name...)
	desc = append(desc, 0)
	desc = append(desc, fields...)
	desc = append(desc, 0)
	e.dbg("sendFile: ZFILE desc=%q", string(desc))
	// 弹窗提前到发 ZFILE 时就出现（旧版在收到 ZRPOS 后才发 start，被 ZSKIP 拒收时全程无反馈）。
	e.progress("start", name, size, 0, "上传 "+name)
	// ZFILE header + descriptor data.
	e.sendZFile(desc)
	// Wait for ZRPOS / ZSKIP. 弹框期间远端 rz 会持续重发 ZRINIT，这些陈旧帧需跳过。
	var startPos uint32
	e.rxRaw = e.rxRaw[:0]
	// 取证：设 10s 读超时。若服务器接受 ZFILE 后保持沉默（等我们发数据）或发了无法解析的帧，
	// 超时会触发 readFrame err 并 dump 出累计原始字节，帮助区分“死锁”与“误析”。
	e.setReadDeadline(time.Now().Add(10 * time.Second))
	defer e.setReadDeadline(time.Time{})
	for havePos := false; !havePos; {
		fr, err := e.readFrame()
		raw := hex.EncodeToString(e.rxRaw)
		e.rxRaw = e.rxRaw[:0]
		if err != nil {
			if cerr := e.checkCancel(); cerr != nil {
				return cerr
			}
			e.dbg("sendFile: readFrame err=%v rxRaw=%s", err, raw)
			return err
		}
		e.dbg("sendFile: got frame typ=%d pos=%d rxRaw=%s", fr.typ, fr.pos, raw)
		switch fr.typ {
		case zskip:
			// 服务器拒收（如水木对超大附件直接跳过），必须上报 skipped，否则弹窗不显示且误报成功。
			e.lastSkipped = true
			e.progress("skipped", name, size, 0, "服务器拒收 "+name+"（已跳过，可能超出附件大小限制）")
			return nil
		case zrpos:
			startPos = fr.pos
			havePos = true
		case zabort, zferr:
			return fmt.Errorf("%w: ZFILE 阶段收到 ZABORT/ZFERR", errRemoteAbort)
		default:
			// ZRINIT/ZRQINIT/ZACK 等陈旧或非预期帧，继续等待真正的 ZRPOS。
			continue
		}
	}
	// ⚠️ 关键：拿到 ZRPOS 后必须立刻清除取证用的读超时 deadline。
	// net.Conn.SetDeadline 同时约束读和写；若留着它进入数据循环，服务器接收窗口填满
	// 导致 Write 阻塞时会在 10 秒后触发“写超时”，sendFile 误判失败、提前发 ZFIN，
	// 于是服务器收到半截文件回 ZRPOS(0) 要求从头重传。清掉后交给 TCP 流控自然节流。
	e.setReadDeadline(time.Time{})
	return e.sendFileData(path, name, size, startPos)
}

// sendFileData 以 zmodem2(Rust 工作版)验证过的「窗口流控 + 周期性 ZACK」向水木 rz 发送文件数据。
// 两次纠错：①纯 ZCRCG 猛灌使服务器 TCP 零窗口卡死(每 4096 子包 conn.Write 阻 20~36s);②每包都 ZCRCW
// 停等被水木直接拒收(offset 永卡在 0、狂刷 ZRPOS(0))。根因看清水木 ZRINIT:日志 first frame
// typ=1 pos=587202560=0x23000000 → 小端 flags[3]=0x23 = CANFDX|CANOVIO|hex32pos,**CANOVIO 已置位**!
// 所以 zmodem2 走 can_ovio=true 分支:max_subpackets_per_ack = streaming_window = 10。正确做法是每个数据帧
// = 帧首一次 ZDATA 头 + 9 个裸 ZCRCG 子包 + 帧尾 1 个 ZCRCW,发完 ZCRCW 才停下等 ZACK(offset 驱动);
// offset>=size 发 ZEOF 收尾,否则从该 offset 重发新帧。既以连续流高速接收,又靠周期性 ZACK 精确限速防过冲。
// ZEOF 后按 SendEofOps：ZRINIT=已收好；ZACK=陈旧确认(忽略)；ZNAK=重发 ZEOF；ZRPOS(pos>0)=递归重发。
func (e *zmodemEngine) sendFileData(path, name string, size int64, startPos uint32) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	e.progress("start", name, size, int64(startPos), "上传 "+name)

	// sendZEOFAndWait：发 ZEOF(size) 并循环等真正 ZRINIT（对齐 FQTerm SendEofOps）。
	sendZEOFAndWait := func() error {
		e.setReadDeadline(time.Time{})
		e.sendHexHeader(zeof, uint32(size))
		zeofDeadline := time.Now().Add(60 * time.Second)
		for {
			e.setReadDeadline(zeofDeadline)
			fr, rerr := e.readFrame()
			if rerr != nil {
				if cerr := e.checkCancel(); cerr != nil {
					e.setReadDeadline(time.Time{})
					return cerr
				}
				e.dbg("sendFileData: ZEOF-wait readFrame err=%v", rerr)
				e.setReadDeadline(time.Time{})
				return fmt.Errorf("zmodem: 未收到文件完成确认: %w", rerr)
			}
			e.dbg("sendFileData: ZEOF-wait frame typ=%d pos=%d", fr.typ, fr.pos)
			switch fr.typ {
			case zrinit:
				e.setReadDeadline(time.Time{})
				return nil // 文件成功接收
			case zack:
				continue // 陈旧确认,继续等 ZRINIT
			case znak:
				e.sendHexHeader(zeof, uint32(size)) // 重发 ZEOF
			case zrpos:
				if fr.pos == 0 || int64(fr.pos) >= size {
					continue // 陈旧 ZRPOS(0) 或已收全,继续等真正 ZRINIT
				}
				e.dbg("sendFileData: ZEOF-wait ZRPOS restart at %d", fr.pos)
				e.setReadDeadline(time.Time{})
				return e.sendFileData(path, name, size, fr.pos)
			case zabort, zferr:
				e.setReadDeadline(time.Time{})
				return fmt.Errorf("%w: ZEOF 阶段收到 ZABORT/ZFERR", errRemoteAbort)
			default:
				continue
			}
		}
	}

	pos := startPos
	buf := make([]byte, 1024) // zmodem2 SUBPACKET_MAX_SIZE=1024
	const window = 10         // 水木 ZRINIT3=0x23 含 CANOVIO → zmodem2 can_ovio=true → max_subpackets_per_ack=10
	overall := time.Now().Add(600 * time.Second)
	first := true // 本帧是否需要先发 ZDATA 头（帧首）
	sent := 0     // 本帧已发子包数
	for {
		if cerr := e.checkCancel(); cerr != nil {
			return cerr
		}
		if time.Now().After(overall) {
			e.setReadDeadline(time.Time{})
			return fmt.Errorf("zmodem: 上传整体超时")
		}
		if _, serr := f.Seek(int64(pos), 0); serr != nil {
			return serr
		}
		n, _ := f.Read(buf)
		if n <= 0 {
			return sendZEOFAndWait()
		}
		next := pos + uint32(n)
		isLastData := int64(next) >= size
		sent++
		sub := byte(zcRCG)
		if isLastData || sent == window {
			sub = zcRCW // 帧尾或文件末尾：请求并等待 ZACK
		}
		if first { // 帧首：ZDATA hex 头 + 数据 + 子包尾 + CRC
			if derr := e.sendDataFrame(pos, buf[:n], sub); derr != nil {
				return derr
			}
			first = false
		} else { // 帧内：裸子包(转义数据 + ZDLE + 子包尾 + CRC)，不带头
			if derr := e.sendSubpacket(buf[:n], sub); derr != nil {
				return derr
			}
		}
		e.progress("transfer", name, size, int64(next), "")
		if sub != zcRCW {
			pos = next
			continue // 帧内连续流,不等 ACK
		}

		// 帧尾 ZCRCW：停下等服务器 ZACK/ZRPOS，发送位置由其 offset 驱动。
	ack:
		for {
			e.setReadDeadline(time.Now().Add(20 * time.Second))
			fr, aerr := e.readFrame()
			raw := hex.EncodeToString(e.rxRaw)
			e.rxRaw = e.rxRaw[:0]
			if aerr != nil {
				if cerr := e.checkCancel(); cerr != nil {
					return cerr
				}
				e.dbg("sendFileData: ack-wait timeout expected=%d rxRaw=%s", next, raw)
				e.setReadDeadline(time.Time{})
				return fmt.Errorf("zmodem: 等待 ACK 超时(pos %d)", next)
			}
			e.dbg("sendFileData: ack typ=%d pos=%d expected=%d rxRaw=%s", fr.typ, fr.pos, next, raw)
			switch fr.typ {
			case zack, zrpos:
				if int64(fr.pos) >= size {
					e.dbg("sendFileData: ack off=%d>=size, ZEOF", fr.pos)
					return sendZEOFAndWait()
				}
				if fr.pos != next {
					e.dbg("sendFileData: ack off=%d != expected=%d, reposition", fr.pos, next)
				}
				pos = fr.pos
				first = true // 新帧需重发 ZDATA 头
				sent = 0
				break ack
			case zabort, zferr:
				e.setReadDeadline(time.Time{})
				return fmt.Errorf("%w: 数据阶段收到 ZABORT/ZFERR", errRemoteAbort)
			default:
				// ZRINIT/ZNAK/其它：数据态按 zmodem2 一律忽略，继续等真正的 ZACK/ZRPOS。
				continue ack
			}
		}
	}
}

// sendZSInit 与 FQTerm SendZSInit 对齐，但采用 hex 头 + 二进制数据块。
// attn 数据为空（一个 NUL 字节），subch=ZCRCW。
func (e *zmodemEngine) sendZSInit() error {
	return e.sendHexHdrData(zsinit, 0, []byte{0}, zcRCW)
}

func (e *zmodemEngine) sendZFile(desc []byte) {
	// ZFILE 采用 hex 头 + 二进制描述符数据块（兼容只解析 hex 的 BBS 服务器）。
	e.sendHexHdrData(zfile, 0, desc, zcRCW)
}
