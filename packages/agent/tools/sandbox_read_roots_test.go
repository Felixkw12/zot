package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSandboxReadOnlyRoots(t *testing.T) {
	workspace := t.TempDir()
	ext := t.TempDir()
	file := filepath.Join(ext, "reference.md")
	if err := os.WriteFile(file, []byte("reference material"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewSandbox(workspace)
	s.Lock()
	s.SetReadOnlyRoots([]string{ext})
	raw, err := json.Marshal(map[string]string{"path": file})
	if err != nil {
		t.Fatal(err)
	}
	read := &ReadTool{CWD: workspace, Sandbox: s}
	if _, err := read.Execute(context.Background(), raw, nil); err != nil {
		t.Fatalf("read extension reference: %v", err)
	}
	glob := &GlobTool{CWD: workspace, Sandbox: s}
	raw, err = json.Marshal(map[string]string{"path": ext, "pattern": "*.md"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := glob.Execute(context.Background(), raw, nil); err != nil {
		t.Fatalf("browse extension references: %v", err)
	}
	if err := s.CheckWritePath(file); err == nil {
		t.Fatal("extension write allowed")
	}
	if err := s.CheckCommand("bash " + filepath.ToSlash(filepath.Join(ext, "script.sh"))); err == nil {
		t.Fatal("extension script execution allowed")
	}
	if err := s.CheckReadPath(filepath.Join(ext+"-other", "secret")); err == nil {
		t.Fatal("sibling extension read allowed")
	}
	if err := s.CheckReadPath(filepath.Join(ext, "..", "secret")); err == nil {
		t.Fatal("parent traversal allowed")
	}

	// Jail exceptions never override packaged-agent manifest permissions.
	s.SetPermissions(&PermissionSet{})
	if err := s.CheckReadPath(file); err == nil {
		t.Fatal("empty manifest read scopes bypassed")
	}
	var p PermissionSet
	p.FS.Read = []string{workspace}
	s.SetPermissions(&p)
	if err := s.CheckReadPath(file); err == nil {
		t.Fatal("manifest read scope bypassed")
	}
	p.FS.Read = []string{ext}
	if err := s.CheckReadPath(file); err != nil {
		t.Fatalf("declared extension read: %v", err)
	}
	s.SetPermissions(nil)

	replacement := t.TempDir()
	s.SetReadOnlyRoots([]string{replacement})
	if err := s.CheckReadPath(file); err == nil {
		t.Fatal("stale extension root retained")
	}
	if err := s.CheckReadPath(filepath.Join(replacement, "ref.md")); err != nil {
		t.Fatalf("replacement root: %v", err)
	}
	s.SetReadOnlyRoots(nil)
	if err := s.CheckReadPath(filepath.Join(replacement, "ref.md")); err == nil {
		t.Fatal("cleared root retained")
	}
	s.SetReadOnlyRoots([]string{filepath.Join(ext, "missing"), file})
	if err := s.CheckReadPath(filepath.Join(ext, "missing", "ref.md")); err == nil {
		t.Fatal("nonexistent root granted access")
	}
	if err := s.CheckReadPath(file); err == nil {
		t.Fatal("file accepted as directory root")
	}
}

func TestSandboxReadOnlyRootsSymlinks(t *testing.T) {
	base := t.TempDir()
	ext := filepath.Join(base, "extension")
	outside := t.TempDir()
	if err := os.Mkdir(ext, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "installed")
	if err := os.Symlink(ext, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	s := NewSandbox(t.TempDir())
	s.Lock()
	s.SetReadOnlyRoots([]string{link})
	if err := s.CheckReadPath(filepath.Join(link, "ref.md")); err != nil {
		t.Fatalf("symlinked installation: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(ext, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckReadPath(filepath.Join(link, "escape", "missing", "secret")); err == nil {
		t.Fatal("symlink escape allowed")
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckReadPath(filepath.Join(link, "secret")); err == nil {
		t.Fatal("retargeted installation granted new access")
	}
}
