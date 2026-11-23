package service

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/alexazhou/llm_mock_server/internal/model"
	"github.com/alexazhou/llm_mock_server/internal/utils"
)

// 第一版断言/条件操作符。
const (
	OpExists    = "exists"
	OpAbsent    = "absent"
	OpEq        = "eq"
	OpNe        = "ne"
	OpType      = "type"
	OpContains  = "contains"
	OpRegex     = "regex"
	OpGt        = "gt"
	OpGte       = "gte"
	OpLt        = "lt"
	OpLte       = "lte"
	OpLengthEq  = "length_eq"
	OpLengthGte = "length_gte"
	OpLengthLte = "length_lte"
)

var supportedOps = map[string]bool{
	OpExists: true, OpAbsent: true, OpEq: true, OpNe: true, OpType: true,
	OpContains: true, OpRegex: true, OpGt: true, OpGte: true, OpLt: true, OpLte: true,
	OpLengthEq: true, OpLengthGte: true, OpLengthLte: true,
}

var typeNames = map[string]bool{
	"null": true, "boolean": true, "number": true, "string": true, "array": true, "object": true,
}

// CompiledAssertion 是加载期编译完成的断言或条件。
type CompiledAssertion struct {
	Index int
	Name  string
	Key   *SystemKey
	Op    string

	ExpectedLiteral    any
	HasExpectedLiteral bool
	ExpectedRef        *SystemKey

	Default    any
	HasDefault bool

	regex *regexp.Regexp
}

// AssertionResult 是一条断言/条件的求值结果，可直接放进诊断 details。
type AssertionResult struct {
	Index   int
	Name    string
	Key     string
	Op      string
	Matched bool
	Reason  string
	Actual  any
	// HasActual=false 表示内部 missing（诊断里省略该字段）。
	HasActual      bool
	Expected       any
	HasExpected    bool
	DefaultApplied bool
	// EvaluatorError=true 表示求值器自身失败（区别于「不匹配」）。
	EvaluatorError bool
}

// CompileAssertionList 编译一个断言/条件列表。scope 只用于错误信息定位，
// allowCount=false 时禁止引用 request.count（assert 与 input.assert 的要求）。
func CompileAssertionList(scope string, list []model.Assertion, allowCount bool) ([]*CompiledAssertion, error) {
	compiled := make([]*CompiledAssertion, 0, len(list))
	names := make(map[string]int, len(list))
	for i, raw := range list {
		loc := fmt.Sprintf("%s[%d]", scope, i)
		if raw.Name != "" {
			if prev, ok := names[raw.Name]; ok {
				return nil, fmt.Errorf("%s: duplicate assertion name %q (already used by %s[%d])", loc, raw.Name, scope, prev)
			}
			names[raw.Name] = i
		}
		key, err := CompileSystemKey(loc, raw.Key, raw.Field, raw.Mode, raw.Path, allowCount)
		if err != nil {
			return nil, err
		}
		if raw.Op == "" {
			return nil, fmt.Errorf("%s: op is required", loc)
		}
		if !supportedOps[raw.Op] {
			return nil, fmt.Errorf("%s: unknown op %q", loc, raw.Op)
		}
		assertion := &CompiledAssertion{Index: i, Name: raw.Name, Key: key, Op: raw.Op}

		switch raw.Op {
		case OpExists, OpAbsent:
			if raw.Expected != nil || raw.Default != nil {
				return nil, fmt.Errorf("%s: %s must not define expected or default", loc, raw.Op)
			}
		default:
			if raw.Expected == nil {
				return nil, fmt.Errorf("%s: op %q requires expected", loc, raw.Op)
			}
		}
		if raw.Default != nil {
			value, err := utils.FromYAMLNode(raw.Default.Node)
			if err != nil {
				return nil, fmt.Errorf("%s: default: %v", loc, err)
			}
			assertion.Default = value
			assertion.HasDefault = true
		}
		if raw.Expected != nil {
			if ref, ok, err := asSystemKeyRef(raw.Expected.Node); err != nil {
				return nil, fmt.Errorf("%s: expected: %v", loc, err)
			} else if ok {
				compiledRef, err := CompileSystemKey(loc+" (expected)", ref.Key, ref.Field, ref.Mode, ref.Path, false)
				if err != nil {
					return nil, err
				}
				assertion.ExpectedRef = compiledRef
			} else {
				value, err := utils.FromYAMLNode(raw.Expected.Node)
				if err != nil {
					return nil, fmt.Errorf("%s: expected: %v", loc, err)
				}
				assertion.ExpectedLiteral = value
				assertion.HasExpectedLiteral = true
			}
		}
		if err := validateExpectedForOp(loc, assertion); err != nil {
			return nil, err
		}
		compiled = append(compiled, assertion)
	}
	return compiled, nil
}

