package service

import (
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/alexazhou/llm_mock_server/internal/constants"
	"github.com/alexazhou/llm_mock_server/internal/model"
)

func decodeOutput(t *testing.T, yamlText string) *model.RoundOutput {
	t.Helper()
	var out model.RoundOutput
	if err := yaml.Unmarshal([]byte(yamlText), &out); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	return &out
}

func effectiveWithStatus(t *testing.T, status int, stream string) *EffectiveConfig {
	t.Helper()
	defaults := defaultScriptDefaults()
	defaults.Status = status
	effective, err := ResolveOutputConfig(nil, nil, defaults)
	if err != nil {
		t.Fatalf("resolve effective config: %v", err)
	}
	effective.Status = status
	effective.Stream = stream
	return effective
}

// pathString 把 FieldError 路径渲染成 a.b[0].c 形式，便于断言。
func pathString(parts []any) string {
	var b strings.Builder
	for i, part := range parts {
		switch typed := part.(type) {
		case string:
			if i > 0 {
				b.WriteString(".")
			}
			b.WriteString(typed)
		case int:
			b.WriteString("[")
			b.WriteString(strconv.Itoa(typed))
			b.WriteString("]")
		}
	}
	return b.String()
}

func TestCompileStepsDefaultProcess(t *testing.T) {
	ef := effectiveWithStatus(t, 200, constants.StreamAuto)

	steps, err := CompileSteps("rounds[0].output", decodeOutput(t, "content: hi"), ef)
	if err != nil {
		t.Fatalf("compile default steps: %v", err)
	}
	if len(steps) != 2 || steps[0].Op != OpSend || steps[0].Source != SourceOutput || steps[1].Op != OpEnd {
		t.Fatalf("normal output must default to send(output) -> end: %+v", steps)
	}

	steps, err = CompileSteps("rounds[0].output", decodeOutput(t, `
error:
  message: boom
`), ef)
	if err != nil {
		t.Fatalf("compile error-only steps: %v", err)
	}
	if len(steps) != 2 || steps[0].Source != SourceError || steps[1].Op != OpEnd {
		t.Fatalf("error-only output must default to send(error) -> end: %+v", steps)
	}

	steps, err = CompileSteps("rounds[0].output", decodeOutput(t, "{}"), ef)
	if err != nil {
		t.Fatalf("compile empty output: %v", err)
	}
	if len(steps) != 1 || steps[0].Op != OpEnd {
		t.Fatalf("empty output must default to end only: %+v", steps)
	}

	if _, err := CompileSteps("rounds[0].output", decodeOutput(t, `
content: hi
error:
  message: boom
`), ef); err == nil {
		t.Fatal("normal fields + error without steps must be rejected")
	}
}

func TestCompileStepsRejectsEmptyList(t *testing.T) {
	ef := effectiveWithStatus(t, 200, constants.StreamAuto)
	if _, err := CompileSteps("rounds[0].output", decodeOutput(t, "content: hi\nsteps: []\n"), ef); err == nil {
		t.Fatal("steps: [] must be rejected")
	}
}

func TestCompileStepsRequiresTerminator(t *testing.T) {
	ef := effectiveWithStatus(t, 200, constants.StreamTrue)
	cases := map[string]string{
		"no terminator": `
content: hi
steps:
  - op: send
`,
		"end not last": `
content: hi
steps:
  - op: end
  - op: send
`,
		"infinite wait not last": `
content: hi
steps:
  - op: wait
    duration: infinite
  - op: end
`,
	}
	for name, body := range cases {
		if _, err := CompileSteps("rounds[0].output", decodeOutput(t, body), ef); err == nil {
			t.Fatalf("%s must be rejected", name)
		}
	}

	ok := `
content: hi
steps:
  - op: send
  - op: wait
    duration: infinite
`
	steps, err := CompileSteps("rounds[0].output", decodeOutput(t, ok), ef)
	if err != nil {
		t.Fatalf("infinite wait as last step must be accepted: %v", err)
	}
	if len(steps) != 2 || !steps[1].Infinite {
		t.Fatalf("unexpected steps: %+v", steps)
	}
}

