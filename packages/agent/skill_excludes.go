package agent

import (
	"path/filepath"
	"strings"
)

func resolveSkillExcludes(paths []string, cwd, home string) []string {
	var out []string
	for _, path := range paths {
		if path == "" {
			continue
		}
		if path == "~" {
			path = home
		} else if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, "~\\") {
			path = filepath.Join(home, path[2:])
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(cwd, path)
		}
		out = append(out, filepath.Clean(path))
	}
	return out
}
