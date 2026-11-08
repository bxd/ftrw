package main

import (
	"io"

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
