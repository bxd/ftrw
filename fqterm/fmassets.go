package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// fmassets.go：把本机任意文件安全地变成 WebView2 能引用的 URL。
//
// 前端只能拿到「FileService 亲自签过名」的地址：URL 形如
// /__ftrwfile?p=<绝对路径>&k=<HMAC(sessionSecret, 规范化路径)>。
// 没有合法 k 的请求一律 403，这样即便页面里混进了不可信内容，也无法凭路径读盘上任意文件。
// 命中后走 http.ServeFile，自带 Range 支持 —— 音视频可拖动播放、图片/PDF 直接显示。

const fmFileRoute = "/__ftrwfile"

// newFMAssetHandler 包住内嵌资源服务器：/__ftrwfile 交给本机文件，其余原样转发。
func newFMAssetHandler(assets embed.FS, s *FileService) http.Handler {
	base := application.AssetFileServerFS(assets)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != fmFileRoute {
			base.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		q := r.URL.Query()
		p := q.Get("p")
		k := q.Get("k")
		if p == "" || !s.VerifyToken(p, k) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		// 按扩展名补一个 Content-Type（ServeFile 也会猜，但像 .md/.m4v 之类系统未必认）。
		if ct := mime.TypeByExtension(filepath.Ext(p)); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		http.ServeFile(w, r, p)
	})
}

// signToken 对规范化后的绝对路径算短令牌；SignPath/Preview 内部用。
func (s *FileService) signToken(abs string) string {
	mac := hmac.New(sha256.New, s.sessionSecret)
	mac.Write([]byte(abs))
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyToken 校验 (路径, 令牌) 对：常量时间比较，失败即拒绝。
func (s *FileService) VerifyToken(p, token string) bool {
	if token == "" {
		return false
	}
	want := s.signToken(canonicalPath(p))
	return subtle.ConstantTimeCompare([]byte(want), []byte(token)) == 1
}

// buildFileURL 拼出前端可直接塞进 src 的签名地址。
func (s *FileService) buildFileURL(abs string) string {
	return fmFileRoute + "?p=" + url.QueryEscape(abs) + "&k=" + s.signToken(abs)
}

// canonicalPath 清洗成绝对路径，作为签名基准（同一文件的多次签名要稳定一致）。
func canonicalPath(p string) string {
	abs := filepath.Clean(p)
	if !filepath.IsAbs(abs) {
		if a, err := filepath.Abs(abs); err == nil {
			abs = a
		}
	}
	return abs
}
