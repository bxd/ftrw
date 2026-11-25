package main

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/text/encoding/simplifiedchinese"
)

// fakeZConn 是脚本化的 ZMODEM 对端：in 是预先编排好的发送方（sz）字节流，
// out 收集引擎写回的全部帧，供断言。
type fakeZConn struct {
	in  []byte
	out []byte
}

func (c *fakeZConn) Read(p []byte) (int, error) {
	if len(c.in) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.in)
	c.in = c.in[n:]
	return n, nil
}

func (c *fakeZConn) Write(p []byte) (int, error) {
	c.out = append(c.out, p...)
	return len(p), nil
}

func (c *fakeZConn) Close() error                { return nil }
func (c *fakeZConn) resize(cols, rows int) error { return nil }

// timeoutOnceConn 在指定字节消费完后制造一次读超时，用于验证接收端补发
// ZRPOS 后仍留在会话里续收，而不是把后续 ZMODEM 二进制数据交回终端。
type timeoutOnceConn struct {
	fakeZConn
	untilTimeout int
	timedOut     bool
}

func (c *timeoutOnceConn) Read(p []byte) (int, error) {
	if !c.timedOut && c.untilTimeout == 0 {
		c.timedOut = true
		return 0, os.ErrDeadlineExceeded
	}
	n, err := c.fakeZConn.Read(p)
	c.untilTimeout -= n
	return n, err
}

// timeoutRepeatedConn 连续制造多次读超时，验证接收端不会按固定重试次数退出会话。
type timeoutRepeatedConn struct {
	fakeZConn
	left int
}

func (c *timeoutRepeatedConn) Read(p []byte) (int, error) {
	if c.left > 0 {
		c.left--
		return 0, os.ErrDeadlineExceeded
	}
	return c.fakeZConn.Read(p)
}

// ---- 测试侧帧构造（与引擎发送端同族编码）----

func tHexHdr(typ byte, field uint32) []byte {
	h := headerBytes(typ, field)
	const hx = "0123456789abcdef"
	out := []byte{zpad, zpad, zdle, zhex}
	for _, c := range h {
		out = append(out, hx[c>>4], hx[c&0xf])
	}
	crc := crc16XMODEM(h[:])
	out = append(out, hx[crc>>12], hx[(crc>>8)&0xf], hx[(crc>>4)&0xf], hx[crc&0xf])
	return append(out, '\r', 0, 0x11) // 水木实测行尾：CR NUL XON
}

func tBin32Hdr(typ byte, field uint32) []byte {
	h := headerBytes(typ, field)
	out := []byte{zpad, zdle, zbin32}
	out = append(out, zEscapeHeader(h[:])...)
	var cb [4]byte
	binary.LittleEndian.PutUint32(cb[:], zcrc32(h[:]))
	return append(out, zEscapeHeader(cb[:])...)
}

func tSubpacket(data []byte, subch byte, crc32f bool) []byte {
	return tSubpacketMode(data, subch, crc32f, false)
}

// tTelnetSubpacket 复刻水木 sz：数据中的 CR 不做 ZDLE 转义，而由 Telnet 链路展开成 CR NUL。
func tTelnetSubpacket(data []byte, subch byte, crc32f bool) []byte {
	return tSubpacketMode(data, subch, crc32f, true)
}

// tRawCRSubpacket 模拟某些 Telnet 链路不重复展开文件原有的 CR NUL：
// 此时线上的 CR NUL 既可能是 Telnet 扩展，也可能是文件内容，必须由 CRC 判定。
func tRawCRSubpacket(data []byte, subch byte, crc32f bool) []byte {
	var out []byte
	for _, c := range data {
		if c == '\r' {
			out = append(out, c)
			continue
		}
		out = append(out, zEscape([]byte{c})...)
	}
	out = append(out, zdle, subch)
	cover := append(append([]byte{}, data...), subch)
	if crc32f {
		var cb [4]byte
		binary.LittleEndian.PutUint32(cb[:], zcrc32(cover))
		return append(out, zEscape(cb[:])...)
	}
	crc := crc16XMODEM(cover)
	return append(out, zEscape([]byte{byte(crc >> 8), byte(crc)})...)
}

