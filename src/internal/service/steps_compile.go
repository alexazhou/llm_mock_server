package service

import (
	"encoding/base64"
	"fmt"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/alexazhou/llm_mock_server/internal/constants"
	"github.com/alexazhou/llm_mock_server/internal/utils"

	"github.com/alexazhou/llm_mock_server/internal/model"
)

// send.source 的取值。
const (
	SourceOutput    = "output"
	SourceHeartbeat = "heartbeat"
	SourceError     = "error"
	SourceRaw       = "raw"
)

// step.op 的取值。
const (
	OpSend        = "send"
	OpWait        = "wait"
	OpEnd         = "end"
	EndModeNormal = "normal"
	EndModeAbort  = "abort"
)

// CompiledChunks 是 send.chunks 编译后的分片设置。
type CompiledChunks struct {
	Size    int
	HasSize bool
	Sizes   []int
	Flush   *bool
}

// CompiledStep 是一个编译完成的 step：静态字段已校验、selector 已解析。
// 协议能力（流式/非流式、Responses 无 stream.end 等）在 prepare 阶段再校验，
// 因为同一份 output 会被三个协议复用，且 config.stream: auto 依赖请求。
type CompiledStep struct {
	Index    int
	Op       string
	Source   string
	Target   *Target
	Interval int64 // 纳秒
	Chunks   *CompiledChunks
	Duration int64 // 纳秒，仅 wait
	Infinite bool  // wait.duration: infinite
	Mode     string
	Text     string
	Count    int
	// Raw 是 source=raw 的原始字节：text/base64 在加载期解码，fixture 由加载器读取。
	Raw []byte
	// RawSource 记录 raw 的来源（text/base64/fixture），仅用于诊断。
	RawSource string
	// Fixture 是 source=raw 且 RawSource=fixture 时的相对路径；加载器据此读取字节回填 Raw。
	Fixture string
}

// CompileSteps 编译并静态校验 output.steps。
// path 是 output 在剧本内的字段路径（如 rounds[0].output），用于错误定位。
// 返回值是「默认过程或显式 steps」的完整列表。
func CompileSteps(path string, out *model.RoundOutput, ef *EffectiveConfig) ([]CompiledStep, error) {
	hasNormal := out.Content != nil || out.Reasoning != nil || len(out.ToolCalls) > 0
	hasError := out.Error != nil

	if IsBodylessStatus(ef.Status) {
		if hasNormal || hasError {
			return nil, fieldErrAt(path, "",
				fmt.Sprintf("status %d must not produce a response body; omit content, reasoning, tool_calls and error", ef.Status))
		}
	}

	if len(out.Steps) == 0 && out.Steps != nil {
		return nil, fieldErrAt(path, "steps", "steps must not be empty")
	}

	if len(out.Steps) == 0 {
		return defaultSteps(path, out, hasNormal, hasError, ef)
	}

	steps := make([]CompiledStep, 0, len(out.Steps))
	for i := range out.Steps {
		raw := &out.Steps[i]
		stepPath := fmt.Sprintf("steps[%d]", i)
		step, err := compileStep(path, stepPath, raw, out, ef)
		if err != nil {
			return nil, err
		}
		step.Index = i
		steps = append(steps, step)
	}

	last := len(steps) - 1
	for i, step := range steps {
		switch step.Op {
		case OpEnd:
			if i != last {
				return nil, fieldErrAt(path, fmt.Sprintf("steps[%d].op", i), "end must be the last step")
			}
		case OpWait:
			if step.Infinite && i != last {
				return nil, fieldErrAt(path, fmt.Sprintf("steps[%d].duration", i),
					"wait.duration: infinite must be the last step")
			}
		}
	}
	final := steps[last]
	if final.Op != OpEnd && !(final.Op == OpWait && final.Infinite) {
		return nil, fieldErrAt(path, fmt.Sprintf("steps[%d]", last),
			"steps must end with end or wait.duration: infinite; the framework never appends send or end")
	}
	return steps, nil
}

// defaultSteps 在省略 steps 时按 output 内容生成默认过程。
func defaultSteps(path string, out *model.RoundOutput, hasNormal, hasError bool, ef *EffectiveConfig) ([]CompiledStep, error) {
	if hasNormal && hasError {
		return nil, fieldErrAt(path, "steps",
			"output declares both normal fields and error; explicit steps are required to define the send order")
	}
	switch {
	case hasNormal:
		return []CompiledStep{
			{Index: 0, Op: OpSend, Source: SourceOutput, Count: 1},
			{Index: 1, Op: OpEnd, Mode: EndModeNormal},
		}, nil
	case hasError:
		return []CompiledStep{
			{Index: 0, Op: OpSend, Source: SourceError, Count: 1},
			{Index: 1, Op: OpEnd, Mode: EndModeNormal},
		}, nil
	default:
		return []CompiledStep{{Index: 0, Op: OpEnd, Mode: EndModeNormal}}, nil
	}
}

