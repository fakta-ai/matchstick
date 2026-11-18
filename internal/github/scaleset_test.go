package github

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

// ensure ensures the pool's scale set in the runner group, as run does.
func ensure(ctx context.Context, c *Client, pool, group string, labels ...string) (*ScaleSet, error) {
	s := c.ScaleSet("ci", pool, group, labels)
	return s, s.Ensure(ctx)
}

// find finds the pool's scale set, its runner group being "linux", as status does.
func find(t *testing.T, c *Client, pool string) *ScaleSet {
	t.Helper()
	s := c.ScaleSet("ci", pool, "linux", nil)
	if err := s.Find(t.Context()); err != nil {
		t.Fatal(err)
	}
	return s
}

// idOf is the ID of a scale set found or ensured.
func idOf(t *testing.T, s *ScaleSet) int {
	t.Helper()
	id, err := s.id()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestEnsureCreatesAMissingScaleSetOnce(t *testing.T) {
	f := newFakeGitHub(t)
	f.addScaleSet("other-build", nil, nil)
	c := f.client()
	s, err := ensure(t.Context(), c, "build", "linux", "self-hosted", "x64")
	if err != nil {
		t.Fatal(err)
	}
	created := f.log("create", "update")
	if !slices.Equal(created, []string{"create ci-build group=1 labels=self-hosted,x64 disableUpdate=true"}) {
		t.Fatalf("GitHub was asked %q", created)
	}
	again, err := ensure(t.Context(), c, "build", "linux", "x64", "self-hosted")
	if err != nil {
		t.Fatal(err)
	}
	if got := f.log("create", "update"); len(got) != 1 {
		t.Errorf("a second run changed the scale set: %q", got)
	}
	if idOf(t, again) != idOf(t, s) {
		t.Errorf("the second run found scale set %d, the first created %d", idOf(t, again), idOf(t, s))
	}
	// The scale set ensured is the created one: runners register in it.
	if _, err := s.JITConfig(t.Context(), "ci-build-x"); err != nil {
		t.Fatal(err)
	}
	if got := f.log("register"); len(got) != 1 || got[0] != "register ci-build-x in "+strconv.Itoa(idOf(t, s)) {
		t.Errorf("registered %q; scale set %d", got, idOf(t, s))
	}
}

func TestEnsureUpdatesChangedLabels(t *testing.T) {
	f := newFakeGitHub(t)
	id := f.addScaleSet("ci-build", []string{"self-hosted", "arm64"}, nil)
	s, err := ensure(t.Context(), f.client(), "build", "linux", "self-hosted", "x64")
	if err != nil {
		t.Fatal(err)
	}
	if got := f.log("create", "update"); !slices.Equal(got,
		[]string{"update " + strconv.Itoa(id) + " group=1 labels=self-hosted,x64"}) {
		t.Errorf("GitHub was asked %q", got)
	}
	if idOf(t, s) != id {
		t.Errorf("scale set %d, want %d", idOf(t, s), id)
	}
}

func TestEachPoolsScaleSetGoesInItsRunnerGroupAndIsFoundThereAfterARestart(t *testing.T) {
	f := newFakeGitHub(t)
	f.addGroup("default")
	f.addGroup("deploy")
	pools := map[string]string{"build": "default", "test": "default", "deploy": "deploy", "release": "deploy"}
	ids := map[string]int{}
	for pool, group := range pools {
		s, err := ensure(t.Context(), f.client(), pool, group, pool)
		if err != nil {
			t.Fatal(err)
		}
		ids[pool] = idOf(t, s)
		if got := f.groupOf(ids[pool]); got != group || s.Group() != group {
			t.Errorf("pool %s: scale set in %q, reported in %q; want %q", pool, got, s.Group(), group)
		}
	}
	if got := f.log("create"); len(got) != 4 {
		t.Errorf("created %q", got)
	}
	// A restart: a new client finds each scale set in its own group, changing nothing and looking nowhere else.
	before := len(f.log(""))
	for pool, group := range pools {
		s, err := ensure(t.Context(), f.client(), pool, group, pool)
		if err != nil {
			t.Fatal(err)
		}
		if idOf(t, s) != ids[pool] || s.Group() != group {
			t.Errorf("pool %s after a restart: scale set %d in %q, want %d in %q", pool, idOf(t, s), s.Group(),
				ids[pool], group)
		}
	}
	for _, e := range f.log("")[before:] {
		if strings.HasPrefix(e, "create") || strings.HasPrefix(e, "update") || e == "list groups" {
			t.Errorf("after a restart GitHub was asked %q", e)
		}
	}
}

func TestEnsureMovesAScaleSetWhosePoolChangedRunnerGroup(t *testing.T) {
	f := newFakeGitHub(t)
	id := f.addScaleSetIn("default", "ci-deploy", []string{"deploy"}, nil)
	deploy := f.addGroup("deploy")
	c := f.client()
	// status, before run: it finds the scale set where it still is and changes nothing.
	s := c.ScaleSet("ci", "deploy", "deploy", []string{"deploy"})
	if err := s.Find(t.Context()); err != nil || idOf(t, s) != id || s.Group() != "default" {
		t.Fatalf("found %v in %q, %v", s.found.Load(), s.Group(), err)
	}
	if got := f.log("create", "update"); len(got) != 0 {
		t.Fatalf("finding asked GitHub %q", got)
	}
	// run: the same scale set moves to the new group, keeping its ID; nothing is created.
	s, err := ensure(t.Context(), c, "deploy", "deploy", "deploy")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"update " + strconv.Itoa(id) + " group=" + strconv.Itoa(deploy) + " labels=deploy"}
	if got := f.log("create", "update"); !slices.Equal(got, want) {
		t.Errorf("GitHub was asked %q, want %q", got, want)
	}
	if idOf(t, s) != id || f.groupOf(id) != "deploy" || s.Group() != "deploy" {
		t.Errorf("scale set %d in %q (reported %q), want %d in deploy", idOf(t, s), f.groupOf(id), s.Group(), id)
	}
	if _, err := s.JITConfig(t.Context(), "ci-deploy-x"); err != nil {
		t.Fatal(err)
	}
	if got := f.log("register"); !slices.Equal(got, []string{"register ci-deploy-x in " + strconv.Itoa(id)}) {
		t.Errorf("registered %q", got)
	}
	// The next start finds it in its new group and changes nothing.
	if _, err := ensure(t.Context(), f.client(), "deploy", "deploy", "deploy"); err != nil {
		t.Fatal(err)
	}
	if got := f.log("create", "update"); len(got) != 1 {
		t.Errorf("GitHub was asked %q", got)
	}
}

