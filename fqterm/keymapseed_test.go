package main

import (
	"strings"
	"testing"
)

// 出厂绑定补种的回归重点在两件事：新版新增的绑定要能进到老用户的表里，
// 而老用户已经改过/删掉的绝不能在下次启动时复活。

func binding(keys, cmd string) KeyBinding {
	return KeyBinding{Keys: keys, Command: cmd, Enabled: true}
}

func TestSeedFactoryBindingsAddsNewBindingOnce(t *testing.T) {
	factory := DefaultKeymap()
	// 模拟 v0.1.x 的存量文件：只有站点栏那条，且没有 seeded 台账
	cur := KeymapConfig{Enabled: true, TimeoutMs: 1000, Bindings: []KeyBinding{binding("M-x p", "app.toggle-sidebar")}}

	seedFactoryBindings(&cur, factory)
	if len(cur.Bindings) != len(factory.Bindings) {
		t.Fatalf("补种后绑定数 = %d, want %d", len(cur.Bindings), len(factory.Bindings))
	}
	if len(cur.Seeded) != len(factory.Bindings) {
		t.Fatalf("台账数 = %d, want %d", len(cur.Seeded), len(factory.Bindings))
	}

	// 第二次加载（此时台账已落盘）不得再塞一条
	before := len(cur.Bindings)
	seedFactoryBindings(&cur, factory)
	if len(cur.Bindings) != before {
		t.Fatalf("重复补种: 绑定数 %d -> %d", before, len(cur.Bindings))
	}
}

func TestSeedFactoryBindingsRespectsUserRebindAndUnbind(t *testing.T) {
	factory := DefaultKeymap()

	// 用户把 quake 切换改到自己的键位：命令 id 还在，不该再塞一条出厂的回来。
	// 不断言总条数，因为出厂表新增命令（标签切换等）本来就该被补进来。
	rebound := KeymapConfig{Bindings: []KeyBinding{
		binding("M-x p", "app.toggle-sidebar"),
		binding("C-c q", "app.toggle-quake-mode"),
	}}
	seedFactoryBindings(&rebound, factory)
	if n := countByCommand(rebound.Bindings, "app.toggle-quake-mode"); n != 1 {
		t.Fatalf("改键后被塞回出厂绑定，quake 命令现有 %d 条: %+v", n, rebound.Bindings)
	}
	if n := countByCommand(rebound.Bindings, "app.toggle-sidebar"); n != 1 {
		t.Fatalf("站点栏命令现有 %d 条", n)
	}
	for _, b := range rebound.Bindings {
		if b.Command == "app.toggle-quake-mode" && b.Keys != "C-c q" {
			t.Fatalf("用户的键位被覆盖了: %q", b.Keys)
		}
	}

	// 用户删掉了站点栏绑定并保存过（台账已记满）：不得复活
	removed := KeymapConfig{
		Bindings: []KeyBinding{binding("M-x M-q", "app.toggle-quake-mode")},
		Seeded:   seededIDs(factory.Bindings),
	}
	seedFactoryBindings(&removed, factory)
	if len(removed.Bindings) != 1 {
		t.Fatalf("已解绑的出厂绑定复活: %+v", removed.Bindings)
	}
}

func TestDefaultKeymapSeedsTabCycling(t *testing.T) {
	// 标签循环切换靠出厂绑定开箱即用，但它们的键是终端选区扩展正在用的键，
	// 所以这两条必须能在绑定表里看到、能改、能停用 —— 前提是先被正确补种。
	want := map[string]string{"tab.prev": "S-<LEFT>", "tab.next": "S-<RIGHT>", "tab.close": "M-x k"}
	got := map[string]string{}
	for _, b := range DefaultKeymap().Bindings {
		got[b.Command] = b.Keys
	}
	for cmd, keys := range want {
		if got[cmd] != keys {
			t.Fatalf("出厂绑定 %s = %q, want %q", cmd, got[cmd], keys)
		}
	}
}

func TestDefaultKeymapSeedsPostActions(t *testing.T) {
	// 编辑工具栏那七个动作要开箱即用，靠的就是出厂绑定被正确补种；
	// 键位都收在 M-x 一族里，且不能把已经占用的 M-x p / M-x M-q / S-<方向键> 撞掉。
	want := map[string]string{
		"post.sep":            "M-x s",
		"post.sep-random":     "M-x r",
		"post.sep-mixed":      "M-x m",
		"post.sep-pick":       "M-x l",
		"post.vertical":       "M-x v",
		"post.insert":         "M-x i",
		"post.toolbar-toggle": "M-x t",
	}
	got := map[string]string{}
	for _, b := range DefaultKeymap().Bindings {
		got[b.Command] = b.Keys
	}
	for cmd, keys := range want {
		if got[cmd] != keys {
			t.Fatalf("出厂绑定 %s = %q, want %q", cmd, got[cmd], keys)
		}
	}
}

