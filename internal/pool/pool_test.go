package pool_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/fakta-ai/matchstick/internal/github"
	githubfake "github.com/fakta-ai/matchstick/internal/github/fake"
	"github.com/fakta-ai/matchstick/internal/metrics"
	"github.com/fakta-ai/matchstick/internal/naming"
	"github.com/fakta-ai/matchstick/internal/plan"
	"github.com/fakta-ai/matchstick/internal/pool"
	"github.com/fakta-ai/matchstick/internal/provider"
	providerfake "github.com/fakta-ai/matchstick/internal/provider/fake"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

var settings = pool.Settings{
	Instance:    "ci",
	Name:        "build",
	Limits:      plan.Limits{Maximum: 3, RegistrationTimeout: plan.Duration(10 * time.Minute)},
	MaxLifetime: 3 * time.Hour,
	Interval:    30 * time.Second,
	Claim:       pool.Claim{URL: "https://matchstick.example:8443/claim", Certificate: "-----BEGIN CERTIFICATE-----\n"},
}

// world is a pool with its cloud and GitHub in memory.
type world struct {
	t        *testing.T
	clock    *clock
	machines *providerfake.Provider
	runners  *githubfake.Runners
	metrics  *metrics.Metrics
	pool     *pool.Pool
}

func newWorld(t *testing.T) *world {
	t.Helper()
	c := &clock{t: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)}
	w := &world{t: t, clock: c, machines: providerfake.New(c.now), runners: githubfake.New(c.now)}
	w.restart()
	return w
}

// restart replaces the pool with a new one over the same cloud and GitHub, as a process restart would: everything the
// pool remembered is gone.
func (w *world) restart() {
	w.metrics = metrics.New(prometheus.NewRegistry())
	w.pool = pool.New(settings, w.machines, w.runners, w.metrics, slog.New(slog.NewTextHandler(io.Discard, nil)),
		w.clock.now)
}

func (w *world) round() plan.Decision {
	w.t.Helper()
	d, err := w.pool.Round(context.Background())
	if err != nil {
		w.t.Fatalf("round: %v", err)
	}
	return d
}

func (w *world) machineList() []plan.Machine {
	w.t.Helper()
	ms, err := w.machines.List(context.Background())
	if err != nil {
		w.t.Fatal(err)
	}
	return ms
}

func (w *world) runnerList() []plan.Runner {
	w.t.Helper()
	rs, err := w.runners.List(context.Background())
	if err != nil {
		w.t.Fatal(err)
	}
	return rs
}

func (w *world) names() []string {
	var names []string
	for _, m := range w.machineList() {
		names = append(names, m.Name)
	}
	return names
}

// boot brings every pending machine up and registers its runner online and idle, as a working image would.
func (w *world) boot() {
	registered := map[string]bool{}
	for _, r := range w.runnerList() {
		registered[r.Name] = true
	}
	for _, m := range w.machineList() {
		if m.State == plan.Pending {
			w.machines.SetState(m.Name, plan.Running)
			if registered[m.Name] {
				w.runners.SetRunner(m.Name, true, false)
			}
		}
	}
}

func (w *world) counter(c *prometheus.CounterVec, labels ...string) float64 {
	return testutil.ToFloat64(c.WithLabelValues(append([]string{settings.Name}, labels...)...))
}

func (w *world) expectMachines(n int) {
	w.t.Helper()
	if got := len(w.machineList()); got != n {
		w.t.Fatalf("machines = %d (%v), want %d", got, w.names(), n)
	}
}

func (w *world) expectRunners(n int) {
	w.t.Helper()
	if got := len(w.runnerList()); got != n {
		w.t.Fatalf("registrations = %d, want %d", got, n)
	}
}

func TestCreatesAMachineWithARegistrationPerJob(t *testing.T) {
	w := newWorld(t)
	w.pool.SetDemand(2)
	w.round()
	w.expectMachines(2)
	w.expectRunners(2)
	for _, m := range w.machineList() {
		if _, err := naming.Created("ci", "build", m.Name); err != nil {
			t.Errorf("machine name: %v", err)
		}
		// The machine gets its bootstrap only; its configuration stays with the pool until it claims it.
		var b pool.Bootstrap
		raw := w.machines.Bootstrap(m.Name)
		if err := json.Unmarshal(raw, &b); err != nil || b != (pool.Bootstrap{Machine: m.Name,
			ClaimURL: settings.Claim.URL, ClaimCertificate: settings.Claim.Certificate}) {
			t.Errorf("machine %s bootstrap %s (%v)", m.Name, raw, err)
		}
		if strings.Contains(string(raw), "jit:") {
			t.Errorf("machine %s was given its runner configuration: %s", m.Name, raw)
		}
		if want := w.clock.now().Add(settings.MaxLifetime); !m.ExpiresAt.Equal(want) {
			t.Errorf("expires %v, want %v", m.ExpiresAt, want)
		}
	}
	// The next round sees them and creates no more.
	w.round()
	w.expectMachines(2)
}

