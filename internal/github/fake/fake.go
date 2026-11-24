// Package fake is the GitHub side of a pool in memory, for tests and for trying the planner without GitHub.
package fake

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/fakta-ai/matchstick/internal/github"
	"github.com/fakta-ai/matchstick/internal/plan"
)

// Runners keeps registrations in memory. A registration starts offline; tests bring it online with SetRunner.
type Runners struct {
	mu       sync.Mutex
	now      func() time.Time
	nextID   int64
	runners  map[int64]plan.Runner
	failures map[string][]error
	// listed is the status List shows instead of a runner's own, by name (Misreport).
	listed map[string]status
}

type status struct{ online, busy bool }

var _ github.Runners = (*Runners)(nil)

// New returns registrations holding the given runners. now stamps new registrations.
func New(now func() time.Time, runners ...plan.Runner) *Runners {
	r := &Runners{now: now, nextID: 1, runners: map[int64]plan.Runner{}, failures: map[string][]error{},
		listed: map[string]status{}}
	for _, runner := range runners {
		r.runners[runner.ID] = runner
		r.nextID = max(r.nextID, runner.ID+1)
	}
	return r
}

// Fail makes the next calls of op ("jit", "list" or "remove") fail with the given errors, one call each.
func (r *Runners) Fail(op string, errs ...error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures[op] = append(r.failures[op], errs...)
}

func (r *Runners) failure(op string) error {
	queue := r.failures[op]
	if len(queue) == 0 {
		return nil
	}
	r.failures[op] = queue[1:]
	return queue[0]
}

// JITConfig adds an offline registration.
func (r *Runners) JITConfig(_ context.Context, name string) (github.JIT, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.failure("jit"); err != nil {
		return github.JIT{}, err
	}
	id := r.nextID
	r.nextID++
	r.runners[id] = plan.Runner{ID: id, Name: name, CreatedAt: r.now()}
	return github.JIT{RunnerID: id, Config: "jit:" + name}, nil
}

// List returns the registrations sorted by name, each with the status Misreport set for it, if any.
func (r *Runners) List(context.Context) ([]plan.Runner, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.failure("list"); err != nil {
		return nil, err
	}
	runners := make([]plan.Runner, 0, len(r.runners))
	for _, runner := range r.runners {
		if s, ok := r.listed[runner.Name]; ok {
			runner.Online, runner.Busy = s.online, s.busy
		}
		runners = append(runners, runner)
	}
	slices.SortFunc(runners, func(a, b plan.Runner) int { return strings.Compare(a.Name, b.Name) })
	return runners, nil
}

// Remove removes a registration, refusing busy runners as GitHub does.
func (r *Runners) Remove(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.failure("remove"); err != nil {
		return err
	}
	if r.runners[id].Busy {
		return github.ErrJobRunning
	}
	delete(r.runners, id)
	return nil
}

// SetRunner changes a registration's status, as GitHub would when its runner connects or takes a job. List shows it
// from then on, ending any Misreport of the runner.
func (r *Runners) SetRunner(name string, online, busy bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, runner := range r.runners {
		if runner.Name == name {
			runner.Online, runner.Busy = online, busy
			r.runners[id] = runner
			delete(r.listed, name)
			return
		}
	}
	panic("fake: no runner " + name)
}

// Misreport makes List show the runner with this status, while Remove still answers by the runner's own, until
// SetRunner or Finish: GitHub's runner list can show a runner offline and idle while it runs a job, and refuse to
// remove its registration meanwhile.
func (r *Runners) Misreport(name string, online, busy bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.listed[name] = status{online: online, busy: busy}
}

// Finish removes a runner's registration, as GitHub does when an ephemeral runner completes its job.
func (r *Runners) Finish(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, runner := range r.runners {
		if runner.Name == name {
			delete(r.runners, id)
			delete(r.listed, name)
			return
		}
	}
}