// compileStep 校验并编译单个 step。
func compileStep(path, stepPath string, raw *model.Step, out *model.RoundOutput, ef *EffectiveConfig) (CompiledStep, error) {
	step := CompiledStep{Op: raw.Op, Mode: EndModeNormal, Count: 1}
	if raw.Interval != nil {
		step.Interval = int64(raw.Interval.D)
	}
	switch raw.Op {
	case OpSend:
		return compileSendStep(path, stepPath, raw, out, ef, step)
	case OpWait:
		return compileWaitStep(path, stepPath, raw, step)
	case OpEnd:
		return compileEndStep(path, stepPath, raw, ef, step)
	case "":
		return step, fieldErrAt(path, stepPath+".op", "op is required")
	default:
		return step, fieldErrAt(path, stepPath+".op",
			fmt.Sprintf("unknown op %q; only send, wait and end are supported", raw.Op))
	}
}

// compileSendStep 校验 send 的 source 专属字段、chunks 与 target。
func compileSendStep(path, stepPath string, raw *model.Step, out *model.RoundOutput, ef *EffectiveConfig, step CompiledStep,
) (CompiledStep, error) {
	step.Source = raw.Source
	if step.Source == "" {
		step.Source = SourceOutput
	}
	if raw.Duration != nil {
		return step, fieldErrAt(path, stepPath+".duration", "duration is only valid on wait")
	}
	if raw.Mode != "" {
		return step, fieldErrAt(path, stepPath+".mode", "mode is only valid on end")
	}

	switch step.Source {
	case SourceOutput:
		if raw.Text != nil || raw.Base64 != nil || raw.Fixture != nil || raw.Count != nil {
			return step, fieldErrAt(path, stepPath,
				"source: output only accepts target, interval and chunks; text/base64/fixture/count belong to other sources")
		}
		if raw.Target != "" {
			target, err := ParseTarget(raw.Target)
			if err != nil {
				return step, fieldErrAt(path, stepPath+".target", err.Error())
			}
			step.Target = &target
		}
	case SourceHeartbeat:
		if raw.Target != "" || raw.Base64 != nil || raw.Fixture != nil {
			return step, fieldErrAt(path, stepPath,
				"source: heartbeat only accepts text, count, interval and chunks")
		}
		if raw.Text != nil {
			if strings.ContainsAny(*raw.Text, "\r\n") {
				return step, fieldErrAt(path, stepPath+".text", "heartbeat text must not contain CR or LF")
			}
			step.Text = *raw.Text
		}
		if raw.Count != nil {
			if *raw.Count <= 0 {
				return step, fieldErrAt(path, stepPath+".count", "heartbeat count must be a positive integer")
			}
			step.Count = *raw.Count
		}
	case SourceError:
		if raw.Target != "" || raw.Text != nil || raw.Base64 != nil || raw.Fixture != nil || raw.Count != nil {
			return step, fieldErrAt(path, stepPath,
				"source: error takes no dedicated fields; it references the sibling output.error")
		}
		if out.Error == nil {
			return step, fieldErrAt(path, stepPath+".source", "source: error requires output.error to be declared")
		}
	case SourceRaw:
		if raw.Target != "" || raw.Count != nil {
			return step, fieldErrAt(path, stepPath,
				"source: raw only accepts exactly one of text, base64 or fixture (plus interval and chunks)")
		}
		sources := 0
		if raw.Text != nil {
			sources++
			step.Raw = []byte(*raw.Text)
			step.RawSource = "text"
		}
		if raw.Base64 != nil {
			sources++
			decoded, err := decodeStrictBase64(*raw.Base64)
			if err != nil {
				return step, fieldErrAt(path, stepPath+".base64", err.Error())
			}
			step.Raw = decoded
			step.RawSource = "base64"
		}
		if raw.Fixture != nil {
			sources++
			// fixture 的字节由加载器在编译传输层时读取并回填。
			step.RawSource = "fixture"
			step.Fixture = *raw.Fixture
		}
		if sources != 1 {
			return step, fieldErrAt(path, stepPath, "source: raw requires exactly one of text, base64 or fixture")
		}
		if step.RawSource != "fixture" && len(step.Raw) == 0 {
			return step, fieldErrAt(path, stepPath, "source: raw must resolve to at least one byte")
		}
	default:
		return step, fieldErrAt(path, stepPath+".source",
			fmt.Sprintf("unknown source %q; expected output, heartbeat, error or raw", raw.Source))
	}

	if raw.Chunks != nil {
		chunks, err := compileChunks(path, stepPath+".chunks", raw.Chunks)
		if err != nil {
			return step, err
		}
		step.Chunks = chunks
	}
	if step.Target != nil && step.Target.Block == BlockHeaders && step.Target.Milestone == MilestoneEnd && step.Chunks != nil {
		return step, fieldErrAt(path, stepPath+".chunks",
			"target: headers.end produces no data records; chunks must be omitted")
	}
	if err := validateTargetCapability(path, stepPath, step, out, ef); err != nil {
		return step, err
	}
	if step.Source == SourceHeartbeat && ef.Stream == constants.StreamFalse {
		return step, fieldErrAt(path, stepPath+".source",
			"source: heartbeat requires streaming output")
	}
	return step, nil
}

