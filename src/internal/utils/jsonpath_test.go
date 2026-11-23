package utils

import (
	"strings"
	"testing"
)

func TestCompileJSONPathRejects(t *testing.T) {
	for _, raw := range []string{
		"$.",        // 空字段名
		"$..role",   // 递归下降不支持
		"$[1:2]",    // 切片不支持
		"$[?(@.x)]", // 谓词不支持
		"$['key'",   // 缺 ]
		"$['']",     // 空键
		"$[abc]",    // 非法下标
		"$.a.b[0",   // 缺 ]
	} {
		if _, err := CompileJSONPath(raw); err == nil {
			t.Fatalf("expected error for %q", raw)
		}
	}
}

func TestJSONPathEval(t *testing.T) {
	root := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "sys"},
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": nil},
		},
		"special.field": "dotted",
		"tools":         []any{map[string]any{"name": "t1"}, map[string]any{"name": "t2"}},
		"nullv":         nil,
	}
	cases := []struct {
		path  string
		want  any
		found bool
	}{
		{"", root, true},
		{"$", root, true},
		{"$.messages[0].role", "system", true},
		{"messages[1].role", "user", true},
		{"$.messages[-1].role", "assistant", true},
		{"$['special.field']", "dotted", true},
		{"$.messages[9].role", nil, false},
		{"$.missing", nil, false},
		{"$.nullv", nil, true},
	}
	for _, c := range cases {
		p, err := CompileJSONPath(c.path)
		if err != nil {
			t.Fatalf("compile %q: %v", c.path, err)
		}
		got, found := p.Eval(root)
		if found != c.found {
			t.Fatalf("%q: found=%v want %v", c.path, found, c.found)
		}
		if c.found && !Equal(got, c.want) {
			t.Fatalf("%q: got %#v want %#v", c.path, got, c.want)
		}
	}

	p, err := CompileJSONPath("$.messages[*].role")
	if err != nil {
		t.Fatal(err)
	}
	got, found := p.Eval(root)
	if !found {
		t.Fatal("wildcard projection should be found")
	}
	roles, ok := got.([]any)
	if !ok || len(roles) != 3 || roles[0] != "system" || roles[2] != "assistant" {
		t.Fatalf("unexpected wildcard result: %#v", got)
	}

	// 中间路径缺失或非数组 ⇒ missing，而不是空数组。
	p, _ = CompileJSONPath("$.missing[*].role")
	if _, found := p.Eval(root); found {
		t.Fatal("wildcard over missing path must be missing")
	}
	p, _ = CompileJSONPath("$.tools[*].name")
	if got, found := p.Eval(root); !found || len(got.([]any)) != 2 {
		t.Fatalf("unexpected tools wildcard: %#v found=%v", got, found)
	}
}

func TestJSONPathRejectsRecursiveDescentMessage(t *testing.T) {
	_, err := CompileJSONPath("$..role")
	if err == nil || !strings.Contains(err.Error(), "recursive descent") {
		t.Fatalf("expected recursive descent hint, got %v", err)
	}
}
