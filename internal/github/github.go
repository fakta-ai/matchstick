// Package github is matchstick's side of GitHub: one pool's runner scale set, its runner registrations, and the
// listener that reports its demand.
package github

import (
	"context"
	"errors"

	"github.com/fakta-ai/matchstick/internal/plan"
)

// ErrJobRunning is returned when GitHub refuses to remove a registration because its runner is running a job.
var ErrJobRunning = errors.New("the runner is running a job")

// Runners is what a pool needs from GitHub. Implementations are safe for concurrent use.
type Runners interface {
	// JITConfig registers a runner under name and returns its single-use configuration.
	JITConfig(ctx context.Context, name string) (JIT, error)
	// List returns the pool's registrations: those whose names the pool owns.
	List(ctx context.Context) ([]plan.Runner, error)
	// Remove removes a registration. Removing one that is already gone succeeds; a runner running a job gives
	// ErrJobRunning.
	Remove(ctx context.Context, id int64) error
}

// JIT is a registered runner's single-use configuration.
type JIT struct {
	RunnerID int64
	Config   string
}