// tMixedTelnetSubpacket 同时包含 Telnet 展开的 CR 和文件原有 CR NUL：
// 前者线上为 CR NUL，后者也是 CR NUL，必须由 CRC 逐点判定该删哪个 NUL。
func tMixedTelnetSubpacket(data []byte, subch byte, crc32f bool) []byte {
	var out []byte
	for i, c := range data {
		if c == '\r' {
			out = append(out, c)
			if i+1 >= len(data) || data[i+1] != 0 {
				out = append(out, 0)
			}
			continue
		}
		out = append(out, zEscape([]byte{c})...)
	}
	out = append(out, zdle, subch)
	cover := append(append([]byte{}, data...), subch)
	if crc32f {
		var cb [4]byte
		binary.LittleEndian.PutUint32(cb[:], zcrc32(cover))
		return append(out, zEscape(cb[:])...)
	}
	crc := crc16XMODEM(cover)
	return append(out, zEscape([]byte{byte(crc >> 8), byte(crc)})...)
}

func tSubpacketMode(data []byte, subch byte, crc32f, telnetCR bool) []byte {
	var out []byte
	for _, c := range data {
		if telnetCR && c == '\r' {
			out = append(out, '\r', 0)
			continue
		}
		out = append(out, zEscape([]byte{c})...)
	}
	out = append(out, zdle, subch)
	cover := append(append([]byte{}, data...), subch)
	if crc32f {
		var cb [4]byte
		binary.LittleEndian.PutUint32(cb[:], zcrc32(cover))
		return append(out, zEscape(cb[:])...)
	}
	crc := crc16XMODEM(cover)
	return append(out, zEscape([]byte{byte(crc >> 8), byte(crc)})...)
}

