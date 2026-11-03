// Command matchstick-adapter serves matchstick's operations over JSON-RPC (rpc) or MCP (mcp) on standard input and
// output. It holds no secrets: for each call it runs the matchstick command line, which reads its own.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fakta-ai/matchstick/internal/adapter"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `matchstick-adapter: matchstick's operations over JSON-RPC or MCP, on standard input and output

Usage:
  matchstick-adapter rpc [--matchstick <path>]   JSON-RPC 2.0, one message per line
  matchstick-adapter mcp [--matchstick <path>]   MCP over stdio

Flags:
  --matchstick <path>   the matchstick executable to run (default: matchstick on PATH)
  -h, --help            this help
  --version             print the version

Example:
  echo '{"jsonrpc":"2.0","id":1,"method":"status","params":{"config":"matchstick.yaml"}}' | matchstick-adapter rpc
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	switch args[0] {
	case "-h", "--help":
		fmt.Print(usage)
		return 0
	case "--version":
		fmt.Println("matchstick-adapter", version)
		return 0
	case "rpc", "mcp":
	default:
		fmt.Fprintf(os.Stderr, "matchstick-adapter: unknown mode %q\n\n%s", args[0], usage)
		return 2
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	path := flags.String("matchstick", "matchstick", "the matchstick executable to run")
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	exec := adapter.Command(*path)
	tools, err := adapter.Tools(ctx, exec)
	if err != nil {
		fmt.Fprintln(os.Stderr, "matchstick-adapter:", err)
		return 1
	}
	if args[0] == "rpc" {
		err = adapter.ServeRPC(ctx, os.Stdin, os.Stdout, tools, exec)
	} else {
		err = adapter.MCPServer(version, tools, exec).Run(ctx, &mcp.StdioTransport{})
	}
	if err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "matchstick-adapter:", err)
		return 1
	}
	return 0
}
