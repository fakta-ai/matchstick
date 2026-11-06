package pool_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/fakta-ai/matchstick/internal/plan"
	"github.com/fakta-ai/matchstick/internal/pool"
	"github.com/fakta-ai/matchstick/internal/provider"
	providerfake "github.com/fakta-ai/matchstick/internal/provider/fake"
)

func (w *world) claim(name string, proof json.RawMessage) (string, error) {
	return w.pool.Claim(context.Background(), name, proof)
}

// expectClaim checks that machine name's claim with proof gets its configuration.
func (w *world) expectClaim(name string, proof json.RawMessage) {
	w.t.Helper()
	config, err := w.claim(name, proof)
	if err != nil || config != "jit:"+name {
		w.t.Fatalf("claim of %s: %q, %v", name, config, err)
	}
}

// expectRefused checks that machine name's claim with proof is refused for good.
func (w *world) expectRefused(name string, proof json.RawMessage) {
	w.t.Helper()
	if config, err := w.claim(name, proof); !errors.Is(err, pool.ErrRefused) || config != "" {
		w.t.Fatalf("claim of %s: %q, %v; want refused", name, config, err)
	}
}

// created creates n machines and returns their names.
func (w *world) created(n int) []string {
	w.t.Helper()
	w.pool.SetDemand(n)
	w.round()
	w.expectMachines(n)
	return w.names()
}

func TestClaimReleasesTheConfigurationOnce(t *testing.T) {
	w := newWorld(t)
	name := w.created(1)[0]
	proof := w.machines.Proof(name)
	w.expectClaim(name, proof)
	// A retry, a replayed document, and a job on the machine fetching its own document all get nothing.
	w.expectRefused(name, proof)
	w.boot()
	w.runners.SetRunner(name, true, true)
	w.expectRefused(name, w.machines.Proof(name))
	if released, refused := w.counter(w.metrics.Claims, "released"), w.counter(w.metrics.Claims, "refused"); released != 1 ||
		refused != 2 {
		t.Errorf("claims released %v, refused %v", released, refused)
	}
}

func TestClaimWithAnotherMachinesProofIsRefused(t *testing.T) {
	w := newWorld(t)
	names := w.created(2)
	a, b := names[0], names[1]
	w.expectRefused(a, w.machines.Proof(b))
	w.expectRefused(a, w.machines.Proof("ci-build-0000000zzzz"))
	w.expectRefused(a, json.RawMessage(`{"id": ""}`))
	w.expectRefused(a, json.RawMessage(`"not a proof"`))
	// The refusals took nothing: each machine still gets its own configuration.
	w.expectClaim(a, w.machines.Proof(a))
	w.expectClaim(b, w.machines.Proof(b))
}

func TestClaimForAMachineThePoolDidNotCreateIsRefused(t *testing.T) {
	w := newWorld(t)
	name := w.created(1)[0]
	for _, other := range []string{"", "ci-other-0000000aaaa", "ci-build-x-0000000aaaa", "nope", "ci-build-0000000aaaa"} {
		w.expectRefused(other, w.machines.Proof(name))
	}
	w.expectClaim(name, w.machines.Proof(name))
}

func TestClaimIsRetriedAfterTheProofCouldNotBeChecked(t *testing.T) {
	w := newWorld(t)
	name := w.created(1)[0]
	w.machines.Fail("identify", errors.New("the cloud is unavailable"))
	if _, err := w.claim(name, w.machines.Proof(name)); err == nil || errors.Is(err, pool.ErrRefused) {
		t.Fatalf("claim: %v, want a retryable error", err)
	}
	if got := w.counter(w.metrics.Claims, "failed"); got != 1 {
		t.Errorf("failed claims %v", got)
	}
	w.expectClaim(name, w.machines.Proof(name))
}

