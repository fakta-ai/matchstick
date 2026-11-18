package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"

	"github.com/fakta-ai/matchstick/internal/naming"
	"github.com/fakta-ai/matchstick/internal/plan"
)

// Credentials sign in to GitHub: a GitHub App's private key with its client and installation IDs, or a token.
type Credentials struct {
	AppClientID       string
	AppInstallationID int64
	AppPrivateKey     string
	Token             string
}

// Client is a signed-in connection to the organisation, repository or enterprise at URL.
type Client struct {
	url      string
	scaleSet *scaleset.Client
	rest     *restClient
	log      *slog.Logger
	now      func() time.Time                     // the clock; time.Now outside tests
	after    func(time.Duration) <-chan time.Time // the listener's backoff timer; time.After outside tests
}

// NewClient signs in to GitHub. version is reported to GitHub as the client's version.
func NewClient(url string, c Credentials, version string, log *slog.Logger) (*Client, error) {
	info := scaleset.SystemInfo{System: "matchstick", Version: version}
	var (
		client *scaleset.Client
		err    error
	)
	if c.AppPrivateKey != "" {
		client, err = scaleset.NewClientWithGitHubApp(scaleset.ClientWithGitHubAppConfig{
			GitHubConfigURL: url, SystemInfo: info,
			GitHubAppAuth: scaleset.GitHubAppAuth{ClientID: c.AppClientID, InstallationID: c.AppInstallationID,
				PrivateKey: c.AppPrivateKey},
		}, scaleset.WithLogger(log))
	} else {
		client, err = scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
			GitHubConfigURL: url, PersonalAccessToken: c.Token, SystemInfo: info,
		}, scaleset.WithLogger(log))
	}
	if err != nil {
		return nil, fmt.Errorf("github: %w", err)
	}
	rest, err := newRESTClient(url, c)
	if err != nil {
		return nil, err
	}
	return &Client{url: url, scaleSet: client, rest: rest, log: log, now: time.Now, after: time.After}, nil
}

// ScaleSet is one pool's runner scale set, named <instance>-<pool>, in the pool's runner group. Find or Ensure looks
// it up; until one of them succeeds, JITConfig and Demand fail, and Listen ensures it before it listens.
type ScaleSet struct {
	client   *Client
	instance string
	pool     string
	group    string
	labels   []scaleset.Label
	log      *slog.Logger
	found    atomic.Pointer[location]
}

// location is where Find or Ensure found the scale set.
type location struct {
	id    int
	group string
}

var _ Runners = (*ScaleSet)(nil)

// ErrNoScaleSet is returned by Find when the pool's scale set does not exist yet.
var ErrNoScaleSet = errors.New("the scale set does not exist; matchstick run creates it")

// ScaleSet returns the pool's scale set, not yet looked up: its runner group and the labels Ensure gives it.
func (c *Client) ScaleSet(instance, pool, runnerGroup string, labels []string) *ScaleSet {
	want := make([]scaleset.Label, 0, len(labels))
	for _, l := range labels {
		want = append(want, scaleset.Label{Type: "System", Name: l})
	}
	return &ScaleSet{client: c, instance: instance, pool: pool, group: runnerGroup, labels: want,
		log: c.log.With("pool", pool)}
}

// Name is the scale set's name, <instance>-<pool>.
func (s *ScaleSet) Name() string { return s.instance + "-" + s.pool }

// Group is the runner group Find or Ensure found the scale set in; empty before either succeeds.
func (s *ScaleSet) Group() string {
	if l := s.found.Load(); l != nil {
		return l.group
	}
	return ""
}

// Find looks the scale set up, changing nothing: in the pool's runner group, or else in the one other runner group
// that holds it, from which Ensure would move it.
func (s *ScaleSet) Find(ctx context.Context) error {
	l, err := s.lookUp(ctx)
	if err != nil {
		return err
	}
	if l.set == nil {
		return fmt.Errorf("github: scale set %q: %w", s.Name(), ErrNoScaleSet)
	}
	s.found.Store(&location{id: l.set.ID, group: l.in})
	return nil
}

// Ensure makes the scale set what the pool configures: it creates it in the pool's runner group if no runner group
// holds it, moves it there from the one other group that does, and sets its labels. A scale set moved keeps its ID,
// its runners and the jobs assigned to it.
func (s *ScaleSet) Ensure(ctx context.Context) error {
	l, err := s.lookUp(ctx)
	if err != nil {
		return err
	}
	name := s.Name()
	moved := l.set != nil && l.in != s.group
	var id int
	switch {
	case l.set == nil:
		created, err := s.client.scaleSet.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{
			Name: name, RunnerGroupID: l.group.ID, Labels: s.labels,
			RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true},
		})
		if err != nil {
			return fmt.Errorf("github: creating scale set %q in runner group %q: %w", name, s.group, err)
		}
		id = created.ID
	case moved || !sameLabels(l.set.Labels, s.labels):
		l.set.RunnerGroupID, l.set.Labels = l.group.ID, s.labels
		if _, err := s.client.scaleSet.UpdateRunnerScaleSet(ctx, l.set.ID, l.set); err != nil {
			return fmt.Errorf("github: updating scale set %q: %w", name, err)
		}
		if moved {
			s.log.Info("moved the scale set to the pool's runner group", "scaleSet", name, "from", l.in,
				"to", s.group)
		}
		id = l.set.ID
	default:
		id = l.set.ID
	}
	s.found.Store(&location{id: id, group: s.group})
	return nil
}

