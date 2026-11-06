// Package fake is a Provider in memory, for tests and for trying the planner without a cloud.
package fake

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/fakta-ai/matchstick/internal/plan"
	"github.com/fakta-ai/matchstick/internal/provider"
)

// Provider keeps machines in memory. Created machines start pending; tests move them along with SetState.
//
// Each created machine gets a made-up id, as a cloud gives each VM one. Its proof of identity is {"id": "<id>"}: Proof
// returns it, as the cloud would to code running on that machine.
type Provider struct {
	mu         sync.Mutex
	now        func() time.Time
	machines   map[string]plan.Machine
	bootstraps map[string][]byte
	ids        map[string]string
	nextID     int
	failures   map[string][]error
}

var _ provider.Provider = (*Provider)(nil)

// New returns a provider holding the given machines. now stamps created machines.
func New(now func() time.Time, machines ...plan.Machine) *Provider {
	p := &Provider{now: now, machines: map[string]plan.Machine{}, bootstraps: map[string][]byte{},
		ids: map[string]string{}, failures: map[string][]error{}}
	for _, m := range machines {
		p.machines[m.Name] = m
	}
	return p
}

// Fail makes the next calls of op ("create", "list", "delete" or "identify") fail with the given errors, one call
// each.
func (p *Provider) Fail(op string, errs ...error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failures[op] = append(p.failures[op], errs...)
}

func (p *Provider) failure(op string) error {
	queue := p.failures[op]
	if len(queue) == 0 {
		return nil
	}
	p.failures[op] = queue[1:]
	return queue[0]
}

// Create adds a pending machine.
func (p *Provider) Create(_ context.Context, spec provider.CreateSpec) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.failure("create"); err != nil {
		return err
	}
	if _, ok := p.machines[spec.Name]; ok {
		return fmt.Errorf("machine %s already exists", spec.Name)
	}
	p.machines[spec.Name] = plan.Machine{Name: spec.Name, State: plan.Pending, CreatedAt: p.now(),
		ExpiresAt: spec.ExpiresAt}
	p.bootstraps[spec.Name] = slices.Clone(spec.Bootstrap)
	p.nextID++
	p.ids[spec.Name] = fmt.Sprintf("vm-%d", p.nextID)
	return nil
}

// List returns the machines sorted by name.
func (p *Provider) List(context.Context) ([]plan.Machine, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.failure("list"); err != nil {
		return nil, err
	}
	machines := make([]plan.Machine, 0, len(p.machines))
	for _, m := range p.machines {
		machines = append(machines, m)
	}
	slices.SortFunc(machines, func(a, b plan.Machine) int { return strings.Compare(a.Name, b.Name) })
	return machines, nil
}

// Delete removes a machine at once.
func (p *Provider) Delete(_ context.Context, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.failure("delete"); err != nil {
		return err
	}
	delete(p.machines, name)
	delete(p.bootstraps, name)
	delete(p.ids, name)
	return nil
}

// fakeProof is the fake cloud's signed document: here, only the machine's id.
type fakeProof struct {
	ID string `json:"id"`
}

// Identify accepts a proof carrying the id of the machine name, while that machine exists.
func (p *Provider) Identify(_ context.Context, name string, proof json.RawMessage) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.failure("identify"); err != nil {
		return err
	}
	var doc fakeProof
	if err := json.Unmarshal(proof, &doc); err != nil {
		return fmt.Errorf("%w: %w", provider.ErrNotProven, err)
	}
	if id, ok := p.ids[name]; !ok || doc.ID == "" || doc.ID != id {
		return fmt.Errorf("%w: id %q is not machine %s's", provider.ErrNotProven, doc.ID, name)
	}
	return nil
}

// Proof returns the proof code running on machine name would get from the cloud; a made-up id if there is no such
// machine.
func (p *Provider) Proof(name string) json.RawMessage {
	p.mu.Lock()
	defer p.mu.Unlock()
	id, ok := p.ids[name]
	if !ok {
		id = "vm-stranger"
	}
	b, err := json.Marshal(fakeProof{ID: id})
	if err != nil {
		panic(err)
	}
	return b
}

// SetState changes a machine's state, as its cloud would.
func (p *Provider) SetState(name string, state plan.MachineState) {
	p.mu.Lock()
	defer p.mu.Unlock()
	m, ok := p.machines[name]
	if !ok {
		panic("fake: no machine " + name)
	}
	m.State = state
	p.machines[name] = m
}

// Bootstrap returns what a machine was given at creation.
func (p *Provider) Bootstrap(name string) []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.bootstraps[name])
}
