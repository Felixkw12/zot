package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/agent/tools"
	"github.com/patriceckhart/zot/packages/core"
)

func TestExtensionReadRootsLifecycle(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	explicit := t.TempDir()
	t.Setenv("ZOT_HOME", home)
	global := filepath.Join(home, "extensions", "bundle")
	project := filepath.Join(cwd, ".zot", "extensions", "bundle")
	disabled := filepath.Join(home, "extensions", "disabled")
	writeBundle := func(dir, manifest string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, "skills"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "extension.json"), []byte(manifest), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeBundle(global, `{"name":"bundle","skills":["skills"]}`)
	writeBundle(project, `{"name":"bundle","skills":["skills"]}`)
	writeBundle(explicit, `{"name":"explicit","skills":["skills"]}`)
	writeBundle(disabled, `{"name":"disabled","enabled":false,"skills":["skills"]}`)

	for _, noExt := range []bool{false, true} {
		name := "discovery"
		if noExt {
			name = "explicit-only"
		}
		t.Run(name, func(t *testing.T) {
			args := Args{CWD: cwd, Exts: []string{explicit}, NoExt: noExt}
			sandbox := tools.NewSandbox(cwd)
			sandbox.Lock()
			r := Resolved{CWD: cwd, Sandbox: sandbox, ToolRegistry: core.Registry{}}
			mgr, stop, errs := loadNonInteractiveExtensions(context.Background(), args, &r, "test")
			defer stop()
			if len(errs) != 0 {
				t.Fatalf("load extensions: %v", errs)
			}
			check := func(dir string, allowed bool) {
				t.Helper()
				err := sandbox.CheckReadPath(filepath.Join(dir, "reference.md"))
				if (err == nil) != allowed {
					t.Fatalf("read %s: allowed = %v, want %v, error = %v", dir, err == nil, allowed, err)
				}
			}
			check(explicit, true)
			check(project, true) // Inside the jail regardless of discovery.
			check(global, false)
			check(disabled, false)

			// Moving the jail root retains only the selected extension grants,
			// not unrestricted read access to the previous working directory.
			sandbox.Root = t.TempDir()
			check(project, !noExt)
			check(cwd, false)

			// Agent rebuilds retain the shared jail and refresh bundle roots,
			// even when the manager has no registered tools.
			rebuilt := Resolved{CWD: cwd, Sandbox: tools.NewSandbox(cwd), ToolRegistry: core.Registry{}}
			rebuilt.UseSandbox(sandbox)
			rebuilt.MergeExtensionTools(&extToolAdapter{mgr: mgr})
			if !sandbox.Locked() {
				t.Fatal("rebuild unlocked jail")
			}
			check(explicit, true)

			// Disabling an extension and reloading removes its old grant.
			writeBundle(explicit, `{"name":"explicit","enabled":false,"skills":["skills"]}`)
			stats := mgr.Reload(context.Background(), time.Second)
			if len(stats.Errors) != 0 {
				t.Fatalf("reload extensions: %v", stats.Errors)
			}
			rebuilt.MergeExtensionTools(&extToolAdapter{mgr: mgr})
			check(explicit, false)
			writeBundle(explicit, `{"name":"explicit","skills":["skills"]}`)

			// No extension source clears all grants on a shared sandbox.
			rebuilt.MergeExtensionTools(nil)
			check(project, false)
		})
	}
}
