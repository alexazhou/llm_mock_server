package service

import "testing"

func TestParseTargetAcceptsDocumentedForms(t *testing.T) {
	cases := []struct {
		raw       string
		block     BlockKind
		toolIndex int
		milestone Milestone
		n         int
	}{
		{"headers.end", BlockHeaders, 0, MilestoneEnd, 0},
		{"payload.start", BlockPayload, 0, MilestoneStart, 0},
		{"payload[:10]", BlockPayload, 0, MilestonePrefix, 10},
		{"payload.end", BlockPayload, 0, MilestoneEnd, 0},
		{"reasoning.start", BlockReasoning, 0, MilestoneStart, 0},
		{"reasoning[:5]", BlockReasoning, 0, MilestonePrefix, 5},
		{"reasoning.end", BlockReasoning, 0, MilestoneEnd, 0},
		{"content.start", BlockContent, 0, MilestoneStart, 0},
		{"content[:0]", BlockContent, 0, MilestonePrefix, 0},
		{"content.end", BlockContent, 0, MilestoneEnd, 0},
		{"tool[0].start", BlockTool, 0, MilestoneStart, 0},
		{"tool[1].end", BlockTool, 1, MilestoneEnd, 0},
		{"tool[0].name.start", BlockToolName, 0, MilestoneStart, 0},
		{"tool[0].name[:3]", BlockToolName, 0, MilestonePrefix, 3},
		{"tool[0].name.end", BlockToolName, 0, MilestoneEnd, 0},
		{"tool[2].arguments[:8]", BlockToolArguments, 2, MilestonePrefix, 8},
		{"tool[0].arguments.end", BlockToolArguments, 0, MilestoneEnd, 0},
		{"generation.end", BlockGeneration, 0, MilestoneEnd, 0},
		{"stream.end", BlockStream, 0, MilestoneEnd, 0},
	}
	for _, tc := range cases {
		target, err := ParseTarget(tc.raw)
		if err != nil {
			t.Fatalf("ParseTarget(%q) failed: %v", tc.raw, err)
		}
		if target.Block != tc.block || target.ToolIndex != tc.toolIndex || target.Milestone != tc.milestone || target.N != tc.n {
			t.Fatalf("ParseTarget(%q) = %+v, want block=%v tool=%d milestone=%v n=%d",
				tc.raw, target, tc.block, tc.toolIndex, tc.milestone, tc.n)
		}
	}
}

func TestParseTargetRejectsInvalidForms(t *testing.T) {
	cases := []string{
		"",
		"payload",              // 缺后缀
		"content[1:2]",         // 不支持 [A:B]
		"content[-1]",          // 不支持负数
		"content[::2]",         // 不支持步长
		"content[:abc]",        // 非整数
		"tool[a].name",         // 索引非整数
		"tool[-1].arguments",   // 负索引
		"tool[0]",              // 缺里程碑
		"tool[0].id",           // 工具只支持 name/arguments
		"tool[0][:3]",          // tool 块不支持切片
		"generation.start",     // 生命周期块只有 end
		"stream[:5]",           // 生命周期块不支持切片
		"headers",              // headers 只有 end
		"headers.start",        // headers 只有 end
		"unknown.end",          // 未知块
		"content..end",         // 双点
		"payload.end.extra",    // 多余后缀
		"tool[0].name[:3].end", // 后缀混用
	}
	for _, raw := range cases {
		if _, err := ParseTarget(raw); err == nil {
			t.Fatalf("ParseTarget(%q) must fail", raw)
		}
	}
}

func TestParseTargetZeroSliceEqualsStart(t *testing.T) {
	// [:0] 与 .start 等价：都表示尚未发送第一个字符。
	prefix, err := ParseTarget("content[:0]")
	if err != nil {
		t.Fatalf("parse content[:0]: %v", err)
	}
	start, err := ParseTarget("content.start")
	if err != nil {
		t.Fatalf("parse content.start: %v", err)
	}
	if prefix.Block != start.Block || prefix.N != 0 || start.Milestone != MilestoneStart {
		t.Fatalf("[:0] and .start must point at the same logical position: %+v vs %+v", prefix, start)
	}
}
