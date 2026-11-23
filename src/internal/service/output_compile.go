package service

import (
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/alexazhou/llm_mock_server/internal/model"
	"github.com/alexazhou/llm_mock_server/internal/utils"
)

// compileOutputTransport 在加载期完成每个 output 的输出配置解析、steps 编译与 fixture 读取。
// 协议相关能力（流式/非流式、Responses 无 stream.end、Anthropic/Responses 不可停在工具名中间）
// 留到 prepare 阶段，因为同一份 output 会被三个协议复用，且 config.stream: auto 依赖请求。
func compileOutputTransport(loaded *Loaded) error {
	limits := loaded.Resolved
	for id, compiled := range loaded.Compiled {
		if compiled.Doc == nil {
			return fmt.Errorf("script %q is missing its YAML document", id)
		}
		root := utils.Root(compiled.Doc)
		file := compiled.Entry.AbsPath
		scriptDir := filepathDir(file)

		for i := range compiled.Rounds {
			round := &compiled.Script.Rounds[i]
			for j := range compiled.Rounds[i].Outputs {
				candidate := &compiled.Rounds[i].Outputs[j]
				base, path := outputNode(root, i, round, j)
				if err := compileOneOutput(candidate, compiled, base, path, scriptDir, limits); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// compileOneOutput 编译单个 output 的有效配置与 steps。
func compileOneOutput(candidate *CompiledOutput, compiled *CompiledScript, base *yaml.Node, path, scriptDir string,
	limits *model.ResolvedServer,
) error {
	file := compiled.Entry.AbsPath
	effective, err := ResolveOutputConfig(candidate.Output.Config, compiled.Script.Config, limits.ScriptConfig)
	if err != nil {
		configNode := firstNonNil(utils.Lookup(base, "config"), base)
		return locateFieldError(file, configNode, path+".config", err)
	}
	steps, err := CompileSteps(path, candidate.Output, effective)
	if err != nil {
		return locateFieldError(file, base, "", err)
	}
	for k := range steps {
		if steps[k].Fixture == "" {
			continue
		}
		data, err := LoadFixture(scriptDir, steps[k].Fixture, limits.MaxFixtureBytes)
		if err != nil {
			node := firstNonNil(utils.Lookup(base, "steps", steps[k].Index, "fixture"), base)
			return utils.NewPositionError(file, node,
				fmt.Sprintf("%s.steps[%d].fixture", path, steps[k].Index), err.Error())
		}
		steps[k].Raw = data
	}
	candidate.Effective = effective
	candidate.Steps = steps
	if err := compileOutputValues(candidate, base, path, file); err != nil {
		return err
	}
	return nil
}

// compileOutputValues 编译「输出什么」：content / reasoning / tool_calls / error。
// 只做加载期能判定的部分（系统 key 语法、arguments 结构与 JSON 可表达性、
// tool 名与 id 的静态约束）；动态值的类型、长度与协议上限留到 prepare 阶段。
func compileOutputValues(candidate *CompiledOutput, base *yaml.Node, path, file string) error {
	out := candidate.Output
	content, err := CompileTextValue(out.Content)
	if err != nil {
		return utils.NewPositionError(file, firstNonNil(utils.Lookup(base, "content"), base),
			path+".content", err.Error())
	}
	reasoning, err := CompileTextValue(out.Reasoning)
	if err != nil {
		return utils.NewPositionError(file, firstNonNil(utils.Lookup(base, "reasoning"), base),
			path+".reasoning", err.Error())
	}
	calls := make([]CompiledToolCall, 0, len(out.ToolCalls))
	for k := range out.ToolCalls {
		raw := out.ToolCalls[k]
		call := CompiledToolCall{ID: raw.ID, Name: raw.Name}
		arguments, err := CompileOutputValue("arguments", raw.Arguments.Node)
		if err != nil {
			return utils.NewPositionError(file, firstNonNil(utils.Lookup(base, "tool_calls", k, "arguments"), base),
				fmt.Sprintf("%s.tool_calls[%d]", path, k), err.Error())
		}
		call.Arguments = arguments
		calls = append(calls, call)
	}
	var protoErr *CompiledError
	if out.Error != nil {
		message, err := CompileTextValue(out.Error.Message)
		if err != nil {
			return utils.NewPositionError(file, firstNonNil(utils.Lookup(base, "error", "message"), base),
				path+".error.message", err.Error())
		}
		param, err := CompileTextValue(out.Error.Param)
		if err != nil {
			return utils.NewPositionError(file, firstNonNil(utils.Lookup(base, "error", "param"), base),
				path+".error.param", err.Error())
		}
		protoErr = &CompiledError{Type: out.Error.Type, Code: out.Error.Code, Message: message, Param: param}
	}
	candidate.Content = content
	candidate.Reasoning = reasoning
	candidate.ToolCalls = calls
	candidate.Error = protoErr
	return nil
}

// outputNode 定位一个 output 的原始 YAML 节点与字段路径（单 output 与 outputs 候选两种形态）。
func outputNode(root *yaml.Node, roundIndex int, round *model.Round, candidateIndex int) (*yaml.Node, string) {
	if round.Output != nil {
		return firstNonNil(utils.Lookup(root, "rounds", roundIndex, "output"), root),
			fmt.Sprintf("rounds[%d].output", roundIndex)
	}
	return firstNonNil(utils.Lookup(root, "rounds", roundIndex, "outputs", candidateIndex), root),
		fmt.Sprintf("rounds[%d].outputs[%d]", roundIndex, candidateIndex)
}

func filepathDir(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[:i]
		}
	}
	return "."
}
