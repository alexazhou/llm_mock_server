package service

import (
	"fmt"
	"strconv"
	"strings"
)

// BlockKind 是逻辑块类型。时间线位置 = 块 + 里程碑。
type BlockKind int

const (
	// BlockHeaders 是 HTTP 响应头，只提供 .end。
	BlockHeaders BlockKind = iota
	BlockPayload
	BlockReasoning
	BlockContent
	BlockTool
	BlockToolName
	BlockToolArguments
	BlockGeneration
	BlockStream
)

func (b BlockKind) String() string {
	switch b {
	case BlockHeaders:
		return "headers"
	case BlockPayload:
		return "payload"
	case BlockReasoning:
		return "reasoning"
	case BlockContent:
		return "content"
	case BlockTool:
		return "tool"
	case BlockToolName:
		return "name"
	case BlockToolArguments:
		return "arguments"
	case BlockGeneration:
		return "generation"
	case BlockStream:
		return "stream"
	}
	return "unknown"
}

// Milestone 是块内定位后缀。
type Milestone int

const (
	// MilestoneStart 是「已进入逻辑块、尚未发送第一个字符」的位置。
	MilestoneStart Milestone = iota
	// MilestoneEnd 是逻辑块完整结束后的位置。
	MilestoneEnd
	// MilestonePrefix 是内容块开始后的第 N 个 Unicode code point 位置。
	MilestonePrefix
)

// Target 是 send.target 解析后的绝对目标。
type Target struct {
	Raw       string
	Block     BlockKind
	ToolIndex int // 仅 tool 相关块有意义
	Milestone Milestone
	N         int // MilestonePrefix 时的字符数
}

// ContentBlock 判断该块是否承载可切片的文本内容。
func (t Target) ContentBlock() bool {
	switch t.Block {
	case BlockPayload, BlockReasoning, BlockContent, BlockToolName, BlockToolArguments:
		return true
	}
	return false
}

// IsToolBlock 判断该目标是否指向工具或其子块（Anthropic/Responses 不允许停在中间）。
func (t Target) IsToolBlock() bool {
	return t.Block == BlockTool || t.Block == BlockToolName || t.Block == BlockToolArguments
}

// ParseTarget 解析 send.target 语法：
//
//	selector := block.start | block.end | block[:N]
//	          | headers.end | generation.end | stream.end
//
// block := payload | reasoning | content | tool[i] | tool[i].name | tool[i].arguments
func ParseTarget(raw string) (Target, error) {
	target := Target{Raw: raw}
	text := strings.TrimSpace(raw)
	if text == "" {
		return target, fmt.Errorf("target must not be empty")
	}

	// 生命周期块只支持 .end。
	for _, fixed := range []struct {
		literal string
		block   BlockKind
	}{
		{"headers.end", BlockHeaders},
		{"generation.end", BlockGeneration},
		{"stream.end", BlockStream},
	} {
		if text == fixed.literal {
			target.Block = fixed.block
			target.Milestone = MilestoneEnd
			return target, nil
		}
	}

	blockText := text
	suffix := ""
	switch {
	case strings.HasPrefix(text, "tool["):
		// tool[i] 自身带方括号，先切出完整块名，剩余部分才是子块与里程碑。
		end := strings.IndexByte(text, ']')
		if end < 0 {
			return target, fmt.Errorf("target %q: tool index must be closed with ']'", raw)
		}
		blockText = text[:end+1]
		suffix = text[end+1:]
	case strings.IndexByte(text, '[') >= 0:
		// 形如 content[:N]：只有前缀切片带方括号后缀。
		idx := strings.IndexByte(text, '[')
		blockText = text[:idx]
		suffix = text[idx:]
	case strings.IndexByte(text, '.') >= 0:
		dot := strings.IndexByte(text, '.')
		blockText = text[:dot]
		suffix = text[dot:]
	}

	block, toolIndex, err := parseBlock(blockText, raw)
	if err != nil {
		return target, err
	}
	target.Block = block
	target.ToolIndex = toolIndex

	// tool[i].name / tool[i].arguments 是子块，先剥掉再解析里程碑。
	if block == BlockTool {
		switch {
		case strings.HasPrefix(suffix, ".name"):
			target.Block = BlockToolName
			suffix = suffix[len(".name"):]
		case strings.HasPrefix(suffix, ".arguments"):
			target.Block = BlockToolArguments
			suffix = suffix[len(".arguments"):]
		}
	}

	switch {
	case suffix == ".start":
		target.Milestone = MilestoneStart
	case suffix == ".end":
		target.Milestone = MilestoneEnd
	case strings.HasPrefix(suffix, "[:") && strings.HasSuffix(suffix, "]"):
		numberText := suffix[2 : len(suffix)-1]
		value, err := strconv.Atoi(numberText)
		if err != nil || value < 0 {
			return target, fmt.Errorf("target %q: slice length must be a non-negative integer", raw)
		}
		target.Milestone = MilestonePrefix
		target.N = value
	case suffix == "":
		return target, fmt.Errorf("target %q is missing a .start/.end suffix or [:N] slice", raw)
	default:
		return target, fmt.Errorf("target %q: unsupported suffix %q", raw, suffix)
	}

	// 块能力校验：哪些块支持 .start / 切片。
	switch target.Block {
	case BlockTool:
		if target.Milestone == MilestonePrefix {
			return target, fmt.Errorf("target %q: tool blocks do not support [:N] slices; target tool[i].name or tool[i].arguments", raw)
		}
	case BlockPayload, BlockReasoning, BlockContent, BlockToolName, BlockToolArguments:
	default:
		return target, fmt.Errorf("target %q: block %s does not support .start or [:N]", raw, target.Block)
	}
	return target, nil
}

// parseBlock 解析逻辑块名（含 tool[i] 显式 0-based 索引）。
func parseBlock(text, raw string) (BlockKind, int, error) {
	switch text {
	case "payload":
		return BlockPayload, 0, nil
	case "reasoning":
		return BlockReasoning, 0, nil
	case "content":
		return BlockContent, 0, nil
	}
	if !strings.HasPrefix(text, "tool[") {
		return 0, 0, fmt.Errorf("target %q: unknown logical block %q", raw, text)
	}
	rest := strings.TrimPrefix(text, "tool[")
	end := strings.IndexByte(rest, ']')
	if end < 0 {
		return 0, 0, fmt.Errorf("target %q: tool index must be closed with ']'", raw)
	}
	indexText := rest[:end]
	index, err := strconv.Atoi(indexText)
	if err != nil || index < 0 {
		return 0, 0, fmt.Errorf("target %q: tool index must be a non-negative integer, got %q", raw, indexText)
	}
	switch tail := rest[end+1:]; tail {
	case "":
		return BlockTool, index, nil
	case ".name":
		return BlockToolName, index, nil
	case ".arguments":
		return BlockToolArguments, index, nil
	default:
		return 0, 0, fmt.Errorf("target %q: tool target only supports tool[i], tool[i].name and tool[i].arguments", raw)
	}
}
