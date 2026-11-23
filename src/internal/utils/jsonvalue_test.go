package utils

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

func TestEqualFollowsJSONSemantics(t *testing.T) {
	if !Equal(Number("1"), Number("1.0")) {
		t.Fatal("1 and 1.0 must be equal")
	}
	if Equal(Number("1"), "1") {
		t.Fatal("number and string must not be equal")
	}
	if !Equal(Number("0.1"), Number("0.10")) {
		t.Fatal("0.1 and 0.10 must be equal")
	}
	if !Equal(map[string]any{"a": Number("1"), "b": "x"}, map[string]any{"b": "x", "a": Number("1.00")}) {
		t.Fatal("objects must ignore key order and compare numbers exactly")
	}
	if Equal([]any{Number("1"), Number("2")}, []any{Number("2"), Number("1")}) {
		t.Fatal("arrays must keep order")
	}
	if Equal(nil, false) {
		t.Fatal("null and false must not be equal")
	}
}

func TestNumberMarshalsRaw(t *testing.T) {
	raw, err := CompactJSON(map[string]any{"n": Number("1.50"), "i": Number("9007199254740993")})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"1.50"`) {
		t.Fatalf("numbers must not be quoted: %s", raw)
	}
	if !strings.Contains(string(raw), "9007199254740993") {
		t.Fatalf("large integers must keep full precision: %s", raw)
	}
}

func TestContainsAndLength(t *testing.T) {
	if ok, err := Contains("北京今天晴", "晴"); err != nil || !ok {
		t.Fatalf("string contains failed: %v %v", ok, err)
	}
	if ok, _ := Contains([]any{Number("1"), "x"}, Number("1.0")); !ok {
		t.Fatal("array contains must reuse equality semantics")
	}
	if _, err := Contains(Number("1"), Number("1")); err == nil {
		t.Fatal("contains on a number must error")
	}
	if n, ok := LengthOf("北京"); !ok || n != 2 {
		t.Fatalf("string length must count code points, got %d %v", n, ok)
	}
	if n, ok := LengthOf([]any{1, 2, 3}); !ok || n != 3 {
		t.Fatalf("array length must count elements, got %d", n)
	}
	if n, ok := LengthOf(map[string]any{"a": 1}); !ok || n != 1 {
		t.Fatalf("object length must count keys, got %d", n)
	}
}

func TestPreviewTruncatesOnCodePointBoundary(t *testing.T) {
	small := strings.Repeat("a", 100)
	if got := Preview(small); got != small {
		t.Fatalf("small value must pass through, got %#v", got)
	}

	big := strings.Repeat("北", 3000) // 9000 字节
	preview, ok := Preview(big).(map[string]any)
	if !ok {
		t.Fatalf("expected preview envelope, got %#v", Preview(big))
	}
	text, _ := preview["preview"].(string)
	if preview["truncated"] != true {
		t.Fatal("expected truncated=true")
	}
	if len(text) > maxPreviewBytes {
		t.Fatalf("preview must be <= %d bytes, got %d", maxPreviewBytes, len(text))
	}
	if !utf8.ValidString(text) || strings.ContainsRune(text, '\uFFFD') {
		t.Fatalf("truncation must not split a code point: %q", text[max(0, len(text)-8):])
	}
	if !strings.HasPrefix(text, `"北`) {
		t.Fatalf("preview must keep the JSON string prefix: %q", text[:8])
	}
}

func TestNormalizeJSONKeepsNumberText(t *testing.T) {
	dec := json.NewDecoder(strings.NewReader(`{"n": 1.50, "big": 9007199254740993}`))
	dec.UseNumber()
	var raw map[string]any
	if err := dec.Decode(&raw); err != nil {
		t.Fatal(err)
	}
	normalized := NormalizeJSON(raw).(map[string]any)
	if normalized["n"] != Number("1.50") {
		t.Fatalf("number text must be preserved, got %#v", normalized["n"])
	}
	if normalized["big"] != Number("9007199254740993") {
		t.Fatalf("big integer must keep precision, got %#v", normalized["big"])
	}
}

func TestFromYAMLNodeTypes(t *testing.T) {
	node := yamlNodeForTest(t)
	value, err := FromYAMLNode(&node)
	if err != nil {
		t.Fatal(err)
	}
	obj, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("expected object, got %T", value)
	}
	if obj["s"] != "x" || obj["b"] != true || obj["n"] != Number("1.0") || obj["z"] != nil {
		t.Fatalf("unexpected decoded value: %#v", obj)
	}
}

func yamlNodeForTest(t *testing.T) yaml.Node {
	t.Helper()
	var node yaml.Node
	if err := yaml.Unmarshal([]byte("s: x\nb: true\nn: 1.0\nz: null\n"), &node); err != nil {
		t.Fatal(err)
	}
	return node
}
