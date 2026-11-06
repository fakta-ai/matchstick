package github

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

func TestEnsureScaleSetCreatesAMissingScaleSetOnce(t *testing.T) {
	f := newFakeGitHub(t)
	f.addScaleSet("other-build", nil, nil)
	c := f.client()
	s, err := c.EnsureScaleSet(t.Context(), "ci", "build", "linux", []string{"self-hosted", "x64"})
	if err != nil {
		t.Fatal(err)
	}
	created := f.log("create", "update")
	if !slices.Equal(created, []string{"create ci-build group=1 labels=self-hosted,x64 disableUpdate=true"}) {
		t.Fatalf("GitHub was asked %q", created)
	}
	again, err := c.EnsureScaleSet(t.Context(), "ci", "build", "linux", []string{"x64", "self-hosted"})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.log("create", "update"); len(got) != 1 {
		t.Errorf("a second run changed the scale set: %q", got)
	}
	if again.id != s.id {
		t.Errorf("the second run found scale set %d, the first created %d", again.id, s.id)
	}
	// The returned scale set is the created one: runners register in it.
	if _, err := s.JITConfig(t.Context(), "ci-build-x"); err != nil {
		t.Fatal(err)
	}
	if got := f.log("register"); len(got) != 1 || got[0] != "register ci-build-x in "+strconv.Itoa(s.id) {
		t.Errorf("registered %q; scale set %d", got, s.id)
	}
}

func TestEnsureScaleSetUpdatesChangedLabels(t *testing.T) {
	f := newFakeGitHub(t)
	id := f.addScaleSet("ci-build", []string{"self-hosted", "arm64"}, nil)
	s, err := f.client().EnsureScaleSet(t.Context(), "ci", "build", "linux", []string{"self-hosted", "x64"})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.log("create", "update"); !slices.Equal(got, []string{"update " + strconv.Itoa(id) + " labels=self-hosted,x64"}) {
		t.Errorf("GitHub was asked %q", got)
	}
	if s.id != id {
		t.Errorf("scale set %d, want %d", s.id, id)
	}
}

func TestEnsureScaleSetFailsWithoutTheRunnerGroup(t *testing.T) {
	f := newFakeGitHub(t)
	if _, err := f.client().EnsureScaleSet(t.Context(), "ci", "build", "linux", []string{"x64"}); err == nil {
		t.Error("no error for a missing runner group")
	}
	if got := f.log("create", "update"); len(got) != 0 {
		t.Errorf("GitHub was asked %q", got)
	}
}

func TestFindScaleSetChangesNothing(t *testing.T) {
	f := newFakeGitHub(t)
	f.addScaleSet("ci-test", nil, nil)
	c := f.client()
	if _, err := c.FindScaleSet(t.Context(), "ci", "build", "linux"); !errors.Is(err, ErrNoScaleSet) {
		t.Errorf("missing scale set: %v", err)
	}
	if got := f.log("create", "update"); len(got) != 0 {
		t.Errorf("GitHub was asked %q", got)
	}
	s, err := c.FindScaleSet(t.Context(), "ci", "test", "linux")
	if err != nil || s.id != 101 {
		t.Errorf("found %+v, %v", s, err)
	}
}

func TestDemandIsTheScaleSetsAssignedJobs(t *testing.T) {
	f := newFakeGitHub(t)
	f.addScaleSet("ci-busy", nil, &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 7, TotalRunningJobs: 3})
	f.addScaleSet("ci-new", nil, nil)
	c := f.client()
	for pool, want := range map[string]*int{"busy": new(7), "new": nil} {
		s, err := c.FindScaleSet(t.Context(), "ci", pool, "linux")
		if err != nil {
			t.Fatal(err)
		}
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
	s, err := f.client().FindScaleSet(t.Context(), "ci", "build", "linux")
	if err != nil {
		t.Fatal(err)
	}
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
	s, err := f.client().FindScaleSet(t.Context(), "ci", "build", "linux")
	if err != nil {
		t.Fatal(err)
	}
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

// listen runs the listener for the pool's scale set until stop is called, which waits for it to return.
func listen(t *testing.T, c *Client, sk *sink) (stop func()) {
	t.Helper()
	s, err := c.FindScaleSet(t.Context(), "ci", "build", "linux")
	if err != nil {
		t.Fatal(err)
	}
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
