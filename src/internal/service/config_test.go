package service

import (
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/alexazhou/llm_mock_server/internal/constants"
	"github.com/alexazhou/llm_mock_server/internal/model"
)

func defaultScriptDefaults() model.ScriptDefaults {
	return model.ScriptDefaults{
		Stream:     constants.StreamAuto,
		Status:     200,
		Headers:    map[string]string{"X-Server": "server"},
		Flush:      true,
		LineEnding: constants.LineEndingLF,
		Usage: model.UsageValues{
			InputTokens:     10,
			OutputTokens:    20,
			ReasoningTokens: 30,
		},
		UsageMode: constants.UsageModeAuto,
	}
}

func decodeOutputConfig(t *testing.T, yamlText string) *model.OutputConfig {
	t.Helper()
	var cfg model.OutputConfig
	if err := yaml.Unmarshal([]byte(yamlText), &cfg); err != nil {
		t.Fatalf("decode output config: %v", err)
	}
	return &cfg
}

func decodeScriptConfig(t *testing.T, yamlText string) *model.ScriptConfig {
	t.Helper()
	var cfg model.ScriptConfig
	if err := yaml.Unmarshal([]byte(yamlText), &cfg); err != nil {
		t.Fatalf("decode script config: %v", err)
	}
	return &cfg
}

func TestResolveOutputConfigFallbackChain(t *testing.T) {
	defaults := defaultScriptDefaults()

	// 没有任何覆盖：全部落到 server 默认值。
	bare, err := ResolveOutputConfig(nil, nil, defaults)
	if err != nil {
		t.Fatalf("resolve bare config: %v", err)
	}
	if bare.Status != 200 || bare.Stream != constants.StreamAuto || !bare.Flush || bare.LineEnding != constants.LineEndingLF {
		t.Fatalf("unexpected bare config: %+v", bare)
	}
	if bare.Headers["X-Server"] != "server" {
		t.Fatalf("server headers must be inherited: %#v", bare.Headers)
	}
	if bare.Usage.InputTokens.Literal != 10 || bare.Usage.ReasoningTokens.Literal != 30 {
		t.Fatalf("unexpected inherited usage: %+v", bare.Usage)
	}

	// 剧本 config 覆盖 server 默认值；0、false、空 map 都是显式值。
	script := decodeScriptConfig(t, `
stream: false
status: 202
headers: {}
flush: false
line_ending: crlf
usage:
  input_tokens: 0
usage_mode: omit
`)
	effective, err := ResolveOutputConfig(nil, script, defaults)
	if err != nil {
		t.Fatalf("resolve script config: %v", err)
	}
	if effective.Status != 202 || effective.Stream != constants.StreamFalse || effective.Flush || effective.LineEnding != constants.LineEndingCRLF {
		t.Fatalf("script config not applied: %+v", effective)
	}
	if len(effective.Headers) != 0 {
		t.Fatalf("empty headers map must clear inherited headers atomically: %#v", effective.Headers)
	}
	if effective.Usage.InputTokens.Literal != 0 {
		t.Fatalf("explicit zero usage must override: %+v", effective.Usage)
	}
	if effective.Usage.OutputTokens.Literal != 20 {
		t.Fatalf("untouched usage field must fall back: %+v", effective.Usage)
	}

	// output.config 覆盖剧本 config；headers 整体替换。
	output := decodeOutputConfig(t, `
status: 503
headers:
  X-Output: out
stream: true
`)
	effective, err = ResolveOutputConfig(output, script, defaults)
	if err != nil {
		t.Fatalf("resolve output config: %v", err)
	}
	if effective.Status != 503 || effective.Stream != constants.StreamTrue {
		t.Fatalf("output config not applied: %+v", effective)
	}
	if len(effective.Headers) != 1 || effective.Headers["X-Output"] != "out" {
		t.Fatalf("output headers must replace the layer below atomically: %#v", effective.Headers)
	}
}

func TestResolveOutputConfigDynamicUsage(t *testing.T) {
	defaults := defaultScriptDefaults()
	output := decodeOutputConfig(t, `
usage:
  cached_input_tokens:
    key: request.json_body
    path: $.cached_tokens
`)
	effective, err := ResolveOutputConfig(output, nil, defaults)
	if err != nil {
		t.Fatalf("resolve dynamic usage: %v", err)
	}
	if !effective.Usage.CachedInputTokens.Dynamic() {
		t.Fatalf("cached_input_tokens must be dynamic: %+v", effective.Usage.CachedInputTokens)
	}
	if effective.Usage.CachedInputTokens.Ref.Name != "request.json_body" {
		t.Fatalf("unexpected ref: %+v", effective.Usage.CachedInputTokens.Ref)
	}
	if effective.Usage.InputTokens.Dynamic() || effective.Usage.InputTokens.Literal != 10 {
		t.Fatalf("other usage fields must stay static: %+v", effective.Usage.InputTokens)
	}
}

func TestResolveOutputConfigRejectsBadUsage(t *testing.T) {
	defaults := defaultScriptDefaults()
	cases := []struct {
		name string
		body string
	}{
		{"negative", "usage:\n  output_tokens: -1\n"},
		{"string", "usage:\n  output_tokens: \"12\"\n"},
		{"float", "usage:\n  output_tokens: 1.5\n"},
		{"default", "usage:\n  output_tokens:\n    key: request.json_body\n    path: $.n\n    default: 0\n"},
		{"unknown key", "usage:\n  output_tokens:\n    key: request.nope\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			output := decodeOutputConfig(t, tc.body)
			if _, err := ResolveOutputConfig(output, nil, defaults); err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}
}

func TestStreamingResolution(t *testing.T) {
	cases := []struct {
		stream string
		req    bool
		want   bool
	}{
		{constants.StreamAuto, true, true},
		{constants.StreamAuto, false, false},
		{constants.StreamTrue, false, true},
		{constants.StreamFalse, true, false},
	}
	for _, tc := range cases {
		cfg := &EffectiveConfig{Stream: tc.stream}
		if got := cfg.Streaming(tc.req); got != tc.want {
			t.Fatalf("Streaming(%q, request.stream=%v) = %v, want %v", tc.stream, tc.req, got, tc.want)
		}
	}
}

func TestIsBodylessStatus(t *testing.T) {
	for _, status := range []int{204, 205, 304} {
		if !IsBodylessStatus(status) {
			t.Fatalf("%d must be bodyless", status)
		}
	}
	for _, status := range []int{200, 201, 203, 400, 500} {
		if IsBodylessStatus(status) {
			t.Fatalf("%d must not be bodyless", status)
		}
	}
}
