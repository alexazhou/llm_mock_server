package service

import (
	"fmt"

	"github.com/alexazhou/llm_mock_server/internal/constants"
	"github.com/alexazhou/llm_mock_server/internal/model"
)

// 本文件定义流式输出的「逻辑位置」与 target 求值：所有 selector 都折算成
// (stage, offset) 序对，游标只前移。stage 顺序即协议记录顺序：
// headers.end → payload.start → reasoning → content → tool[0].name → tool[0].arguments
// → … → payload.end → generation.end → stream.end。

const (
	stageHeaders       int64 = 0
	stagePayloadStart  int64 = 1
	stageReasoning     int64 = 2
	stageContent       int64 = 3
	stageToolNameBase  int64 = 10
	stagePayloadEnd    int64 = 1_000_000
	stageGenerationEnd int64 = 1_000_001
	stageStreamEnd     int64 = 1_000_002
)

// streamPos 是逻辑时间线上的一个位置。
type streamPos struct {
	stage  int64
	offset int64
}

// after 判断 p 是否严格位于 q 之后。
func (p streamPos) after(q streamPos) bool {
	if p.stage != q.stage {
		return p.stage > q.stage
	}
	return p.offset > q.offset
}

// before 判断 p 是否严格位于 q 之前。
func (p streamPos) before(q streamPos) bool { return q.after(p) }

func (p streamPos) String() string { return fmt.Sprintf("%d:%d", p.stage, p.offset) }

// toolNameStage / toolArgsStage 返回第 i 个工具的 name / arguments 阶段号。
func toolNameStage(index int) int64 { return stageToolNameBase + 2*int64(index) }
func toolArgsStage(index int) int64 { return stageToolNameBase + 2*int64(index) + 1 }

// streamGeometry 汇总用于求值 target 的静态长度（全部来自 prepare 后的快照）。
type streamGeometry struct {
	reasoningLen int
	contentLen   int
	toolNames    []int
	toolArgs     []int
}

// geometryOf 从 prepared 快照计算各内容块的长度（Unicode code point / 字节）。
func geometryOf(prepared *PreparedOutput) streamGeometry {
	geometry := streamGeometry{
		reasoningLen: len([]rune(prepared.Reasoning)),
		contentLen:   len([]rune(prepared.Content)),
	}
	for _, call := range prepared.ToolCalls {
		geometry.toolNames = append(geometry.toolNames, len([]rune(call.Name)))
		geometry.toolArgs = append(geometry.toolArgs, len(call.Arguments))
	}
	return geometry
}