// asSystemKeyRef 识别 expected 的系统 key 引用形态 {key, field?, mode?, path?, default?}。
// 其它映射（含任何额外字段或非字符串值）一律按字面量对象处理。
func asSystemKeyRef(n *yaml.Node) (refKeyRef, bool, error) {
	if n == nil || n.Kind != yaml.MappingNode {
		return refKeyRef{}, false, nil
	}
	var ref refKeyRef
	hasKey := false
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, value := n.Content[i].Value, n.Content[i+1]
		switch key {
		case "key", "field", "mode", "path", "default":
		default:
			return refKeyRef{}, false, nil
		}
		if value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
			return refKeyRef{}, false, nil
		}
		switch key {
		case "key":
			ref.Key = value.Value
			hasKey = true
		case "field":
			ref.Field = value.Value
		case "mode":
			ref.Mode = value.Value
		case "path":
			ref.Path = value.Value
		}
	}
	if !hasKey {
		return refKeyRef{}, false, nil
	}
	if ref.Key == "" {
		return refKeyRef{}, false, fmt.Errorf("system key reference requires a non-empty key")
	}
	return ref, true, nil
}

// refKeyRef 是 expected 引用形态的字段集合。
type refKeyRef struct {
	Key   string
	Field string
	Mode  string
	Path  string
}

func validateExpectedForOp(loc string, a *CompiledAssertion) error {
	switch a.Op {
	case OpType:
		if a.ExpectedRef != nil {
			return nil
		}
		name, ok := a.ExpectedLiteral.(string)
		if !ok || !typeNames[name] {
			return fmt.Errorf("%s: type expects null/boolean/number/string/array/object", loc)
		}
	case OpRegex:
		if a.ExpectedRef != nil {
			return nil
		}
		pattern, ok := a.ExpectedLiteral.(string)
		if !ok {
			return fmt.Errorf("%s: regex expects a Go RE2 pattern string", loc)
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return fmt.Errorf("%s: invalid regex %q: %v", loc, pattern, err)
		}
		a.regex = re
	case OpGt, OpGte, OpLt, OpLte:
		if a.ExpectedRef == nil && utils.TypeName(a.ExpectedLiteral) != "number" {
			return fmt.Errorf("%s: %s expects a number", loc, a.Op)
		}
	case OpLengthEq, OpLengthGte, OpLengthLte:
		if a.ExpectedRef != nil {
			return nil
		}
		if utils.TypeName(a.ExpectedLiteral) != "number" {
			return fmt.Errorf("%s: %s expects a non-negative integer", loc, a.Op)
		}
		if cmp, err := utils.CompareNumbers(a.ExpectedLiteral, utils.Number("0")); err != nil || cmp < 0 {
			return fmt.Errorf("%s: %s expects a non-negative integer", loc, a.Op)
		}
	}
	return nil
}

// Evaluate 求值一条断言/条件。EvaluatorError=true 时调用方应返回 condition error，
// 而不是「不匹配」。
func (a *CompiledAssertion) Evaluate(ctx *model.RequestContext) AssertionResult {
	result := AssertionResult{Index: a.Index, Name: a.Name, Key: a.Key.String(), Op: a.Op}
	actual, found, err := a.Key.Read(ctx)
	if err != nil {
		result.EvaluatorError = true
		result.Reason = err.Error()
		return result
	}
	if !found && a.HasDefault && a.Op != OpExists && a.Op != OpAbsent {
		actual = a.Default
		found = true
		result.DefaultApplied = true
	}
	result.Actual = actual
	result.HasActual = found

	var expected any
	hasExpected := false
	if a.ExpectedRef != nil {
		value, refFound, err := a.ExpectedRef.Read(ctx)
		if err != nil {
			result.EvaluatorError = true
			result.Reason = "expected: " + err.Error()
			return result
		}
		if !refFound {
			result.Reason = fmt.Sprintf("expected key %s is missing", a.ExpectedRef.String())
			return result
		}
		expected, hasExpected = value, true
	} else if a.HasExpectedLiteral {
		expected, hasExpected = a.ExpectedLiteral, true
	}
	if hasExpected {
		result.Expected = expected
		result.HasExpected = true
	}

	matched, reason := evaluateOp(a, actual, found, expected, hasExpected)
	result.Matched = matched
	result.Reason = reason
	return result
}