func TestEnsureChangesNothingWhenItCannotTellWhereTheScaleSetIs(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(f *fakeGitHub)
		want  string
	}{
		"no such runner group": {func(*fakeGitHub) {}, `runner group "deploy"`},
		"a group the credential cannot use": {func(f *fakeGitHub) {
			f.addGroup("deploy")
			f.configure(func() { f.refuseGroups["deploy"] = true })
		}, `runner group "deploy"`},
		"the other groups cannot be listed": {func(f *fakeGitHub) {
			f.addGroup("deploy")
			f.configure(func() { f.failGroupList = true })
		}, "other runner groups"},
		"in two other groups": {func(f *fakeGitHub) {
			f.addGroup("deploy")
			f.addScaleSetIn("default", "ci-deploy", nil, nil)
			f.addScaleSetIn("linux", "ci-deploy", nil, nil)
		}, "runner groups default, linux; delete all but one"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeGitHub(t)
			tc.setup(f)
			s, err := ensure(t.Context(), f.client(), "deploy", "deploy", "deploy")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %v, want one mentioning %q", err, tc.want)
			}
			if got := f.log("create", "update"); len(got) != 0 {
				t.Errorf("GitHub was asked %q", got)
			}
			if _, err := s.JITConfig(t.Context(), "ci-deploy-x"); err == nil {
				t.Error("a runner registered in a scale set never ensured")
			}
		})
	}
}

func TestFindChangesNothing(t *testing.T) {
	f := newFakeGitHub(t)
	f.addScaleSet("ci-test", nil, nil)
	c := f.client()
	if err := c.ScaleSet("ci", "build", "linux", nil).Find(t.Context()); !errors.Is(err, ErrNoScaleSet) {
		t.Errorf("missing scale set: %v", err)
	}
	if got := f.log("create", "update"); len(got) != 0 {
		t.Errorf("GitHub was asked %q", got)
	}
	if s := find(t, c, "test"); idOf(t, s) != 101 || s.Group() != "linux" {
		t.Errorf("found %d in %q", idOf(t, s), s.Group())
	}
}

