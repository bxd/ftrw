package main

import (
	"io"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
	"golang.org/x/text/transform"
)

// encodingFor maps a ServerEncodingID to a legacy CJK encoding.
// 0=GBK(GB18030 superset) 1=BIG5 2=UTF8(nil) 3=HKSCS(Big5 fallback) 4=UAO(GB18030 fallback)
func encodingFor(id int) encoding.Encoding {
	switch id {
	case 0:
		return simplifiedchinese.GB18030
	case 1:
		return traditionalchinese.Big5
	case 3:
		return traditionalchinese.Big5
	case 4:
		return simplifiedchinese.GB18030
	default: // 2 = UTF-8, passthrough
		return nil
	}
}

// decodeReader wraps a raw byte stream, decoding legacy bytes into UTF-8 so the
// frontend (xterm.js) always receives UTF-8. ASCII control/escape sequences pass
// through unchanged because GB18030/Big5 are ASCII-transparent for bytes < 0x80.
func decodeReader(r io.Reader, id int) io.Reader {
	enc := encodingFor(id)
	if enc == nil {
		return r
	}
	return transform.NewReader(r, enc.NewDecoder())
}

// encodeBytes converts a UTF-8 string from the frontend into the site's legacy
// encoding bytes for transmission.
func encodeBytes(s string, id int) []byte {
	enc := encodingFor(id)
	if enc == nil {
		return []byte(s)
	}
	b, _, err := transform.String(enc.NewEncoder(), s)
	if err != nil {
		return []byte(s)
	}
	return []byte(b)
}

// charsetEncoding 把 HTTP 头里的字符集名字换成对应的老编码（Content-Disposition 的
// filename*=gbk”… 会用到）。UTF-8、纯 ASCII 与认不出的一律返回 nil，调用方原样处理。
func charsetEncoding(name string) encoding.Encoding {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "gbk", "gb2312", "gb18030", "cp936", "ms936", "x-windows-cp936":
		return simplifiedchinese.GB18030
	case "big5", "big5-hkscs", "cp950", "ms950":
		return traditionalchinese.Big5
	}
	return nil
}

// decodeCharset 把按 charset 编码的名字转成 UTF-8。raw 是 url.PathUnescape 的产物：
// Go 不校验 UTF-8，非 UTF-8 字节会原样留在 string 里，所以能当字节序列再解一遍。
// 解不动（charset 写错、字节不是合法 GBK）就原样给回，不能让一个怪名字把整条链接
// 的探测判成失败。
func decodeCharset(raw, charset string) string {
	enc := charsetEncoding(charset)
	if enc == nil {
		return raw
	}
	if s, _, err := transform.String(enc.NewDecoder(), raw); err == nil && !strings.ContainsRune(s, utf8.RuneError) {
		return s
	}
	return raw
}