func TestNeverExceedsTheMaximum(t *testing.T) {
	w := newWorld(t)
	w.pool.SetDemand(10)
	w.round()
	w.expectMachines(3)
	// Stopped machines still count until they are gone; a failing delete keeps them.
	for _, m := range w.machineList() {
		w.machines.SetState(m.Name, plan.Stopped)
	}
	w.machines.Fail("delete", errors.New("boom"), errors.New("boom"), errors.New("boom"))
	w.round()
	w.expectMachines(3)
}

func TestCreatesNothingUntilDemandIsKnown(t *testing.T) {
	w := newWorld(t)
	w.round()
	w.expectMachines(0)
}

func TestJobLifecycle(t *testing.T) {
	w := newWorld(t)
	w.pool.SetDemand(1)
	w.round()
	w.boot()
	name := w.names()[0]
	w.runners.SetRunner(name, true, true) // took the job
	w.round()
	w.expectMachines(1)

	// The job ends: GitHub removes the ephemeral registration and the image powers the machine off.
	w.runners.Finish(name)
	w.machines.SetState(name, plan.Stopped)
	w.pool.SetDemand(0)
	w.round()
	w.expectMachines(0)
	if got := w.counter(w.metrics.Deletions, "finished"); got != 1 {
		t.Errorf("finished deletions = %v, want 1", got)
	}
}

func TestFinishedMachineLosesItsRegistrationFirst(t *testing.T) {
	w := newWorld(t)
	w.pool.SetDemand(1)
	w.round()
	w.boot()
	name := w.names()[0]
	w.machines.SetState(name, plan.Stopped) // crashed before GitHub removed the registration
	w.pool.SetDemand(0)
	w.round()
	w.expectMachines(0)
	w.expectRunners(0)
}

func TestFinishedMachineIsDeletedEvenIfGitHubFails(t *testing.T) {
	w := newWorld(t)
	w.pool.SetDemand(1)
	w.round()
	w.boot()
	w.machines.SetState(w.names()[0], plan.Stopped)
	w.pool.SetDemand(0)
	w.runners.Fail("remove", errors.New("github unavailable"))
	w.round()
	w.expectMachines(0)
}

func TestExpiredMachineIsDeletedMidJob(t *testing.T) {
	w := newWorld(t)
	w.pool.SetDemand(1)
	w.round()
	w.boot()
	name := w.names()[0]
	w.runners.SetRunner(name, true, true)
	w.clock.advance(settings.MaxLifetime)
	w.round()
	if names := w.names(); len(names) != 1 || names[0] == name {
		t.Fatalf("machines = %v: want the expired one replaced", names)
	}
	if got := w.counter(w.metrics.Deletions, "expired"); got != 1 {
		t.Errorf("expired deletions = %v, want 1", got)
	}
}

func TestUnregisteredMachineIsDeletedAfterTheTimeout(t *testing.T) {
	w := newWorld(t)
	w.pool.SetDemand(1)
	w.round()
	name := w.names()[0]
	w.machines.SetState(name, plan.Running)
	w.runners.Finish(name) // its registration vanished, and the runner never connected

	w.clock.advance(9 * time.Minute)
	w.round()
	w.expectMachines(1)

	w.clock.advance(2 * time.Minute)
	w.round()
	if names := w.names(); len(names) != 1 || names[0] == name {
		t.Fatalf("machines = %v: want the unregistered one replaced", names)
	}
}

func TestMachineWhoseRunnerNeverConnectsIsDeletedWithItsRegistration(t *testing.T) {
	w := newWorld(t)
	w.pool.SetDemand(1)
	w.round()
	name := w.names()[0]
	w.machines.SetState(name, plan.Running) // boots, but the runner never comes online
	w.clock.advance(11 * time.Minute)
	w.round()
	for _, r := range w.runnerList() {
		if r.Name == name {
			t.Errorf("registration of %s kept", name)
		}
	}
	if names := w.names(); len(names) != 1 || names[0] == name {
		t.Fatalf("machines = %v: want %s replaced", names, name)
	}
}

func TestMachinesThatNeverRegisterPauseCreation(t *testing.T) {
	w := newWorld(t)
	w.pool.SetDemand(3)
	w.round()
	for _, m := range w.machineList() {
		w.machines.SetState(m.Name, plan.Running)
		w.runners.Finish(m.Name)
	}
	w.clock.advance(11 * time.Minute)
	w.round()
	w.expectMachines(0)
	if got := testutil.ToFloat64(w.metrics.CreationPaused.WithLabelValues("build")); got != 1 {
		t.Errorf("creation paused = %v, want 1", got)
	}
	w.clock.advance(5 * time.Minute)
	w.round()
	w.expectMachines(0)
	w.clock.advance(6 * time.Minute)
	w.round()
	w.expectMachines(3)
}