func TestRestartMidBootLeavesNothingToClaim(t *testing.T) {
	w := newWorld(t)
	name := w.created(1)[0]
	w.restart()
	w.expectRefused(name, w.machines.Proof(name))
	// The image powers the machine off when its claim is refused; the next round deletes it and its registration.
	w.machines.SetState(name, plan.Stopped)
	w.round()
	w.expectMachines(0)
	w.expectRunners(0)
}

func TestRestartMidBootMachineThatStaysUpIsDeletedAtTheTimeout(t *testing.T) {
	w := newWorld(t)
	name := w.created(1)[0]
	w.restart()
	w.machines.SetState(name, plan.Running)
	w.clock.advance(11 * time.Minute)
	if d := w.round(); len(d.Deletions) != 1 || d.Deletions[0].Reason != plan.Unregistered {
		t.Fatalf("decision %+v", d)
	}
	w.expectMachines(0)
	w.expectRunners(0)
}

func TestConfigurationIsDroppedAfterTheRegistrationTimeout(t *testing.T) {
	w := newWorld(t)
	name := w.created(1)[0]
	w.clock.advance(10*time.Minute + time.Second)
	w.expectRefused(name, w.machines.Proof(name))
}

func TestDeletedMachineLosesItsConfiguration(t *testing.T) {
	w := newWorld(t)
	name := w.created(1)[0]
	w.machines.SetState(name, plan.Failed)
	w.round()
	w.expectRefused(name, w.machines.Proof(name))
}

// failAfterCreate is a provider whose creations the cloud accepts and then fails.
type failAfterCreate struct {
	*providerfake.Provider
	names []string
}

func (f *failAfterCreate) Create(ctx context.Context, spec provider.CreateSpec) error {
	f.names = append(f.names, spec.Name)
	if err := f.Provider.Create(ctx, spec); err != nil {
		return err
	}
	return errors.New("provisioning failed")
}

func TestFailedCreationLeavesNothingToClaim(t *testing.T) {
	w := newWorld(t)
	machines := &failAfterCreate{Provider: w.machines}
	p := pool.New(settings, machines, w.runners, w.metrics, slog.New(slog.NewTextHandler(io.Discard, nil)), w.clock.now)
	p.SetDemand(1)
	if _, err := p.Round(context.Background()); err != nil {
		t.Fatal(err)
	}
	name := machines.names[0]
	if _, err := p.Claim(context.Background(), name, w.machines.Proof(name)); !errors.Is(err, pool.ErrRefused) {
		t.Errorf("claim: %v, want refused", err)
	}
	w.expectRunners(0)
}

// claimOnCreate is a provider whose machines boot and claim before Create returns, as a quick cloud allows.
type claimOnCreate struct {
	*providerfake.Provider
	pool   *pool.Pool
	config string
	err    error
}

func (c *claimOnCreate) Create(ctx context.Context, spec provider.CreateSpec) error {
	if err := c.Provider.Create(ctx, spec); err != nil {
		return err
	}
	c.config, c.err = c.pool.Claim(ctx, spec.Name, c.Proof(spec.Name))
	return nil
}

func TestMachineCanClaimBeforeItsCreationReturns(t *testing.T) {
	w := newWorld(t)
	machines := &claimOnCreate{Provider: w.machines}
	p := pool.New(settings, machines, w.runners, w.metrics, slog.New(slog.NewTextHandler(io.Discard, nil)), w.clock.now)
	machines.pool = p
	p.SetDemand(1)
	if _, err := p.Round(context.Background()); err != nil {
		t.Fatal(err)
	}
	if machines.err != nil || machines.config == "" {
		t.Errorf("claim during creation: %q, %v", machines.config, machines.err)
	}
}

func TestConcurrentClaimsReleaseOnce(t *testing.T) {
	w := newWorld(t)
	name := w.created(1)[0]
	proof := w.machines.Proof(name)
	var wg sync.WaitGroup
	var mu sync.Mutex
	released := 0
	for range 20 {
		wg.Go(func() {
			if _, err := w.claim(name, proof); err == nil {
				mu.Lock()
				released++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if released != 1 {
		t.Errorf("released %d times", released)
	}
}
