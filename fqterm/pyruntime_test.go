package main

import (
	"os"
	"path/filepath"
	"testing"
)

// pyruntime 单测只覆盖纯逻辑（扩展名归一、清单解析、发现、runner 落盘）。
// venv 创建与 JSON-RPC worker 依赖真实 Python，交给构建后手动验收（可选 FTRW_PYTEST=1 触发）。

func TestNormalizeExts(t *testing.T) {
	got := normalizeExts([]string{"DOCX", " .pdf", "", "txt"})
	want := []string{".docx", ".pdf", ".txt"}
	if len(got) != len(want) {
		t.Fatalf("数量不符：%v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d]=%s 应为 %s", i, got[i], want[i])
		}
	}
}

func TestPluginsDiscovery(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "docx-preview")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"),
		[]byte(`{"title":"DOCX","extensions":[".docx"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte("def preview(p,c): return {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 一个无 plugin.json 的目录应被忽略。
	if err := os.MkdirAll(filepath.Join(root, "junk"), 0o755); err != nil {
		t.Fatal(err)
	}

	r := &pyRuntime{root: root, workers: map[string]*pyWorker{}, probed: true, available: true}
	tools := r.plugins()
	if len(tools) != 1 {
		t.Fatalf("应发现 1 个插件，得 %d：%+v", len(tools), tools)
	}
	if tools[0].ID != "docx-preview" || tools[0].Kind != "preview" {
		t.Errorf("元数据不符：%+v", tools[0])
	}
	if len(tools[0].Extensions) != 1 || tools[0].Extensions[0] != ".docx" {
		t.Errorf("扩展名不符：%v", tools[0].Extensions)
	}

	if _, _, ok := r.manifestByID("docx-preview"); !ok {
		t.Errorf("manifestByID 应命中")
	}
	if _, _, ok := r.manifestByID("nope"); ok {
		t.Errorf("不存在的 id 不应命中")
	}
}

func TestPluginsActions(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "archive")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"),
		[]byte(`{"title":"压缩包","extensions":[".zip"],"actions":[{"id":"extract","title":"解压"},{"id":"compress","title":"压缩"},{"title":"无 id 被过滤"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &pyRuntime{root: root, workers: map[string]*pyWorker{}, probed: true, available: true}
	tools := r.plugins()
	if len(tools) != 1 {
		t.Fatalf("应发现 1 个插件，得 %d", len(tools))
	}
	if len(tools[0].Actions) != 2 {
		t.Fatalf("应保留 2 条带 id 的动作，得 %d：%+v", len(tools[0].Actions), tools[0].Actions)
	}
	if tools[0].Actions[0].ID != "extract" || tools[0].Actions[0].Title != "解压" {
		t.Errorf("动作元数据不符：%+v", tools[0].Actions[0])
	}
	// actionFunc 默认应为 action。
	m, _, ok := r.manifestByID("archive")
	if !ok || m.ActionFunc != "action" {
		t.Errorf("actionFunc 默认不符：%+v ok=%v", m, ok)
	}
}

func TestNoPythonDisablesPlugins(t *testing.T) {
	r := &pyRuntime{root: t.TempDir(), workers: map[string]*pyWorker{}, probed: true, available: false}
	if got := r.plugins(); got != nil {
		t.Errorf("无 Python 应返回空，得 %+v", got)
	}
}

func TestEnsureRunner(t *testing.T) {
	r := &pyRuntime{root: t.TempDir(), workers: map[string]*pyWorker{}}
	path, err := r.ensureRunner()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != string(runnerPySrc) {
		t.Errorf("runner 内容应与内嵌一致")
	}
}
