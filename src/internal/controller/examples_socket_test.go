package controller

import (
	"errors"
	"net"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/alexazhou/llm_mock_server/internal/service"
)

// 本文件用真实 socket 验证示例剧本库里的传输异常场景。全部读原始字节，
// 不用 http.Client：只有裸 TCP 才能区分「完整流 / 缺结束标记 / EOF / RST / 挂起」。

// chunkTerminator 是 chunked 传输的零长度终止块：出现它表示响应体正常结束。
const chunkTerminator = "0\r\n\r\n"

// exampleSocketServer 加载仓库内的示例剧本库并起真实监听。
func exampleSocketServer(t *testing.T) string {
	t.Helper()
	loaded, err := service.Load("../../examples/server.yaml")
	if err != nil {
		t.Fatalf("load examples: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &http.Server{Handler: NewRouter(loaded)}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return listener.Addr().String()
}

// TestExampleHangBeforeHeaders 头前挂起：连接建立、请求已发出，但一个字节都不返回。
func TestExampleHangBeforeHeaders(t *testing.T) {
	address := exampleSocketServer(t)
	conn, _ := dialStream(t, address, "/mock/hang-before-headers/v1/chat/completions")
	defer conn.Close()
	payload, _ := readFor(t, conn, 800*time.Millisecond)
	if payload != "" {
		t.Fatalf("hang-before-headers must not send any byte, got %q", payload)
	}
}

// TestExampleHangAfterHeaders 头后 body 挂起：拿到 200 与头，但 body 永不出现。
func TestExampleHangAfterHeaders(t *testing.T) {
	address := exampleSocketServer(t)
	conn, _ := dialStream(t, address, "/mock/hang-after-headers/v1/chat/completions")
	defer conn.Close()
	payload, _ := readFor(t, conn, 800*time.Millisecond)
	if !strings.Contains(payload, "200 OK") {
		t.Fatalf("must commit 200 headers before hanging, got %q", payload)
	}
	if strings.Contains(payload, "data:") {
		t.Fatalf("hang-after-headers must not send body before wait, got %q", payload)
	}
}

// TestExampleEOFMidOutput 部分输出后 EOF：有 data 但没有结束标记，连接正常关闭。
func TestExampleEOFMidOutput(t *testing.T) {
	address := exampleSocketServer(t)
	conn, _ := dialStream(t, address, "/mock/eof-mid-output/v1/chat/completions")
	defer conn.Close()
	payload, err := readFor(t, conn, 1200*time.Millisecond)
	if !strings.Contains(payload, "data:") {
		t.Fatalf("must send partial records: %q", payload)
	}
	if strings.Contains(payload, "[DONE]") {
		t.Fatalf("eof-mid-output must not send [DONE]: %q", payload)
	}
	// HTTP/1.1 keep-alive 下正常结束不会 EOF：响应结束的标志是 chunked 终止块。
	if !strings.Contains(payload, chunkTerminator) {
		t.Fatalf("partial output must end the response cleanly (chunked terminator): %q", payload)
	}
	_ = err
}

// TestExampleRSTMidOutput 部分输出后 RST：客户端读到 ECONNRESET 而不是 EOF。
func TestExampleRSTMidOutput(t *testing.T) {
	address := exampleSocketServer(t)
	conn, _ := dialStream(t, address, "/mock/rst-mid-output/v1/chat/completions")
	defer conn.Close()
	payload, err := readFor(t, conn, 1500*time.Millisecond)
	if !strings.Contains(payload, "data:") {
		t.Fatalf("must send partial records before reset: %q", payload)
	}
	if !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("abort must surface as ECONNRESET, got %v", err)
	}
}

// TestExampleHangAfterComplete 完整输出后挂起：协议完整但没有 EOF。
func TestExampleHangAfterComplete(t *testing.T) {
	address := exampleSocketServer(t)
	conn, _ := dialStream(t, address, "/mock/hang-after-complete/v1/chat/completions")
	defer conn.Close()
	payload, err := readFor(t, conn, 1200*time.Millisecond)
	if !strings.Contains(payload, "[DONE]") {
		t.Fatalf("complete protocol must include [DONE]: %q", payload)
	}
	if strings.Contains(payload, chunkTerminator) {
		t.Fatalf("hang-after-complete must keep the response open: %q", payload)
	}
	_ = err
}

// TestExampleMissingDone 缺 [DONE]：流正常结束但没有结束标记。
func TestExampleMissingDone(t *testing.T) {
	address := exampleSocketServer(t)
	conn, _ := dialStream(t, address, "/mock/missing-done/v1/chat/completions")
	defer conn.Close()
	payload, err := readFor(t, conn, 1200*time.Millisecond)
	if !strings.Contains(payload, "data:") {
		t.Fatalf("must send records: %q", payload)
	}
	if strings.Contains(payload, "[DONE]") {
		t.Fatalf("missing-done must not send [DONE]: %q", payload)
	}
	if !strings.Contains(payload, chunkTerminator) {
		t.Fatalf("missing-done should end the response cleanly: %q", payload)
	}
	_ = err
}

// TestExampleMissingMessageStop Anthropic 缺 message_stop。
func TestExampleMissingMessageStop(t *testing.T) {
	address := exampleSocketServer(t)
	conn, _ := dialStream(t, address, "/mock/missing-message-stop/v1/messages")
	defer conn.Close()
	payload, err := readFor(t, conn, 1200*time.Millisecond)
	if !strings.Contains(payload, "data:") {
		t.Fatalf("must send anthropic records: %q", payload)
	}
	if strings.Contains(payload, "message_stop") {
		t.Fatalf("missing-message-stop must not send message_stop: %q", payload)
	}
	if !strings.Contains(payload, chunkTerminator) {
		t.Fatalf("missing-message-stop should end the response cleanly: %q", payload)
	}
	_ = err
}

// TestExampleSlowChunks 慢速分片：后续字节片要等 interval 才到。
func TestExampleSlowChunks(t *testing.T) {
	address := exampleSocketServer(t)
	conn, _ := dialStream(t, address, "/mock/slow-chunks/v1/chat/completions")
	defer conn.Close()

	early, _ := readFor(t, conn, 100*time.Millisecond)
	later, _ := readFor(t, conn, 600*time.Millisecond)
	if len(later) == 0 {
		t.Fatalf("slow-chunks should deliver more bytes after the interval (early=%d)", len(early))
	}
	// 100ms 内不应把整份响应发完：若没有 interval，第一窗口就会拿到全部。
	if len(early) > 0 && len(later) == 0 {
		t.Fatalf("no bytes after the interval; interval may be ignored")
	}
}

// TestExampleHeartbeatInterleaved heartbeat 与正文交错。
func TestExampleHeartbeatInterleaved(t *testing.T) {
	address := exampleSocketServer(t)
	conn, _ := dialStream(t, address, "/mock/heartbeat-interleaved/v1/chat/completions")
	defer conn.Close()
	payload, _ := readFor(t, conn, 1200*time.Millisecond)
	if !strings.Contains(payload, ": ping") {
		t.Fatalf("heartbeat must appear as SSE comment, got %q", payload)
	}
	if !strings.Contains(payload, "data:") {
		t.Fatalf("interleaved script must also send content, got %q", payload)
	}
}

// TestExampleStreamError 流式 error 事件。
func TestExampleStreamError(t *testing.T) {
	address := exampleSocketServer(t)
	conn, _ := dialStream(t, address, "/mock/stream-error/v1/chat/completions")
	defer conn.Close()
	payload, _ := readFor(t, conn, 1200*time.Millisecond)
	if !strings.Contains(payload, "interrupted by the upstream") {
		t.Fatalf("stream-error must carry the configured error message, got %q", payload)
	}
}
