// Package provider declares what matchstick needs from a cloud: create, list and delete one pool's machines, and check
// a machine's proof of which machine it is.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/fakta-ai/matchstick/internal/plan"
)

// ErrCapacity marks a creation the cloud refused for lack of room (quota, capacity). The pool retries later.
var ErrCapacity = errors.New("no capacity")

// ErrNotProven marks a proof that does not show its sender is the machine it names.
var ErrNotProven = errors.New("the proof does not identify the machine")

// Provider manages one pool's machines in one cloud. Implementations are safe for concurrent use.
type Provider interface {
	// Create starts a machine and returns once the cloud has accepted it; the machine may still be booting. An error
	// wrapping ErrCapacity means the cloud has no room now.
	Create(ctx context.Context, spec CreateSpec) error
	// List returns every machine of this pool, including ones half-created or being deleted.
	List(ctx context.Context) ([]plan.Machine, error)
	// Delete removes a machine and everything created with it. Deleting a machine that is already gone succeeds.
	Delete(ctx context.Context, name string) error
	// Identify returns nil if proof, a document the cloud gives only to the machine itself, shows that its sender is
	// this pool's machine name. An error wrapping ErrNotProven means it does not; any other error means the check could
	// not be made now.
	Identify(ctx context.Context, name string, proof json.RawMessage) error
}

// CreateSpec is one machine to create.
type CreateSpec struct {
	Name string
	// Bootstrap is handed to the machine's image as it is (on Azure, as user data). Anyone who can read the machine
	// can read it, so it holds public information only.
	Bootstrap []byte
	// ExpiresAt is recorded on the machine; the planner deletes the machine after it.
	ExpiresAt time.Time
}
