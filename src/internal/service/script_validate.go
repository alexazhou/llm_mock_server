package service

import (
	"errors"
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

// ValidateScriptSemantics 校验已解码剧本的字段值，并在原始 YAML 节点上定位
// 出错位置：错误同时带文件、行列号（能定位时）与字段路径。
func ValidateScriptSemantics(file string, doc *yaml.Node, script *model.Script) error {
	root := rootOf(doc, file)

	versionNode, hasVersion := utils.Field(root, "version")
	if !hasVersion {
		return utils.NewPositionError(file, root, "version", "version is required")
	}
	if script.Version != constants.SchemaVersion {
		return utils.NewPositionError(file, versionNode, "version",
			fmt.Sprintf("version must be the integer %d, got %d", constants.SchemaVersion, script.Version))
	}

	idNode, hasID := utils.Field(root, "id")
	if !hasID {
		return utils.NewPositionError(file, root, "id", "id is required")
	}
	if !scriptIDPattern.MatchString(script.ID) {
		return utils.NewPositionError(file, idNode, "id",
			fmt.Sprintf("id %q must match %s", script.ID, constants.ScriptIDPattern))
	}

	if err := model.ValidateScriptConfig(script.Config); err != nil {
		return locateFieldError(file, firstNonNil(utils.Lookup(root, "config"), root), "config", err)
	}

	for i := range script.Assert {
		if msg := assertionShapeError(script.Assert[i]); msg != "" {
			return utils.NewPositionError(file, utils.Lookup(root, "assert", i),
				fmt.Sprintf("assert[%d]", i), msg)
		}
	}

	for i := range script.Rounds {
		round := &script.Rounds[i]
		roundPath := fmt.Sprintf("rounds[%d]", i)
		roundNode := utils.Lookup(root, "rounds", i)
		if round.Input != nil {
			for j := range round.Input.Assert {
				if msg := assertionShapeError(round.Input.Assert[j]); msg != "" {
					return utils.NewPositionError(file, utils.Lookup(roundNode, "input", "assert", j),
						fmt.Sprintf("%s.input.assert[%d]", roundPath, j), msg)
				}
			}
		}
		if round.Output != nil {
			outNode, _ := utils.Field(roundNode, "output")
			if err := validateOutputAt(file, outNode, roundPath+".output", round.Output); err != nil {
				return err
			}
		}
		for j := range round.Outputs {
			outNode := utils.Lookup(roundNode, "outputs", j)
			outPath := fmt.Sprintf("%s.outputs[%d]", roundPath, j)
			if err := validateOutputAt(file, outNode, outPath, &round.Outputs[j]); err != nil {
				return err
			}
			for k := range round.Outputs[j].When {
				if msg := assertionShapeError(round.Outputs[j].When[k]); msg != "" {
					return utils.NewPositionError(file, utils.Lookup(outNode, "when", k),
						fmt.Sprintf("%s.when[%d]", outPath, k), msg)
				}
			}
		}
	}
	return nil
}

// DuplicateIDError 在重复 id 场景下给出后出现文件 id 节点的位置。
func DuplicateIDError(file string, doc *yaml.Node, id, previousRelPath string) error {
	idNode := utils.Lookup(utils.Root(doc), "id")
	return utils.NewPositionError(file, idNode, "id",
		fmt.Sprintf("duplicate script id %q (already defined in %s)", id, previousRelPath))
}

// validateOutputAt 校验单个 output 的字段值，错误定位到具体节点。
func validateOutputAt(file string, outNode *yaml.Node, path string, out *model.Output) error {
	if err := model.ValidateOutputConfig(out.Config); err != nil {
		return locateFieldError(file, firstNonNil(utils.Lookup(outNode, "config"), outNode), path+".config", err)
	}
	for k := range out.ToolCalls {
		if out.ToolCalls[k].Name == "" {
			return utils.NewPositionError(file, utils.Lookup(outNode, "tool_calls", k, "name"),
				fmt.Sprintf("%s.tool_calls[%d].name", path, k), "name must be a non-empty string")
		}
		if out.ToolCalls[k].Arguments.Node == nil {
			return utils.NewPositionError(file, utils.Lookup(outNode, "tool_calls", k),
				fmt.Sprintf("%s.tool_calls[%d]", path, k), "arguments is required")
		}
	}
	if out.Error != nil && out.Error.Message == nil {
		return utils.NewPositionError(file, firstNonNil(utils.Lookup(outNode, "error"), outNode),
			path+".error", "error.message is required")
	}
	return nil
}

func assertionShapeError(a model.Assertion) string {
	if a.Key == "" {
		return "key is required"
	}
	if a.Op == "" {
		return "op is required"
	}
	return ""
}

// locateFieldError 把 model.FieldError 的字段路径映射到原始 YAML 节点位置。
// base 是路径起点，basePath 是起点在配置树中的展示路径。
func locateFieldError(file string, base *yaml.Node, basePath string, err error) error {
	var fe *model.FieldError
	if !errors.As(err, &fe) {
		return utils.NewPositionError(file, base, basePath, err.Error())
	}
	shown := basePath
	for _, segment := range fe.Path {
		switch v := segment.(type) {
		case string:
			shown = utils.JoinPath(shown, v)
		case int:
			shown = fmt.Sprintf("%s[%d]", shown, v)
		}
	}
	return utils.NewPositionError(file, firstNonNil(utils.Lookup(base, fe.Path...), base), shown, fe.Msg)
}

func firstNonNil(nodes ...*yaml.Node) *yaml.Node {
	for _, n := range nodes {
		if n != nil {
			return n
		}
	}
	return nil
}

func rootOf(doc *yaml.Node, file string) *yaml.Node {
	if root := utils.Root(doc); root != nil {
		return root
	}
	return &yaml.Node{Line: 1, Column: 1, Tag: "!!map"}
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
