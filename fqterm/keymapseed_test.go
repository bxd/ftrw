package main

import "testing"

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
	want := map[string]string{"tab.prev": "S-<LEFT>", "tab.next": "S-<RIGHT>"}
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
