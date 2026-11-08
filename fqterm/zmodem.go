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

	"github.com/wailsapp/wails/v3/pkg/application"
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

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// detectZmodem strictly matches the canonical ZMODEM header introducer.
// 与 FQTerm IdleChar 一致：ZDLE 前必须有 ZPAD('*')。hex 帧为 `**X`、
// 二进制帧为 `*X`，两者都含子串 `ZPAD ZDLE`。旧版额外匹配“裸 ZDLE+格式字节”
// 会把屏幕数据里偶发的 0x18 'A'/'B'/'C' 误判为帧头。
func detectZmodem(raw []byte) bool {
	for i := 0; i+1 < len(raw); i++ {
		if raw[i] == zpad && raw[i+1] == zdle {
			return true // ZPAD ZDLE ...
		}
	}
	return false
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
	typ   byte
	pos   uint32
	data  []byte
	subch byte
	crcOK bool
}

func (ts *TerminalService) runZmodem(s *Session, initial []byte) {
	defer func() {
		if r := recover(); r != nil {
			ts.emitData(s.ID, fmt.Sprintf("\r\n\x1b[31m[ZMODEM 异常: %v]\x1b[0m\r\n", r))
		}
	}()
	dlDir := ts.store.Prefs.ZmodemDir
	if dlDir == "" {
		dlDir = filepath.Join(ts.store.Dir(), "downloads")
	}
	os.MkdirAll(dlDir, 0o755)
	_ = initial
	s.zmCancel.Store(false) // 新一轮传输，清除上一轮可能残留的取消标志
	s.zmCooldown.Store(0)   // 会话已正式启动，解除残留帧冷却
	s.zmActive.Store(true)  // 传输期间防空闲必须静默（注入的 NUL 会打乱帧流）
	defer s.zmActive.Store(false)
	e := &zmodemEngine{sess: s, rwc: s.conn, dlDir: dlDir, direction: "recv", prebuf: initial, ts: ts, dbgOn: ts.dbgPath != ""}
	if len(s.uploadFiles) > 0 {
		e.upload = s.uploadFiles
		e.direction = "send"
		s.uploadFiles = nil
	}
	if err := e.run(); err != nil {
		if !errors.Is(err, errCancelled) { // 取消已由 checkCancel 上报过 cancelled，不重复报错误
			e.progress("error", "", 0, 0, "ZMODEM: "+err.Error())
		}
	}
	ts.emitData(s.ID, "\r\n")
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
	application.Get().Event.Emit("zmodem:progress", &ZmodemProgress{
		ID: e.sess.ID, Direction: e.direction, Filename: name,
		Size: size, Done: done, State: state, Message: msg,
	})
}

// errCancelled 表示用户通过进度弹窗取消了传输，区别于真实协议错误。
var errCancelled = errors.New("zmodem: 已取消")

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
				continue // 畸形帧，重新同步
			}
			return fr, nil
		case zbin, zbin32:
			fr, ferr := e.readBinFrame(f == zbin32)
			if ferr != nil {
				continue
			}
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
	for len(hx) < 16 {
		c, err := e.rxByte()
		if err != nil {
			return frame{}, err
		}
		if !isHexDigit(c) {
			break
		}
		hx = append(hx, c)
	}
	if len(hx) < 2 {
		return frame{}, fmt.Errorf("bad hex header")
	}
	// 消费行尾 CR/LF（尽力，忽略可选 XON）。
	for i := 0; i < 2; i++ {
		c, err := e.rxByte()
		if err != nil || c == '\n' {
			break
		}
	}
	n := len(hx) / 2
	b := make([]byte, n)
	for i := 0; i < n; i++ {
		b[i] = byte(hexVal(hx[i*2])<<4 | hexVal(hx[i*2+1]))
	}
	var h [5]byte
	copy(h[:], b)
	return frame{typ: b[0], pos: headerPos(h), crcOK: true}, nil
}

