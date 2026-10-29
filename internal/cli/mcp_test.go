package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMain lets a test run the CLI in a child process: the test binary
// re-executes itself with TWINSTUB_TEST_EXEC set and acts as twinstub.
func TestMain(m *testing.M) {
	if os.Getenv("TWINSTUB_TEST_EXEC") == "1" {
		var args []string
		_ = json.Unmarshal([]byte(os.Getenv("TWINSTUB_TEST_ARGS")), &args)
		os.Args = append([]string{"twinstub"}, args...)
		Execute()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// TestMCPStdoutCarriesOnlyProtocol runs `twinstub mcp` with an embedded
// engine, makes the engine log, and checks that every stdout line is a
// JSON-RPC message while the logs land on stderr.
func TestMCPStdoutCarriesOnlyProtocol(t *testing.T) {
	if conn, err := net.DialTimeout("tcp", "localhost:9090", 200*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Skip("an engine runs on localhost:9090, so twinstub mcp would use it instead of embedding one")
	}
	port := freePort(t)
	cfg := filepath.Join(t.TempDir(), "twinstub.yaml")
	if err := os.WriteFile(cfg, []byte(fmt.Sprintf(`version: 1
server:
  port: %d
endpoints:
  - id: ping
    when: {method: GET, path: /ping}
    reply: {status: 200, body: '{"pong": true}'}
`, port)), 0o600); err != nil {
		t.Fatal(err)
	}

	args, _ := json.Marshal([]string{"mcp", "-c", cfg, "--log-format", "json"})
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "TWINSTUB_TEST_EXEC=1", "TWINSTUB_TEST_ARGS="+string(args))
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	lines := bufio.NewScanner(stdout)
	lines.Buffer(make([]byte, 1<<20), 1<<20)
	var raw []string
	send := func(msg string) { _, _ = io.WriteString(stdin, msg+"\n") }
	recv := func() map[string]any {
		t.Helper()
		if !lines.Scan() {
			t.Fatalf("stdout closed; stderr:\n%s", stderr.String())
		}
		raw = append(raw, lines.Text())
		m := map[string]any{}
		if err := json.Unmarshal(lines.Bytes(), &m); err != nil || m["jsonrpc"] != "2.0" {
			t.Fatalf("stdout line is not JSON-RPC: %q", lines.Text())
		}
		return m
	}

	send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`)
	recv()
	send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"twinstub_status","arguments":{}}}`)
	status := recv()
	text := resultText(status)
	if !strings.Contains(text, `"mode":"embedded"`) || !strings.Contains(text, fmt.Sprintf(`"public_port":%d`, port)) {
		t.Fatalf("status = %s", text)
	}

	// A hit and a miss on the public port make the engine log.
	for _, path := range []string{"/ping", "/nothing-here"} {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d%s", port, path))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"twinstub_explain_miss","arguments":{}}}`)
	if miss := resultText(recv()); !strings.Contains(miss, "/nothing-here") {
		t.Errorf("explain_miss = %s", miss)
	}

	_ = stdin.Close()
	for lines.Scan() {
		raw = append(raw, lines.Text())
		t.Errorf("unexpected stdout after the last answer: %q", lines.Text())
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("exit: %v; stderr:\n%s", err, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("twinstub mcp did not exit after stdin closed")
	}

	logs := stderr.String()
	for _, want := range []string{"embedded engine started", "request (no match)"} {
		if !strings.Contains(logs, want) {
			t.Errorf("stderr lacks %q:\n%s", want, logs)
		}
	}
	if len(raw) != 3 {
		t.Errorf("stdout had %d lines, want the 3 answers", len(raw))
	}
}

// resultText is the text of a tools/call answer.
func resultText(m map[string]any) string {
	res, _ := m["result"].(map[string]any)
	content, _ := res["content"].([]any)
	if len(content) == 0 {
		return fmt.Sprint(m)
	}
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	return text
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port
}
