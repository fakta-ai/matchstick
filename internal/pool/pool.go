// Package pool runs one pool: each round it gathers what the cloud and GitHub report, asks the planner what to do,
// and does it.
package pool

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/fakta-ai/matchstick/internal/github"
	"github.com/fakta-ai/matchstick/internal/metrics"
	"github.com/fakta-ai/matchstick/internal/naming"
	"github.com/fakta-ai/matchstick/internal/plan"
	"github.com/fakta-ai/matchstick/internal/provider"
)

const (
	// parallel is how many calls one round makes at once.
	parallel = 4
	// capacityBackoffFirst and capacityBackoffLongest bound the pause after the cloud reports no capacity.
	capacityBackoffFirst   = 2 * time.Minute
	capacityBackoffLongest = 30 * time.Minute
	// unregisteredLimit machines in a row that never register pause creation for unregisteredPause.
	unregisteredLimit = 3
	unregisteredPause = 10 * time.Minute
)

// ActTimeout bounds the actions of one round; calls still running then are cancelled and planned again next round.
const ActTimeout = 5 * time.Minute

// Settings are one pool's settings.
type Settings struct {
	Instance    string
	Name        string
	Limits      plan.Limits
	MaxLifetime time.Duration
	Interval    time.Duration
	// Claim is where machines claim their runner configuration; it goes into every machine's bootstrap.
	Claim Claim
}

// Claim is the claim endpoint as machines see it.
type Claim struct {
	// URL is the endpoint machines post their claim to.
	URL string
	// Certificate is the endpoint's certificate, PEM: the only one machines trust for URL.
	Certificate string
}

// Pool is one pool's loop and the little it remembers between rounds: demand, drain, backoff, when it last saw the
// pool with a job, when it first saw each machine that is on its way out, and the runner configurations of machines
// that have not claimed theirs yet. All of it is rebuilt or reset harmlessly after a restart.
type Pool struct {
	settings Settings
	machines provider.Provider
	runners  github.Runners
	metrics  *metrics.Metrics
	log      *slog.Logger
	now      func() time.Time
	wake     chan struct{}
	// started is when the pool was made; machines created before it stand for jobs it did not see.
	started time.Time

	mu              sync.Mutex
	demand          *int
	draining        bool
	pausedUntil     time.Time
	capacityBackoff time.Duration
	unregistered    int
	lastSuccess     time.Time
	// lastActive is when the pool last had a job, as far as this process saw: a demand above 0, the moment demand
	// fell to 0, or a busy runner.
	lastActive time.Time
	held       map[string]held
	// leaving is when each machine listed stopped, failed or being deleted was first seen so; only Round touches it.
	leaving map[string]time.Time
}

// New returns a pool. now is the clock; tests pass their own.
func New(s Settings, machines provider.Provider, runners github.Runners, m *metrics.Metrics, log *slog.Logger,
	now func() time.Time) *Pool {
	return &Pool{settings: s, machines: machines, runners: runners, metrics: m, now: now, started: now(),
		log: log.With("pool", s.Name), wake: make(chan struct{}, 1), held: map[string]held{}}
}

// Name returns the pool's name.
func (p *Pool) Name() string { return p.settings.Name }

// SetDemand records the jobs assigned to the pool, as the listener reports them, and starts a round. A demand above 0,
// or one that falls to 0, is the pool's latest activity: the idle timeout counts from the end of its last job.
func (p *Pool) SetDemand(n int) {
	p.mu.Lock()
	if n > 0 || p.demand != nil && *p.demand > 0 {
		p.lastActive = p.now()
	}
	p.demand = new(n)
	p.mu.Unlock()
	p.metrics.Demand.WithLabelValues(p.settings.Name).Set(float64(n))
	p.Wake()
}

// SetDraining starts or stops draining the pool, and starts a round.
func (p *Pool) SetDraining(draining bool) {
	p.mu.Lock()
	p.draining = draining
	p.mu.Unlock()
	p.metrics.Draining.WithLabelValues(p.settings.Name).Set(boolFloat(draining))
	p.Wake()
}

// Draining reports whether the pool drains.
func (p *Pool) Draining() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.draining
}

// SetListening records whether the pool's listener holds a message session for its scale set.
func (p *Pool) SetListening(listening bool) {
	p.metrics.Listening.WithLabelValues(p.settings.Name).Set(boolFloat(listening))
}

// JobStarted records how long a job waited between GitHub assigning it to the pool's scale set and to one of the
// pool's runners.
func (p *Pool) JobStarted(wait time.Duration) {
	p.metrics.JobStartup.WithLabelValues(p.settings.Name).Observe(wait.Seconds())
}

// LastSuccess returns when the last round that could see the pool's machines gathered them; zero before the first.
func (p *Pool) LastSuccess() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastSuccess
}