func TestSuccessiveMachinesThatNeverConnectPauseCreation(t *testing.T) {
	w := newWorld(t)
	w.pool.SetDemand(1)
	for range 3 {
		w.round() // creates one machine, its JIT registration stays offline
		for _, m := range w.machineList() {
			w.machines.SetState(m.Name, plan.Running)
		}
		w.clock.advance(11 * time.Minute)
	}
	w.round() // the third is deleted: creation pauses
	if got := testutil.ToFloat64(w.metrics.CreationPaused.WithLabelValues("build")); got != 1 {
		t.Fatalf("creation paused = %v, want 1", got)
	}
	w.expectMachines(0)
}

func TestDrainBeforeDemandIsKnown(t *testing.T) {
	w := newWorld(t)
	w.pool.SetDemand(1)
	w.round()
	w.boot()
	w.restart()
	w.pool.SetDraining(true)
	w.round()
	w.expectMachines(0)
}

// drainOnCreate is a provider that starts a drain when its first machine is created, as an operator's drain request
// arriving mid-round would.
type drainOnCreate struct {
	*providerfake.Provider
	pool *pool.Pool
	once sync.Once
}

func (d *drainOnCreate) Create(ctx context.Context, spec provider.CreateSpec) error {
	d.once.Do(func() { d.pool.SetDraining(true) })
	return d.Provider.Create(ctx, spec)
}

