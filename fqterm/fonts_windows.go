//go:build windows

package main

import (
	"sort"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// listSystemFonts enumerates installed font family names from the Windows
// registry font tables (both machine-wide and per-user).
func listSystemFonts() []string {
	seen := make(map[string]bool)
	collectFonts(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion\Fonts`, seen)
	collectFonts(registry.CURRENT_USER, `SOFTWARE\Microsoft\Windows NT\CurrentVersion\Fonts`, seen)

	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func collectFonts(root registry.Key, path string, seen map[string]bool) {
	k, err := registry.OpenKey(root, path, registry.READ)
	if err != nil {
		return
	}
	defer k.Close()

	names, err := k.ReadValueNames(0)
	if err != nil {
		return
	}
	for _, n := range names {
		// Value names look like "Arial (TrueType)" / "MS Gothic (OpenType)".
		// The CSS family name is the part before the first " (".
		if i := strings.Index(n, " ("); i > 0 {
			n = n[:i]
		}
		n = strings.TrimSpace(n)
		if n != "" {
			seen[n] = true
		}
	}
}
