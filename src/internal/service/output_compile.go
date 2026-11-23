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
