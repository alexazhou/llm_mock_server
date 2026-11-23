package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const examplesConfig = "../doc/design/2026-11-17-configurable-go-mock-server/examples/server.yaml"

func TestValidateDocumentedExamples(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"validate", "--config", examplesConfig}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("validate failed (%d): %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "ok: 4 scripts") {
		t.Fatalf("unexpected stdout: %q", stdout.String())
	}
}

func TestScriptsOutputsSortedJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"scripts", "--config", examplesConfig}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("scripts failed (%d): %s", code, stderr.String())
	}
	var summaries []struct {
		ID   string `json:"id"`
		URLs struct {
			OpenAI    string `json:"openai"`
			Anthropic string `json:"anthropic"`
			Responses string `json:"responses"`
		} `json:"urls"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &summaries); err != nil {
		t.Fatalf("stdout is not a JSON array: %v (%q)", err, stdout.String())
	}
	if len(summaries) != 4 {
		t.Fatalf("expected 4 scripts, got %d", len(summaries))
	}
	ids := []string{"chat-non-stream", "chat-stream", "retry-error", "tool-conversation"}
	for i, id := range ids {
		if summaries[i].ID != id {
			t.Fatalf("script[%d] = %q, want %q", i, summaries[i].ID, id)
		}
	}
	if summaries[0].URLs.Responses != "/mock/chat-non-stream/v1/responses" {
		t.Fatalf("unexpected responses url %q", summaries[0].URLs.Responses)
	}
}

func writeServerFixture(t *testing.T, serverYAML, scriptYAML string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "server.yaml"), []byte(serverYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if scriptYAML != "" {
		if err := os.WriteFile(filepath.Join(root, "scripts", "a.yaml"), []byte(scriptYAML), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(root, "server.yaml")
}

func TestValidateReportsConfigErrors(t *testing.T) {
	configPath := writeServerFixture(t, "version: 1\nscripts_dir: scripts\n",
		"version: 1\nid: a\nrounds: []\n")
	var stdout, stderr bytes.Buffer
	code := run([]string{"validate", "--config", configPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "a.yaml") || !strings.Contains(stderr.String(), "non-empty list") {
		t.Fatalf("expected file+reason in stderr, got %q", stderr.String())
	}
}

func TestUsageErrors(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(nil, &stdout, &stderr); code != 2 {
		t.Fatalf("expected exit 2 without subcommand, got %d", code)
	}
	if code := run([]string{"bogus"}, &stdout, &stderr); code != 2 {
		t.Fatalf("expected exit 2 for unknown subcommand, got %d", code)
	}
	if code := run([]string{"validate", "--unknown"}, &stdout, &stderr); code != 2 {
		t.Fatalf("expected exit 2 for unknown flag, got %d", code)
	}
	if code := run([]string{"validate", "extra"}, &stdout, &stderr); code != 2 {
		t.Fatalf("expected exit 2 for unexpected argument, got %d", code)
	}
}

func TestDefaultConfigPathIsCurrentDirectory(t *testing.T) {
	configPath := writeServerFixture(t, "version: 1\nscripts_dir: scripts\n",
		"version: 1\nid: a\nrounds:\n  - output: {}\n")
	t.Chdir(filepath.Dir(configPath))

	var stdout, stderr bytes.Buffer
	if code := run([]string{"validate"}, &stdout, &stderr); code != 0 {
		t.Fatalf("validate without --config failed (%d): %s", code, stderr.String())
	}
}

func TestServeRefusesInvalidConfig(t *testing.T) {
	configPath := writeServerFixture(t, "version: 1\nlisten: 127.0.0.1:0\nscripts_dir: scripts\n", "")
	var stdout, stderr bytes.Buffer
	start := time.Now()
	code := run([]string{"serve", "--config", configPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("serve should fail fast on invalid config")
	}
	if !strings.Contains(stderr.String(), "port between 1 and 65535") {
		t.Fatalf("unexpected stderr: %q", stderr.String())
	}
}

func TestServeListensAndRegistersRoutes(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and starts a server")
	}
	freePort := freeTCPPort(t)
	configPath := writeServerFixture(t,
		fmt.Sprintf("version: 1\nlisten: 127.0.0.1:%d\nscripts_dir: scripts\n", freePort),
		"version: 1\nid: hello\nrounds:\n  - output:\n      content: hi\n")

	binary := filepath.Join(t.TempDir(), "mock-server")
	build := exec.Command("go", "build", "-o", binary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}
	cmd := exec.Command(binary, "serve", "--config", configPath)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start failed: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	url := fmt.Sprintf("http://127.0.0.1:%d/mock/hello/v1/chat/completions", freePort)
	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := http.Post(url, "application/json", strings.NewReader("{}"))
		if err != nil {
			lastErr = err
			time.Sleep(50 * time.Millisecond)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotImplemented {
			t.Fatalf("expected 501 placeholder, got %d: %s", resp.StatusCode, body)
		}
		if !bytes.Contains(body, []byte("mock_not_implemented")) {
			t.Fatalf("unexpected body: %s", body)
		}
		return
	}
	t.Fatalf("server did not become ready: %v (output: %s)", lastErr, out.String())
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("cannot allocate port: %v", err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}
