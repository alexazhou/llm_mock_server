package utils

import (
	"fmt"
	"strconv"
	"strings"
)

// JSONPath 是配置参考定义的简单 JSONPath 子集：
//
//	$                     根
//	.field                对象字段
//	['special.field']     括号键名
//	[0] / [-1]            数组下标（负数从末尾倒数）
//	[*]                   数组通配投影
//
// 不支持递归下降（..）、切片（[m:n]）与谓词（[?()]）；非法语法在加载时直接报错。
type JSONPath struct {
	raw       string
	steps     []pathStep
	wildcards bool
}

type pathStep struct {
	// kind: field / index / wildcard
	kind  byte
	field string
	index int
}

// Raw 返回原始表达式，用于诊断。
func (p *JSONPath) Raw() string {
	if p == nil {
		return ""
	}
	return p.raw
}

// CompileJSONPath 解析 JSONPath 表达式。空字符串与 "$" 表示根路径。
func CompileJSONPath(raw string) (*JSONPath, error) {
	p := &JSONPath{raw: raw}
	s := strings.TrimSpace(raw)
	if s == "" || s == "$" {
		return p, nil
	}
	if strings.HasPrefix(s, "$") {
		s = s[1:]
	}
	// 根标识可省略：`messages[0].role` 与 `$.messages[0].role` 等价。
	if s != "" && !strings.HasPrefix(s, ".") && !strings.HasPrefix(s, "[") {
		end := strings.IndexAny(s, ".[")
		if end < 0 {
			end = len(s)
		}
		field := s[:end]
		if err := validateFieldName(raw, field); err != nil {
			return nil, err
		}
		p.steps = append(p.steps, pathStep{kind: 'f', field: field})
		s = s[end:]
	}
	for len(s) > 0 {
		switch {
		case strings.HasPrefix(s, "."):
			rest := s[1:]
			if strings.HasPrefix(rest, ".") {
				return nil, fmt.Errorf("invalid JSONPath %q: recursive descent (..) is not supported", raw)
			}
			end := strings.IndexAny(rest, ".[")
			if end < 0 {
				end = len(rest)
			}
			field := rest[:end]
			if field == "" {
				return nil, fmt.Errorf("invalid JSONPath %q: empty field name after '.'", raw)
			}
			if err := validateFieldName(raw, field); err != nil {
				return nil, err
			}
			p.steps = append(p.steps, pathStep{kind: 'f', field: field})
			s = rest[end:]
		case strings.HasPrefix(s, "["):
			end := strings.Index(s, "]")
			if end < 0 {
				return nil, fmt.Errorf("invalid JSONPath %q: missing ']'", raw)
			}
			body := s[1:end]
			s = s[end+1:]
			switch {
			case body == "*":
				p.steps = append(p.steps, pathStep{kind: '*'})
				p.wildcards = true
			case strings.HasPrefix(body, "'") && strings.HasSuffix(body, "'") && len(body) >= 2:
				key := body[1 : len(body)-1]
				if key == "" {
					return nil, fmt.Errorf("invalid JSONPath %q: empty bracket key", raw)
				}
				p.steps = append(p.steps, pathStep{kind: 'f', field: key})
			default:
				idx, err := strconv.Atoi(body)
				if err != nil {
					return nil, fmt.Errorf("invalid JSONPath %q: %q is not an index, a quoted key or '*'", raw, body)
				}
				p.steps = append(p.steps, pathStep{kind: 'i', index: idx})
			}
		default:
			return nil, fmt.Errorf("invalid JSONPath %q: must start a segment with '.' or '[', got %q", raw, s)
		}
	}
	return p, nil
}

// validateFieldName 拒绝会被误当作不支持语法的字段写法（递归下降、切片、谓词）。
func validateFieldName(raw, field string) error {
	switch {
	case strings.Contains(field, ".."):
		return fmt.Errorf("invalid JSONPath %q: recursive descent (..) is not supported", raw)
	case strings.ContainsAny(field, "[]?():"):
		return fmt.Errorf("invalid JSONPath %q: slices and filter predicates are not supported", raw)
	}
	return nil
}

// Eval 在已解析的 JSON 值上求值。found=false 表示路径缺失（与 JSON null 严格区分）。
// 含通配投影时结果是一维聚合数组；否则是命中的单个值。
func (p *JSONPath) Eval(root any) (value any, found bool) {
	if p == nil || len(p.steps) == 0 {
		return root, true
	}
	current := []any{root}
	for _, step := range p.steps {
		next := make([]any, 0, len(current))
		for _, cur := range current {
			switch step.kind {
			case '*':
				items, ok := cur.([]any)
				if !ok {
					continue
				}
				next = append(next, items...)
			case 'i':
				items, ok := cur.([]any)
				if !ok {
					continue
				}
				idx := step.index
				if idx < 0 {
					idx += len(items)
				}
				if idx < 0 || idx >= len(items) {
					continue
				}
				next = append(next, items[idx])
			default:
				obj, ok := cur.(map[string]any)
				if !ok {
					continue
				}
				v, ok := obj[step.field]
				if !ok {
					continue
				}
				next = append(next, v)
			}
		}
		if len(next) == 0 {
			return nil, false
		}
		current = next
	}
	if p.wildcards {
		return current, true
	}
	return current[0], true
}