// validateTargetCapability 做加载期能判定的协议与长度校验：
// 1) 有效 stream 明确为 false 时，内容/生命周期 target 都无效（auto 留到 prepare）；
// 2) 静态字面量内容块上的 [:N] 必须不越界。动态引用留到 prepare 解析后校验。
func validateTargetCapability(path, stepPath string, step CompiledStep, out *model.RoundOutput, ef *EffectiveConfig) error {
	target := step.Target
	if target == nil {
		return nil
	}
	if target.Block == BlockHeaders && target.Milestone == MilestoneEnd {
		return nil
	}
	if ef.Stream == constants.StreamFalse {
		return fieldErrAt(path, stepPath+".target",
			"non-streaming output only accepts headers.end as an explicit send target")
	}
	if target.Milestone != MilestonePrefix {
		return nil
	}
	length, static, err := staticTargetLength(target, out)
	if err != nil {
		return fieldErrAt(path, stepPath+".target", err.Error())
	}
	if static && target.N > length {
		return fieldErrAt(path, stepPath+".target",
			fmt.Sprintf("[:%d] exceeds the %d Unicode code points of %s", target.N, length, target.Raw))
	}
	return nil
}

// staticTargetLength 返回目标内容块的静态长度（Unicode code point 数）。
// static=false 表示该块包含系统 key 动态引用，长度只能在 prepare 阶段确定。
func staticTargetLength(target *Target, out *model.RoundOutput) (int, bool, error) {
	switch target.Block {
	case BlockContent:
		if out.Content == nil {
			return 0, false, fmt.Errorf("content is not declared for this target")
		}
		if out.Content.Ref != nil {
			return 0, false, nil
		}
		return utf8.RuneCountInString(out.Content.Literal), true, nil
	case BlockReasoning:
		if out.Reasoning == nil {
			return 0, false, fmt.Errorf("reasoning is not declared for this target")
		}
		if out.Reasoning.Ref != nil {
			return 0, false, nil
		}
		return utf8.RuneCountInString(out.Reasoning.Literal), true, nil
	case BlockToolName:
		call, ok := toolAt(out, target.ToolIndex)
		if !ok {
			return 0, false, fmt.Errorf("tool[%d] is not declared", target.ToolIndex)
		}
		return utf8.RuneCountInString(call.Name), true, nil
	case BlockToolArguments:
		call, ok := toolAt(out, target.ToolIndex)
		if !ok {
			return 0, false, fmt.Errorf("tool[%d] is not declared", target.ToolIndex)
		}
		value, err := utils.FromYAMLNode(call.Arguments.Node)
		if err != nil || hasSystemKeyLeaf(call.Arguments.Node) {
			return 0, false, nil
		}
		raw, err := utils.CompactJSON(value)
		if err != nil {
			return 0, false, nil
		}
		return utf8.RuneCount(raw), true, nil
	}
	return 0, false, nil
}

// toolAt 按 0-based 下标取工具声明。
func toolAt(out *model.RoundOutput, index int) (*model.ToolCall, bool) {
	if index < 0 || index >= len(out.ToolCalls) {
		return nil, false
	}
	return &out.ToolCalls[index], true
}

