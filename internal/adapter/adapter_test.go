package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fakta-ai/matchstick/internal/op"
)

// fakeMatchstick answers like the command line: "echo" returns its input as {"got": input}; "fail" fails with an
// error object; "slow" waits for cancellation.
type fakeMatchstick struct {
	mu    sync.Mutex
	calls [][]string
	input []string
}

func (f *fakeMatchstick) exec(ctx context.Context, args []string, stdin []byte) ([]byte, []byte, int, error) {
	f.mu.Lock()
	f.calls = append(f.calls, args)
	f.input = append(f.input, string(stdin))
	f.mu.Unlock()
	switch args[0] {
	case "--help":
		return []byte(`{"tools":[{"name":"echo","description":"Echoes.","inputSchema":{"type":"object"},` +
			`"outputSchema":{"type":"object"},"annotations":{"readOnlyHint":true,"destructiveHint":false,` +
			`"idempotentHint":true,"openWorldHint":false}},{"name":"fail","inputSchema":{"type":"object"}},` +
			`{"name":"slow","inputSchema":{"type":"object"}}]}`), nil, 0, nil
	case "echo":
		return []byte(`{"got":` + string(stdin) + `}`), nil, 0, nil
	case "fail":
		return nil, []byte(`{"error":{"code":"invalid_input","message":"pool is required"}}`), 2, nil
	case "slow":
		<-ctx.Done()
		return nil, []byte(`{"error":{"code":"internal","message":"killed"}}`), 1, nil
	}
	return nil, nil, 0, nil
}

func rpcSession(t *testing.T, f *fakeMatchstick, lines ...string) []map[string]any {
	t.Helper()
	tools, err := Tools(context.Background(), f.exec)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := ServeRPC(context.Background(), strings.NewReader(strings.Join(lines, "\n")+"\n"), &out, tools,
		f.exec); err != nil {
		t.Fatal(err)
	}
	var responses []map[string]any
	for line := range strings.Lines(out.String()) {
		var r map[string]any
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("response %q: %v", line, err)
		}
		responses = append(responses, r)
	}
	return responses
}

func TestRPC(t *testing.T) {
	f := &fakeMatchstick{}
	got := rpcSession(t, f,
		`{"jsonrpc":"2.0","id":1,"method":"echo","params":{"pool":"build","_meta":{"progressToken":"x"}}}`,
		`not json`,
		`{"jsonrpc":"2.0","id":2,"method":"fail","params":{}}`,
		`{"jsonrpc":"2.0","id":3,"method":"nope"}`,
		`{"jsonrpc":"2.0","method":"echo","params":{}}`,
	)
	byID := map[string]map[string]any{}
	for _, r := range got {
		id, _ := json.Marshal(r["id"])
		byID[string(id)] = r
	}
	if len(got) != 4 {
		t.Fatalf("responses %v: want 4 (the notification gets none)", got)
	}
	if res, _ := json.Marshal(byID["1"]["result"]); string(res) != `{"got":{"pool":"build"}}` {
		t.Errorf("echo result %s: want the input without _meta", res)
	}
	if e := byID["null"]["error"].(map[string]any); e["code"].(float64) != rpcParseError {
		t.Errorf("parse error %v", e)
	}
	e := byID["2"]["error"].(map[string]any)
	if e["code"].(float64) != rpcInvalidParams || e["data"].(map[string]any)["code"] != op.CodeInvalidInput {
		t.Errorf("fail error %v", e)
	}
	if e := byID["3"]["error"].(map[string]any); e["code"].(float64) != rpcMethodNotFound {
		t.Errorf("unknown method %v", e)
	}
	for _, c := range f.calls {
		if c[0] == "echo" && strings.Join(c, " ") != "echo --json --input -" {
			t.Errorf("ran %v", c)
		}
	}
}

func TestRPCCancellation(t *testing.T) {
	f := &fakeMatchstick{}
	tools, _ := Tools(context.Background(), f.exec)
	r, w := ioPipe()
	var out syncBuffer
	done := make(chan error)
	go func() { done <- ServeRPC(context.Background(), r, &out, tools, f.exec) }()
	_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":7,"method":"slow","params":{}}` + "\n"))
	time.Sleep(50 * time.Millisecond)
	_, _ = w.Write([]byte(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":7}}` + "\n"))
	_ = w.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled request did not stop")
	}
	if !strings.Contains(out.String(), `"id":7`) {
		t.Errorf("no response for the cancelled request: %s", out.String())
	}
}

func TestRPCStopsOnCancelWithInputOpen(t *testing.T) {
	f := &fakeMatchstick{}
	tools, _ := Tools(context.Background(), f.exec)
	r, w := ioPipe()
	defer func() { _ = w.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	var out syncBuffer
	done := make(chan error)
	go func() { done <- ServeRPC(ctx, r, &out, tools, f.exec) }()
	_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"slow","params":{}}` + "\n"))
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("ServeRPC returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ServeRPC did not return after cancellation")
	}
	if !strings.Contains(out.String(), `"id":1`) {
		t.Errorf("the call in flight got no answer: %s", out.String())
	}
}

func TestMCP(t *testing.T) {
	f := &fakeMatchstick{}
	tools, _ := Tools(context.Background(), f.exec)
	server := MCPServer("test", tools, f.exec)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx := context.Background()
	go func() { _ = server.Run(ctx, serverTransport) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	list, err := session.ListTools(ctx, nil)
	if err != nil || len(list.Tools) != 3 || !list.Tools[0].Annotations.ReadOnlyHint {
		t.Fatalf("tools %+v %v", list, err)
	}
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"pool": "build"}})
	if err != nil || res.IsError {
		t.Fatalf("echo: %+v %v", res, err)
	}
	if b, _ := json.Marshal(res.StructuredContent); string(b) != `{"got":{"pool":"build"}}` {
		t.Errorf("structured content %s", b)
	}
	res, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "fail", Arguments: map[string]any{}})
	if err != nil || !res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, op.CodeInvalidInput) {
		t.Errorf("fail: %+v %v", res, err)
	}
}

// TestAgainstTheRealCommandLine builds matchstick and serves its decide operation over JSON-RPC.
func TestAgainstTheRealCommandLine(t *testing.T) {
	if testing.Short() {
		t.Skip("builds matchstick")
	}
	bin := filepath.Join(t.TempDir(), "matchstick")
	build := exec.Command("go", "build", "-o", bin, "../../cmd/matchstick")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	run := Command(bin)
	tools, err := Tools(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	req := `{"jsonrpc":"2.0","id":1,"method":"decide","params":{"observation":{"now":"2026-11-03T12:00:00Z",` +
		`"machines":[],"runners":[],"demand":2},"limits":{"maximum":1,"spares":0,"registrationTimeout":"10m"}}}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"decide","params":{}}` + "\n"
	if err := ServeRPC(context.Background(), strings.NewReader(req), &out, tools, run); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"create":1`) || !strings.Contains(out.String(), `"code":"invalid_input"`) {
		t.Errorf("responses:\n%s", out.String())
	}
	_ = os.Remove(bin)
}

type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func ioPipe() (*os.File, *os.File) {
	r, w, err := os.Pipe()
	if err != nil {
		panic(err)
	}
	return r, w
}
