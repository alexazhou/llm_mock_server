package service

import (
	"fmt"
	"regexp"

	"gopkg.in/yaml.v3"

	"github.com/alexazhou/llm_mock_server/internal/constants"
	"github.com/alexazhou/llm_mock_server/internal/model"
	"github.com/alexazhou/llm_mock_server/internal/utils"
)

var scriptIDPattern = regexp.MustCompile(constants.ScriptIDPattern)

// prohibitedFields 是不能出现在剧本、round 或 output 层级的字段：协议与 URL
// 只由剧本 id 推导，剧本无权声明。
var prohibitedFields = map[string]string{
	"protocol": "script must not declare \"protocol\"; the request URL selects the protocol",
	"url":      "script must not declare \"url\"; protocol URLs are generated from the script id",
	"path":     "script must not declare \"path\"; protocol URLs are generated from the script id",
	"route":    "script must not declare \"route\"; routes are generated from the script id",
}

// ValidateScriptNode 在原始 YAML 节点上执行 A 阶段的结构规则，错误带行号：
// 顶层字段集合、rounds 非空、output/outputs 互斥、outputs 非空且 fallback 唯一且在末尾。
func ValidateScriptNode(file string, doc *yaml.Node) error {
	root, err := rootMapping(file, doc)
	if err != nil {
		return err
	}
	fields := fieldMap(root)
	if err := checkProhibited(file, root, "", fields); err != nil {
		return err
	}
	for _, key := range []string{"config", "assert", "rounds"} {
		if n, ok := fields[key]; ok && n.Tag == "!!null" {
			return utils.NewPositionError(file, n, key, fmt.Sprintf("field %q must not be null", key))
		}
	}
	assertNode, ok := fields["assert"]
	if ok && assertNode.Kind != yaml.SequenceNode {
		return utils.NewPositionError(file, assertNode, "assert", "assert must be a list")
	}

	rounds, ok := fields["rounds"]
	if !ok {
		return utils.NewPositionError(file, root, "rounds", "rounds is required and must be a non-empty list")
	}
	if rounds.Kind != yaml.SequenceNode || len(rounds.Content) == 0 {
		return utils.NewPositionError(file, rounds, "rounds", "rounds must be a non-empty list")
	}

	for i, roundNode := range rounds.Content {
		roundPath := fmt.Sprintf("rounds[%d]", i)
		if roundNode.Kind != yaml.MappingNode {
			return utils.NewPositionError(file, roundNode, roundPath, "round must be a mapping")
		}
		roundFields := fieldMap(roundNode)
		if err := checkProhibited(file, roundNode, roundPath, roundFields); err != nil {
			return err
		}
		for key, n := range roundFields {
			if key != "input" && key != "output" && key != "outputs" {
				return utils.NewPositionError(file, n, utils.JoinPath(roundPath, key),
					fmt.Sprintf("field %q is not allowed in a round; a round only accepts input, output and outputs", key))
			}
		}
		if _, ok := roundFields["input"]; ok {
			inputNode := roundFields["input"]
			if inputNode.Kind != yaml.MappingNode {
				return utils.NewPositionError(file, inputNode, roundPath+".input", "input must be a mapping containing only assert")
			}
			inputFields := fieldMap(inputNode)
			for key, n := range inputFields {
				if key != "assert" {
					return utils.NewPositionError(file, n, roundPath+".input."+key,
						fmt.Sprintf("field %q is not allowed in input; input only accepts assert", key))
				}
			}
			if a, ok := inputFields["assert"]; ok {
				if a.Tag == "!!null" {
					return utils.NewPositionError(file, a, roundPath+".input.assert", "assert must not be null")
				}
				if a.Kind != yaml.SequenceNode {
					return utils.NewPositionError(file, a, roundPath+".input.assert", "assert must be a list")
				}
			}
		}

		outputNode, hasOutput := roundFields["output"]
		outputsNode, hasOutputs := roundFields["outputs"]
		switch {
		case hasOutput && hasOutputs:
			return utils.NewPositionError(file, outputNode, roundPath,
				"round must not define both output and outputs")
		case !hasOutput && !hasOutputs:
			return utils.NewPositionError(file, roundNode, roundPath,
				"round must define either output or outputs")
		case hasOutput:
			if outputNode.Kind != yaml.MappingNode {
				return utils.NewPositionError(file, outputNode, roundPath+".output", "output must be a mapping")
			}
			outFields := fieldMap(outputNode)
			if err := checkProhibited(file, outputNode, roundPath+".output", outFields); err != nil {
				return err
			}
			if _, ok := outFields["when"]; ok {
				return utils.NewPositionError(file, outFields["when"], roundPath+".output.when",
					"when is only allowed on outputs candidates; a single output is unconditional")
			}
		case hasOutputs:
			if outputsNode.Kind != yaml.SequenceNode || len(outputsNode.Content) == 0 {
				return utils.NewPositionError(file, outputsNode, roundPath+".outputs", "outputs must be a non-empty list")
			}
			fallbackIndex := -1
			for j, item := range outputsNode.Content {
				itemPath := fmt.Sprintf("%s.outputs[%d]", roundPath, j)
				if item.Kind != yaml.MappingNode {
					return utils.NewPositionError(file, item, itemPath, "outputs candidate must be a mapping")
				}
				itemFields := fieldMap(item)
				if err := checkProhibited(file, item, itemPath, itemFields); err != nil {
					return err
				}
				whenNode, hasWhen := itemFields["when"]
				if !hasWhen {
					if fallbackIndex >= 0 {
						return utils.NewPositionError(file, item, itemPath,
							fmt.Sprintf("outputs must have at most one fallback; another fallback already exists at outputs[%d]", fallbackIndex))
					}
					fallbackIndex = j
					if j != len(outputsNode.Content)-1 {
						return utils.NewPositionError(file, item, itemPath,
							"fallback output (without when) must be the last outputs candidate")
					}
					continue
				}
				if whenNode.Kind != yaml.SequenceNode || len(whenNode.Content) == 0 {
					return utils.NewPositionError(file, whenNode, itemPath+".when",
						"when must be a non-empty list; omit when to declare an unconditional fallback")
				}
			}
		}
	}
	return nil
}