func evaluateOp(a *CompiledAssertion, actual any, found bool, expected any, hasExpected bool) (bool, string) {
	switch a.Op {
	case OpExists:
		return found, missingReason(found)
	case OpAbsent:
		return !found, missingReason(!found)
	}
	if !found {
		return false, "actual value is missing"
	}
	if !hasExpected {
		return false, "expected value is missing"
	}
	switch a.Op {
	case OpEq:
		if utils.Equal(actual, expected) {
			return true, ""
		}
		return false, "values are not equal"
	case OpNe:
		if !utils.Equal(actual, expected) {
			return true, ""
		}
		return false, "values are equal"
	case OpType:
		name, _ := expected.(string)
		if utils.TypeName(actual) == name {
			return true, ""
		}
		return false, fmt.Sprintf("actual type is %s, expected %s", utils.TypeName(actual), name)
	case OpContains:
		ok, err := utils.Contains(actual, expected)
		if err != nil {
			return false, err.Error()
		}
		if ok {
			return true, ""
		}
		return false, "actual does not contain expected"
	case OpRegex:
		text, ok := actual.(string)
		if !ok {
			return false, fmt.Sprintf("regex requires a string actual value, got %s", utils.TypeName(actual))
		}
		re := a.regex
		if re == nil {
			pattern, ok := expected.(string)
			if !ok {
				return false, "regex expects a pattern string"
			}
			compiled, err := regexp.Compile(pattern)
			if err != nil {
				return false, fmt.Sprintf("invalid regex: %v", err)
			}
			re = compiled
		}
		if re.MatchString(text) {
			return true, ""
		}
		return false, "string does not match the pattern"
	case OpGt, OpGte, OpLt, OpLte:
		cmp, err := utils.CompareNumbers(actual, expected)
		if err != nil {
			return false, err.Error()
		}
		switch a.Op {
		case OpGt:
			return cmp > 0, comparisonReason(cmp, ">")
		case OpGte:
			return cmp >= 0, comparisonReason(cmp, ">=")
		case OpLt:
			return cmp < 0, comparisonReason(cmp, "<")
		default:
			return cmp <= 0, comparisonReason(cmp, "<=")
		}
	case OpLengthEq, OpLengthGte, OpLengthLte:
		length, ok := utils.LengthOf(actual)
		if !ok {
			return false, fmt.Sprintf("%s requires a string, array or object, got %s", a.Op, utils.TypeName(actual))
		}
		target, err := utils.CompareNumbers(utils.Number(fmt.Sprintf("%d", length)), expected)
		if err != nil {
			return false, err.Error()
		}
		switch a.Op {
		case OpLengthEq:
			return target == 0, comparisonReason(target, "==")
		case OpLengthGte:
			return target >= 0, comparisonReason(target, ">=")
		default:
			return target <= 0, comparisonReason(target, "<=")
		}
	}
	return false, "unsupported operator " + a.Op
}

func missingReason(found bool) string {
	if found {
		return ""
	}
	return "value is missing"
}

func comparisonReason(cmp int, op string) string {
	if (cmp > 0 && strings.Contains(op, ">")) || (cmp < 0 && strings.Contains(op, "<")) || (cmp == 0 && !strings.ContainsAny(op, "<>")) {
		return ""
	}
	if cmp == 0 {
		return "values are equal but the operator requires otherwise"
	}
	if cmp > 0 {
		return "actual is greater than expected"
	}
	return "actual is less than expected"
}

// EvaluateList 顺序求值一个断言列表，返回全部结果与第一个失败项。
func EvaluateList(assertions []*CompiledAssertion, ctx *model.RequestContext) ([]AssertionResult, *AssertionResult) {
	results := make([]AssertionResult, 0, len(assertions))
	for _, assertion := range assertions {
		result := assertion.Evaluate(ctx)
		results = append(results, result)
		if !result.Matched {
			failed := result
			return results, &failed
		}
	}
	return results, nil
}
