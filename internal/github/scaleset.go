package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
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
	return &Client{url: url, scaleSet: client, rest: rest, log: log}, nil
}

// ScaleSet is one pool's runner scale set.
type ScaleSet struct {
	client   *Client
	id       int
	instance string
	pool     string
	log      *slog.Logger
}

var _ Runners = (*ScaleSet)(nil)

// ErrNoScaleSet is returned by FindScaleSet when the pool's scale set does not exist yet.
var ErrNoScaleSet = errors.New("the scale set does not exist; matchstick run creates it")

// FindScaleSet finds the pool's scale set, named <instance>-<pool>, in the runner group, changing nothing.
func (c *Client) FindScaleSet(ctx context.Context, instance, pool, runnerGroup string) (*ScaleSet, error) {
	group, err := c.scaleSet.GetRunnerGroupByName(ctx, runnerGroup)
	if err != nil {
		return nil, fmt.Errorf("github: runner group %q: %w", runnerGroup, err)
	}
	name := instance + "-" + pool
	existing, err := c.scaleSet.GetRunnerScaleSet(ctx, group.ID, name)
	if err != nil {
		return nil, fmt.Errorf("github: scale set %q: %w", name, err)
	}
	if existing == nil {
		return nil, fmt.Errorf("github: scale set %q: %w", name, ErrNoScaleSet)
	}
	return &ScaleSet{client: c, id: existing.ID, instance: instance, pool: pool, log: c.log.With("pool", pool)}, nil
}

// EnsureScaleSet finds the pool's scale set, named <instance>-<pool>, in the runner group, creating it or updating its
// labels as needed.
func (c *Client) EnsureScaleSet(ctx context.Context, instance, pool, runnerGroup string,
	labels []string) (*ScaleSet, error) {
	group, err := c.scaleSet.GetRunnerGroupByName(ctx, runnerGroup)
	if err != nil {
		return nil, fmt.Errorf("github: runner group %q: %w", runnerGroup, err)
	}
	want := make([]scaleset.Label, 0, len(labels))
	for _, l := range labels {
		want = append(want, scaleset.Label{Type: "System", Name: l})
	}
	name := instance + "-" + pool
	existing, err := c.scaleSet.GetRunnerScaleSet(ctx, group.ID, name)
	if err != nil {
		return nil, fmt.Errorf("github: scale set %q: %w", name, err)
	}
	s := &ScaleSet{client: c, instance: instance, pool: pool, log: c.log.With("pool", pool)}
	switch {
	case existing == nil:
		created, err := c.scaleSet.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{
			Name: name, RunnerGroupID: group.ID, Labels: want, RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true},
		})
		if err != nil {
			return nil, fmt.Errorf("github: creating scale set %q: %w", name, err)
		}
		s.id = created.ID
	case !sameLabels(existing.Labels, want):
		existing.Labels = want
		if _, err := c.scaleSet.UpdateRunnerScaleSet(ctx, existing.ID, existing); err != nil {
			return nil, fmt.Errorf("github: updating scale set %q: %w", name, err)
		}
		s.id = existing.ID
	default:
		s.id = existing.ID
	}
	return s, nil
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
	c, err := s.client.scaleSet.GenerateJitRunnerConfig(ctx,
		&scaleset.RunnerScaleSetJitRunnerSetting{Name: name, WorkFolder: "_work"}, s.id)
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
	set, err := s.client.scaleSet.GetRunnerScaleSetByID(ctx, s.id)
	if err != nil {
		return nil, fmt.Errorf("github: scale set %d: %w", s.id, err)
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
}

// Listen holds a message session for the scale set until ctx ends, reporting demand to sink and acquiring the jobs
// GitHub offers unless the pool drains. A failed session is replaced after a backoff.
func (s *ScaleSet) Listen(ctx context.Context, maxRunners int, sink Sink) {
	owner, err := os.Hostname()
	if err != nil {
		owner = "matchstick"
	}
	backoff := 5 * time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := s.listen(ctx, owner, maxRunners, sink)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > 5*time.Minute {
			backoff = 5 * time.Second
		}
		s.log.Warn("listener stopped; reconnecting", "error", err, "after", backoff.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, 2*time.Minute)
	}
}

func (s *ScaleSet) listen(ctx context.Context, owner string, maxRunners int, sink Sink) error {
	session, err := s.client.scaleSet.MessageSessionClient(ctx, s.id, owner)
	if err != nil {
		return fmt.Errorf("opening a message session: %w", err)
	}
	defer func() {
		if err := session.Close(context.WithoutCancel(ctx)); err != nil {
			s.log.Warn("closing the message session", "error", err)
		}
	}()
	l, err := listener.New(session, listener.Config{ScaleSetID: s.id, MaxRunners: maxRunners,
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
	if draining || len(m.JobAvailableMessages) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(m.JobAvailableMessages))
	for _, j := range m.JobAvailableMessages {
		ids = append(ids, j.RunnerRequestID)
	}
	if _, err := s.session.AcquireJobs(ctx, ids); err != nil {
		return fmt.Errorf("acquiring jobs: %w", err)
	}
	return nil
}