func TestCompileStepsSourceFieldRules(t *testing.T) {
	ef := effectiveWithStatus(t, 200, constants.StreamTrue)
	rejected := map[string]string{
		"output with text": `
content: hi
steps:
  - op: send
    source: output
    text: nope
  - op: end
`,
		"heartbeat with target": `
content: hi
steps:
  - op: send
    source: heartbeat
    target: content.end
  - op: end
`,
		"heartbeat with CRLF text": `
content: hi
steps:
  - op: send
    source: heartbeat
    text: "a\r\nb"
  - op: end
`,
		"heartbeat count zero": `
content: hi
steps:
  - op: send
    source: heartbeat
    count: 0
  - op: end
`,
		"error without declaration": `
content: hi
steps:
  - op: send
    source: error
  - op: end
`,
		"raw without payload": `
content: hi
steps:
  - op: send
    source: raw
  - op: end
`,
		"raw with two payloads": `
content: hi
steps:
  - op: send
    source: raw
    text: a
    base64: YQ==
  - op: end
`,
		"raw with target": `
content: hi
steps:
  - op: send
    source: raw
    text: a
    target: content.end
  - op: end
`,
		"unknown source": `
content: hi
steps:
  - op: send
    source: nope
  - op: end
`,
		"wait with source": `
content: hi
steps:
  - op: wait
    duration: 1s
    source: output
  - op: end
`,
		"end with mode typo": `
content: hi
steps:
  - op: end
    mode: crash
`,
	}
	for name, body := range rejected {
		if _, err := CompileSteps("rounds[0].output", decodeOutput(t, body), ef); err == nil {
			t.Fatalf("%s must be rejected", name)
		}
	}

	ok := `
config:
  status: 503
error:
  message: boom
steps:
  - op: send
    source: heartbeat
    text: ping
    count: 2
  - op: send
    source: raw
    base64: cGluZwo=
  - op: send
    source: error
  - op: end
    mode: abort
`
	steps, err := CompileSteps("rounds[0].output", decodeOutput(t, ok), ef)
	if err != nil {
		t.Fatalf("valid mixed steps must compile: %v", err)
	}
	if steps[0].Count != 2 || steps[0].Text != "ping" {
		t.Fatalf("heartbeat fields not compiled: %+v", steps[0])
	}
	if steps[1].RawSource != "base64" || string(steps[1].Raw) != "ping\n" {
		t.Fatalf("raw base64 not decoded: %+v", steps[1])
	}
	if steps[3].Mode != EndModeAbort {
		t.Fatalf("end mode not compiled: %+v", steps[3])
	}
}

func TestCompileStepsChunksRules(t *testing.T) {
	ef := effectiveWithStatus(t, 200, constants.StreamTrue)
	rejected := map[string]string{
		"both size and sizes": `
content: hi
steps:
  - op: send
    chunks:
      size: 10
      sizes: [1, 2]
  - op: end
`,
		"neither": `
content: hi
steps:
  - op: send
    chunks:
      flush: false
  - op: end
`,
		"zero size": `
content: hi
steps:
  - op: send
    chunks:
      size: 0
  - op: end
`,
		"empty sizes": `
content: hi
steps:
  - op: send
    chunks:
      sizes: []
  - op: end
`,
		"negative size in sizes": `
content: hi
steps:
  - op: send
    chunks:
      sizes: [5, -1]
  - op: end
`,
		"chunks with headers.end": `
config:
  status: 202
content: hi
steps:
  - op: send
    target: headers.end
    chunks:
      size: 4
  - op: end
`,
	}
	for name, body := range rejected {
		if _, err := CompileSteps("rounds[0].output", decodeOutput(t, body), ef); err == nil {
			t.Fatalf("%s must be rejected", name)
		}
	}
}

func TestCompileStepsBodylessStatus(t *testing.T) {
	for _, status := range []int{204, 205, 304} {
		ef := effectiveWithStatus(t, status, constants.StreamAuto)
		if _, err := CompileSteps("rounds[0].output", decodeOutput(t, "content: hi"), ef); err == nil {
			t.Fatalf("status %d with content must be rejected", status)
		}
		// 只允许 headers / wait / end。
		if _, err := CompileSteps("rounds[0].output", decodeOutput(t, `
steps:
  - op: send
    target: headers.end
  - op: end
`), ef); err != nil {
			t.Fatalf("status %d with headers.end + end must be accepted: %v", status, err)
		}
	}
}

func TestCompileStepsTargetForwardSyntax(t *testing.T) {
	ef := effectiveWithStatus(t, 200, constants.StreamTrue)
	steps, err := CompileSteps("rounds[0].output", decodeOutput(t, `
content: hello world
steps:
  - op: send
    target: content[:5]
  - op: send
    target: content.end
  - op: end
`), ef)
	if err != nil {
		t.Fatalf("compile sliced steps: %v", err)
	}
	if steps[0].Target == nil || steps[0].Target.Milestone != MilestonePrefix || steps[0].Target.N != 5 {
		t.Fatalf("slice target not parsed: %+v", steps[0].Target)
	}
	if steps[1].Target == nil || steps[1].Target.Milestone != MilestoneEnd {
		t.Fatalf("end target not parsed: %+v", steps[1].Target)
	}
	// 错误必须带出错的 step 字段路径，供调用方在原始 YAML 节点上定位行列号。
	_, err = CompileSteps("rounds[0].output", decodeOutput(t, `
content: hi
steps:
  - op: send
    target: content[1:2]
  - op: end
`), ef)
	if err == nil {
		t.Fatal("content[1:2] must be rejected")
	}
	fieldErr, ok := err.(*model.FieldError)
	if !ok {
		t.Fatalf("expected *model.FieldError, got %T: %v", err, err)
	}
	got := pathString(fieldErr.Path)
	if !strings.HasSuffix(got, "steps[0].target") {
		t.Fatalf("expected the error path to point at steps[0].target, got %q (%v)", got, fieldErr.Path)
	}
}
