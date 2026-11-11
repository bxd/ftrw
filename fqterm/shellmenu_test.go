package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// shellmenu_test.go：原生右键菜单的纯逻辑部分（路径清洗、插件菜单项构建、命令 id 反查、
// 文案消毒）。真正弹菜单依赖 Windows Shell COM，交给构建后手动验收。

func absJoin(dir, name string) string { return filepath.Join(dir, name) }

func TestCleanShellPaths(t *testing.T) {
	dir := t.TempDir()
	a := absJoin(dir, "a.zip")
	b := absJoin(dir, "b.txt")
	got := cleanShellPaths([]string{a, "  ", "", b, a, "relative/x", absJoin(dir, "c")})
	// 期望：a、b、c 三个，去重去空去相对。
	if len(got) != 3 {
		t.Fatalf("数量=%d 期望 3：%v", len(got), got)
	}
	if got[0] != a || got[1] != b || got[2] != absJoin(dir, "c") {
		t.Errorf("顺序/内容不符：%v", got)
	}
}

func TestCleanShellPathsDedupCaseInsensitive(t *testing.T) {
	dir := t.TempDir()
	a := absJoin(dir, "Note.TXT")
	dup := filepath.Join(dir, "note.txt") // 仅大小写不同
	got := cleanShellPaths([]string{a, dup})
	if len(got) != 1 {
		t.Fatalf("大小写去重失效：%v", got)
	}
}

func TestCleanShellPathsCap(t *testing.T) {
	dir := t.TempDir()
	var raw []string
	for i := 0; i < shellMaxPaths+40; i++ {
		raw = append(raw, absJoin(dir, fmt.Sprintf("f%d", i)))
	}
	got := cleanShellPaths(raw)
	if len(got) != shellMaxPaths {
		t.Fatalf("未截到上限：%d 期望 %d", len(got), shellMaxPaths)
	}
}

func TestCleanShellPathsDropsTooLong(t *testing.T) {
	dir := t.TempDir()
	long := absJoin(dir, strings.Repeat("x", shellMaxPathLen)) // 远超上限
	got := cleanShellPaths([]string{long})
	if len(got) != 0 {
		t.Fatalf("超长路径未被剔除：%v", got)
	}
}

func TestBuildPluginMenuItems(t *testing.T) {
	tools := []PluginTool{
		{ID: "archive", Title: "压缩包", Actions: []PluginAction{
			{ID: "extract", Title: "解压到当前目录"},
			{ID: "compress", Title: "压缩选定文件"},
		}},
		{ID: "docx-preview", Title: "Docx 预览"}, // 无 actions，不产项
		{ID: "x", Title: "第二个", Actions: []PluginAction{{ID: "go", Title: "执行"}}},
	}
	items := buildPluginMenuItems(items2toolsCheck(t, tools))
	if len(items) != 3 {
		t.Fatalf("菜单项=%d 期望 3：%+v", len(items), items)
	}
	// cmd 从 shellPluginCmdBase 起连续分配。
	for i, it := range items {
		if want := uint32(shellPluginCmdBase) + uint32(i); it.cmd != want {
			t.Errorf("[%d].cmd=%#x 期望 %#x", i, it.cmd, want)
		}
	}
	if items[0].pluginID != "archive" || items[0].actionID != "extract" {
		t.Errorf("首项映射错：%+v", items[0])
	}
	if !strings.Contains(items[0].title, "压缩包") || !strings.Contains(items[0].title, "解压") {
		t.Errorf("标题应含插件名与动作名：%q", items[0].title)
	}
	// 第三个 item 属于第二个插件。
	if items[2].pluginID != "x" || items[2].actionID != "go" {
		t.Errorf("末项映射错：%+v", items[2])
	}
}

// items2toolsCheck 是恒等辅助，让测试里工具构造与调用一行完成（也校验入参类型）。
func items2toolsCheck(_ *testing.T, tools []PluginTool) []PluginTool { return tools }

func TestSanitizeMenuText(t *testing.T) {
	if got := sanitizeMenuText("ab\x00c\td\r\nef"); got != "abcdef" {
		t.Errorf("控制字符未剥净：%q", got)
	}
	long := sanitizeMenuText(strings.Repeat("字", shellMaxTitleRunes+50))
	if n := len([]rune(long)); n > shellMaxTitleRunes {
		t.Errorf("未截断：%d > %d", n, shellMaxTitleRunes)
	}
	// 中间点应保留（非控制字符）。
	if got := sanitizeMenuText("压缩包 · 解压"); got != "压缩包 · 解压" {
		t.Errorf("正常文案被改：%q", got)
	}
}

func TestLookupPluginMenuItem(t *testing.T) {
	tools := []PluginTool{{ID: "p", Title: "T", Actions: []PluginAction{{ID: "a1"}, {ID: "a2"}}}}
	items := buildPluginMenuItems(tools)
	if it, ok := lookupPluginMenuItem(items, uint32(shellPluginCmdBase)+1); !ok || it.actionID != "a2" {
		t.Errorf("反查第二条失败：%+v ok=%v", it, ok)
	}
	if _, ok := lookupPluginMenuItem(items, 5); ok {
		t.Errorf("原生命令 id 不应命中插件项")
	}
	if _, ok := lookupPluginMenuItem(items, uint32(shellPluginCmdBase)+uint32(len(items))); ok {
		t.Errorf("越界 id 不应命中")
	}
}
