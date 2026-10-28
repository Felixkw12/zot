package skills

import (
	"path/filepath"
	"strings"
)

// excludedSkillPath checks both lexical paths and resolved targets so a link
// cannot accidentally reintroduce an excluded directory. This is a discovery
// preference, not an access-control boundary.
func excludedSkillPath(path string, excludes []string) bool {
	if len(excludes) == 0 {
		return false
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	resolved, resolveErr := filepath.EvalSymlinks(absolute)
	for _, exclude := range excludes {
		if exclude == "" {
			continue
		}
		if withinSkillPath(exclude, absolute) {
			return true
		}
		target, err := filepath.EvalSymlinks(exclude)
		if err == nil && resolveErr == nil && withinSkillPath(target, resolved) {
			return true
		}
	}
	return false
}

func withinSkillPath(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
