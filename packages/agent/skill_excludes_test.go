package agent

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestResolveSkillExcludes(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	cwd := filepath.Join(root, "project")
	absolute := filepath.Join(root, "absolute")
	got := resolveSkillExcludes([]string{"", "~/skills", "~\\skills", "~", "nested/../excluded", absolute}, cwd, home)
	want := []string{filepath.Join(home, "skills"), filepath.Join(home, "skills"), home, filepath.Join(cwd, "excluded"), absolute}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("excludes = %v, want %v", got, want)
	}
}

func TestSkillExcludesApplyToResolveAndSnapshot(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	t.Setenv("ZOT_HOME", home)
	t.Setenv("ZOT_AGENT_SKILLS", "")
	const name = "issue-249-excluded"
	dir := filepath.Join(project, ".zot", "skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: "+name+"\ndescription: exclusion regression\n---\nbody"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := Args{CWD: project, WithSkills: true, NoExt: true}
	if err := SaveConfig(Config{Provider: "openai", Model: "gpt-5"}); err != nil {
		t.Fatal(err)
	}
	before, err := Resolve(args, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(before.SystemPrompt, name) {
		t.Fatal("test skill was not discovered before exclusion")
	}
	if err := SaveConfig(Config{Provider: "openai", Model: "gpt-5", SkillExclude: []string{filepath.Join(".zot", "skills", name)}}); err != nil {
		t.Fatal(err)
	}
	after, err := Resolve(args, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(after.SystemPrompt, name) {
		t.Fatal("excluded skill still in model manifest")
	}
	for _, skill := range userSkillSnapshot(args) {
		if skill.Name == name {
			t.Fatal("excluded skill still in picker/pin snapshot")
		}
	}
	cfg, err := LoadConfig()
	if err != nil || len(cfg.SkillExclude) != 1 {
		t.Fatalf("config round trip: %v, %v", cfg.SkillExclude, err)
	}
}
