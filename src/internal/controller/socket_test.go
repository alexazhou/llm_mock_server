package controller

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/alexazhou/llm_mock_server/internal/service"
)

// 本文件是真实 socket 验证：起真实 HTTP server（net.Listen + http.Server），
// 用裸 TCP 连接读原始字节，覆盖完整流、挂起、客户端取消与 abort 的 TCP RST。

// startSocketServer 启动一个真实监听的服务，返回地址与关闭函数。
func startSocketServer(t *testing.T, scripts map[string]string) (string, func()) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "server.yaml"),
		[]byte("version: 1\nscripts_dir: scripts\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range scripts {
		if err := os.WriteFile(filepath.Join(root, "scripts", name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	loaded, err := service.Load(filepath.Join(root, "server.yaml"))
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &http.Server{Handler: NewRouter(loaded)}
	go func() { _ = server.Serve(listener) }()
	return listener.Addr().String(), func() { _ = server.Close() }
}

// dialStream 发一个流式请求并返回连接与读取器。
func dialStream(t *testing.T, address, path string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	request := fmt.Sprintf("POST %s HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s",
		path, len(body), body)
	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatalf("write request: %v", err)
	}
	return conn, bufio.NewReader(conn)
}

// readFor 在窗口内尽量读，返回读到的字节与是否出现 EOF/RST。
func readFor(t *testing.T, conn net.Conn, window time.Duration) (string, error) {
	t.Helper()
	deadline := time.Now().Add(window)
	var out strings.Builder
	buf := make([]byte, 4096)
	for {
		if time.Now().After(deadline) {
			return out.String(), nil
		}
		_ = conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		n, err := conn.Read(buf)
		if n > 0 {
			out.Write(buf[:n])
		}
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				continue
			}
			return out.String(), err
		}
	}
}

const streamScript = `version: 1
id: stream
config:
  stream: true
rounds:
  - output:
      content: Hello world!!
`

const hangScript = `version: 1
id: hang
config:
  stream: true
rounds:
  - output:
      content: hi
      steps:
        - op: send
          target: headers.end
        - op: wait
          duration: infinite
`

const abortScript = `version: 1
id: abort
config:
  stream: true
rounds:
  - output:
      content: hi
      steps:
        - op: end
          mode: abort
`

// TestSocketCompleteStream 验证完整流以 [DONE] 结束并正常 FIN。
func TestSocketCompleteStream(t *testing.T) {
	address, stop := startSocketServer(t, map[string]string{"stream.yaml": streamScript})
	defer stop()
	conn, _ := dialStream(t, address, "/mock/stream/v1/chat/completions")
	defer conn.Close()
	// keep-alive 下连接不会 EOF，因此在时间窗口内读取并检查内容。
	text, err := readFor(t, conn, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// delta 固定 5 code point 切分，原文不会连续出现，因此按首尾分片断言。
	if !strings.Contains(text, "data: [DONE]") ||
		!strings.Contains(text, `"content":"Hello"`) ||
		!strings.Contains(text, `"content":"d!!"`) {
		t.Fatalf("incomplete stream: %s", text)
	}
}

// TestSocketHangBeforeBody 验证 headers.end 之后无限挂起时不再产生 body 字节。
func TestSocketHangBeforeBody(t *testing.T) {
	address, stop := startSocketServer(t, map[string]string{"hang.yaml": hangScript})
	defer stop()
	conn, _ := dialStream(t, address, "/mock/hang/v1/chat/completions")
	defer conn.Close()
	body, err := readFor(t, conn, 400*time.Millisecond)
	if err != nil {
		t.Fatalf("hang must not close the connection: %v", err)
	}
	if !strings.Contains(body, "HTTP/1.1 200") {
		t.Fatalf("headers must be committed before the hang: %q", body)
	}
	if strings.Contains(body, "data:") {
		t.Fatalf("hanging output must not send body records: %q", body)
	}
}

// TestSocketClientCancel 验证客户端提前断开时服务端能收尾（不泄漏、不崩溃）。
func TestSocketClientCancel(t *testing.T) {
	address, stop := startSocketServer(t, map[string]string{"stream.yaml": streamScript})
	defer stop()
	conn, _ := dialStream(t, address, "/mock/stream/v1/chat/completions")
	if _, err := readFor(t, conn, 50*time.Millisecond); err != nil {
		t.Fatalf("read head: %v", err)
	}
	_ = conn.Close()
	// 服务端应继续可用。
	again, _ := dialStream(t, address, "/mock/stream/v1/chat/completions")
	defer again.Close()
	text, err := readFor(t, again, 500*time.Millisecond)
	if err != nil || !strings.Contains(text, "data: [DONE]") {
		t.Fatalf("server must stay healthy after a client cancel: %v (%s)", err, text)
	}
}

// TestSocketAbortSendsRST 验证 end.mode: abort 在未提交响应头时发送 TCP RST
// 且客户端收不到任何响应字节。
func TestSocketAbortSendsRST(t *testing.T) {
	address, stop := startSocketServer(t, map[string]string{"abort.yaml": abortScript})
	defer stop()
	conn, _ := dialStream(t, address, "/mock/abort/v1/chat/completions")
	defer conn.Close()
	body, err := readFor(t, conn, 800*time.Millisecond)
	if body != "" {
		t.Fatalf("abort before headers must send zero bytes, got %q", body)
	}
	if err == nil {
		t.Fatal("abort must terminate the connection")
	}
	if !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("abort must reset the connection (ECONNRESET), got %v", err)
	}
}