func cat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// TestZmodemReceiveSmth 复刻水木 sz 下载（Ctrl+Y 触发）的真实帧序：
// hex ZRQINIT → ZBIN32 ZSINIT+3 字节数据 → hex ZFILE+二进制描述符（CRC16 不终算）→
// ZBIN32 ZDATA 帧首头 + 背靠背多子包（ZCRCG/ZCRCG/ZCRCW）→ 第二帧 ZCRCE 收尾 → ZEOF → ZFIN。
// 旧实现有三处硬伤：hex 头后的数据段不读（ZFILE 被判坏描述符直接 ZSKIP）、
// 一个 ZDATA 帧只消费一个子包（其余当噪声丢）、ZFIN 回 "**OO" 污染 BBS 命令行。
func TestZmodemReceiveSmth(t *testing.T) {
	c1 := bytes.Repeat([]byte("a"), 1024)
	c2 := bytes.Repeat([]byte("b"), 1024)
	c3 := bytes.Repeat([]byte("c"), 512)
	total := len(c1) + len(c2) + len(c3)
	desc := append(append([]byte("att.jpg"), 0), []byte("2560 0 644 0 1 2560 0")...)
	desc = append(desc, 0)

	stream := cat(
		tHexHdr(zrqinit, 0),
		tBin32Hdr(zsinit, 0), tSubpacket([]byte{0, 0, 0}, zcRCW, true),
		tHexHdr(zfile, 0), tSubpacket(desc, zcRCW, false),
		tBin32Hdr(zdata, 0),
		tSubpacket(c1, zcRCG, true),
		tSubpacket(c2, zcRCG, true),
		tSubpacket(c3, zcRCW, true),
		tBin32Hdr(zeof, uint32(total)),
		tBin32Hdr(zfin, 0),
	)

	// 模拟 readLoop：检测命中的整块字节全部经 prebuf 喂给引擎，socket 本身置空由 EOF 兑底。
	conn := &fakeZConn{}
	dl := t.TempDir()
	e := &zmodemEngine{
		sess:      &Session{ID: "test"},
		rwc:       conn,
		dlDir:     dl,
		direction: "recv",
		prebuf:    stream,
	}

	if err := e.run(); err != nil {
		t.Fatalf("run: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dl, "att", "att.jpg"))
	if err != nil {
		t.Fatalf("下载文件不存在: %v", err)
	}
	if want := append(append(append([]byte{}, c1...), c2...), c3...); !bytes.Equal(got, want) {
		t.Fatalf("文件内容不符: got %d bytes, want %d bytes", len(got), len(want))
	}

	sent := string(conn.out)
	// 开场应答：带 CANFC32 flags 的 ZRINIT（线上字段 00 00 00 23 → hex "0100000023"）
	if !strings.Contains(sent, "0100000023") {
		t.Errorf("未回应带 flags 的 ZRINIT: %q", sent)
	}
	// ZSINIT 后必须回 ZACK(0)，否则水木等待确认、不会发送 ZFILE。
	ack0 := strings.Index(sent, "0300000000")
	if ack0 < 0 {
		t.Errorf("ZSINIT 后未回 ZACK(0): %q", sent)
	}
	// ZFILE 后回 ZRPOS(0)
	rpos0 := strings.Index(sent, "0900000000")
	if rpos0 < 0 {
		t.Errorf("ZFILE 后未回 ZRPOS(0): %q", sent)
	} else if ack0 >= 0 && ack0 > rpos0 {
		t.Errorf("ZSINIT 的 ZACK 晚于 ZFILE 应答，水木仍会等待确认: %q", sent)
	}
	// ZCRCW 帧尾后回 ZACK，offset=2560 → LE 00 0a 00 00 → "03000a0000"
	if !strings.Contains(sent, "03000a0000") {
		t.Errorf("ZCRCW 后未回带 offset 的 ZACK: %q", sent)
	}
	// ZFIN 断会：水木/FQTerm 接收状态机回 ZFIN（pos=0），且绝不出现 "**OO"
	if !strings.Contains(sent, "0800000000") {
		t.Errorf("ZFIN 后未回 ZFIN: %q", sent)
	}
	if strings.Contains(sent, "**OO") {
		t.Errorf("断会误发 **OO，会污染 BBS 命令行")
	}
}

// TestZmodemReceiveSmthPrematureEofResumes 复刻实测截断：服务端先发 232 字节文章头，
// 随即发 ZEOF(232)，但 ZFILE 声明总长 1024。接收方必须请求 ZRPOS(232) 续传，
// 不能把 232/1024 误报为下载完成；补齐后只接受 ZEOF(1024)。
func TestZmodemReceiveSmthPrematureEofResumes(t *testing.T) {
	head := bytes.Repeat([]byte("h"), 232)
	tail := bytes.Repeat([]byte("t"), 1024-len(head))
	desc := append(append([]byte("post.txt"), 0), []byte("1024 0 644 0 1 1024 0")...)
	desc = append(desc, 0)

	stream := cat(
		tHexHdr(zrqinit, 0),
		tHexHdr(zfile, 0), tSubpacket(desc, zcRCW, false),
		tBin32Hdr(zdata, 0), tSubpacket(head, zcRCE, true),
		tBin32Hdr(zeof, uint32(len(head))), // 提前 EOF：应触发续传请求
		tBin32Hdr(zdata, uint32(len(head))), tSubpacket(tail, zcRCE, true),
		tBin32Hdr(zeof, 1024),
		tBin32Hdr(zfin, 0),
	)

	conn := &fakeZConn{}
	dl := t.TempDir()
	e := &zmodemEngine{
		sess:      &Session{ID: "test"},
		rwc:       conn,
		dlDir:     dl,
		direction: "recv",
		prebuf:    stream,
	}
	if err := e.run(); err != nil {
		t.Fatalf("run: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dl, "post", "post.txt"))
	if err != nil {
		t.Fatalf("下载文件不存在: %v", err)
	}
	want := append(append([]byte{}, head...), tail...)
	if !bytes.Equal(got, want) {
		t.Fatalf("提前 EOF 后未正确续传: got %d bytes, want %d bytes", len(got), len(want))
	}
	if sent := string(conn.out); !strings.Contains(sent, "09e8000000") {
		t.Errorf("提前 EOF 后未请求 ZRPOS(232): %q", sent)
	}
}

// TestZmodemReceiveSmthEarlyFinIsError 验证文件未收完时收到 ZFIN：仍回 ZFIN 让服务端退出，
// 但必须返回错误，不能 progress(done) 假报成功。
func TestZmodemReceiveSmthEarlyFinIsError(t *testing.T) {
	head := bytes.Repeat([]byte("h"), 232)
	desc := append(append([]byte("post.txt"), 0), []byte("1024 0 644 0 1 1024 0")...)
	desc = append(desc, 0)
	stream := cat(
		tHexHdr(zrqinit, 0),
		tHexHdr(zfile, 0), tSubpacket(desc, zcRCW, false),
		tBin32Hdr(zdata, 0), tSubpacket(head, zcRCE, true),
		tBin32Hdr(zfin, 0),
	)

	conn := &fakeZConn{}
	dl := t.TempDir()
	e := &zmodemEngine{
		sess:      &Session{ID: "test"},
		rwc:       conn,
		dlDir:     dl,
		direction: "recv",
		prebuf:    stream,
	}
	if err := e.run(); err == nil || !strings.Contains(err.Error(), "传输提前结束") {
		t.Fatalf("未收完时 ZFIN 应返回明确错误，实际 %v", err)
	}
	if sent := string(conn.out); !strings.Contains(sent, "0800000000") {
		t.Errorf("提前 ZFIN 后未回 ZFIN 让服务端退出: %q", sent)
	}
}

// TestZmodemReceiveGBKFilename 复刻水木 ZFILE 使用 GBK 原始字节发送中文名：
// 接收方必须先解码再落盘，且合法 UTF-8 文件名仍保持原样。
func TestZmodemReceiveGBKFilename(t *testing.T) {
	const wantName = "水木文章.TXT"
	rawName, err := simplifiedchinese.GBK.NewEncoder().String(wantName)
	if err != nil {
		t.Fatal(err)
	}
	desc := append(append([]byte(rawName), 0), []byte("4 0 644 0 1 4 0")...)
	desc = append(desc, 0)
	payload := []byte("data")
	stream := cat(
		tHexHdr(zrqinit, 0),
		tHexHdr(zfile, 0), tSubpacket(desc, zcRCW, false),
		tBin32Hdr(zdata, 0), tSubpacket(payload, zcRCE, true),
		tBin32Hdr(zeof, uint32(len(payload))),
		tBin32Hdr(zfin, 0),
	)

	conn := &fakeZConn{}
	dl := t.TempDir()
	e := &zmodemEngine{
		sess:      &Session{ID: "test"},
		rwc:       conn,
		dlDir:     dl,
		direction: "recv",
		prebuf:    stream,
	}
	if err := e.run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dl, "水木文章", wantName))
	if err != nil {
		t.Fatalf("GBK 文件名未正确解码后落盘: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("文件内容不符: got %q, want %q", got, payload)
	}
}

// TestHexNoDataFrameReturnsImmediately 复刻本次卡死取证：水木 ZRQINIT 发完
// 「14 hex + CR NUL XON」就停住等应答。readHexFrame 绝不能为找可选 XON/下一帧
// 再阻塞读 socket；旧实现前瞻等 12 秒，ZRINIT 超过服务端握手窗口。
func TestHexNoDataFrameReturnsImmediately(t *testing.T) {
	conn := &fakeZConn{}                                           // 没有后续字节：任何前瞻都会立刻 EOF
	e := &zmodemEngine{rwc: conn, prebuf: tHexHdr(zrqinit, 0)[4:]} // 前 4 字节 **B 由 readFrame 消费
	fr, err := e.readHexFrame()
	if err != nil {
		t.Fatalf("无数据 hex 帧应读完即返回: %v", err)
	}
	if fr.typ != zrqinit || fr.pos != 0 {
		t.Fatalf("帧解析错误: typ=%d pos=%d", fr.typ, fr.pos)
	}
	// XON 应原样留给外层扫描，而不是被无界前瞻吞掉。
	if len(e.prebuf) != 1 || e.prebuf[0] != 0x11 {
		t.Errorf("XON 应留在 prebuf，实际 %x", e.prebuf)
	}
}

// TestZmodemReadBinFrameTelnetCRNULFromLog 使用取证日志中的真实坏帧：
// 数据里有一个裸 CR，Telnet 链路在其后插入 NUL。去掉该 NUL 后 CRC32 与线上值完全一致。
func TestZmodemReadBinFrameTelnetCRNULFromLog(t *testing.T) {
	raw, err := hex.DecodeString("2a18430a80020000e98d4f622d706172610000000000040000000266660000f2a700000d005900001853d000000a186beec2755d11")
	if err != nil {
		t.Fatal(err)
	}
	e := &zmodemEngine{prebuf: raw[3:]} // 前 3 字节 * ZDLE ZBIN32 由 readFrame 消费
	fr, err := e.readBinFrame(true)
	if err != nil {
		t.Fatalf("readBinFrame: %v", err)
	}
	want := []byte{0x2d, 0x70, 0x61, 0x72, 0x61, 0, 0, 0, 0, 0, 4, 0, 0, 0, 2, 0x66, 0x66, 0, 0, 0xf2, 0xa7, 0, 0, 0x0d, 0x59, 0, 0, 0x13, 0xd0, 0, 0, 0x0a}
	if fr.typ != zdata || fr.pos != 640 || !fr.crcOK || !bytes.Equal(fr.data, want) {
		t.Fatalf("Telnet CR NUL 帧解析错误: typ=%d pos=%d crcOK=%v data=%x", fr.typ, fr.pos, fr.crcOK, fr.data)
	}
}

// TestZmodemReceiveTelnetCRNUL 验证完整下载中多个 CR（包括原始 CR NUL）都能正确还原，
// 且 ZFILE 声明大小为 0 时仍按 ZEOF 的实际 offset 完成。
func TestZmodemReceiveTelnetCRNUL(t *testing.T) {
	payload := []byte("first\r\nsecond\r\x00third\rlast")
	desc := append(append([]byte("post.txt"), 0), []byte("0 0 644 0 1 0 0")...)
	desc = append(desc, 0)
	stream := cat(
		tHexHdr(zrqinit, 0),
		tBin32Hdr(zsinit, 0), tSubpacket([]byte{0, 0, 0}, zcRCW, true),
		tBin32Hdr(zfile, 0), tSubpacket(desc, zcRCW, true),
		tBin32Hdr(zdata, 0), tTelnetSubpacket(payload, zcRCE, true),
		tBin32Hdr(zeof, uint32(len(payload))),
		tBin32Hdr(zfin, 0),
	)

	conn := &fakeZConn{}
	dl := t.TempDir()
	e := &zmodemEngine{
		sess:      &Session{ID: "test"},
		rwc:       conn,
		dlDir:     dl,
		direction: "recv",
		prebuf:    stream,
	}
	if err := e.run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dl, "post", "post.txt"))
	if err != nil {
		t.Fatalf("下载文件不存在: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("Telnet CR NUL 未正确还原: got %x, want %x", got, payload)
	}
}

func TestZmodemReceiveResumesAfterFrameTimeout(t *testing.T) {
	payload := []byte("payload after retry")
	desc := append(append([]byte("post.txt"), 0), []byte("0 0 644 0 1 0 0")...)
	desc = append(desc, 0)
	prefix := cat(tHexHdr(zrqinit, 0), tHexHdr(zfile, 0), tSubpacket(desc, zcRCW, false))
	rest := cat(
		tBin32Hdr(zdata, 0), tSubpacket(payload, zcRCE, true),
		tBin32Hdr(zeof, uint32(len(payload))),
		tBin32Hdr(zfin, 0),
	)
	conn := &timeoutOnceConn{fakeZConn: fakeZConn{in: rest}}
	dl := t.TempDir()
	e := &zmodemEngine{
		sess:      &Session{ID: "test"},
		rwc:       conn,
		dlDir:     dl,
		direction: "recv",
		prebuf:    prefix,
	}
	if err := e.run(); err != nil {
		t.Fatalf("超时后续收失败: %v", err)
	}
	if !conn.timedOut {
		t.Fatal("测试未触发读超时")
	}
	got, err := os.ReadFile(filepath.Join(dl, "post", "post.txt"))
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("超时后文件内容错误: %x（%v）", got, err)
	}
}

func TestZmodemReadDataSectionAmbiguousCRNUL(t *testing.T) {
	payload := []byte("head\r\x00tail")
	for _, crc32f := range []bool{false, true} {
		name := "CRC16"
		if crc32f {
			name = "CRC32"
		}
		t.Run(name, func(t *testing.T) {
			e := &zmodemEngine{prebuf: tRawCRSubpacket(payload, zcRCG, crc32f)}
			data, subch, ok, err := e.readDataSection(crc32f)
			if err != nil {
				t.Fatal(err)
			}
			if !ok || subch != zcRCG || !bytes.Equal(data, payload) {
				t.Fatalf("二义 CR NUL 解析错误: subch=%c crcOK=%v data=%x want=%x", subch, ok, data, payload)
			}
		})
	}
}

func TestZmodemReadDataSectionMixedCRNUL(t *testing.T) {
	// 四个二义点中，第 1/3 个是 Telnet 扩展应删除，第 2/4 个是文件原有 NUL 应保留。
	payload := []byte("a\rb\r\x00c\r\nd\r\x00e")
	for _, crc32f := range []bool{false, true} {
		name := "CRC16"
		if crc32f {
			name = "CRC32"
		}
		t.Run(name, func(t *testing.T) {
			e := &zmodemEngine{prebuf: tMixedTelnetSubpacket(payload, zcRCG, crc32f)}
			data, subch, ok, err := e.readDataSection(crc32f)
			if err != nil {
				t.Fatal(err)
			}
			if !ok || subch != zcRCG || !bytes.Equal(data, payload) {
				t.Fatalf("混合 CR NUL 解析错误: subch=%c crcOK=%v data=%x want=%x", subch, ok, data, payload)
			}
		})
	}
}

func TestZmodemReceiveWaitsBeyondThreeTimeouts(t *testing.T) {
	payload := []byte("payload after four timeouts")
	desc := append(append([]byte("post.txt"), 0), []byte("0 0 644 0 1 0 0")...)
	desc = append(desc, 0)
	prefix := cat(tHexHdr(zrqinit, 0), tHexHdr(zfile, 0), tSubpacket(desc, zcRCW, false))
	rest := cat(
		tBin32Hdr(zdata, 0), tSubpacket(payload, zcRCE, true),
		tBin32Hdr(zeof, uint32(len(payload))),
		tBin32Hdr(zfin, 0),
	)
	conn := &timeoutRepeatedConn{fakeZConn: fakeZConn{in: rest}, left: 4}
	dl := t.TempDir()
	e := &zmodemEngine{
		sess: &Session{ID: "test"}, rwc: conn, dlDir: dl,
		direction: "recv", prebuf: prefix,
	}
	if err := e.run(); err != nil {
		t.Fatalf("多次超时后应继续会话: %v", err)
	}
	if conn.left != 0 {
		t.Fatalf("测试未消耗全部超时: left=%d", conn.left)
	}
	got, err := os.ReadFile(filepath.Join(dl, "post", "post.txt"))
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("多次超时后文件内容错误: %x（%v）", got, err)
	}
}