// lookup is what lookUp found: the pool's runner group, and the scale set, if any runner group holds it, with that
// group's name.
type lookup struct {
	group *scaleset.RunnerGroup
	set   *scaleset.RunnerScaleSet
	in    string
}

// lookUp finds the pool's runner group and the scale set: in that group, or else in the other runner groups, by the
// IDs GitHub's REST API lists them with. Scale set names are unique within a runner group only, so a scale set in two
// other groups is an error, not a choice.
func (s *ScaleSet) lookUp(ctx context.Context) (lookup, error) {
	group, err := s.client.scaleSet.GetRunnerGroupByName(ctx, s.group)
	if err != nil {
		return lookup{}, fmt.Errorf("github: runner group %q: %w", s.group, err)
	}
	name := s.Name()
	set, err := s.client.scaleSet.GetRunnerScaleSet(ctx, group.ID, name)
	if err != nil {
		return lookup{}, fmt.Errorf("github: scale set %q in runner group %q: %w", name, s.group, err)
	}
	if set != nil {
		return lookup{group: group, set: set, in: s.group}, nil
	}
	others, err := s.client.rest.runnerGroups(ctx)
	if err != nil {
		return lookup{}, fmt.Errorf("github: looking for scale set %q in other runner groups: %w", name, err)
	}
	l := lookup{group: group}
	var in []string
	for _, other := range others {
		if other.ID == group.ID || strings.EqualFold(other.Name, s.group) {
			continue
		}
		found, err := s.client.scaleSet.GetRunnerScaleSet(ctx, other.ID, name)
		if err != nil {
			return lookup{}, fmt.Errorf("github: scale set %q in runner group %q: %w", name, other.Name, err)
		}
		if found != nil {
			l.set, l.in = found, other.Name
			in = append(in, other.Name)
		}
	}
	if len(in) > 1 {
		return lookup{}, fmt.Errorf("github: scale set %q is in runner groups %s; delete all but one",
			name, strings.Join(in, ", "))
	}
	return l, nil
}

// id is the scale set's ID, once Find or Ensure found it.
func (s *ScaleSet) id() (int, error) {
	if l := s.found.Load(); l != nil {
		return l.id, nil
	}
	return 0, fmt.Errorf("github: scale set %q has not been found in runner group %q", s.Name(), s.group)
}

func sameLabels(a, b []scaleset.Label) bool {
	names := func(ls []scaleset.Label) []string {
		out := make([]string, 0, len(ls))
		for _, l := range ls {
			out = append(out, l.Name)
		}
		slices.Sort(out)
		return out
	}
	return slices.Equal(names(a), names(b))
}

// JITConfig registers a runner in the scale set and returns its single-use configuration.
func (s *ScaleSet) JITConfig(ctx context.Context, name string) (JIT, error) {
	id, err := s.id()
	if err != nil {
		return JIT{}, err
	}
	c, err := s.client.scaleSet.GenerateJitRunnerConfig(ctx,
		&scaleset.RunnerScaleSetJitRunnerSetting{Name: name, WorkFolder: "_work"}, id)
	if err != nil {
		return JIT{}, fmt.Errorf("github: registering runner %s: %w", name, err)
	}
	if c.Runner == nil {
		return JIT{}, fmt.Errorf("github: registering runner %s: no runner in the response", name)
	}
	return JIT{RunnerID: int64(c.Runner.ID), Config: c.EncodedJITConfig}, nil
}

// List returns the registrations whose names the pool owns, from GitHub's runner list.
func (s *ScaleSet) List(ctx context.Context) ([]plan.Runner, error) {
	all, err := s.client.rest.runners(ctx)
	if err != nil {
		return nil, err
	}
	runners := []plan.Runner{}
	for _, r := range all {
		created, err := naming.Created(s.instance, s.pool, r.Name)
		if err != nil {
			continue
		}
		runners = append(runners, plan.Runner{ID: r.ID, Name: r.Name, CreatedAt: created,
			Online: r.Status == "online", Busy: r.Busy})
	}
	return runners, nil
}

// Remove removes a registration.
func (s *ScaleSet) Remove(ctx context.Context, id int64) error {
	err := s.client.scaleSet.RemoveRunner(ctx, id)
	switch {
	case err == nil, errors.Is(err, scaleset.RunnerNotFoundError), errors.Is(err, scaleset.NotFoundError):
		return nil
	case errors.Is(err, scaleset.JobStillRunningError):
		return ErrJobRunning
	default:
		return fmt.Errorf("github: removing runner %d: %w", id, err)
	}
}

