package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadCompilesOutputTransport 验证加载期会编译 steps 并把错误定位到行列号。
func TestLoadCompilesOutputTransport(t *testing.T) {
	script := `version: 1
id: sliced
rounds:
  - output:
      content: hello world
      steps:
        - op: send
          target: content[:5]
        - op: send
          target: content.end
        - op: end
`
	loaded, err := Load(writeFixture(t, "", map[string]string{"sliced.yaml": script}))
	if err != nil {
		t.Fatalf("load script: %v", err)
	}
	compiled := loaded.Compiled["sliced"]
	if compiled == nil {
		t.Fatal("script must be compiled")
	}
	steps := compiled.Rounds[0].Outputs[0].Steps
	if len(steps) != 3 || steps[0].Target == nil || steps[0].Target.N != 5 {
		t.Fatalf("steps not compiled into the round output: %+v", steps)
	}
	if compiled.Rounds[0].Outputs[0].Effective == nil {
		t.Fatal("effective output config must be resolved at load time")
	}
}

func TestLoadReportsStepsErrorPosition(t *testing.T) {
	script := `version: 1
id: badsteps
rounds:
  - output:
      content: hi
      steps:
        - op: send
          target: content[1:2]
        - op: end
`
	_, err := Load(writeFixture(t, "", map[string]string{"bad.yaml": script}))
	if err == nil {
		t.Fatal("invalid selector must fail the load")
	}
	message := err.Error()
	if !strings.Contains(message, "bad.yaml:") {
		t.Fatalf("error must carry the file name: %v", message)
	}
	if !strings.Contains(message, "steps[0].target") {
		t.Fatalf("error must carry the field path: %v", message)
	}
	if !strings.Contains(message, "1:2") {
		t.Fatalf("error must carry the offending selector: %v", message)
	}
}

func TestLoadFixtureRules(t *testing.T) {
	scriptWith := func(fixture string) string {
		return `version: 1
id: rawout
rounds:
  - output:
      content: hi
      steps:
        - op: send
          source: raw
          fixture: ` + fixture + `
        - op: end
`
	}

	t.Run("valid", func(t *testing.T) {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "server.yaml"), []byte("version: 1\nscripts_dir: scripts\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "scripts", "raw.bin"), []byte("data: x\n\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "scripts", "rawout.yaml"), []byte(scriptWith("raw.bin")), 0o644); err != nil {
			t.Fatal(err)
		}
		loaded, err := Load(filepath.Join(root, "server.yaml"))
		if err != nil {
			t.Fatalf("load fixture: %v", err)
		}
		steps := loaded.Compiled["rawout"].Rounds[0].Outputs[0].Steps
		if string(steps[0].Raw) != "data: x\n\n" {
			t.Fatalf("fixture bytes not loaded: %q", steps[0].Raw)
		}
	})

	// C6-07 / C6-08 是两条对称判据：软链逃逸必须拒，指向剧本目录内普通文件的软链必须允许。
	t.Run("symlink inside is allowed", func(t *testing.T) {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "server.yaml"), []byte("version: 1\nscripts_dir: scripts\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "scripts", "target.bin"), []byte("ok\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(root, "scripts", "target.bin"), filepath.Join(root, "scripts", "link.bin")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "scripts", "rawout.yaml"), []byte(scriptWith("link.bin")), 0o644); err != nil {
			t.Fatal(err)
		}
		loaded, err := Load(filepath.Join(root, "server.yaml"))
		if err != nil {
			t.Fatalf("symlink pointing to a regular file inside the script dir must be accepted: %v", err)
		}
		steps := loaded.Compiled["rawout"].Rounds[0].Outputs[0].Steps
		if string(steps[0].Raw) != "ok\n" {
			t.Fatalf("fixture bytes not loaded through the symlink: %q", steps[0].Raw)
		}
	})

	rejected := []struct {
		name    string
		fixture string
		setup   func(t *testing.T, root string)
	}{
		{"escape", "../outside.bin", func(t *testing.T, root string) {
			if err := os.WriteFile(filepath.Join(root, "outside.bin"), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"absolute", "/etc/hosts", nil},
		{"missing", "missing.bin", nil},
		{"directory", "dir", func(t *testing.T, root string) {
			if err := os.MkdirAll(filepath.Join(root, "scripts", "dir"), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink outside", "link.bin", func(t *testing.T, root string) {
			if err := os.WriteFile(filepath.Join(root, "outside.bin"), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(root, "outside.bin"), filepath.Join(root, "scripts", "link.bin")); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "server.yaml"), []byte("version: 1\nscripts_dir: scripts\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if tc.setup != nil {
				tc.setup(t, root)
			}
			if err := os.WriteFile(filepath.Join(root, "scripts", "rawout.yaml"), []byte(scriptWith(tc.fixture)), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(filepath.Join(root, "server.yaml")); err == nil {
				t.Fatalf("fixture %q must be rejected", tc.fixture)
			}
		})
	}

	t.Run("too large", func(t *testing.T) {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "server.yaml"),
			[]byte("version: 1\nscripts_dir: scripts\nruntime:\n  max_fixture_bytes: 4\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "scripts", "raw.bin"), []byte("0123456789"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "scripts", "rawout.yaml"), []byte(scriptWith("raw.bin")), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := Load(filepath.Join(root, "server.yaml"))
		if err == nil || !strings.Contains(err.Error(), "max_fixture_bytes") {
			t.Fatalf("oversized fixture must be rejected with max_fixture_bytes, got %v", err)
		}
	})
}