func TestDetectZmodemValidatesHeaderCRC(t *testing.T) {
	if !detectZmodem(tHexHdr(zrqinit, 0)) {
		t.Fatal("合法 hex 帧头未识别")
	}
	if !detectZmodem(tBin32Hdr(zdata, 1234)) {
		t.Fatal("合法 ZBIN32 帧头未识别")
	}
	noise := []byte{0xff, 0xd8, zpad, zdle, 0x55, 0x00}
	if detectZmodem(noise) {
		t.Fatal("二进制噪声被误判为 ZMODEM")
	}
	bad := append([]byte{}, tBin32Hdr(zdata, 1234)...)
	bad[4] ^= 1
	if detectZmodem(bad) {
		t.Fatal("帧头 CRC 损坏仍被误判为 ZMODEM")
	}
}

func TestSessionDiscardZmodemTail(t *testing.T) {
	now := time.Unix(1000, 0)
	s := &Session{}
	s.zmDrain.Store(true)
	s.zmLastRaw.Store(now.Add(-time.Second).UnixNano())
	if !s.discardZmodemTail(now) {
		t.Fatal("静默窗口内的残留二进制应丢弃")
	}
	if s.discardZmodemTail(now.Add(zmDrainQuiet + time.Second)) {
		t.Fatal("残留数据静默后应恢复终端输出")
	}
	if s.zmDrain.Load() {
		t.Fatal("静默后未清除残留丢弃状态")
	}
}

