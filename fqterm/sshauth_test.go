package main

import "testing"

// A 系列（界面已有字段、这轮才真正接上后端/前端）的出厂值与分流断言。
// 判据都取自前端 frontend/src/keymap.ts 与 terminal.ts 的兜底口径：两边必须一字不差，
// 否则旧站点缺键时「界面显示的和实际发出去的」会各说各话。

func TestSSHAuthFor(t *testing.T) {
	cases := []struct {
		name string
		site Site
		kind AuthKind
		key  string
	}{
		{"没填私钥走密码", Site{Password: "pw"}, AuthPassword, ""},
		{"只有空格的私钥不算填了", Site{KeyFile: "   ", Password: "pw"}, AuthPassword, ""},
		{"填了私钥走公钥", Site{KeyFile: `C:\Users\me\.ssh\id_ed25519`}, AuthPublicKey, `C:\Users\me\.ssh\id_ed25519`},
		{"私钥两边的空白被去掉", Site{KeyFile: " /home/me/.ssh/id_rsa\n"}, AuthPublicKey, "/home/me/.ssh/id_rsa"},
	}
	for _, c := range cases {
		got := sshAuthFor(c.site)
		if got.Kind != c.kind {
			t.Errorf("%s: kind=%d 期望 %d", c.name, got.Kind, c.kind)
		}
		if got.KeyFile != c.key {
			t.Errorf("%s: keyFile=%q 期望 %q", c.name, got.KeyFile, c.key)
		}
		if got.Kind == AuthPassword && got.Password != c.site.Password {
			t.Errorf("%s: 密码没透传：%q", c.name, got.Password)
		}
	}

	// 口令只在公钥分支有意义，别把它悄悄塞进密码分支。
	a := sshAuthFor(Site{Passphrase: "secret"})
	if a.Kind != AuthPassword || a.Passphrase != "" {
		t.Errorf("无私钥时不应带出口令：kind=%d passphrase=%q", a.Kind, a.Passphrase)
	}
	b := sshAuthFor(Site{KeyFile: "k", Passphrase: "secret"})
	if b.Passphrase != "secret" {
		t.Errorf("私钥口令丢了：%q", b.Passphrase)
	}
}

func TestASeriesSiteDefaults(t *testing.T) {
	d := DefaultSite()
	// keyType 0=BBS、backspace 1=^?：与 keymap.ts 里 `?? 0` / `?? 1` 的兜底同值。
	if d.KeyType != 0 || d.Backspace != 1 {
		t.Errorf("按键出厂值错：keyType=%d backspace=%d", d.KeyType, d.Backspace)
	}
	// 旧站点解码缺 keyFile 拿到空串即可，不能被 normalizeSite 补出东西来。
	if n := normalizeSite(Site{ProtocolType: 0}); n.KeyFile != "" || n.Passphrase != "" {
		t.Errorf("normalizeSite 不该碰私钥字段：keyFile=%q passphrase=%q", n.KeyFile, n.Passphrase)
	}
}

func TestASeriesPreferenceDefaults(t *testing.T) {
	// 回滚行数：前端 terminal.ts 的 scrollbackOf(undefined) 退回 2000，必须和这里的出厂值一致。
	if p := defaultPreferences(); p.ScrollbackLines != 2000 {
		t.Errorf("scrollbackLines 出厂值=%d，前端兜底用的是 2000", p.ScrollbackLines)
	}
}
