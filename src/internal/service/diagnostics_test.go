package service

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestDiagnosticsEmitsOneJSONLinePerRequest 锁定固定 JSONL 诊断：
// 每条请求结束输出一行可解析 JSON，含请求、结果与结束原因上下文。
func TestDiagnosticsEmitsOneJSONLinePerRequest(t *testing.T) {
	configPath := writeFixture(t, "", map[string]string{"hello.yaml": `version: 1
id: hello
rounds:
  - output:
      content: hi
`})
	loaded, err := Load(configPath)
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	var out bytes.Buffer
	executor := NewExecutor(loaded, WithDiagnostics(&out))

	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	if status, ferr := execute(executor, "hello", "openai", body); status != http.StatusOK {
		t.Fatalf("execute failed: %d %v", status, ferr)
	}
	// 第二个请求走错误路径（round 越界），诊断同样要有一行。
	multiRound := `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"x"}]}`
	if status, _ := execute(executor, "hello", "openai", multiRound); status != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for out-of-range round, got %d", status)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 JSONL lines, got %d (%s)", len(lines), out.String())
	}
	var success map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &success); err != nil {
		t.Fatalf("first line is not JSON: %v (%s)", err, lines[0])
	}
	if success["script_id"] != "hello" || success["result"] != "output" || success["end_reason"] != "completed" {
		t.Fatalf("unexpected success event: %#v", success)
	}
	if success["request_count"] != float64(1) {
		t.Fatalf("diagnostics must carry request_count: %#v", success)
	}
	if _, ok := success["written_bytes"]; !ok {
		t.Fatalf("diagnostics must carry written_bytes: %#v", success)
	}

	var failure map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &failure); err != nil {
		t.Fatalf("second line is not JSON: %v (%s)", err, lines[1])
	}
	if failure["result"] != "framework_error" {
		t.Fatalf("unexpected failure event: %#v", failure)
	}
	frameworkError, ok := failure["framework_error"].(map[string]any)
	if !ok || frameworkError["code"] != "mock_round_not_found" {
		t.Fatalf("failure event must carry framework_error: %#v", failure)
	}
	// round 越界时 request.count 未分配，不能写成 0。
	if _, has := failure["request_count"]; has {
		t.Fatalf("unassigned request_count must be omitted, got %#v", failure["request_count"])
	}
}

// TestDiagnosticsDisabledByDefault 锁定「未注入 writer 时不输出」。
func TestDiagnosticsDisabledByDefault(t *testing.T) {
	executor := newExecutor(t, "", map[string]string{"hello.yaml": `version: 1
id: hello
rounds:
  - output:
      content: hi
`})
	if executor.Diagnostics != nil {
		t.Fatalf("diagnostics must be nil by default, got %#v", executor.Diagnostics)
	}
}