// ValidateScriptSemantics 校验已解码剧本的字段值：version、id、config、
// tool_calls 与 error 的必填约束。
func ValidateScriptSemantics(file, relPath string, script *model.Script) error {
	if script.Version != constants.SchemaVersion {
		return fmt.Errorf("%s: version must be the integer %d, got %d", file, constants.SchemaVersion, script.Version)
	}
	if !scriptIDPattern.MatchString(script.ID) {
		return fmt.Errorf("%s: id %q must match %s", file, script.ID, constants.ScriptIDPattern)
	}
	if err := model.ValidateScriptConfig(script.Config); err != nil {
		return fmt.Errorf("%s: %v", file, err)
	}
	for i, assertion := range script.Assert {
		if err := validateAssertionShape(assertion); err != nil {
			return fmt.Errorf("%s: assert[%d]: %v", file, i, err)
		}
	}

	for i, round := range script.Rounds {
		roundPath := fmt.Sprintf("rounds[%d]", i)
		if round.Input != nil {
			for j, assertion := range round.Input.Assert {
				if err := validateAssertionShape(assertion); err != nil {
					return fmt.Errorf("%s: %s.input.assert[%d]: %v", file, roundPath, j, err)
				}
			}
		}
		if round.Output != nil {
			if err := validateOutputShape(round.Output); err != nil {
				return fmt.Errorf("%s: %s.output: %v", file, roundPath, err)
			}
		}
		for j := range round.Outputs {
			if err := validateOutputShape(&round.Outputs[j]); err != nil {
				return fmt.Errorf("%s: %s.outputs[%d]: %v", file, roundPath, j, err)
			}
			for k, condition := range round.Outputs[j].When {
				if err := validateAssertionShape(condition); err != nil {
					return fmt.Errorf("%s: %s.outputs[%d].when[%d]: %v", file, roundPath, j, k, err)
				}
			}
		}
	}
	return nil
}

func validateOutputShape(out *model.Output) error {
	if err := model.ValidateOutputConfig(out.Config); err != nil {
		return err
	}
	for i, call := range out.ToolCalls {
		if call.Name == "" {
			return fmt.Errorf("tool_calls[%d].name must be a non-empty string", i)
		}
		if call.Arguments.Node == nil {
			return fmt.Errorf("tool_calls[%d].arguments is required", i)
		}
	}
	if out.Error != nil {
		if out.Error.Message == nil {
			return fmt.Errorf("error.message is required")
		}
	}
	return nil
}

func validateAssertionShape(a model.Assertion) error {
	if a.Key == "" {
		return fmt.Errorf("key is required")
	}
	if a.Op == "" {
		return fmt.Errorf("op is required")
	}
	return nil
}

