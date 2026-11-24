package service

import (
	"path/filepath"
	"sort"
	"testing"

	"github.com/alexazhou/llm_mock_server/internal/model"
)

// 本文件锁定「示例剧本库整体可加载」：`go test` 就能发现任何一份示例被写坏。
// 示例库是文档的一部分（用户照抄），坏掉的示例比没有示例更糟。

// exampleRequiredScripts 是 E 阶段要求覆盖的最小剧本集合（按能力分组）。
var exampleRequiredScripts = []string{
	// 三协议单轮
	"chat-non-stream", "chat-stream",
	// 工具两轮
	"tool-two-round",
	// 计数与条件分支
	"retry-then-success", "fail-second", "tools-branch", "no-match",
	// 异常传输
	"hang-before-headers", "hang-after-headers", "eof-mid-output", "rst-mid-output",
	"hang-after-complete", "missing-done", "missing-message-stop",
	"slow-chunks", "heartbeat-interleaved",
	// 协议错误
	"http-400", "http-402", "http-503", "logical-failure-200", "stream-error",
	// raw
	"raw-malformed-sse", "raw-wrong-order",
	// usage / 多模态
	"usage-cache", "multimodal",
}

// TestExampleScriptsAllLoad 校验整个示例库能一次加载，且覆盖 E 要求的全部能力。
func TestExampleScriptsAllLoad(t *testing.T) {
	configPath := filepath.Join("..", "..", "examples", "server.yaml")
	loaded, err := Load(configPath)
	if err != nil {
		t.Fatalf("示例剧本库必须能通过加载与静态校验: %v", err)
	}
	present := map[string]bool{}
	for _, entry := range loaded.Registry.Entries() {
		present[entry.Script.ID] = true
	}
	var missing []string
	for _, id := range exampleRequiredScripts {
		if !present[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("示例库缺少必需剧本: %v", missing)
	}
	if len(present) < len(exampleRequiredScripts) {
		t.Fatalf("示例库剧本数 %d 少于必需集合 %d", len(present), len(exampleRequiredScripts))
	}
}

// TestExampleScriptsExposeThreeProtocolURLs 锁定「每个剧本固定生成三条标准 URL」。
func TestExampleScriptsExposeThreeProtocolURLs(t *testing.T) {
	loaded, err := Load(filepath.Join("..", "..", "examples", "server.yaml"))
	if err != nil {
		t.Fatalf("load examples: %v", err)
	}
	for _, entry := range loaded.Registry.Entries() {
		id := entry.Script.ID
		urls := model.URLsFor(id)
		want := map[string]string{
			"openai":    "/mock/" + id + "/v1/chat/completions",
			"anthropic": "/mock/" + id + "/v1/messages",
			"responses": "/mock/" + id + "/v1/responses",
		}
		got := map[string]string{"openai": urls.OpenAI, "anthropic": urls.Anthropic, "responses": urls.Responses}
		for protocol, expect := range want {
			if got[protocol] != expect {
				t.Fatalf("剧本 %s 的 %s URL = %q, want %q", id, protocol, got[protocol], expect)
			}
		}
	}
}
