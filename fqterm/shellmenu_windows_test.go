package main

import (
	"testing"
	"unsafe"
)

// CMINVOKECOMMANDINFO 的字段序与尺寸已对照 SDK ShObjIdl_core.h 原文核实：9 字段、
// x64 sizeof=56。曾缺 fMask/dwHotKey/hIcon 使 cbSize=48，Shell 校验 cbSize 不等直接
// E_INVALIDARG（0x80070057），属性等所有原生动词全失败。在这里锁死尺寸与关键偏移防再犯。
func TestCMInvokeCommandInfoLayout(t *testing.T) {
	if got := unsafe.Sizeof(cminvoke{}); got != 56 {
		t.Fatalf("cminvoke 尺寸 = %d, want 56（CMINVOKECOMMANDINFO x64）", got)
	}
	var c cminvoke
	if unsafe.Offsetof(c.fMask) != 4 || unsafe.Offsetof(c.hwnd) != 8 ||
		unsafe.Offsetof(c.lpVerb) != 16 || unsafe.Offsetof(c.lpParameters) != 24 ||
		unsafe.Offsetof(c.lpDirectory) != 32 || unsafe.Offsetof(c.nShow) != 40 ||
		unsafe.Offsetof(c.dwHotKey) != 44 || unsafe.Offsetof(c.hIcon) != 48 {
		t.Fatal("cminvoke 字段偏移与 CMINVOKECOMMANDINFO 不符")
	}
}
