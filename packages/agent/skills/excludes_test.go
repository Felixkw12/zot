package skills

import (
	"os"
	"path/filepath"
	"testing"
)

func writeDiscoverySkill(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(path, []byte("---\nname: "+name+"\ndescription: test skill\n---\nbody\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDiscoverSourcesSkipsHiddenDirectories(t *testing.T) {
	root := t.TempDir()
	writeDiscoverySkill(t, root, "visible")
	trash := filepath.Join(root, ".trash")
	writeDiscoverySkill(t, trash, "deleted")
	writeDiscoverySkill(t, filepath.Join(root, "category", ".hidden"), "nested")
	got, errs := DiscoverSources([]Source{{Root: root}}, false)
	if len(errs) != 0 || len(got) != 1 || got[0].Name != "visible" {
		t.Fatalf("discovery = %v, errors = %v", got, errs)
	}
	// Explicit source roots remain usable even when hidden.
	got, errs = DiscoverSources([]Source{{Root: trash}}, false)
	if len(errs) != 0 || len(got) != 1 || got[0].Name != "deleted" {
		t.Fatalf("explicit hidden root = %v, errors = %v", got, errs)
	}
}

func TestDiscoverSourcesExcluding(t *testing.T) {
	root := t.TempDir()
	excluded := filepath.Join(root, "synced")
	file := writeDiscoverySkill(t, excluded, "duplicate")
	sibling := writeDiscoverySkill(t, filepath.Join(root, "synced-other"), "keep")
	local := writeDiscoverySkill(t, filepath.Join(root, "local"), "duplicate")
	cases := []struct {
		name     string
		sources  []Source
		excludes []string
		want     int
	}{
		{"subtree", []Source{{Root: root}}, []string{excluded}, 2},
		{"source root", []Source{{Root: excluded}, {Root: filepath.Dir(local)}}, []string{excluded}, 1},
		{"explicit file", []Source{{Root: file}, {Root: sibling}}, []string{file}, 1},
		{"ancestor", []Source{{Root: excluded}}, []string{root}, 0},
		{"missing exclusion", []Source{{Root: filepath.Dir(sibling)}}, []string{filepath.Join(root, "missing")}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, errs := DiscoverSourcesExcluding(tc.sources, false, tc.excludes)
			if len(errs) != 0 || len(got) != tc.want {
				t.Fatalf("got %d skills, want %d, errors = %v", len(got), tc.want, errs)
			}
			for _, skill := range got {
				if sameSkillFile(skill.Path, file) {
					t.Fatal("excluded skill discovered")
				}
				if skill.Name == "duplicate" && !sameSkillFile(skill.Path, local) {
					t.Fatalf("local copy did not win: %s", skill.Path)
				}
			}
		})
	}
	got, errs := DiscoverSourcesExcluding([]Source{{Root: root}}, true, []string{root})
	if len(errs) != 0 || len(got) != len(loadBuiltins()) {
		t.Fatalf("filesystem exclusions affected builtins: %v, %v", got, errs)
	}
}

func TestDiscoverSourcesExcludingSymlinkAliases(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	file := writeDiscoverySkill(t, target, "excluded")
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(target, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	fileAlias := filepath.Join(root, "file", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(fileAlias), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(file, fileAlias); err != nil {
		t.Fatal(err)
	}
	for _, exclude := range []string{target, alias} {
		got, errs := DiscoverSourcesExcluding([]Source{{Root: root}, {Root: alias}, {Root: fileAlias}}, false, []string{exclude})
		if len(errs) != 0 || len(got) != 0 {
			t.Fatalf("exclude %s: got %v, errors = %v", exclude, got, errs)
		}
	}
}