// Wake starts a round soon. Wakes while a round runs fold into one.
func (p *Pool) Wake() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// Run runs rounds until ctx ends: at once, then on every wake and every interval.
func (p *Pool) Run(ctx context.Context) {
	ticker := time.NewTicker(p.settings.Interval)
	defer ticker.Stop()
	for {
		if _, err := p.Round(ctx); err != nil && ctx.Err() == nil {
			p.log.Error("round failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-p.wake:
		case <-ticker.C:
		}
	}
}

// Observe gathers what the cloud and GitHub report now. It fails only if the machines cannot be listed; a failed
// runner list leaves Runners nil, and the planner skips the rules that need it.
func (p *Pool) Observe(ctx context.Context) (plan.Observation, error) {
	machines, err := p.machines.List(ctx)
	if err != nil {
		return plan.Observation{}, err
	}
	o := plan.Observation{Now: p.now(), Machines: machines}
	if runners, err := p.runners.List(ctx); err != nil {
		p.log.Warn("runner list unavailable; skipping the rules that need it", "error", err)
	} else {
		o.Runners = runners
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.demand != nil {
		o.Demand = new(*p.demand)
	}
	o.Draining = p.draining
	o.CreationPaused = o.Now.Before(p.pausedUntil)
	o.LastActive, o.Started = p.lastActive, p.started
	return o, nil
}

// Round gathers, decides and acts once, and returns what it decided.
func (p *Pool) Round(ctx context.Context) (plan.Decision, error) {
	start := p.now()
	o, err := p.Observe(ctx)
	if err != nil {
		p.metrics.Rounds.WithLabelValues(p.settings.Name, "failure").Inc()
		return plan.Decision{}, err
	}
	p.mu.Lock()
	p.lastSuccess = o.Now
	p.mu.Unlock()
	p.metrics.LastSuccess.WithLabelValues(p.settings.Name).Set(float64(o.Now.Unix()))
	d := plan.Decide(o, p.settings.Limits)
	p.mu.Lock()
	if d.Spares.LastActive.After(p.lastActive) {
		p.lastActive = d.Spares.LastActive
	}
	p.mu.Unlock()
	p.metrics.Spares.WithLabelValues(p.settings.Name).Set(float64(d.Spares.Count))
	p.dropHeld(o, d)
	p.report(o)
	var wait time.Duration
	p.leaving, wait = plan.Leaving(p.leaving, o)
	p.metrics.DeletionWait.WithLabelValues(p.settings.Name).Set(wait.Seconds())
	p.trackUnregistered(o, d)
	actCtx, cancel := context.WithTimeout(ctx, ActTimeout)
	p.act(actCtx, d)
	cancel()

	p.metrics.Rounds.WithLabelValues(p.settings.Name, "success").Inc()
	p.metrics.RoundDuration.WithLabelValues(p.settings.Name).Observe(p.now().Sub(start).Seconds())
	return d, nil
}

// act carries out a decision: removals and deletions first, then creations. Each call stands alone; a failed one is
// logged, and the next round plans it again.
func (p *Pool) act(ctx context.Context, d plan.Decision) {
	limit := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	run := func(f func()) {
		limit <- struct{}{}
		wg.Go(func() {
			defer func() { <-limit }()
			f()
		})
	}
	for _, r := range d.Removals {
		run(func() { p.remove(ctx, r) })
	}
	for _, del := range d.Deletions {
		run(func() { p.delete(ctx, del) })
	}
	wg.Wait()
	for range d.Create {
		limit <- struct{}{}
		// A pause or a drain that began during this round stops the creations it has not started.
		if p.paused() || p.Draining() {
			<-limit
			break
		}
		wg.Go(func() {
			defer func() { <-limit }()
			p.create(ctx)
		})
	}
	wg.Wait()
}

func (p *Pool) remove(ctx context.Context, r plan.Removal) {
	if err := p.runners.Remove(ctx, r.RunnerID); err != nil {
		p.log.Warn("registration removal failed", "runner", r.Name, "reason", r.Reason, "error", err)
		return
	}
	p.metrics.Removals.WithLabelValues(p.settings.Name, string(r.Reason)).Inc()
	p.log.Info("registration removed", "runner", r.Name, "reason", r.Reason)
}

func (p *Pool) delete(ctx context.Context, d plan.Deletion) {
	log := p.log.With("machine", d.Machine, "reason", d.Reason)
	if d.RunnerID != 0 {
		err := p.runners.Remove(ctx, d.RunnerID)
		switch {
		case err == nil:
			p.metrics.Removals.WithLabelValues(p.settings.Name, string(d.Reason)).Inc()
		case d.Guarded && errors.Is(err, github.ErrJobRunning):
			log.Info("runner took a job; keeping its machine")
			return
		case d.Guarded:
			log.Warn("registration removal failed; keeping the machine for now", "error", err)
			return
		default:
			log.Warn("registration removal failed; deleting the machine anyway", "error", err)
		}
	}
	if err := p.machines.Delete(ctx, d.Machine); err != nil {
		log.Warn("machine deletion failed", "error", err)
		return
	}
	p.metrics.Deletions.WithLabelValues(p.settings.Name, string(d.Reason)).Inc()
	log.Info("machine deleted")
}

func (p *Pool) create(ctx context.Context) {
	now := p.now()
	name := naming.New(p.settings.Instance, p.settings.Name, now)
	log := p.log.With("machine", name)
	jit, err := p.runners.JITConfig(ctx, name)
	if err != nil {
		p.metrics.CreationFailures.WithLabelValues(p.settings.Name, "registration").Inc()
		log.Warn("runner registration failed", "error", err)
		return
	}
	bootstrap, err := p.bootstrap(name)
	if err != nil {
		p.metrics.CreationFailures.WithLabelValues(p.settings.Name, "cloud").Inc()
		log.Warn("machine creation failed", "error", err)
		p.removeAfterFailure(ctx, log, jit.RunnerID)
		return
	}
	// Held before the cloud is asked, so a machine that boots quickly finds its configuration.
	p.hold(name, jit, now)
	err = p.machines.Create(ctx, provider.CreateSpec{Name: name, Bootstrap: bootstrap,
		ExpiresAt: now.Add(p.settings.MaxLifetime)})
	if err != nil {
		p.drop(name)
		kind := "cloud"
		if errors.Is(err, provider.ErrCapacity) {
			kind = "capacity"
			p.pauseForCapacity()
		}
		p.metrics.CreationFailures.WithLabelValues(p.settings.Name, kind).Inc()
		log.Warn("machine creation failed", "kind", kind, "error", err)
		p.removeAfterFailure(ctx, log, jit.RunnerID)
		return
	}
	p.mu.Lock()
	p.capacityBackoff = 0
	p.mu.Unlock()
	p.metrics.Creations.WithLabelValues(p.settings.Name).Inc()
	log.Info("machine created")
}

// removeAfterFailure removes the registration of a machine that was not created now, rather than wait for the orphan
// rule.
func (p *Pool) removeAfterFailure(ctx context.Context, log *slog.Logger, runnerID int64) {
	if err := p.runners.Remove(context.WithoutCancel(ctx), runnerID); err != nil {
		log.Warn("registration removal failed; a later round removes it", "error", err)
	}
}

func (p *Pool) paused() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.now().Before(p.pausedUntil)
}

func (p *Pool) pauseForCapacity() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.capacityBackoff = min(max(2*p.capacityBackoff, capacityBackoffFirst), capacityBackoffLongest)
	p.pauseLocked(p.capacityBackoff, "no capacity")
}