// targetPosition 把一个 selector 折算成逻辑位置。长度不合法或协议不允许的位置
// （例如 Anthropic/Responses 停在工具名中间）返回错误，由 prepare 阶段拒绝。
func targetPosition(protocol string, geometry streamGeometry, target *Target) (streamPos, error) {
	switch target.Block {
	case BlockHeaders:
		if target.Milestone != MilestoneEnd {
			return streamPos{}, fmt.Errorf("headers only provides .end")
		}
		return streamPos{stage: stageHeaders}, nil
	case BlockPayload:
		switch target.Milestone {
		case MilestoneStart:
			return streamPos{stage: stagePayloadStart}, nil
		case MilestoneEnd:
			return streamPos{stage: stagePayloadEnd}, nil
		}
		// payload[:N] 跨 reasoning + content 计字符；工具参数不参与字符切片。
		total := geometry.reasoningLen + geometry.contentLen
		if target.N > total {
			return streamPos{}, fmt.Errorf(
				"[:%d] exceeds the %d Unicode code points of payload", target.N, total)
		}
		if geometry.reasoningLen > 0 && target.N <= geometry.reasoningLen {
			return streamPos{stage: stageReasoning, offset: int64(target.N)}, nil
		}
		return streamPos{stage: stageContent, offset: int64(target.N - geometry.reasoningLen)}, nil
	case BlockReasoning:
		switch target.Milestone {
		case MilestoneStart:
			return streamPos{stage: stageReasoning}, nil
		case MilestoneEnd:
			return streamPos{stage: stageReasoning, offset: int64(geometry.reasoningLen + 1)}, nil
		}
		if target.N > geometry.reasoningLen {
			return streamPos{}, fmt.Errorf(
				"[:%d] exceeds the %d Unicode code points of reasoning", target.N, geometry.reasoningLen)
		}
		return streamPos{stage: stageReasoning, offset: int64(target.N)}, nil
	case BlockContent:
		switch target.Milestone {
		case MilestoneStart:
			return streamPos{stage: stageContent}, nil
		case MilestoneEnd:
			return streamPos{stage: stageContent, offset: int64(geometry.contentLen + 1)}, nil
		}
		if target.N > geometry.contentLen {
			return streamPos{}, fmt.Errorf(
				"[:%d] exceeds the %d Unicode code points of content", target.N, geometry.contentLen)
		}
		return streamPos{stage: stageContent, offset: int64(target.N)}, nil
	case BlockTool:
		if target.ToolIndex < 0 || target.ToolIndex >= len(geometry.toolArgs) {
			return streamPos{}, fmt.Errorf("tool[%d] is not declared", target.ToolIndex)
		}
		switch target.Milestone {
		case MilestoneStart:
			if protocol != constants.ProtocolOpenAI {
				return streamPos{}, fmt.Errorf(
					"protocol %s cannot stop at tool[%d].start; the tool name is sent atomically",
					protocol, target.ToolIndex)
			}
			return streamPos{stage: toolNameStage(target.ToolIndex)}, nil
		case MilestoneEnd:
			return streamPos{stage: toolArgsStage(target.ToolIndex),
				offset: int64(geometry.toolArgs[target.ToolIndex] + 1)}, nil
		}
		return streamPos{}, fmt.Errorf("tool[%d] does not accept a prefix selector", target.ToolIndex)
	case BlockToolName:
		if target.ToolIndex < 0 || target.ToolIndex >= len(geometry.toolNames) {
			return streamPos{}, fmt.Errorf("tool[%d] is not declared", target.ToolIndex)
		}
		switch target.Milestone {
		case MilestoneStart:
			return streamPos{stage: toolNameStage(target.ToolIndex)}, nil
		case MilestoneEnd:
			return streamPos{stage: toolNameStage(target.ToolIndex), offset: 1}, nil
		}
		if protocol != constants.ProtocolOpenAI {
			return streamPos{}, fmt.Errorf(
				"protocol %s cannot stop in the middle of tool[%d].name; the name is sent atomically",
				protocol, target.ToolIndex)
		}
		if target.N > geometry.toolNames[target.ToolIndex] {
			return streamPos{}, fmt.Errorf("[:%d] exceeds tool[%d].name", target.N, target.ToolIndex)
		}
		return streamPos{stage: toolNameStage(target.ToolIndex), offset: int64(target.N)}, nil
	case BlockToolArguments:
		if target.ToolIndex < 0 || target.ToolIndex >= len(geometry.toolArgs) {
			return streamPos{}, fmt.Errorf("tool[%d] is not declared", target.ToolIndex)
		}
		switch target.Milestone {
		case MilestoneStart:
			return streamPos{stage: toolArgsStage(target.ToolIndex)}, nil
		case MilestoneEnd:
			return streamPos{stage: toolArgsStage(target.ToolIndex),
				offset: int64(geometry.toolArgs[target.ToolIndex] + 1)}, nil
		}
		// arguments 的 [:N] 按 UTF-8 字节计（协议 delta 也是字节流）。
		if target.N > geometry.toolArgs[target.ToolIndex] {
			return streamPos{}, fmt.Errorf("[:%d] exceeds tool[%d].arguments", target.N, target.ToolIndex)
		}
		return streamPos{stage: toolArgsStage(target.ToolIndex), offset: int64(target.N)}, nil
	case BlockGeneration:
		if target.Milestone != MilestoneEnd {
			return streamPos{}, fmt.Errorf("generation only provides .end")
		}
		return streamPos{stage: stageGenerationEnd}, nil
	case BlockStream:
		if target.Milestone != MilestoneEnd {
			return streamPos{}, fmt.Errorf("stream only provides .end")
		}
		return streamPos{stage: stageStreamEnd}, nil
	}
	return streamPos{}, fmt.Errorf("unsupported target %q", target.Raw)
}

// validateStreamTargets 在 prepare 阶段静态模拟游标，拒绝回退或重复的 target。
func validateStreamTargets(prepared *PreparedOutput) *model.FrameworkError {
	geometry := geometryOf(prepared)
	cursor := streamPos{stage: stageHeaders}
	for i := range prepared.Steps {
		step := &prepared.Steps[i]
		if step.Op != OpSend || step.Source != SourceOutput {
			continue
		}
		position := streamPos{stage: stageStreamEnd}
		if step.Target != nil {
			resolved, err := targetPosition(prepared.Protocol, geometry, step.Target)
			if err != nil {
				return prepareError(fmt.Sprintf("steps[%d].target: %v", step.Index, err), nil)
			}
			position = resolved
		}
		if !position.after(cursor) {
			return prepareError(fmt.Sprintf(
				"steps[%d].target: output cursor must move strictly forward", step.Index), nil)
		}
		cursor = position
	}
	return nil
}
