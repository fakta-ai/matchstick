// Command matchstick gives every GitHub Actions job a fresh cloud machine and deletes it afterwards.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/fakta-ai/matchstick/internal/cli"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	app := &cli.App{
		Name:       "matchstick",
		Summary:    "ephemeral GitHub Actions runners, one fresh cloud VM per job",
		Version:    version,
		Operations: operations(),
		Stdin:      os.Stdin,
		Stdout:     os.Stdout,
		Stderr:     os.Stderr,
	}
	code := app.Main(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}
