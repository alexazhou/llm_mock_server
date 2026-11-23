package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexazhou/llm_mock_server/internal/utils"
)

const examplesConfig = "../../../doc/design/2026-11-17-configurable-go-mock-server/examples/server.yaml"

func TestLoadDocumentedExamples(t *testing.T) {
	loaded, err := Load(examplesConfig)
	if err != nil {
		t.Fatalf("documented examples must validate: %v", err)
	}
	want := []string{"chat-non-stream", "chat-stream", "retry-error", "tool-conversation"}
	got := loaded.Registry.Summaries()
	if len(got) != len(want) {
		t.Fatalf("expected %d scripts, got %d: %+v", len(want), len(got), got)
	}
	for i, id := range want {
		if got[i].ID != id {
			t.Fatalf("script[%d] = %q, want %q (scripts must be sorted by id)", i, got[i].ID, id)
		}
		if got[i].URLs.OpenAI != "/mock/"+id+"/v1/chat/completions" {
			t.Fatalf("unexpected openai url: %q", got[i].URLs.OpenAI)
		}
		if got[i].URLs.Anthropic != "/mock/"+id+"/v1/messages" {
			t.Fatalf("unexpected anthropic url: %q", got[i].URLs.Anthropic)
		}
		if got[i].URLs.Responses != "/mock/"+id+"/v1/responses" {
			t.Fatalf("unexpected responses url: %q", got[i].URLs.Responses)
		}
	}
}

