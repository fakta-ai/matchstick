// Package adapter serves matchstick's operations over JSON-RPC and MCP by running the matchstick command line for
// each call. It holds no secrets: the matchstick it runs reads its own.
package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"

	"github.com/fakta-ai/matchstick/internal/op"
)

// Exec runs the matchstick command line with args and stdin, and returns its stdout, stderr and exit code. An error
// means it could not be run at all.
type Exec func(ctx context.Context, args []string, stdin []byte) (stdout, stderr []byte, code int, err error)

// Command returns an Exec that runs the executable at path.
func Command(path string) Exec {
	return func(ctx context.Context, args []string, stdin []byte) ([]byte, []byte, int, error) {
		cmd := exec.CommandContext(ctx, path, args...)
		cmd.Stdin = bytes.NewReader(stdin)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return stdout.Bytes(), stderr.Bytes(), exit.ExitCode(), nil
		}
		if err != nil {
			return nil, nil, 0, err
		}
		return stdout.Bytes(), stderr.Bytes(), 0, nil
	}
}

// Tools reads the operations as MCP tool definitions from `matchstick --help --json`.
func Tools(ctx context.Context, run Exec) ([]op.Tool, error) {
	stdout, stderr, code, err := run(ctx, []string{"--help", "--json"}, nil)
	if err != nil {
		return nil, fmt.Errorf("running matchstick: %w", err)
	}
	if code != 0 {
		return nil, fmt.Errorf("matchstick --help --json: exit %d: %s", code, stderr)
	}
	var out struct {
		Tools []op.Tool `json:"tools"`
	}
	if err := json.Unmarshal(stdout, &out); err != nil {
		return nil, fmt.Errorf("matchstick --help --json: %w", err)
	}
	return out.Tools, nil
}

// Call runs one operation, named by its tool name, with params as its input and returns its output. When the call
// fails but printed a JSON result, such as a failed check's report, that result comes back with the error.
func Call(ctx context.Context, run Exec, name string, params json.RawMessage) (json.RawMessage, *op.Error) {
	if len(bytes.TrimSpace(params)) == 0 || string(bytes.TrimSpace(params)) == "null" {
		params = json.RawMessage("{}")
	}
	args := append(op.Words(name), "--json", "--input", "-")
	stdout, stderr, code, err := run(ctx, args, params)
	if err != nil {
		return nil, op.Errorf(op.CodeInternal, "running matchstick: %v", err)
	}
	if code != 0 {
		var e struct {
			Error *op.Error `json:"error"`
		}
		if json.Unmarshal(stderr, &e) != nil || e.Error == nil {
			return nil, op.Errorf(op.CodeInternal, "matchstick %s: exit %d: %s", name, code, bytes.TrimSpace(stderr))
		}
		if result := bytes.TrimSpace(stdout); len(result) > 0 && json.Valid(result) {
			return result, e.Error
		}
		return nil, e.Error
	}
	if !json.Valid(stdout) {
		return nil, op.Errorf(op.CodeInternal, "matchstick %s: output is not JSON", name)
	}
	return bytes.TrimSpace(stdout), nil
}

// withoutMeta removes MCP's and JSON-RPC's protocol metadata, _meta, from params.
func withoutMeta(params json.RawMessage) (json.RawMessage, error) {
	if len(bytes.TrimSpace(params)) == 0 {
		return params, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(params, &fields); err != nil {
		return nil, err
	}
	if _, ok := fields["_meta"]; !ok {
		return params, nil
	}
	delete(fields, "_meta")
	return json.Marshal(fields)
}
