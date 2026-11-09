package main

import (
	"log"
	"path/filepath"
	"strings"
)

// ShellSyncResult 是「扫描本机 Shell」的回报，前端拿它决定要不要刷新站点列表、以及
// 在按钮旁边显示一句什么文案。
type ShellSyncResult struct {
	Added   int      `json:"added"`   // 新种的 shell 条目
	Updated int      `json:"updated"` // 路径失效后被重新指回本机的条目
	Removed int      `json:"removed"` // 已卸载、被清掉的条目
	Shells  []string `json:"shells"`  // 本次探测到的 shell 显示名（供提示用）
}

// SyncShells 是给前端调用的绑定方法：扫描 + 落盘 + 回计数。
//
// 站点表是按下标增删的（SaveSite(index)/DeleteSite(index)），所以探测结果必须落成
// 真实、持久的站点条目，而不是在前端里虚拟合并一层 —— 落成条目之后用户才能像对待
// 任何站点一样改它的字体、编码、参数，改动也才留得住。
func (cs *ConfigService) SyncShells() (ShellSyncResult, error) {
	return cs.SyncShellSites()
}

// SyncShellSites 把本机探测到的 shell 同步进站点表。
//
// 三条准则，按「绝不悄悄毁掉用户的东西」排序：
//  1. 只补不重复：按 ShellID 认人；老条目里没有 ShellID 但路径正好等于某个探测到的
//     shell 时，把它认领下来（只补 ID，不置 Auto），这样下次扫描不会再种一条同名的。
//  2. 只修不覆盖：Auto 条目的路径在探测成功且原路径已经不存在时才指回新位置（pwsh 7.4→7.5
//     换目录这种）。用户自己改过的路径、参数一律不动。
//  3. 只删有把握的：清理只针对 Auto 条目，并且它所属的探测源这次确实探到了结果。
//     wsl.exe 探针超时/失败时 probed["wsl"] 不置位，用户的 WSL 站点就会被原样留下 ——
//     把「这次没探到」当成「已经卸载」是这台程序里最讨人嫌的一种自动行为。
func (cs *ConfigService) SyncShellSites() (ShellSyncResult, error) {
	// 探测放在锁外面：wsl -l -q 最长要等 3 秒，不能让整个配置读写陪着阻塞。
	specs, probed := discoverShells()
	return cs.mergeShellSites(specs, probed)
}

// mergeShellSites 是同步的可测部分：给定一批探测结果与一份「哪些探测源这次可信」的
// 名单，算出站点表的新样子。把它从 discoverShells 拆出来就是为了能在测试里喂人造的
// 探测结果 —— 测试机上到底装了哪几个 shell 是不可重现的，拿真探测结果断言计数没有意义。
func (cs *ConfigService) mergeShellSites(specs []ShellSpec, probed map[string]bool) (ShellSyncResult, error) {
	var res ShellSyncResult

	byID := make(map[string]ShellSpec, len(specs))
	for _, sp := range specs {
		if sp.ID == "" || sp.Path == "" {
			continue
		}
		byID[sp.ID] = sp
		res.Shells = append(res.Shells, sp.Name)
	}

	cs.store.mu.Lock()
	sites := append([]Site(nil), cs.store.Sites...)
	cs.store.mu.Unlock()

	seen := make(map[string]bool, len(byID))

	// 先走一遍已有条目：认领 ID、修失效路径、决定要不要删。
	out := make([]Site, 0, len(sites)+len(byID))
	for _, s := range sites {
		if s.ProtocolType != 3 {
			out = append(out, s)
			continue
		}
		// 老配置没有 ShellID：路径能对上探测结果就认领一条，避免下面补种时重复。
		if s.ShellID == "" {
			if id, sp, ok := matchSpecByHost(s.Host, byID); ok && !seen[id] {
				seen[id] = true
				s.ShellID = id
				// 不置 Auto：这是用户自己建的条目，删除权在他手上。
				if sp.Name != "" && strings.TrimSpace(s.Name) == "" {
					s.Name = sp.Name
				}
				res.Updated++
				log.Printf("本地 shell 站点 %q 认领为 %s", s.Host, id)
			}
			out = append(out, s)
			continue
		}

		sp, detected := byID[s.ShellID]
		if detected {
			seen[s.ShellID] = true
		}
		switch {
		case detected && s.Auto && !progExists(s.Host) && progExists(sp.Path):
			// 本机位置变了（升级换目录、Git 重装），旧路径已经起不来，指回探测结果。
			s.Host = sp.Path
			s.ShellArgs = append([]string(nil), sp.Args...)
			res.Updated++
			log.Printf("本地 shell %s 路径失效，已更新为 %s", s.ShellID, sp.Path)
		case !detected && s.Auto && probed[shellSource(s.ShellID)]:
			// 卸载了 Git/MSYS2 之类：这条自动条目已经没有对应的东西了，清掉。
			res.Removed++
			log.Printf("本地 shell %s 已不存在，移除条目 %q", s.ShellID, s.Name)
			continue
		}
		out = append(out, s)
	}

	// 补种探测到、表里还没有的。
	for _, sp := range specs {
		if sp.ID == "" || sp.Path == "" || seen[sp.ID] {
			continue
		}
		out = append(out, siteFromShell(sp))
		seen[sp.ID] = true
		res.Added++
		log.Printf("新增本机 shell 站点：%s（%s）", sp.Name, sp.Path)
	}

	if res.Added == 0 && res.Updated == 0 && res.Removed == 0 {
		// 无变化也要留一行：排障时「到底扫到了什么」跟「改没改东西」同样重要。
		log.Printf("本机 shell 扫描：无变化（探测到 %d 个，可信来源 %d 个）", len(byID), len(probed))
		return res, nil
	}
	if err := cs.store.saveJSON("sites.json", out); err != nil {
		return res, err
	}
	cs.store.mu.Lock()
	cs.store.Sites = out
	cs.store.mu.Unlock()
	log.Printf("本机 shell 同步完成：新增 %d、更新 %d、移除 %d", res.Added, res.Updated, res.Removed)
	return res, nil
}

// matchSpecByHost 用路径反查探测结果。Windows 路径大小写不定、也可能写成相对形式，
// 所以比较前先规整一遍。
func matchSpecByHost(host string, byID map[string]ShellSpec) (string, ShellSpec, bool) {
	want := normalizeShellPath(host)
	if want == "" {
		return "", ShellSpec{}, false
	}
	// 遍历而不是直接查表：表以 ID 为键，手里只有路径。条目数是个位数，无所谓。
	for id, sp := range byID {
		if normalizeShellPath(sp.Path) == want {
			return id, sp, true
		}
	}
	return "", ShellSpec{}, false
}

func normalizeShellPath(p string) string {
	p = strings.TrimSpace(p)
	p = strings.Trim(p, "\"")
	if p == "" {
		return ""
	}
	// 统一成斜杠再比大小写：Windows 路径在 COMSPEC、注册表、用户手填里可能混用两种
	// 分隔符。filepath.ToSlash 在非 Windows 上是空操作（那里 '\' 是合法文件名），
	// 所以这段不需要分平台。
	return strings.ToLower(filepath.ToSlash(p))
}
