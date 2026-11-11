package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

// fmassets 通道：合法签名放行、篡改/缺失拒绝、Range 支持（音视频拖动的前提）。

func TestFMAssetHandlerServeAndReject(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "vid.txt")
	if err := os.WriteFile(f, []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := NewFileService()
	h := newFMAssetHandler(assets, svc)

	tok := func(path string) string { return svc.signToken(canonicalPath(path)) }

	// 合法：200 + 全文。
	req := httptest.NewRequest(http.MethodGet, fmFileRoute+"?p="+url.QueryEscape(f)+"&k="+tok(f), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("合法签名应 200，得 %d body=%s", w.Code, w.Body.String())
	}
	if w.Body.String() != "hello world" {
		t.Errorf("正文不符：%q", w.Body.String())
	}

	// 篡改 token：403。
	bad := httptest.NewRequest(http.MethodGet, fmFileRoute+"?p="+url.QueryEscape(f)+"&k=deadbeef", nil)
	bw := httptest.NewRecorder()
	h.ServeHTTP(bw, bad)
	if bw.Code != http.StatusForbidden {
		t.Errorf("坏 token 应 403，得 %d", bw.Code)
	}

	// 无 token：403。
	none := httptest.NewRequest(http.MethodGet, fmFileRoute+"?p="+url.QueryEscape(f), nil)
	nw := httptest.NewRecorder()
	h.ServeHTTP(nw, none)
	if nw.Code != http.StatusForbidden {
		t.Errorf("无 token 应 403，得 %d", nw.Code)
	}

	// Range：0-4 → 206 "hello"。
	rr := httptest.NewRequest(http.MethodGet, fmFileRoute+"?p="+url.QueryEscape(f)+"&k="+tok(f), nil)
	rr.Header.Set("Range", "bytes=0-4")
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, rr)
	if rw.Code != http.StatusPartialContent {
		t.Fatalf("Range 应 206，得 %d", rw.Code)
	}
	if rw.Body.String() != "hello" {
		t.Errorf("Range 正文不符：%q", rw.Body.String())
	}
}