func checkProhibited(file string, node *yaml.Node, path string, fields map[string]*yaml.Node) error {
	for name, msg := range prohibitedFields {
		if n, ok := fields[name]; ok {
			return utils.NewPositionError(file, n, utils.JoinPath(path, name), msg)
		}
	}
	return nil
}

func fieldMap(n *yaml.Node) map[string]*yaml.Node {
	m := make(map[string]*yaml.Node, len(n.Content)/2)
	for i := 0; i+1 < len(n.Content); i += 2 {
		m[n.Content[i].Value] = n.Content[i+1]
	}
	return m
}

func rootMapping(file string, doc *yaml.Node) (*yaml.Node, error) {
	if doc == nil || doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, fmt.Errorf("%s: empty YAML document is not a valid config", file)
	}
	root := doc.Content[0]
	if root.Tag == "!!null" {
		return nil, utils.NewPositionError(file, root, "", "empty YAML document is not a valid script")
	}
	if root.Kind != yaml.MappingNode {
		return nil, utils.NewPositionError(file, root, "", "script root must be a mapping")
	}
	return root, nil
}

// NullRejecter 收集允许为 null 的节点（expected、default、arguments），
// 其余位置的显式 null 一律视为配置错误。
type NullRejecter struct {
	allowed map[*yaml.Node]bool
}

// NewNullRejecter 根据已解码剧本收集允许 null 的节点。
func NewNullRejecter(script *model.Script) *NullRejecter {
	r := &NullRejecter{allowed: map[*yaml.Node]bool{}}
	if script == nil {
		return r
	}
	addAssertions(r, []model.Assertion(script.Assert))
	for i := range script.Rounds {
		round := &script.Rounds[i]
		if round.Input != nil {
			addAssertions(r, []model.Assertion(round.Input.Assert))
		}
		if round.Output != nil {
			addOutputNulls(r, round.Output)
		}
		for j := range round.Outputs {
			addOutputNulls(r, &round.Outputs[j])
			addAssertions(r, []model.Assertion(round.Outputs[j].When))
		}
	}
	return r
}

func addAssertions(r *NullRejecter, list []model.Assertion) {
	for i := range list {
		if list[i].Expected != nil && list[i].Expected.Node != nil {
			r.allowed[list[i].Expected.Node] = true
		}
		if list[i].Default != nil && list[i].Default.Node != nil {
			r.allowed[list[i].Default.Node] = true
		}
	}
}

func addOutputNulls(r *NullRejecter, out *model.Output) {
	for i := range out.ToolCalls {
		if out.ToolCalls[i].Arguments.Node != nil {
			r.allowed[out.ToolCalls[i].Arguments.Node] = true
		}
	}
	if out.Config != nil && out.Config.Usage != nil {
		for _, v := range []*model.Value{
			out.Config.Usage.InputTokens, out.Config.Usage.OutputTokens,
			out.Config.Usage.CachedInputTokens, out.Config.Usage.ReasoningTokens,
		} {
			if v != nil && v.Node != nil {
				r.allowed[v.Node] = true
			}
		}
	}
	for _, text := range []*model.TextValue{out.Content, out.Reasoning} {
		if text != nil && text.Ref != nil && text.Ref.Default != nil && text.Ref.Default.Node != nil {
			r.allowed[text.Ref.Default.Node] = true
		}
	}
	if out.Error != nil {
		for _, text := range []*model.TextValue{out.Error.Message, out.Error.Param} {
			if text != nil && text.Ref != nil && text.Ref.Default != nil && text.Ref.Default.Node != nil {
				r.allowed[text.Ref.Default.Node] = true
			}
		}
	}
}

// Check 递归拒绝显式 null（除白名单节点外）。
func (r *NullRejecter) Check(file string, n *yaml.Node, path string) error {
	if n == nil {
		return nil
	}
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for i, item := range n.Content {
			child := path
			if n.Kind == yaml.SequenceNode {
				child = fmt.Sprintf("%s[%d]", path, i)
			}
			if err := r.Check(file, item, child); err != nil {
				return err
			}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, val := n.Content[i], n.Content[i+1]
			child := utils.JoinPath(path, key.Value)
			if val.Tag == "!!null" && !r.allowed[val] {
				return utils.NewPositionError(file, val, child,
					fmt.Sprintf("field %q must not be null", key.Value))
			}
			if err := r.Check(file, val, child); err != nil {
				return err
			}
		}
	}
	return nil
}
