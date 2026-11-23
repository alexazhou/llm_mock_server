package utils

import (
	"strings"
	"testing"
)

type sampleConfig struct {
	Version int    `yaml:"version"`
	ID      string `yaml:"id"`
	Nested  *struct {
		Stream string `yaml:"stream"`
	} `yaml:"nested"`
}

func TestDecodeStrictRejects(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantMsg string
	}{
		{"empty file", "", "empty YAML file"},
		{"blank file", " \n\t\n", "empty YAML file"},
		{"unknown field", "version: 1\nid: a\nbogus: 1\n", `unknown field "bogus"`},
		{"nested unknown field", "version: 1\nid: a\nnested:\n  stream: auto\n  extra: 1\n", `unknown field "extra"`},
		{"duplicate key", "version: 1\nid: a\nid: b\n", "duplicate key \"id\""},
		{"anchor", "version: 1\nid: &anchor a\n", "YAML anchor (&anchor) is not supported"},
		{"undefined alias", "version: 1\nid: a\nnested: *missing\n", "unknown anchor"},
		{"alias via anchor", "version: 1\nid: &hl hello\nnested: *hl\n", "YAML anchor (&hl)"},
		{"merge key", "version: 1\nid: a\n<<: {version: 2}\n", "YAML merge key"},
		{"custom tag", "version: 1\nid: !script a\n", `custom YAML tag "!script"`},
		{"invalid type", "version: one\nid: a\n", "cannot unmarshal"},
		{"multiple documents", "version: 1\nid: a\n---\nversion: 2\nid: b\n", "more than one YAML document"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cfg sampleConfig
			err := DecodeStrict([]byte(tt.yaml), "case.yaml", &cfg)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Fatalf("error %q does not contain %q", err.Error(), tt.wantMsg)
			}
			if !strings.HasPrefix(err.Error(), "case.yaml") && !strings.Contains(err.Error(), "case.yaml") {
				t.Fatalf("error %q should mention the file name", err.Error())
			}
		})
	}
}

func TestDecodeStrictAccepts(t *testing.T) {
	var cfg sampleConfig
	err := DecodeStrict([]byte("version: 1\nid: hello\nnested:\n  stream: auto\n"), "ok.yaml", &cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Version != 1 || cfg.ID != "hello" || cfg.Nested == nil || cfg.Nested.Stream != "auto" {
		t.Fatalf("unexpected decode result: %+v", cfg)
	}
}

func TestDecodeStrictReportsLineNumbers(t *testing.T) {
	var cfg sampleConfig
	err := DecodeStrict([]byte("version: 1\nid: a\nbogus: 1\n"), "pos.yaml", &cfg)
	if err == nil {
		t.Fatal("expected error")
	}
	posErr, ok := err.(*PositionError)
	if !ok {
		t.Fatalf("expected *PositionError, got %T: %v", err, err)
	}
	if posErr.Line != 3 {
		t.Fatalf("expected line 3, got %d (%v)", posErr.Line, err)
	}
}