// Demand reads the scale set's jobs, waiting and running, once; nil if GitHub does not report them.
func (s *ScaleSet) Demand(ctx context.Context) (*int, error) {
	id, err := s.id()
	if err != nil {
		return nil, err
	}
	set, err := s.client.scaleSet.GetRunnerScaleSetByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("github: scale set %d: %w", id, err)
	}
	if set == nil || set.Statistics == nil {
		return nil, nil
	}
	return new(set.Statistics.TotalAssignedJobs), nil
}

// Sink receives what the listener learns.
type Sink interface {
	SetDemand(n int)
	Draining() bool
	// SetListening says whether the listener holds a message session: true once one opens, false when it ends.
	SetListening(bool)
	// JobStarted gives, for a job that started on one of the pool's runners, how long it waited between GitHub
	// assigning it to the scale set and to that runner.
	JobStarted(wait time.Duration)
}

// Listen ensures the scale set (Ensure), then holds a message session for it until ctx ends, reporting demand to sink
// and acquiring the jobs GitHub offers unless the pool drains. Until the scale set is ensured, the pool knows no
// demand, so it creates no machines. A failed Ensure or session is retried after a backoff. sink hears false at once,
// true while each session is open and false when it ends.
func (s *ScaleSet) Listen(ctx context.Context, maxRunners int, sink Sink) {
	sink.SetListening(false)
	owner, err := os.Hostname()
	if err != nil {
		owner = "matchstick"
	}
	backoff := 5 * time.Second
	for ctx.Err() == nil {
		started := s.client.now()
		err := s.listen(ctx, owner, maxRunners, sink)
		if ctx.Err() != nil {
			return
		}
		if s.client.now().Sub(started) > 5*time.Minute {
			backoff = 5 * time.Second
		}
		if s.found.Load() == nil {
			s.log.Warn("the pool cannot scale until its scale set is in its runner group; retrying", "error", err,
				"runnerGroup", s.group, "after", backoff.String())
		} else {
			s.log.Warn("listener stopped; reconnecting", "error", err, "after", backoff.String())
		}
		select {
		case <-ctx.Done():
			return
		case <-s.client.after(backoff):
		}
		backoff = min(2*backoff, 2*time.Minute)
	}
}

func (s *ScaleSet) listen(ctx context.Context, owner string, maxRunners int, sink Sink) error {
	if s.found.Load() == nil {
		if err := s.Ensure(ctx); err != nil {
			return err
		}
	}
	id, err := s.id()
	if err != nil {
		return err
	}
	session, err := s.client.scaleSet.MessageSessionClient(ctx, id, owner)
	if err != nil {
		return fmt.Errorf("opening a message session: %w", err)
	}
	defer func() {
		if err := session.Close(context.WithoutCancel(ctx)); err != nil {
			s.log.Warn("closing the message session", "error", err)
		}
	}()
	sink.SetListening(true)
	defer sink.SetListening(false)
	l, err := listener.New(session, listener.Config{ScaleSetID: id, MaxRunners: maxRunners,
		Logger: s.log.With("component", "listener")})
	if err != nil {
		return err
	}
	return l.Run(ctx, &scaler{session: session, listener: l, maxRunners: maxRunners, sink: sink})
}

// scaler receives the listener's messages. It records and acquires; it never waits on the cloud.
type scaler struct {
	session    *scaleset.MessageSessionClient
	listener   *listener.Listener
	maxRunners int
	sink       Sink
}

func (s *scaler) Scale(ctx context.Context, m *scaleset.RunnerScaleSetMessage) error {
	draining := s.sink.Draining()
	if draining {
		s.listener.SetMaxRunners(0)
	} else {
		s.listener.SetMaxRunners(s.maxRunners)
	}
	if m == nil {
		return nil
	}
	if m.Statistics != nil {
		s.sink.SetDemand(m.Statistics.TotalAssignedJobs)
	}
	if !draining && len(m.JobAvailableMessages) > 0 {
		ids := make([]int64, 0, len(m.JobAvailableMessages))
		for _, j := range m.JobAvailableMessages {
			ids = append(ids, j.RunnerRequestID)
		}
		if _, err := s.session.AcquireJobs(ctx, ids); err != nil {
			return fmt.Errorf("acquiring jobs: %w", err)
		}
	}
	// Reported once the message is handled, so a message GitHub sends again after a failure is not counted twice.
	for _, j := range m.JobStartedMessages {
		if !j.ScaleSetAssignTime.IsZero() && !j.RunnerAssignTime.Before(j.ScaleSetAssignTime) {
			s.sink.JobStarted(j.RunnerAssignTime.Sub(j.ScaleSetAssignTime))
		}
	}
	return nil
}