func TestDemandIsTheScaleSetsAssignedJobs(t *testing.T) {
	f := newFakeGitHub(t)
	f.addScaleSet("ci-busy", nil, &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 7, TotalRunningJobs: 3})
	f.addScaleSet("ci-new", nil, nil)
	c := f.client()
	for pool, want := range map[string]*int{"busy": new(7), "new": nil} {
		s := find(t, c, pool)
		got, err := s.Demand(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if (got == nil) != (want == nil) || got != nil && *got != *want {
			t.Errorf("%s: demand %v, want %v", pool, got, want)
		}
	}
}

func TestJITConfigReturnsTheRegisteredRunner(t *testing.T) {
	f := newFakeGitHub(t)
	f.addScaleSet("ci-build", nil, nil)
	s := find(t, f.client(), "build")
	jit, err := s.JITConfig(t.Context(), "ci-build-1")
	if err != nil || jit.RunnerID != 7001 || jit.Config != "jit-ci-build-1" {
		t.Errorf("got %+v, %v", jit, err)
	}
}

func TestRemoveMapsGitHubsAnswer(t *testing.T) {
	f := newFakeGitHub(t)
	f.addScaleSet("ci-build", nil, nil)
	f.configure(func() {
		f.removals[2] = fakeReply{http.StatusNotFound, `{"typeName":"GitHub.DistributedTask.WebApi.` +
			`AgentNotFoundException, GitHub.DistributedTask.WebApi","message":"No agent found"}`}
		f.removals[3] = fakeReply{http.StatusNotFound, ""}
		f.removals[4] = fakeReply{http.StatusBadRequest, `{"typeName":"GitHub.DistributedTask.WebApi.` +
			`JobStillRunningException, GitHub.DistributedTask.WebApi","message":"The agent is running a job"}`}
		f.removals[5] = fakeReply{http.StatusForbidden, `{"typeName":"AccessDeniedException","message":"denied"}`}
	})
	s := find(t, f.client(), "build")
	for id, want := range map[int64]string{1: "removed", 2: "removed", 3: "removed", 4: "running", 5: "error"} {
		err := s.Remove(t.Context(), id)
		got := "error"
		switch {
		case err == nil:
			got = "removed"
		case errors.Is(err, ErrJobRunning):
			got = "running"
		}
		if got != want {
			t.Errorf("runner %d: %s (%v), want %s", id, got, err, want)
		}
	}
}

// listen runs the listener for the pool's scale set, in the runner group "linux", until stop is called, which waits
// for it to return.
func listen(t *testing.T, c *Client, sk *sink) (stop func()) {
	t.Helper()
	return listenTo(t, c.ScaleSet("ci", "build", "linux", nil), sk)
}

func listenTo(t *testing.T, s *ScaleSet, sk *sink) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Listen(ctx, 5, sk)
	}()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("the listener did not stop")
		}
	}
}