func TestZmodemDownloadDirectoryUnique(t *testing.T) {
	e := &zmodemEngine{dlDir: t.TempDir()}
	first, err := e.createDownloadDir("post.txt")
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.createDownloadDir("post.txt")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(first) != "post" || filepath.Base(second) != "post_2" {
		t.Fatalf("下载目录重名后缀不正确: %q, %q", first, second)
	}
	for _, dir := range []string{first, second} {
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			t.Fatalf("下载目录未创建: %s（%v）", dir, err)
		}
	}
}

func articleRecord(name []byte, data []byte) []byte {
	out := append([]byte{}, attachmentPad...)
	out = append(out, name...)
	out = append(out, 0)
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(data)))
	out = append(out, size[:]...)
	return append(out, data...)
}

func TestParseArticleAttachments(t *testing.T) {
	rawName, err := simplifiedchinese.GBK.NewEncoder().String("图片.jpg")
	if err != nil {
		t.Fatal(err)
	}
	image := append([]byte("jpeg-head"), attachmentPad...)
	image = append(image, []byte("jpeg-tail")...)
	data := append([]byte("标题\r\n正文\r\n"), articleRecord([]byte(rawName), image)...)
	data = append(data, articleRecord([]byte("notes.txt"), []byte("second"))...)

	body, attachments, ok := parseArticleAttachments(data, 0)
	if !ok {
		t.Fatal("合法文章附件容器未识别")
	}
	if string(body) != "标题\r\n正文" {
		t.Fatalf("正文分隔换行处理错误: %q", body)
	}
	if len(attachments) != 2 || attachments[0].name != "图片.jpg" || attachments[1].name != "notes.txt" {
		t.Fatalf("附件名/顺序错误: %+v", attachments)
	}
	if !bytes.Equal(attachments[0].data, image) || string(attachments[1].data) != "second" {
		t.Fatal("附件内容被长度字段之外的标记干扰")
	}
}