// writeFixture 在临时目录写入 server.yaml 与 scripts/ 下的剧本文件。
func writeFixture(t *testing.T, serverYAML string, scripts map[string]string) string {
	t.Helper()
	root := t.TempDir()
	if serverYAML == "" {
		serverYAML = "version: 1\nscripts_dir: scripts\n"
	}
	if err := os.WriteFile(filepath.Join(root, "server.yaml"), []byte(serverYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	scriptsDir := filepath.Join(root, "scripts")
	if err := os.MkdirAll(scriptsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range scripts {
		path := filepath.Join(scriptsDir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(root, "server.yaml")
}

func TestLoadRejectsInvalidScripts(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantMsg string
	}{
		{"empty file", "", "empty YAML file"},
		{"multi document", "version: 1\nid: a\nrounds:\n  - output: {}\n---\nversion: 1\nid: b\n", "more than one YAML document"},
		{"version 2", "version: 2\nid: a\nrounds:\n  - output: {}\n", "version must be the integer 1"},
		{"uppercase id", "version: 1\nid: Hello\nrounds:\n  - output: {}\n", "must match"},
		{"missing rounds", "version: 1\nid: a\n", "rounds is required"},
		{"empty rounds", "version: 1\nid: a\nrounds: []\n", "rounds must be a non-empty list"},
		{"null rounds", "version: 1\nid: a\nrounds:\n", "must not be null"},
		{"output and outputs", "version: 1\nid: a\nrounds:\n  - output: {}\n    outputs:\n      - content: x\n", "both output and outputs"},
		{"neither output nor outputs", "version: 1\nid: a\nrounds:\n  - input:\n      assert: []\n", "either output or outputs"},
		{"empty outputs", "version: 1\nid: a\nrounds:\n  - outputs: []\n", "outputs must be a non-empty list"},
		{"empty when", "version: 1\nid: a\nrounds:\n  - outputs:\n      - when: []\n        content: x\n", "when must be a non-empty list"},
		{"fallback not last", "version: 1\nid: a\nrounds:\n  - outputs:\n      - content: x\n      - when:\n          - key: request.count\n            op: eq\n            expected: 1\n        content: y\n", "must be the last"},
		{"two fallbacks", "version: 1\nid: a\nrounds:\n  - outputs:\n      - when:\n          - key: request.count\n            op: eq\n            expected: 1\n        content: x\n      - content: y\n      - content: z\n", "must be the last"},
		{"protocol field", "version: 1\nid: a\nprotocol: openai\nrounds:\n  - output: {}\n", "must not declare \"protocol\""},
		{"url field", "version: 1\nid: a\nrounds:\n  - output:\n      url: /x\n", "must not declare \"url\""},
		{"route field", "version: 1\nid: a\nrounds:\n  - output:\n      route: /x\n", "must not declare \"route\""},
		{"path field on output", "version: 1\nid: a\nrounds:\n  - output:\n      path: /x\n", "must not declare \"path\""},
		{"when on single output", "version: 1\nid: a\nrounds:\n  - output:\n      when:\n        - key: request.count\n          op: eq\n          expected: 1\n      content: x\n", "when is only allowed on outputs candidates"},
		{"unknown round field", "version: 1\nid: a\nrounds:\n  - config: {}\n    output: {}\n", "not allowed in a round"},
		{"unknown input field", "version: 1\nid: a\nrounds:\n  - input:\n      content: x\n    output: {}\n", "input only accepts assert"},
		{"null config", "version: 1\nid: a\nconfig:\nrounds:\n  - output: {}\n", "must not be null"},
		{"status out of range", "version: 1\nid: a\nconfig:\n  status: 600\nrounds:\n  - output: {}\n", "between 200 and 599"},
		{"reserved header", "version: 1\nid: a\nconfig:\n  headers:\n    Content-Length: \"1\"\nrounds:\n  - output: {}\n", "reserved by the mock framework"},
		{"duplicate header case", "version: 1\nid: a\nconfig:\n  headers:\n    X-A: \"1\"\n    x-a: \"2\"\nrounds:\n  - output: {}\n", "duplicates header"},
		{"header crlf", "version: 1\nid: a\nconfig:\n  headers:\n    X-A: \"a\\r\\nb\"\nrounds:\n  - output: {}\n", "must not contain CR or LF"},
		{"negative duration", "version: 1\nid: a\nconfig:\n  read_body_timeout: -5s\nrounds:\n  - output: {}\n", "must not be negative"},
		{"empty tool call name", "version: 1\nid: a\nrounds:\n  - output:\n      tool_calls:\n        - name: \"\"\n          arguments: {}\n", "name must be a non-empty string"},
		{"empty content", "version: 1\nid: a\nrounds:\n  - output:\n      content: \"\"\n", "must not be an empty string"},
		{"error without message", "version: 1\nid: a\nrounds:\n  - output:\n      error:\n        type: server_error\n", "error.message is required"},
		{"assert without op", "version: 1\nid: a\nassert:\n  - key: request.method\nrounds:\n  - output: {}\n", "op is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configPath := writeFixture(t, "", map[string]string{"case.yaml": tt.content})
			_, err := Load(configPath)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Fatalf("error %q does not contain %q", err.Error(), tt.wantMsg)
			}
		})
	}
}

func TestLoadRejectsDuplicateID(t *testing.T) {
	configPath := writeFixture(t, "", map[string]string{
		"a.yaml": "version: 1\nid: same\nrounds:\n  - output: {}\n",
		"b.yaml": "version: 1\nid: same\nrounds:\n  - output: {}\n",
	})
	if _, err := Load(configPath); err == nil || !strings.Contains(err.Error(), "duplicate script id") {
		t.Fatalf("expected duplicate id error, got %v", err)
	}
}

func TestLoadRejectsInvalidServerConfig(t *testing.T) {
	tests := []struct {
		name    string
		server  string
		wantMsg string
	}{
		{"version 2", "version: 2\n", "version must be the integer 1"},
		{"port zero", "version: 1\nlisten: 127.0.0.1:0\n", "port between 1 and 65535"},
		{"port too big", "version: 1\nlisten: 127.0.0.1:70000\n", "port between 1 and 65535"},
		{"bad listen", "version: 1\nlisten: localhost\n", "host:port"},
		{"unknown field", "version: 1\nmax_requests: 3\n", `unknown field "max_requests"`},
		{"null config", "version: 1\nconfig:\n", "must not be null"},
		{"zero max_active", "version: 1\nruntime:\n  max_active_requests: 0\n", "must be a positive integer"},
		{"negative shutdown", "version: 1\nruntime:\n  shutdown_timeout: -1s\n", "must not be negative"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configPath := writeFixture(t, tt.server, map[string]string{"a.yaml": "version: 1\nid: a\nrounds:\n  - output: {}\n"})
			_, err := Load(configPath)
			if err == nil || !strings.Contains(err.Error(), tt.wantMsg) {
				t.Fatalf("expected error containing %q, got %v", tt.wantMsg, err)
			}
		})
	}
}

func TestLoadResolvesServerDefaults(t *testing.T) {
	configPath := writeFixture(t, "", map[string]string{"a.yaml": "version: 1\nid: a\nrounds:\n  - output: {}\n"})
	loaded, err := Load(configPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	resolved := loaded.Resolved
	if resolved.Listen != "127.0.0.1:9999" {
		t.Fatalf("unexpected listen default: %q", resolved.Listen)
	}
	if resolved.MaxActiveRequests != 256 || resolved.CaptureCapacity != 500 {
		t.Fatalf("unexpected runtime defaults: %+v", resolved)
	}
	if resolved.ScriptConfig.MaxBodyBytes != 8388608 || resolved.ScriptConfig.ReadBodyTimeout.String() != "15s" {
		t.Fatalf("unexpected script config defaults: %+v", resolved.ScriptConfig)
	}
	if resolved.ScriptConfig.Stream != "auto" || resolved.ScriptConfig.UsageMode != "auto" || !resolved.ScriptConfig.Flush {
		t.Fatalf("unexpected stream/usage defaults: %+v", resolved.ScriptConfig)
	}
}

func TestScanScriptFilesOrderingAndSymlinks(t *testing.T) {
	root := t.TempDir()
	scriptsDir := filepath.Join(root, "scripts")
	if err := os.MkdirAll(filepath.Join(scriptsDir, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"b.yaml", "a.yaml", "a/c.yml", "a/b.yaml"} {
		path := filepath.Join(scriptsDir, filepath.FromSlash(rel))
		if err := os.WriteFile(path, []byte("version: 1\nid: x\nrounds:\n  - output: {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(scriptsDir, "notes.txt"), []byte("not yaml"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(scriptsDir, "b.yaml"), filepath.Join(scriptsDir, "link.yaml")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	files, err := ScanScriptFiles(scriptsDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 必须是「相对路径字典序」，不是 WalkDir 的分层顺序：a.yaml < a/b.yaml。
	want := []string{"a.yaml", "a/b.yaml", "a/c.yml", "b.yaml"}
	if len(files) != len(want) {
		t.Fatalf("expected %v, got %v", want, files)
	}
	for i := range want {
		if files[i] != want[i] {
			t.Fatalf("expected %v, got %v", want, files)
		}
	}
}

func TestRenamingScriptFileKeepsURLs(t *testing.T) {
	script := "version: 1\nid: stable\nrounds:\n  - output:\n      content: hi\n"
	first := writeFixture(t, "", map[string]string{"one.yaml": script})
	second := writeFixture(t, "", map[string]string{"nested/renamed.yml": script})

	loadedFirst, err := Load(first)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	loadedSecond, err := Load(second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if loadedFirst.Registry.Summaries()[0] != loadedSecond.Registry.Summaries()[0] {
		t.Fatalf("renaming the file must not change the URLs: %+v vs %+v",
			loadedFirst.Registry.Summaries()[0], loadedSecond.Registry.Summaries()[0])
	}
}

// TestSemanticErrorsCarryNodePosition 锁死「语义校验类错误也必须带节点位置」：
// 每条都必须能取出 *utils.PositionError，且 Line>0、Path 非空。
func TestSemanticErrorsCarryNodePosition(t *testing.T) {
	cases := []struct {
		name      string
		server    string
		script    string
		wantInMsg string
		wantPath  string
	}{
		{
			name:      "id regex",
			script:    "version: 1\nid: Bad-ID\nrounds:\n  - output: {}\n",
			wantInMsg: `id "Bad-ID" must match`,
			wantPath:  "id",
		},
		{
			name:      "version required",
			script:    "id: a\nrounds:\n  - output: {}\n",
			wantInMsg: "version is required",
			wantPath:  "version",
		},
		{
			name:      "config status range",
			script:    "version: 1\nid: a\nconfig:\n  status: 600\nrounds:\n  - output: {}\n",
			wantInMsg: "between 200 and 599",
			wantPath:  "config.status",
		},
		{
			name:      "reserved header",
			script:    "version: 1\nid: a\nconfig:\n  headers:\n    Content-Length: \"1\"\nrounds:\n  - output: {}\n",
			wantInMsg: "reserved by the mock framework",
			wantPath:  "config.headers.Content-Length",
		},
		{
			name:      "tool call name",
			script:    "version: 1\nid: a\nrounds:\n  - outputs:\n      - when:\n          - key: request.count\n            op: eq\n            expected: 2\n        tool_calls:\n          - name: \"\"\n            arguments: {}\n      - content: ok\n",
			wantInMsg: "name must be a non-empty string",
			wantPath:  "rounds[0].outputs[0].tool_calls[0].name",
		},
		{
			name:      "duplicate id across files",
			script:    "version: 1\nid: same\nrounds:\n  - output: {}\n",
			wantInMsg: "duplicate script id",
			wantPath:  "id",
		},
		{
			name:      "server version not 1",
			server:    "version: 2\n",
			wantInMsg: "version must be the integer 1",
			wantPath:  "version",
		},
		{
			name:      "server version missing",
			server:    "listen: 127.0.0.1:9999\n",
			wantInMsg: "version is required",
			wantPath:  "version",
		},
		{
			name:      "server listen not a string",
			server:    "version: 1\nlisten: 9999\n",
			wantInMsg: "listen must be a string",
			wantPath:  "listen",
		},
		{
			name:      "server runtime not positive",
			server:    "version: 1\nruntime:\n  max_active_requests: 0\n",
			wantInMsg: "must be a positive integer",
			wantPath:  "runtime.max_active_requests",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			scripts := map[string]string{"a.yaml": tt.script}
			if tt.name == "duplicate id across files" {
				scripts = map[string]string{"a.yaml": tt.script, "b.yaml": tt.script}
			}
			configPath := writeFixture(t, tt.server, scripts)
			_, err := Load(configPath)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			posErr, ok := err.(*utils.PositionError)
			if !ok {
				t.Fatalf("expected *utils.PositionError carrying position, got %T: %v", err, err)
			}
			if posErr.Line <= 0 {
				t.Fatalf("expected line number, got %d (%v)", posErr.Line, err)
			}
			if posErr.Path != tt.wantPath {
				t.Fatalf("expected path %q, got %q (%v)", tt.wantPath, posErr.Path, err)
			}
			if !strings.Contains(err.Error(), tt.wantInMsg) {
				t.Fatalf("error %q does not contain %q", err.Error(), tt.wantInMsg)
			}
			if !strings.Contains(err.Error(), ".yaml:") {
				t.Fatalf("error %q should contain file:line", err.Error())
			}
		})
	}
}