func (e *zmodemEngine) readBinFrame(crc32f bool) (frame, error) {
	h := make([]byte, 5)
	for i := 0; i < 5; i++ {
		c, err := e.rxByte()
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
		c, err := e.rxByte()
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
		return frame{typ: typ, pos: pos, crcOK: true}, nil
	}

	// 数据块（到 ZDLE+子包类型字节为止），后面自带另一个独立 CRC。
	data := make([]byte, 0, 1024)
	var subch byte
	for {
		c, err := e.rxByte()
		if err != nil {
			return frame{}, err
		}
		if c == zdle {
			c2, err := e.rxByte()
			if err != nil {
				return frame{}, err
			}
			switch c2 {
			case zcRCE, zcRCG, zcRCQ, zcRCW:
				subch = c2
				goto readdatacrc
			default:
				data = append(data, unescape(c2))
				continue
			}
		}
		data = append(data, c)
	}
readdatacrc:
	dcover := append(append([]byte{}, data...), subch)
	ok := true
	if crc32f {
		b := make([]byte, 4)
		for i := 0; i < 4; i++ {
			c, _ := e.rxByte()
			b[i] = c
		}
		ok = zcrc32(dcover) == binary.LittleEndian.Uint32(b)
	} else {
		b := make([]byte, 2)
		for i := 0; i < 2; i++ {
			c, _ := e.rxByte()
			b[i] = c
		}
		ok = crc16XMODEM(dcover) == binary.BigEndian.Uint16(b)
	}
	return frame{typ: typ, pos: pos, data: data, subch: subch, crcOK: ok}, nil
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
	fr, err := e.readFrame()
	if err != nil {
		// 残留噪声/半帧：不回应、不启动会话。旧版在此广播 ZRINIT 会把垃圾注入 BBS 命令行。
		e.dbg("run: readFrame err=%v -> 不启动会话（避免向 BBS 误发 ZRINIT）", err)
		return nil
	}
	if e.ts != nil {
		e.ts.dbg("run: first frame typ=%d pos=%d", fr.typ, fr.pos)
	}
	if fr.typ != zrinit && fr.typ != zrqinit {
		// 只有 ZRINIT（对端 rz 等收）/ ZRQINIT（对端 sz 待发）才是合法开场帧；
		// 取消后在途的 ZACK/ZRPOS/ZEOF 等一律视为残留噪声，不启动会话、不回应。
		e.dbg("run: 非开场帧 typ=%d，视为残留噪声，不启动会话", fr.typ)
		return nil
	}
	if fr.typ == zrinit {
		paths, perr := e.promptUploadFiles()
		if perr != nil || len(paths) == 0 {
			// 用户取消：发 ZABORT 让远端 rz 退出，否则它会持续重发 ZRINIT 反复弹框。
			e.sendHexHeader(zabort, 0)
			e.progress("cancelled", "", 0, 0, "已取消上传")
			return nil
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
	if err := e.sendHexHeader(zrinit, 0); err != nil {
		return err
	}
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
		fr, err := e.readFrame()
		if err != nil {
			if cerr := e.checkCancel(); cerr != nil {
				return cerr
			}
			return err
		}
		switch fr.typ {
		case zsinit:
			continue
		case zfile:
			if err := e.receiveFile(fr); err != nil {
				return err
			}
		case zack, zrinit, zrqinit:
			continue
		case zfin, zcompl:
			e.writeRaw([]byte{zpad, zpad, 'O', 'O'})
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
	name, size, descErr := parseFileDesc(fileHdr.data)
	if descErr != nil {
		e.sendHexHeader(zskip, 0)
		return e.receiver()
	}
	path := uniquePath(e.dlDir, sanitizeName(name))
	f, err := os.Create(path)
	if err != nil {
		e.sendHexHeader(zferr, 0)
		return fmt.Errorf("create %s: %w", name, err)
	}
	defer f.Close()
	e.progress("start", name, size, 0, "接收 "+name)
	e.sendHexHeader(zrpos, 0)

	var offset int64
	for {
		if cerr := e.checkCancel(); cerr != nil {
			return cerr
		}
		fr, err := e.readFrame()
		if err != nil {
			if cerr := e.checkCancel(); cerr != nil {
				return cerr
			}
			return err
		}
		switch fr.typ {
		case zdata:
			if !fr.crcOK {
				// Resync request.
				e.sendHexHeader(zrpos, uint32(offset))
				continue
			}
			if len(fr.data) > 0 {
				f.Write(fr.data)
				offset += int64(len(fr.data))
			}
			switch fr.subch {
			case zcRCW, zcRCQ:
				e.sendHexHeader(zack, uint32(offset))
			case zcRCE:
				e.progress("transfer", name, size, offset, "")
			}
		case zeof:
			f.Close()
			e.progress("done", name, size, offset, "已保存到 "+path)
			e.sendHexHeader(zrinit, 0)
			return nil
		case zfin:
			f.Close()
			e.progress("done", name, size, offset, "已保存到 "+path)
			return nil
		case zabort, zferr:
			return nil
		default:
			return nil
		}
	}
}

func parseFileDesc(data []byte) (string, int64, error) {
	s := string(data)
	s = strings.ReplaceAll(s, "\x00", " ")
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return "", 0, fmt.Errorf("empty descriptor")
	}
	name := filepath.Base(fields[0])
	var size int64
	if len(fields) >= 2 {
		size, _ = strconv.ParseInt(fields[1], 10, 64)
	}
	return name, size, nil
}

func sanitizeName(name string) string {
	name = filepath.Base(name)
	if name == "" || name == "." || name == "/" {
		return "zmodem_file"
	}
	return name
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
			failCount++
			continue
		}
		if e.lastSkipped {
			skipCount++
		} else {
			okCount++
		}
	}
	// End of batch：发 ZFIN，等服务器最终 ZRINIT，再发 "**OO" 让对端 rz 退出 ZMODEM
	// 回到命令模式（lrzsz sz 的标准断会序列）。不干净断会，服务器 rz 会残留、
	// 后续字节泄到 BBS 导致行为异常（如误发无附件文章）。
	e.sendHexHeader(zfin, 0)
	e.setReadDeadline(time.Now().Add(10 * time.Second))
	for {
		fr, err := e.readFrame()
		if err != nil || fr.typ == zrinit {
			break
		}
	}
	e.setReadDeadline(time.Time{})
	e.writeRaw([]byte{zpad, zpad, 'O', 'O'})
	// 收尾事件按批次结果区分：全成功才报“上传完成”；部分跳过要注明；
	// 全部被拒/失败时不发 done，保留 skipped/error 终态避免误报成功。
	switch {
	case okCount > 0 && skipCount == 0 && failCount == 0:
		e.progress("done", "", 0, 0, "上传完成")
	case okCount > 0:
		e.progress("done", "", 0, 0, fmt.Sprintf("上传完成（%d 个文件被跳过/失败）", skipCount+failCount))
	case skipCount > 0 || failCount > 0:
		// 无一成功：保留最后一个 skipped/error 事件作为终态
	default:
		e.progress("done", "", 0, 0, "上传完成")
	}
	return nil
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
			return nil
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
				return nil
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
				return nil
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
				return nil
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