func TestSplitDownloadedArticle(t *testing.T) {
	dir := t.TempDir()
	original := filepath.Join(dir, "post.TXT")
	payload := append([]byte("正文\n"), articleRecord([]byte("same.jpg"), []byte("one"))...)
	payload = append(payload, articleRecord([]byte("same.jpg"), []byte("two"))...)
	if err := os.WriteFile(original, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	paths, split, err := splitDownloadedArticle(original, 0)
	if err != nil || !split {
		t.Fatalf("拆分失败: split=%v err=%v", split, err)
	}
	wantNames := []string{"post_正文.txt", "same.jpg", "same_1.jpg"}
	if len(paths) != len(wantNames) {
		t.Fatalf("预览路径数量错误: %v", paths)
	}
	for i, want := range wantNames {
		if filepath.Base(paths[i]) != want {
			t.Errorf("第 %d 个产物名 = %q，期望 %q", i, filepath.Base(paths[i]), want)
		}
	}
	if got, err := os.ReadFile(paths[0]); err != nil || string(got) != "正文" {
		t.Fatalf("正文文件错误: %q（%v）", got, err)
	}
	if got, err := os.ReadFile(paths[1]); err != nil || string(got) != "one" {
		t.Fatalf("附件一错误: %q（%v）", got, err)
	}
	if got, err := os.ReadFile(paths[2]); err != nil || string(got) != "two" {
		t.Fatalf("附件二错误: %q（%v）", got, err)
	}
	if got, err := os.ReadFile(original); err != nil || !bytes.Equal(got, payload) {
		t.Fatal("原始下载文件被拆分过程修改")
	}
}

func TestSplitDownloadedArticleRejectsMalformed(t *testing.T) {
	cases := map[string][]byte{
		"plain":            []byte("ordinary file"),
		"missing name end": append(append([]byte{}, attachmentPad...), []byte("a.jpg")...),
		"short size":       append(append([]byte{}, attachmentPad...), append([]byte("a.jpg"), 0, 1, 2)...),
		"oversized":        append(append([]byte{}, attachmentPad...), append([]byte("a.jpg"), 0, 0, 0, 0, 9)...),
		"trailing garbage": append(articleRecord([]byte("a.jpg"), []byte("x")), 'x'),
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			original := filepath.Join(dir, "post.TXT")
			if err := os.WriteFile(original, payload, 0o644); err != nil {
				t.Fatal(err)
			}
			paths, split, err := splitDownloadedArticle(original, 0)
			if err != nil || split || len(paths) != 1 || paths[0] != original {
				t.Fatalf("畸形容器应保留原件: paths=%v split=%v err=%v", paths, split, err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 || entries[0].Name() != "post.TXT" {
				t.Fatalf("畸形容器产生了半套拆分文件: %v（%v）", entries, err)
			}
		})
	}
}

func TestSanitizeZmodemName(t *testing.T) {
	cases := map[string]string{
		`C:\tmp\foo:bar?.TXT`: "foo_bar_.TXT",
		"CON.txt":             "zmodem_file.txt",
		"..":                  "zmodem_file",
	}
	for raw, want := range cases {
		if got := sanitizeName(raw); got != want {
			t.Errorf("sanitizeName(%q) = %q，期望 %q", raw, got, want)
		}
	}
}

func TestZmodemReadDataSectionDroppedIACNUL(t *testing.T) {
	// 同一包里同时有 Telnet 展开的 CR、文件原有 CR NUL，以及被链路吞掉的 FF 00。
	// 接收端必须组合还原二义 NUL，并用 CRC32 反查丢失的 FF 00 位置。
	payload := []byte("head\r\n\r\x00middle")
	payload = append(payload, bytes.Repeat([]byte{0xff, 0, 0x39, 0xad}, 64)...)
	payload = append(payload, []byte("tail\r\x00")...)
	wire := tMixedTelnetSubpacket(payload, zcRCG, true)
	encodedIACNUL := []byte{zdle, 0x6d, 0} // zEscape(FF 00)
	idx := bytes.Index(wire, encodedIACNUL)
	if idx < 0 {
		t.Fatal("测试流缺少 FF 00 编码")
	}
	wire = append(wire[:idx], wire[idx+len(encodedIACNUL):]...)

	e := &zmodemEngine{prebuf: wire}
	data, subch, ok, err := e.readDataSection(true)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || subch != zcRCG || !bytes.Equal(data, payload) {
		t.Fatalf("IAC NUL 丢失还原错误: subch=%c crcOK=%v len=%d/%d", subch, ok, len(data), len(payload))
	}
}