func (p *Pool) pauseLocked(d time.Duration, why string) {
	until := p.now().Add(d)
	if until.After(p.pausedUntil) {
		p.pausedUntil = until
		p.metrics.CreationPaused.WithLabelValues(p.settings.Name).Set(1)
		p.log.Warn("creation paused", "for", d.String(), "why", why)
	}
}

// trackUnregistered counts machines deleted in a row because they never registered, and pauses creation when
// unregisteredLimit have. A machine whose runner is online resets the count, and so does the pause.
func (p *Pool) trackUnregistered(o plan.Observation, d plan.Decision) {
	if o.Runners == nil {
		return
	}
	connected := map[string]bool{}
	for _, r := range o.Runners {
		connected[r.Name] = r.Online || r.Busy
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, m := range o.Machines {
		if connected[m.Name] {
			p.unregistered = 0
			break
		}
	}
	for _, del := range d.Deletions {
		if del.Reason == plan.Unregistered {
			p.unregistered++
		}
	}
	if p.unregistered >= unregisteredLimit {
		p.unregistered = 0
		p.pauseLocked(unregisteredPause, "machines do not register")
	}
	if !o.Now.Before(p.pausedUntil) {
		p.metrics.CreationPaused.WithLabelValues(p.settings.Name).Set(0)
	}
}

// report sets the gauges from an observation.
func (p *Pool) report(o plan.Observation) {
	name := p.settings.Name
	states := map[plan.MachineState]int{}
	for _, m := range o.Machines {
		states[m.State]++
	}
	for _, s := range []plan.MachineState{plan.Pending, plan.Running, plan.Stopped, plan.Failed, plan.Deleting} {
		p.metrics.Machines.WithLabelValues(name, string(s)).Set(float64(states[s]))
	}
	if o.Runners == nil {
		return
	}
	statuses := map[string]int{}
	for _, r := range o.Runners {
		statuses[RunnerStatus(r)]++
	}
	for _, s := range []string{"offline", "idle", "busy"} {
		p.metrics.Runners.WithLabelValues(name, s).Set(float64(statuses[s]))
	}
}

// RunnerStatus names a registration's status: offline, idle or busy.
func RunnerStatus(r plan.Runner) string {
	switch {
	case r.Busy:
		return "busy"
	case r.Online:
		return "idle"
	default:
		return "offline"
	}
}

func boolFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
