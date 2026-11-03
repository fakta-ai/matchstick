package adapter

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fakta-ai/matchstick/internal/op"
)

// MCPServer returns an MCP server offering the operations as tools. Each call runs matchstick; its result is the
// tool's structured content, and its error object the tool's error.
func MCPServer(version string, tools []op.Tool, run Exec) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "matchstick", Version: version}, nil)
	for _, t := range tools {
		name := t.Name
		tool := &mcp.Tool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
			Annotations: &mcp.ToolAnnotations{
				ReadOnlyHint:    t.Annotations.ReadOnly,
				DestructiveHint: new(t.Annotations.Destructive),
				IdempotentHint:  t.Annotations.Idempotent,
				OpenWorldHint:   new(t.Annotations.OpenWorld),
			},
		}
		if t.OutputSchema != nil {
			tool.OutputSchema = t.OutputSchema
		}
		server.AddTool(tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			params, err := withoutMeta(req.Params.Arguments)
			if err != nil {
				return errorResult(op.Errorf(op.CodeInvalidInput, "arguments must be an object")), nil
			}
			out, e := Call(ctx, run, name, params)
			if e != nil {
				return errorResult(e), nil
			}
			return &mcp.CallToolResult{
				Content:           []mcp.Content{&mcp.TextContent{Text: string(out)}},
				StructuredContent: out,
			}, nil
		})
	}
	return server
}

func errorResult(e *op.Error) *mcp.CallToolResult {
	b, _ := json.Marshal(map[string]*op.Error{"error": e})
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}
}
