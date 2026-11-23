package service

import (
	"errors"
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/alexazhou/llm_mock_server/internal/model"
	"github.com/alexazhou/llm_mock_server/internal/utils"
)

// CompiledScript 是加载期编译完成的剧本：断言与 when 条件已编译、
// 结构已校验，运行期只读。
type CompiledScript struct {
	Entry  *model.Entry
	Script *model.Script
	Assert []*CompiledAssertion
	Rounds []CompiledRound
}

// CompiledRound 是一轮输出：Index 从 1 开始（等于 output_count + 1）。
type CompiledRound struct {
	Index       int
	InputAssert []*CompiledAssertion
	Outputs     []CompiledOutput
}

// CompiledOutput 是一个 output 候选：Fallback=true 表示没有 when 的无条件项。
type CompiledOutput struct {
	Index    int
	Fallback bool
	When     []*CompiledAssertion
	Output   *model.RoundOutput
}

// CompileScript 编译一个剧本的全部断言与条件；失败时返回带文件与节点的错误，
// 保证启动期拒绝同样可定位。doc 为该剧本的原始 YAML 文档。
func CompileScript(entry *model.Entry, doc *yaml.Node) (*CompiledScript, error) {
	script := entry.Script
	root := utils.Root(doc)
	compiled := &CompiledScript{Entry: entry, Script: script}

	var err error
	compiled.Assert, err = compileAt(entry.AbsPath, utils.Lookup(root, "assert"), "assert", "assert", script.Assert, false)
	if err != nil {
		return nil, err
	}
	for i := range script.Rounds {
		round := &script.Rounds[i]
		roundNode := utils.Lookup(root, "rounds", i)
		cr := CompiledRound{Index: i + 1}
		if round.Input != nil {
			cr.InputAssert, err = compileAt(entry.AbsPath,
				utils.Lookup(roundNode, "input", "assert"),
				fmt.Sprintf("rounds[%d].input.assert", i), "round.input.assert", round.Input.Assert, false)
			if err != nil {
				return nil, err
			}
		}
		if round.Output != nil {
			if len(round.Output.When) > 0 {
				return nil, utils.NewPositionError(entry.AbsPath, utils.Lookup(roundNode, "output", "when"),
					fmt.Sprintf("rounds[%d].output.when", i),
					"when is only allowed on outputs candidates; a single output is unconditional")
			}
			cr.Outputs = []CompiledOutput{{Index: 0, Output: round.Output}}
		} else {
			for j := range round.Outputs {
				out := &round.Outputs[j]
				co := CompiledOutput{Index: j, Fallback: len(out.When) == 0, Output: out}
				if len(out.When) > 0 {
					co.When, err = compileAt(entry.AbsPath,
						utils.Lookup(roundNode, "outputs", j, "when"),
						fmt.Sprintf("rounds[%d].outputs[%d].when", i, j), "when",
						[]model.Assertion(out.When), true)
					if err != nil {
						return nil, err
					}
				}
				cr.Outputs = append(cr.Outputs, co)
			}
		}
		compiled.Rounds = append(compiled.Rounds, cr)
	}
	return compiled, nil
}

// compileAt 把断言编译错误映射到原始 YAML 断言项的位置。
func compileAt(file string, base *yaml.Node, basePath, scopeLabel string, list []model.Assertion, allowCount bool) ([]*CompiledAssertion, error) {
	compiled, err := CompileAssertionList(basePath, list, allowCount)
	if err == nil {
		for _, assertion := range compiled {
			assertion.Scope = scopeLabel
		}
		return compiled, nil
	}
	var ace *AssertionCompileError
	if errors.As(err, &ace) {
		return nil, utils.NewPositionError(file, utils.Lookup(base, ace.Index),
			fmt.Sprintf("%s[%d]", basePath, ace.Index), ace.Message)
	}
	return nil, utils.NewPositionError(file, base, basePath, err.Error())
}