func TestDefaultKeymapSeedsFullscreen(t *testing.T) {
	// 全屏是新加的窗口形态，出厂绑定得开箱即用才能被补种进用户的 keymap.json；
	// 键位撞车由 TestDefaultKeymapHasNoDuplicateKeys 统一拦。
	for _, b := range DefaultKeymap().Bindings {
		if b.Command == "app.toggle-fullscreen" {
			if b.Keys != "M-x f" {
				t.Fatalf("全屏出厂绑定键为 %q, want %q", b.Keys, "M-x f")
			}
			if !b.Enabled {
				t.Fatal("全屏出厂绑定默认是停用的")
			}
			return
		}
	}
	t.Fatal("出厂绑定里没有 app.toggle-fullscreen")
}

func TestDefaultKeymapHasNoDuplicateKeys(t *testing.T) {
	// 一个序列只能落到一个命令：两条出厂绑定撞键时后一条永远不会被触发，
	// 而界面上看上去就是“那个功能的热键按了没反应”，所以在这里拦死。
	seen := map[string]string{}
	for _, b := range DefaultKeymap().Bindings {
		k := strings.ToLower(b.Keys)
		if other, dup := seen[k]; dup {
			t.Fatalf("键位冲突：%q 同时给了 %s 和 %s", b.Keys, other, b.Command)
		}
		seen[k] = b.Command
	}
}

func countByCommand(bs []KeyBinding, cmd string) int {
	n := 0
	for _, b := range bs {
		if b.Command == cmd {
			n++
		}
	}
	return n
}

func TestMergeSeededKeepsLedgerAcrossSave(t *testing.T) {
	// 前端回传的对象不带 seeded（api.ts 的 KeymapConfig 没这字段），台账不能因此被清空
	got := mergeSeeded([]string{"app.toggle-sidebar", "app.toggle-quake-mode"}, nil)
	if len(got) != 2 {
		t.Fatalf("并集丢了已有台账: %v", got)
	}
	if dup := mergeSeeded([]string{"a"}, []string{"a", "b", ""}); len(dup) != 2 {
		t.Fatalf("并集未去重/未忽略空串: %v", dup)
	}
}

func TestDefaultKeymapSeedsFileManager(t *testing.T) {
	// 文件管理器是新加的标签形态，出厂绑定要开箱即用才能被补种进用户的 keybindings.json。
	// 键位撞车由 TestDefaultKeymapHasNoDuplicateKeys 统一拦。
	km := DefaultKeymap()
	var hit *KeyBinding
	for i := range km.Bindings {
		if km.Bindings[i].Command == "files.open-manager" {
			hit = &km.Bindings[i]
		}
	}
	if hit == nil {
		t.Fatal("出厂绑定里没有 files.open-manager")
	}
	if hit.Keys != "M-x e" {
		t.Fatalf("文件管理器出厂键为 %q, want %q", hit.Keys, "M-x e")
	}
	if !hit.Enabled {
		t.Fatal("文件管理器出厂绑定默认是停用的")
	}
	// 补种台账必须记上这个 id，否则用户手工解绑后每次启动又会被塞回来。
	found := false
	for _, id := range km.Seeded {
		if id == "files.open-manager" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Seeded 台账缺 files.open-manager：%v", km.Seeded)
	}
}

func TestDefaultKeymapSeedsEditPath(t *testing.T) {
	// 文件管理器“编辑路径”出厂绑定 M-x d，同样要开箱即用且进补种台账。
	// 键位撞车由 TestDefaultKeymapHasNoDuplicateKeys 统一拦。
	km := DefaultKeymap()
	var hit *KeyBinding
	for i := range km.Bindings {
		if km.Bindings[i].Command == "files.edit-path" {
			hit = &km.Bindings[i]
		}
	}
	if hit == nil {
		t.Fatal("出厂绑定里没有 files.edit-path")
	}
	if hit.Keys != "M-x d" {
		t.Fatalf("编辑路径出厂键为 %q, want %q", hit.Keys, "M-x d")
	}
	if !hit.Enabled {
		t.Fatal("编辑路径出厂绑定默认是停用的")
	}
	found := false
	for _, id := range km.Seeded {
		if id == "files.edit-path" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Seeded 台账缺 files.edit-path：%v", km.Seeded)
	}
}
