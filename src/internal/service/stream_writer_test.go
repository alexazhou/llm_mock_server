package service

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/alexazhou/llm_mock_server/internal/constants"
)

// newPreparedForStream 构造一个流式快照，用于记录级单测。
func newPreparedForStream(protocol string, target streamPos, prepared *PreparedOutput) *PreparedOutput {
	prepared.Protocol = protocol
	prepared.Stream = true
	prepared.LineEnding = constants.LineEndingLF
	prepared.UsageMode = "auto"
	prepared.ResponseID = "resp_test"
	prepared.Created = 1
	prepared.Model = "m"
	prepared.Steps = []CompiledStep{{Op: OpSend, Source: SourceOutput}, {Op: OpEnd}}
	_ = target
	return prepared
}

// recordsText 把记录拼成文本，便于断言顺序与内容。
func recordsText(records [][]byte) string {
	var b strings.Builder
	for _, record := range records {
		b.Write(record)
	}
	return b.String()
}

// sseData 提取记录中的 data 负载。
func sseData(t *testing.T, records [][]byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, record := range records {
		text := string(record)
		for _, line := range strings.Split(text, "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			payload := strings.TrimPrefix(line, "data: ")
			if payload == "[DONE]" {
				out = append(out, map[string]any{"__done": true})
				continue
			}
			var parsed map[string]any
			if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
				t.Fatalf("invalid data payload %q: %v", payload, err)
			}
			out = append(out, parsed)
		}
	}
	return out
}

func TestOpenAIStreamDeltasAndDone(t *testing.T) {
	prepared := newPreparedForStream(constants.ProtocolOpenAI, streamPos{}, &PreparedOutput{
		Content: "Hello world!!", HasContent: true,
		Usage: &PreparedUsage{InputTokens: 3, OutputTokens: 5},
	})
	writer := newStreamWriter(prepared)
	records, err := writer.advanceTo(streamPos{stage: stageStreamEnd})
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	text := recordsText(records)
	if !strings.Contains(text, "data: [DONE]") {
		t.Fatalf("missing [DONE]: %s", text)
	}
	var contents []string
	finish := ""
	for _, event := range sseData(t, records) {
		choices, _ := event["choices"].([]any)
		for _, choice := range choices {
			fields := choice.(map[string]any)
			if delta, ok := fields["delta"].(map[string]any); ok {
				if content, ok := delta["content"].(string); ok {
					contents = append(contents, content)
				}
			}
			if reason, ok := fields["finish_reason"].(string); ok {
				finish = reason
			}
		}
	}
	if strings.Join(contents, "") != "Hello world!!" {
		t.Fatalf("delta 拼接 = %q", strings.Join(contents, ""))
	}
	if len(contents) != 3 || contents[0] != "Hello" || contents[1] != " worl" || contents[2] != "d!!" {
		t.Fatalf("13 字符必须切成 5/5/3，实际 %q", contents)
	}
	if finish != "stop" {
		t.Fatalf("finish_reason = %q", finish)
	}
}

func TestStreamPartialContentStopsBeforeFinish(t *testing.T) {
	prepared := newPreparedForStream(constants.ProtocolOpenAI, streamPos{}, &PreparedOutput{
		Content: "Hello world!!", HasContent: true,
	})
	writer := newStreamWriter(prepared)
	records, err := writer.advanceTo(streamPos{stage: stageContent, offset: 3})
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	text := recordsText(records)
	if strings.Contains(text, "finish_reason\":\"stop") || strings.Contains(text, "[DONE]") {
		t.Fatalf("partial content must not emit finish or [DONE]: %s", text)
	}
	var contents []string
	for _, event := range sseData(t, records) {
		for _, choice := range event["choices"].([]any) {
			delta, _ := choice.(map[string]any)["delta"].(map[string]any)
			if content, ok := delta["content"].(string); ok {
				contents = append(contents, content)
			}
		}
	}
	if strings.Join(contents, "") != "Hel" {
		t.Fatalf("content[:3] must send 3 chars, got %q", contents)
	}
}