func TestDrainStopsTheRoundsRemainingCreations(t *testing.T) {
	w := newWorld(t)
	machines := &drainOnCreate{Provider: w.machines}
	big := settings
	big.Limits.Maximum = 20
	p := pool.New(big, machines, w.runners, w.metrics, slog.New(slog.NewTextHandler(io.Discard, nil)), w.clock.now)
	machines.pool = p
	p.SetDemand(20)
	if _, err := p.Round(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(w.machineList()); n > 4 {
		t.Errorf("%d machines created after the drain began; want at most the 4 already started", n)
	}
}

func TestOrphanRegistrationIsRemoved(t *testing.T) {
	w := newWorld(t)
	// A crash between the registration and the machine leaves a registration with no machine.
	name := naming.New("ci", "build", w.clock.now())
	if _, err := w.runners.JITConfig(context.Background(), name); err != nil {
		t.Fatal(err)
	}
	w.round()
	w.expectRunners(1)
	w.clock.advance(11 * time.Minute)
	w.round()
	w.expectRunners(0)
	if got := w.counter(w.metrics.Removals, "orphan"); got != 1 {
		t.Errorf("orphan removals = %v, want 1", got)
	}
}

func TestFailedCreationRemovesItsRegistration(t *testing.T) {
	w := newWorld(t)
	w.machines.Fail("create", errors.New("invalid image"))
	w.pool.SetDemand(1)
	w.round()
	w.expectMachines(0)
	w.expectRunners(0)
	if got := w.counter(w.metrics.CreationFailures, "cloud"); got != 1 {
		t.Errorf("cloud failures = %v, want 1", got)
	}
	// The next round tries again.
	w.round()
	w.expectMachines(1)
}

func TestFailedCreationAndFailedRemovalLeavesAnOrphanForLater(t *testing.T) {
	w := newWorld(t)
	w.machines.Fail("create", errors.New("invalid image"))
	w.runners.Fail("remove", errors.New("github unavailable"))
	w.pool.SetDemand(0)
	w.pool.SetDemand(1)
	w.round()
	w.expectMachines(0)
	w.expectRunners(1)
	w.pool.SetDemand(0)
	w.clock.advance(11 * time.Minute)
	w.round()
	w.expectRunners(0)
}

func TestCapacityErrorsBackOff(t *testing.T) {
	w := newWorld(t)
	w.machines.Fail("create", fmt.Errorf("quota: %w", provider.ErrCapacity))
	w.pool.SetDemand(1)
	w.round()
	w.expectMachines(0)
	w.expectRunners(0)
	w.round()
	w.expectMachines(0) // paused
	w.clock.advance(2 * time.Minute)
	w.round()
	w.expectMachines(1)
}

func TestScaleDownRemovesAnIdleRegistrationBeforeItsMachine(t *testing.T) {
	w := newWorld(t)
	w.pool.SetDemand(2)
	w.round()
	w.boot()
	w.pool.SetDemand(0)
	w.round()
	w.expectMachines(0)
	w.expectRunners(0)
	if got := w.counter(w.metrics.Deletions, "surplus"); got != 2 {
		t.Errorf("surplus deletions = %v, want 2", got)
	}
}

func TestScaleDownKeepsAMachineWhoseRunnerJustTookAJob(t *testing.T) {
	w := newWorld(t)
	w.pool.SetDemand(1)
	w.round()
	w.boot()
	w.pool.SetDemand(0)
	// Between gather and act the runner takes a job: GitHub refuses the removal.
	w.runners.Fail("remove", github.ErrJobRunning)
	w.round()
	w.expectMachines(1)
	w.expectRunners(1)
}

func TestScaleDownKeepsTheMachineIfTheRegistrationCannotBeRemoved(t *testing.T) {
	w := newWorld(t)
	w.pool.SetDemand(1)
	w.round()
	w.boot()
	w.pool.SetDemand(0)
	w.runners.Fail("remove", errors.New("github unavailable"))
	w.round()
	w.expectMachines(1)
	w.round()
	w.expectMachines(0)
}

func TestRestartKeepsWorkAndCleansUp(t *testing.T) {
	w := newWorld(t)
	w.pool.SetDemand(3)
	w.round()
	w.boot()
	names := w.names()
	busy, done, idle := names[0], names[1], names[2]
	w.runners.SetRunner(busy, true, true)
	w.runners.Finish(done)
	w.machines.SetState(done, plan.Stopped)
	orphan := naming.New("ci", "build", w.clock.now())
	if _, err := w.runners.JITConfig(context.Background(), orphan); err != nil {
		t.Fatal(err)
	}

	w.restart()
	w.clock.advance(11 * time.Minute)
	w.round() // demand unknown: cleans up, creates and scales down nothing
	if got := strings.Join(w.names(), ","); got != strings.Join([]string{busy, idle}, ",") {
		t.Fatalf("machines after restart = %s, want %s,%s", got, busy, idle)
	}
	w.expectRunners(2)

	w.pool.SetDemand(1) // the busy runner's job
	w.round()
	if got := strings.Join(w.names(), ","); got != busy {
		t.Fatalf("machines = %s, want %s", got, busy)
	}
}

func TestGitHubDownStillDeletesFinishedAndExpiredMachines(t *testing.T) {
	w := newWorld(t)
	w.pool.SetDemand(3)
	w.round()
	w.boot()
	names := w.names()
	w.machines.SetState(names[0], plan.Stopped)
	w.clock.advance(settings.MaxLifetime - time.Minute)
	w.runners.SetRunner(names[2], true, true)
	w.machines.Fail("create", errors.New("not reached")) // keep creation out of the picture
	w.restart()
	w.clock.advance(2 * time.Minute)
	w.runners.Fail("list", errors.New("github unavailable"))
	w.round()
	w.expectMachines(0)
}

func TestCloudListFailureDoesNothing(t *testing.T) {
	w := newWorld(t)
	w.pool.SetDemand(1)
	w.machines.Fail("list", errors.New("cloud unavailable"))
	if _, err := w.pool.Round(context.Background()); err == nil {
		t.Fatal("round succeeded without a machine list")
	}
	w.expectMachines(0)
	w.expectRunners(0)
	if !w.pool.LastSuccess().IsZero() {
		t.Error("a failed round counted as a success")
	}
}

func TestDrain(t *testing.T) {
	w := newWorld(t)
	w.pool.SetDemand(2)
	w.round()
	w.boot()
	names := w.names()
	w.runners.SetRunner(names[0], true, true)
	w.pool.SetDraining(true)
	w.round()
	if got := strings.Join(w.names(), ","); got != names[0] {
		t.Fatalf("machines while draining = %s, want only the busy %s", got, names[0])
	}
	w.runners.Finish(names[0])
	w.machines.SetState(names[0], plan.Stopped)
	w.round()
	w.expectMachines(0)
	w.expectRunners(0)
}

func TestRunStopsWithItsContext(t *testing.T) {
	w := newWorld(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		w.pool.Run(ctx)
		close(done)
	}()
	w.pool.SetDemand(1)
	deadline := time.After(5 * time.Second)
	for len(w.machineList()) == 0 {
		select {
		case <-deadline:
			t.Fatal("no machine created by Run")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}

func TestListeningIsAGaugeOfThePool(t *testing.T) {
	w := newWorld(t)
	for _, on := range []bool{false, true, false} {
		w.pool.SetListening(on)
		want := 0.0
		if on {
			want = 1
		}
		if got := testutil.ToFloat64(w.metrics.Listening.WithLabelValues(settings.Name)); got != want {
			t.Errorf("after SetListening(%v): matchstick_listening = %v, want %v", on, got, want)
		}
	}
}