func TestListenRecordsDemandAndAcquiresOfferedJobs(t *testing.T) {
	f := newFakeGitHub(t)
	f.addScaleSet("ci-build", nil, nil)
	f.configure(func() { f.sessionAssigned = 2 })
	stop := listen(t, f.client(), &sink{f: f})
	f.waitForPolls(1)
	f.messages <- fakeMessage{id: 1, assigned: 4, jobs: []int64{11, 12}}
	f.waitForPolls(2)
	f.messages <- fakeMessage{id: 2, assigned: 3}
	f.waitForPolls(3)
	stop()
	want := []string{
		"session opened", "demand 2", "poll capacity=5",
		"demand 4", "acquire [11 12]", "poll capacity=5",
		"demand 3", "poll capacity=5",
		"session closed",
	}
	if got := f.log("session", "demand", "acquire", "poll", "backoff"); !slices.Equal(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestListenWhileDrainingAcquiresNothingAndReportsNoCapacity(t *testing.T) {
	f := newFakeGitHub(t)
	f.addScaleSet("ci-build", nil, nil)
	f.configure(func() { f.sessionAssigned = 3 })
	sk := &sink{f: f, draining: true}
	stop := listen(t, f.client(), sk)
	f.waitForPolls(1)
	f.messages <- fakeMessage{id: 1, assigned: 4, jobs: []int64{21}}
	f.waitForPolls(2)
	sk.drain(false)
	f.messages <- fakeMessage{id: 2, assigned: 5, jobs: []int64{22}}
	f.waitForPolls(3)
	stop()
	want := []string{
		"session opened", "demand 3", "poll capacity=0",
		"demand 4", "poll capacity=0",
		"demand 5", "acquire [22]", "poll capacity=5",
		"session closed",
	}
	if got := f.log("session", "demand", "acquire", "poll", "backoff"); !slices.Equal(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

// backoffs makes the listener's backoff timer fire at once, moving the clock on by the wait.
func backoffs(f *fakeGitHub, c *Client) {
	c.after = func(d time.Duration) <-chan time.Time {
		f.record("backoff " + d.String())
		f.advance(d)
		ch := make(chan time.Time, 1)
		ch <- f.now()
		return ch
	}
}

func TestListenRetriesARefusedSessionWithBackoffKnowingNoDemand(t *testing.T) {
	f := newFakeGitHub(t)
	f.addScaleSet("ci-build", nil, nil)
	f.configure(func() { f.refuseSessions, f.sessionAssigned = 7, 2 })
	c := f.client()
	backoffs(f, c)
	stop := listen(t, c, &sink{f: f})
	f.waitForPolls(1)
	stop()
	want := []string{
		"session refused", "backoff 5s", "session refused", "backoff 10s", "session refused", "backoff 20s",
		"session refused", "backoff 40s", "session refused", "backoff 1m20s", "session refused", "backoff 2m0s",
		"session refused", "backoff 2m0s",
		"session opened", "demand 2", "poll capacity=5", "session closed",
	}
	if got := f.log("session", "demand", "acquire", "poll", "backoff"); !slices.Equal(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestListenBackoffStartsOverAfterALongSession(t *testing.T) {
	f := newFakeGitHub(t)
	f.addScaleSet("ci-build", nil, nil)
	f.configure(func() { f.refuseSessions = 1 })
	c := f.client()
	backoffs(f, c)
	f.messages <- fakeMessage{status: http.StatusBadRequest}                           // a session that fails at once
	f.messages <- fakeMessage{status: http.StatusBadRequest, advance: 6 * time.Minute} // one that fails after six minutes
	stop := listen(t, c, &sink{f: f})
	f.waitForPolls(3)
	stop()
	want := []string{
		"session refused", "backoff 5s",
		"session opened", "poll capacity=5", "session closed", "backoff 10s",
		"session opened", "poll capacity=5", "session closed", "backoff 5s",
		"session opened", "poll capacity=5", "session closed",
	}
	if got := f.log("session", "poll", "backoff"); !slices.Equal(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

// syncBuffer collects log lines written from several goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestAPoolWithoutItsRunnerGroupKnowsNoDemandWhileTheOthersListen(t *testing.T) {
	f := newFakeGitHub(t)
	f.addScaleSet("ci-build", nil, nil)
	f.configure(func() { f.sessionAssigned = 2 })
	c := f.client()
	var logs syncBuffer
	c.log = slog.New(slog.NewTextHandler(&logs, nil))
	retry := make(chan time.Time)
	c.after = func(d time.Duration) <-chan time.Time {
		f.record("backoff " + d.String())
		return retry
	}
	stopDeploy := listenTo(t, c.ScaleSet("ci", "deploy", "deploy", []string{"deploy"}), &sink{f: f, pool: "deploy"})
	stopBuild := listenTo(t, c.ScaleSet("ci", "build", "linux", nil), &sink{f: f, pool: "build"})
	f.waitFor("demand build", 1)
	f.waitFor("backoff", 1)
	if got := f.log("create", "demand", "backoff"); !slices.Equal(got, []string{"backoff 5s", "demand build 2"}) &&
		!slices.Equal(got, []string{"demand build 2", "backoff 5s"}) {
		t.Errorf("with the deploy group missing, GitHub was asked and the pools told %q", got)
	}
	for _, want := range []string{"the pool cannot scale", "pool=deploy", "runnerGroup=deploy",
		`no runner group found with name \"deploy\"`} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("the log lacks %q:\n%s", want, logs.String())
		}
	}
	// Once the group exists, the next attempt creates the scale set there and listens.
	group := f.addGroup("deploy")
	retry <- time.Time{}
	f.waitFor("demand deploy", 1)
	stopDeploy()
	stopBuild()
	if got := f.log("create"); !slices.Equal(got,
		[]string{"create ci-deploy group=" + strconv.Itoa(group) + " labels=deploy disableUpdate=true"}) {
		t.Errorf("created %q", got)
	}
}