func TestStreamPayloadCrossesBlocks(t *testing.T) {
	prepared := newPreparedForStream(constants.ProtocolOpenAI, streamPos{}, &PreparedOutput{
		Reasoning: "think", HasReasoning: true,
		Content: "Hello world!!", HasContent: true,
	})
	geometry := geometryOf(prepared)
	position, err := targetPosition(constants.ProtocolOpenAI, geometry,
		&Target{Block: BlockPayload, Milestone: MilestonePrefix, N: 6, Raw: "payload[:6]"})
	if err != nil {
		t.Fatalf("position: %v", err)
	}
	writer := newStreamWriter(prepared)
	records, err := writer.advanceTo(position)
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	var reasoning, content strings.Builder
	for _, event := range sseData(t, records) {
		for _, choice := range event["choices"].([]any) {
			delta, _ := choice.(map[string]any)["delta"].(map[string]any)
			if text, ok := delta["reasoning_content"].(string); ok {
				reasoning.WriteString(text)
			}
			if text, ok := delta["content"].(string); ok {
				content.WriteString(text)
			}
		}
	}
	// reasoning "think" 占 5 个 code point，payload[:6] = 5 + content 的第 1 个字符。
	if reasoning.String() != "think" || content.String() != "H" {
		t.Fatalf("payload[:6] must cover reasoning(5)+content(1), got reasoning=%q content=%q",
			reasoning.String(), content.String())
	}
}

func TestAnthropicStreamOrderAndSignature(t *testing.T) {
	prepared := newPreparedForStream(constants.ProtocolAnthropic, streamPos{}, &PreparedOutput{
		Reasoning: "think", HasReasoning: true,
		Content: "Hi", HasContent: true,
		Usage: &PreparedUsage{InputTokens: 3, OutputTokens: 5},
	})
	writer := newStreamWriter(prepared)
	records, err := writer.advanceTo(streamPos{stage: stageStreamEnd})
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	var events []string
	for _, record := range records {
		for _, line := range strings.Split(string(record), "\n") {
			if strings.HasPrefix(line, "event: ") {
				events = append(events, strings.TrimPrefix(line, "event: "))
			}
		}
	}
	want := []string{"message_start", "content_block_start", "content_block_delta",
		"content_block_delta", "content_block_stop", "content_block_start", "content_block_delta",
		"content_block_stop", "message_delta", "message_stop"}
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Fatalf("event order = %v want %v", events, want)
	}
	if !strings.Contains(recordsText(records), anthropicThinkingSignature) {
		t.Fatal("thinking block must carry the deterministic signature")
	}
}

func TestResponsesStreamRecords(t *testing.T) {
	prepared := newPreparedForStream(constants.ProtocolResponses, streamPos{}, &PreparedOutput{
		Content: "Hi", HasContent: true,
	})
	writer := newStreamWriter(prepared)
	records, err := writer.advanceTo(streamPos{stage: stageStreamEnd})
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	var events []string
	for _, record := range records {
		for _, line := range strings.Split(string(record), "\n") {
			if strings.HasPrefix(line, "event: ") {
				events = append(events, strings.TrimPrefix(line, "event: "))
			}
		}
	}
	want := []string{"response.created", "response.output_item.added", "response.output_text.delta",
		"response.output_text.done", "response.output_item.done", "response.completed"}
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Fatalf("event order = %v want %v", events, want)
	}
	if strings.Contains(recordsText(records), "[DONE]") {
		t.Fatal("responses must not send [DONE]")
	}
}

func TestHeartbeatBytesShape(t *testing.T) {
	step := &CompiledStep{Source: SourceHeartbeat, Text: "ping", Count: 2}
	got := string(heartbeatBytes(step, "\r\n"))
	if got != ": ping\r\n: ping\r\n" {
		t.Fatalf("heartbeat = %q", got)
	}
	empty := string(heartbeatBytes(&CompiledStep{Source: SourceHeartbeat, Count: 1}, "\r\n"))
	if empty != ": \r\n" {
		t.Fatalf("empty heartbeat = %q", empty)
	}
}

func TestStreamTargetMustMoveForward(t *testing.T) {
	prepared := &PreparedOutput{
		Protocol: constants.ProtocolOpenAI, Stream: true, Content: "Hello", HasContent: true,
		Steps: []CompiledStep{
			{Op: OpSend, Source: SourceOutput, Target: &Target{Block: BlockContent, Milestone: MilestonePrefix, N: 3}},
			{Op: OpSend, Source: SourceOutput, Target: &Target{Block: BlockContent, Milestone: MilestonePrefix, N: 2}},
			{Op: OpEnd},
		},
	}
	if ferr := validateStreamTargets(prepared); ferr == nil {
		t.Fatal("backward target must be rejected")
	}
}