// hasSystemKeyLeaf 判断 YAML 值里是否有叶子形态的系统 key 引用。
func hasSystemKeyLeaf(n *yaml.Node) bool {
	if n == nil {
		return false
	}
	switch n.Kind {
	case yaml.MappingNode:
		hasKey := false
		onlyRefFields := true
		for i := 0; i+1 < len(n.Content); i += 2 {
			switch n.Content[i].Value {
			case "key":
				hasKey = true
			case "field", "mode", "path", "default":
			default:
				onlyRefFields = false
			}
		}
		if hasKey && onlyRefFields {
			return true
		}
		for i := 1; i < len(n.Content); i += 2 {
			if hasSystemKeyLeaf(n.Content[i]) {
				return true
			}
		}
	case yaml.SequenceNode:
		for _, item := range n.Content {
			if hasSystemKeyLeaf(item) {
				return true
			}
		}
	}
	return false
}

// compileWaitStep 校验 wait：只允许 duration 与 interval 之外的空字段。
func compileWaitStep(path, stepPath string, raw *model.Step, step CompiledStep) (CompiledStep, error) {
	if raw.Source != "" || raw.Target != "" || raw.Chunks != nil || raw.Text != nil ||
		raw.Base64 != nil || raw.Fixture != nil || raw.Count != nil || raw.Mode != "" {
		return step, fieldErrAt(path, stepPath, "wait only accepts duration")
	}
	if raw.Duration == nil {
		return step, fieldErrAt(path, stepPath+".duration", "duration is required on wait")
	}
	if raw.Duration.Infinite {
		step.Infinite = true
		return step, nil
	}
	step.Duration = int64(raw.Duration.D)
	return step, nil
}

// compileEndStep 校验 end：只允许 mode，且必须是 normal 或 abort。
func compileEndStep(path, stepPath string, raw *model.Step, ef *EffectiveConfig, step CompiledStep) (CompiledStep, error) {
	if raw.Source != "" || raw.Target != "" || raw.Chunks != nil || raw.Text != nil ||
		raw.Base64 != nil || raw.Fixture != nil || raw.Count != nil || raw.Duration != nil {
		return step, fieldErrAt(path, stepPath, "end only accepts mode")
	}
	if raw.Mode != "" {
		if raw.Mode != EndModeNormal && raw.Mode != EndModeAbort {
			return step, fieldErrAt(path, stepPath+".mode",
				fmt.Sprintf("mode must be %q or %q, got %q", EndModeNormal, EndModeAbort, raw.Mode))
		}
		step.Mode = raw.Mode
	}
	return step, nil
}

// compileChunks 校验 chunks：size 与非空 sizes 必须二选一，且全为正整数。
func compileChunks(path, chunksPath string, raw *model.Chunks) (*CompiledChunks, error) {
	if raw.Size != nil && raw.Sizes != nil {
		return nil, fieldErrAt(path, chunksPath, "chunks requires exactly one of size or sizes")
	}
	if raw.Size == nil && raw.Sizes == nil {
		return nil, fieldErrAt(path, chunksPath, "chunks requires exactly one of size or sizes")
	}
	chunks := &CompiledChunks{Flush: raw.Flush}
	if raw.Size != nil {
		if *raw.Size <= 0 {
			return nil, fieldErrAt(path, chunksPath+".size", "chunks size must be a positive integer")
		}
		chunks.Size = *raw.Size
		chunks.HasSize = true
		return chunks, nil
	}
	if len(raw.Sizes) == 0 {
		return nil, fieldErrAt(path, chunksPath+".sizes", "chunks sizes must be a non-empty list of positive integers")
	}
	for i, size := range raw.Sizes {
		if size <= 0 {
			return nil, fieldErrAt(path, fmt.Sprintf("%s.sizes[%d]", chunksPath, i),
				"chunks sizes must be positive integers")
		}
	}
	chunks.Sizes = append([]int(nil), raw.Sizes...)
	return chunks, nil
}

// decodeStrictBase64 按标准 Base64 严格解码，拒绝任何非规范输入。
func decodeStrictBase64(text string) ([]byte, error) {
	decoded, err := base64.StdEncoding.Strict().DecodeString(text)
	if err != nil {
		return nil, fmt.Errorf("invalid base64: %v", err)
	}
	return decoded, nil
}

// fieldErrAt 构造一个带 output 内相对路径的字段错误。
func fieldErrAt(path, field, msg string) error {
	parts := fieldPath(path)
	if field != "" {
		parts = append(parts, fieldPath(field)...)
	}
	return &model.FieldError{Path: parts, Msg: msg}
}
