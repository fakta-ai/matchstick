// Package provider declares what matchstick needs from a cloud: create, list and delete one pool's machines.
package provider

import (
	"context"
	"errors"
	"time"

	"github.com/fakta-ai/matchstick/internal/plan"
)

// ErrCapacity marks a creation the cloud refused for lack of room (quota, capacity). The pool retries later.
var ErrCapacity = errors.New("no capacity")

// Provider manages one pool's machines in one cloud. Implementations are safe for concurrent use.
type Provider interface {
	// Create starts a machine and returns once the cloud has accepted it; the machine may still be booting. An error
	// wrapping ErrCapacity means the cloud has no room now.
	Create(ctx context.Context, spec CreateSpec) error
	// List returns every machine of this pool, including ones half-created or being deleted.
	List(ctx context.Context) ([]plan.Machine, error)
	// Delete removes a machine and everything created with it. Deleting a machine that is already gone succeeds.
	Delete(ctx context.Context, name string) error
}

// CreateSpec is one machine to create.
type CreateSpec struct {
	Name string
	// JITConfig is the runner's single-use configuration from GitHub, handed to the machine's image.
	JITConfig string
	// ExpiresAt is recorded on the machine; the planner deletes the machine after it.
	ExpiresAt time.Time
}
