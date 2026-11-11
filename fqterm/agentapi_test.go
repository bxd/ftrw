package main

import (
	"os"
	"testing"
)

// Agent API 的出厂安全边界与配置归一在纯函数里，这里把它们钉死；
// HTTP 路由与前端往返留给真机验收（依赖渲染端，起假屏不值得）。

func TestAgentConfigFromPrefs(t *testing.T) {
	p := defaultPreferences()
	if c := agentConfigFromPrefs(p); c.enabled {
		t.Fatalf("出厂偏好必须默认关着 API：%+v", c)
	}
	if c := agentConfigFromPrefs(p); c.port != defaultAgentApiPort {
		t.Errorf("出厂端口=%d, want %d", c.port, defaultAgentApiPort)
	}
	// 端口 0（旧 prefs.json 缺键解码后就是这样）要补成出厂值，不能让监听器去听 0 端口。
	p.AgentApiPort = 0
	if c := agentConfigFromPrefs(p); c.port != defaultAgentApiPort {
		t.Errorf("端口 0 未补出厂值：%d", c.port)
	}
}

func TestNormalizePreferencesAgentFields(t *testing.T) {
	// 开启但没 token：现生一个，且两次归一各自独立（不承诺幂等相同，承诺非空）。
	p := defaultPreferences()
	p.AgentApiEnabled = true
	p.AgentApiToken = ""
	p = normalizePreferences(p)
	if p.AgentApiToken == "" {
		t.Fatal("开启后没 token 就该自动生成")
	}
	// 关着时不生成：prefs.json 里不该躺一个没用的秘密。
	p.AgentApiEnabled = false
	p.AgentApiToken = ""
	if p = normalizePreferences(p); p.AgentApiToken != "" {
		t.Errorf("停用状态不该生成 token：%q", p.AgentApiToken)
	}
	// 越界端口回出厂。
	p.AgentApiPort = 70000
	if p = normalizePreferences(p); p.AgentApiPort != defaultAgentApiPort {
		t.Errorf("越界端口=%d 没回出厂值", p.AgentApiPort)
	}
}

func TestEnvAgentConfig(t *testing.T) {
	t.Setenv(agentAPIEnv, "")
	if _, ok := envAgentConfig(); ok {
		t.Fatal("没设变量不该生效")
	}
	t.Setenv(agentAPIEnv, "4321:sekrit")
	c, ok := envAgentConfig()
	if !ok || !c.enabled || c.port != 4321 || c.token != "sekrit" {
		t.Fatalf("port:token 解析错：%+v ok=%v", c, ok)
	}
	t.Setenv(agentAPIEnv, "4321")
	c, ok = envAgentConfig()
	if !ok || c.port != 4321 || c.token == "" {
		t.Fatalf("只给端口也要能用（补开发 token）：%+v", c)
	}
	t.Setenv(agentAPIEnv, "not-a-port")
	if _, ok := envAgentConfig(); ok {
		t.Fatal("端口不合法必须忽略整个变量")
	}
	// 越界端口同样忽略：监听 privileged/非法端口不是"顺手试一下"的事。
	for _, bad := range []string{"0", "-1", "70000"} {
		t.Setenv(agentAPIEnv, bad)
		if _, ok := envAgentConfig(); ok {
			t.Fatalf("端口 %s 不该被接受", bad)
		}
	}
	_ = os.Unsetenv(agentAPIEnv)
}

func TestAgentConfigChangeDetect(t *testing.T) {
	// SetPreferences 靠两份配置的相等性决定要不要重启监听器：token 变了也必须被看见，
	// 否则"重生成"按下去旧 token 还在服役。
	a := agentConfigFromPrefs(defaultPreferences())
	b := a
	b.token = "different"
	if a == b {
		t.Fatal("token 改动必须让配置比对不相等")
	}
}

func TestNewAgentTokenNonEmptyAndUnique(t *testing.T) {
	x, y := newAgentToken(), newAgentToken()
	if x == "" || y == "" || x == y {
		t.Fatalf("token 生成异常：%q vs %q", x, y)
	}
	if len(x) != 40 {
		t.Fatalf("长度 %d，应为 20 字节 hex=40", len(x))
	}
}
