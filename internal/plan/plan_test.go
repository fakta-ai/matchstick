package plan

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

var now = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

var limits = Limits{Maximum: 4, Spares: 0, RegistrationTimeout: Duration(10 * time.Minute)}

func ago(d time.Duration) time.Time { return now.Add(-d) }

// machine returns a machine created the given time ago, expiring three hours after creation.
func machine(name string, state MachineState, age time.Duration) Machine {
	return Machine{Name: name, State: state, CreatedAt: ago(age), ExpiresAt: ago(age).Add(3 * time.Hour)}
}

func runner(id int64, name string, age time.Duration, busy bool) Runner {
	return Runner{ID: id, Name: name, CreatedAt: ago(age), Online: true, Busy: busy}
}

func TestDecide(t *testing.T) {
	tests := []struct {
		name   string
		obs    Observation
		limits Limits
		want   Decision
	}{
		{
			name: "creates for demand",
			obs:  Observation{Now: now, Runners: []Runner{}, Demand: new(2)},
			want: Decision{Create: 2, Wanted: 2},
		},
		{
			name:   "adds spares within the maximum",
			obs:    Observation{Now: now, Runners: []Runner{}, Demand: new(3)},
			limits: Limits{Maximum: 4, Spares: 2, RegistrationTimeout: limits.RegistrationTimeout},
			want:   Decision{Create: 4, Wanted: 4},
		},
		{
			name: "counts pending and running machines as serving",
			obs: Observation{Now: now, Demand: new(2),
				Machines: []Machine{machine("a", Pending, time.Minute), machine("b", Running, time.Minute)},
				Runners:  []Runner{runner(2, "b", time.Minute, true)}},
			want: Decision{Wanted: 2, Serving: 2, Present: 2},
		},
		{
			name: "never exceeds the maximum while machines are being deleted",
			obs: Observation{Now: now, Demand: new(4), Runners: []Runner{runner(3, "c", time.Minute, true)},
				Machines: []Machine{machine("a", Deleting, time.Hour), machine("b", Stopped, time.Hour),
					machine("c", Running, time.Minute)}},
			want: Decision{Create: 1, Wanted: 4, Serving: 1, Present: 3,
				Deletions: []Deletion{{Machine: "b", Reason: Finished}}},
		},
		{
			name: "creates nothing until demand is known, but still cleans up",
			obs: Observation{Now: now, Runners: []Runner{runner(1, "a", time.Hour, false)},
				Machines: []Machine{machine("a", Stopped, time.Hour)}},
			want: Decision{Present: 1, Deletions: []Deletion{{Machine: "a", Reason: Finished, RunnerID: 1}}},
		},
		{
			name: "creates nothing while paused",
			obs:  Observation{Now: now, Runners: []Runner{}, Demand: new(2), CreationPaused: true},
			want: Decision{Wanted: 2},
		},
		{
			name: "finished: a stopped or failed machine is deleted with its registration",
			obs: Observation{Now: now, Demand: new(0),
				Machines: []Machine{machine("a", Stopped, time.Hour), machine("b", Failed, time.Minute)},
				Runners:  []Runner{{ID: 1, Name: "a", CreatedAt: ago(time.Hour)}}},
			want: Decision{Present: 2, Deletions: []Deletion{
				{Machine: "a", Reason: Finished, RunnerID: 1}, {Machine: "b", Reason: Finished}}},
		},
		{
			name: "expired: deleted even mid-job",
			obs: Observation{Now: now, Demand: new(1),
				Machines: []Machine{machine("a", Running, 3*time.Hour)},
				Runners:  []Runner{runner(1, "a", 3*time.Hour, true)}},
			want: Decision{Create: 1, Wanted: 1, Present: 1, Deletions: []Deletion{{Machine: "a", Reason: Expired, RunnerID: 1}}},
		},
		{
			name: "unregistered: a machine still pending past the registration timeout",
			obs: Observation{Now: now, Demand: new(2), Runners: []Runner{},
				Machines: []Machine{machine("a", Pending, 11*time.Minute), machine("b", Pending, 9*time.Minute)}},
			want: Decision{Create: 1, Wanted: 2, Serving: 1, Present: 2, Deletions: []Deletion{
				{Machine: "a", Reason: Unregistered}}},
		},
		{
			// GitHub removes a just-in-time runner's registration when its job ends, however long the job ran.
			name: "a running machine whose registration is gone ran its job: kept to power off, for the timeout",
			obs: Observation{Now: now, Demand: new(0), Runners: []Runner{},
				Machines: []Machine{machine("ended", Running, 40*time.Minute),
					machine("powering-off", Running, 40*time.Minute), machine("lingering", Running, 40*time.Minute),
					machine("unseen", Running, 11*time.Minute)},
				Deregistered: map[string]time.Time{"ended": now, "powering-off": ago(9 * time.Minute),
					"lingering": ago(11 * time.Minute)}},
			want: Decision{Present: 4, Deletions: []Deletion{
				{Machine: "lingering", Reason: Finished}, {Machine: "unseen", Reason: Finished}}},
		},
		{
			name:   "a machine whose job ended is no spare: the real spare is kept, not deleted as surplus",
			limits: Limits{Maximum: 4, Spares: 1, RegistrationTimeout: limits.RegistrationTimeout},
			obs: Observation{Now: now, Demand: new(0),
				Machines: []Machine{machine("ended", Running, 40*time.Minute),
					machine("spare", Running, 5*time.Minute)},
				Runners:      []Runner{runner(1, "spare", 5*time.Minute, false)},
				Deregistered: map[string]time.Time{"ended": now}},
			want: Decision{Wanted: 1, Serving: 1, Present: 2},
		},
		{
			name: "a machine whose job ended serves no queued job: a replacement is created within the maximum",
			obs: Observation{Now: now, Demand: new(1), Runners: []Runner{},
				Machines:     []Machine{machine("ended", Running, 40*time.Minute)},
				Deregistered: map[string]time.Time{"ended": now}},
			want: Decision{Create: 1, Wanted: 1, Present: 1},
		},
		{
			name:   "a machine whose job ended still counts toward the maximum",
			limits: Limits{Maximum: 1, RegistrationTimeout: limits.RegistrationTimeout},
			obs: Observation{Now: now, Demand: new(1), Runners: []Runner{},
				Machines:     []Machine{machine("ended", Running, 40*time.Minute)},
				Deregistered: map[string]time.Time{"ended": now}},
			want: Decision{Wanted: 1, Present: 1},
		},
		{
			name: "draining leaves a machine whose job ended to power off",
			obs: Observation{Now: now, Demand: new(1), Draining: true, Runners: []Runner{},
				Machines:     []Machine{machine("ended", Running, 40*time.Minute)},
				Deregistered: map[string]time.Time{"ended": now}},
			want: Decision{Present: 1},
		},
		{
			name: "unregistered: a machine whose runner never came online, with its registration",
			obs: Observation{Now: now, Demand: new(1),
				Machines: []Machine{machine("a", Running, 11*time.Minute), machine("b", Running, 9*time.Minute)},
				Runners: []Runner{{ID: 1, Name: "a", CreatedAt: ago(11 * time.Minute)},
					{ID: 2, Name: "b", CreatedAt: ago(9 * time.Minute)}}},
			want: Decision{Wanted: 1, Serving: 1, Present: 2, Deletions: []Deletion{
				{Machine: "a", Reason: Unregistered, RunnerID: 1}}},
		},
		{
			name: "unregistered rule is skipped while the runner list is unknown",
			obs: Observation{Now: now, Demand: new(1),
				Machines: []Machine{machine("a", Running, time.Hour), machine("b", Stopped, time.Hour)}},
			want: Decision{Wanted: 1, Serving: 1, Present: 2, Deletions: []Deletion{{Machine: "b", Reason: Finished}}},
		},
		{
			name: "orphan: a registration past the timeout without a machine",
			obs: Observation{Now: now, Demand: new(0),
				Runners: []Runner{{ID: 1, Name: "old", CreatedAt: ago(11 * time.Minute)},
					{ID: 2, Name: "young", CreatedAt: ago(time.Minute)}}},
			want: Decision{Removals: []Removal{{RunnerID: 1, Name: "old", Reason: Orphan}}},
		},
		{
			name: "surplus: idle registrations go first, oldest first, guarded",
			obs: Observation{Now: now, Demand: new(1),
				Machines: []Machine{machine("new", Running, time.Minute), machine("old", Running, time.Hour),
					machine("busy", Running, 2*time.Hour), machine("boot", Pending, time.Minute)},
				Runners: []Runner{runner(1, "new", time.Minute, false), runner(2, "old", time.Hour, false),
					runner(3, "busy", 2*time.Hour, true)}},
			want: Decision{Wanted: 1, Serving: 2, Present: 4, Deletions: []Deletion{
				{Machine: "old", Reason: Surplus, RunnerID: 2, Guarded: true},
				{Machine: "new", Reason: Surplus, RunnerID: 1, Guarded: true}}},
		},
		{
			name: "surplus never takes offline or busy runners",
			obs: Observation{Now: now, Demand: new(0),
				Machines: []Machine{machine("a", Running, time.Minute), machine("b", Running, time.Minute)},
				Runners: []Runner{{ID: 1, Name: "a", CreatedAt: ago(time.Minute)},
					runner(2, "b", time.Minute, true)}},
			want: Decision{Serving: 2, Present: 2},
		},
		{
			name: "drain: wants nothing, gives up idle runners, keeps busy ones",
			obs: Observation{Now: now, Demand: new(3), Draining: true,
				Machines: []Machine{machine("idle", Running, time.Minute), machine("busy", Running, time.Minute)},
				Runners:  []Runner{runner(1, "idle", time.Minute, false), runner(2, "busy", time.Minute, true)}},
			want: Decision{Serving: 1, Present: 2,
				Deletions: []Deletion{{Machine: "idle", Reason: Surplus, RunnerID: 1, Guarded: true}}},
		},
		{
			name: "drain needs no demand",
			obs: Observation{Now: now, Draining: true,
				Machines: []Machine{machine("idle", Running, time.Minute)},
				Runners:  []Runner{runner(1, "idle", time.Minute, false)}},
			want: Decision{Present: 1,
				Deletions: []Deletion{{Machine: "idle", Reason: Surplus, RunnerID: 1, Guarded: true}}},
		},
		{
			name: "a deleting machine is left alone",
			obs: Observation{Now: now, Demand: new(0), Runners: []Runner{},
				Machines: []Machine{machine("a", Deleting, 5*time.Hour)}},
			want: Decision{Present: 1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := tt.limits
			if l == (Limits{}) {
				l = limits
			}
			got := Decide(tt.obs, l)
			if tt.want.Deletions == nil {
				tt.want.Deletions = []Deletion{}
			}
			if tt.want.Removals == nil {
				tt.want.Removals = []Removal{}
			}
			// Without an idle timeout the pool keeps its spares always.
			if tt.want.Spares == (Spares{}) {
				tt.want.Spares = Spares{Count: l.Spares, Reason: SparesAlways}
				if tt.obs.Draining {
					tt.want.Spares = Spares{Reason: SparesDraining}
				}
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got  %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

// TestDecideNeverExceedsMaximum checks the bound over many observations: whatever was seen, the machines present and
// those created never exceed the maximum when anything is created.
func TestDecideNeverExceedsMaximum(t *testing.T) {
	states := []MachineState{Pending, Running, Stopped, Failed, Deleting}
	for n := range 6 {
		for demand := range 8 {
			for s := range len(states) {
				var machines []Machine
				for i := range n {
					machines = append(machines, machine(string(rune('a'+i)), states[(s+i)%len(states)], time.Minute))
				}
				o := Observation{Now: now, Machines: machines, Runners: []Runner{}, Demand: new(demand)}
				d := Decide(o, limits)
				if d.Create > 0 && d.Present+d.Create > limits.Maximum {
					t.Fatalf("n=%d demand=%d: present %d + create %d > maximum", n, demand, d.Present, d.Create)
				}
				if d.Create < 0 {
					t.Fatalf("negative create")
				}
			}
		}
	}
}

func TestDecisionJSON(t *testing.T) {
	in := `{"now":"2026-11-03T12:00:00Z","machines":[],"runners":[],"demand":1}`
	var o Observation
	if err := json.Unmarshal([]byte(in), &o); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(Decide(o, limits))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"deletions":[],"removals":[],"create":1,"wanted":1,"spares":{"count":0,"reason":"always"},"serving":0,` +
		`"present":0}`
	if string(b) != want {
		t.Errorf("got %s, want %s", b, want)
	}
}

func TestDeregistered(t *testing.T) {
	o := Observation{Now: now,
		Machines: []Machine{machine("seen", Running, time.Hour), machine("new", Running, time.Hour),
			machine("registered", Running, time.Hour), machine("booting", Pending, time.Minute),
			machine("stopped", Stopped, time.Hour)},
		Runners: []Runner{runner(1, "registered", time.Hour, true)}}
	since := map[string]time.Time{"seen": ago(5 * time.Minute), "registered": ago(time.Minute), "gone": ago(time.Hour)}
	want := map[string]time.Time{"seen": ago(5 * time.Minute), "new": now}
	if got := Deregistered(since, o); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	// Without the runner list nothing can be told: the record stays as it was.
	o.Runners = nil
	if got := Deregistered(since, o); !reflect.DeepEqual(got, since) {
		t.Errorf("without the runner list: got %v, want %v", got, since)
	}
}

// idle is a pool that keeps two spares while it has jobs and for 30 minutes after the last.
var idle = Limits{Maximum: 4, Spares: 2, RegistrationTimeout: Duration(10 * time.Minute),
	IdleTimeout: Duration(30 * time.Minute)}

// spare returns an idle machine created the given time ago, with its registration online and idle.
func spare(id int64, name string, age time.Duration) (Machine, Runner) {
	return machine(name, Running, age), runner(id, name, age, false)
}

func TestIdleTimeout(t *testing.T) {
	s1, r1 := spare(1, "s1", 20*time.Minute)
	s2, r2 := spare(2, "s2", 10*time.Minute)
	expired, rExpired := spare(1, "old", 3*time.Hour)
	tests := []struct {
		name   string
		obs    Observation
		limits Limits
		want   Decision
	}{
		{
			name: "a pool that has had no job keeps no spares",
			obs:  Observation{Now: now, Demand: new(0), Runners: []Runner{}},
			want: Decision{Spares: Spares{Reason: SparesQuiet}},
		},
		{
			name: "a job opens the window: a machine for the job and the spares",
			obs:  Observation{Now: now, Demand: new(1), Runners: []Runner{}},
			want: Decision{Create: 3, Wanted: 3, Spares: Spares{Count: 2, Reason: SparesBusy, LastActive: now}},
		},
		{
			name: "a busy runner is a job even while demand reads 0",
			obs: Observation{Now: now, Demand: new(0), Machines: []Machine{machine("a", Running, time.Minute)},
				Runners: []Runner{runner(1, "a", time.Minute, true)}},
			want: Decision{Create: 1, Wanted: 2, Serving: 1, Present: 1,
				Spares: Spares{Count: 2, Reason: SparesBusy, LastActive: now}},
		},
		{
			name: "a job longer than the idle timeout keeps the window open",
			obs: Observation{Now: now, Demand: new(1), LastActive: ago(2 * time.Hour),
				Machines: []Machine{machine("a", Running, 2*time.Hour), s1, s2},
				Runners:  []Runner{runner(3, "a", 2*time.Hour, true), r1, r2}},
			want: Decision{Wanted: 3, Serving: 3, Present: 3,
				Spares: Spares{Count: 2, Reason: SparesBusy, LastActive: now}},
		},
		{
			name: "spares stay for the idle timeout after the last job ended",
			obs: Observation{Now: now, Demand: new(0), LastActive: ago(29 * time.Minute),
				Machines: []Machine{s1, s2}, Runners: []Runner{r1, r2}},
			want: Decision{Wanted: 2, Serving: 2, Present: 2, Spares: Spares{Count: 2, Reason: SparesRecent,
				LastActive: ago(29 * time.Minute), Until: now.Add(time.Minute)}},
		},
		{
			name: "right after a job: its machine powering off is no spare, and neither spare goes as surplus",
			obs: Observation{Now: now, Demand: new(0), LastActive: ago(time.Minute),
				Machines:     []Machine{machine("ended", Running, 40*time.Minute), s1, s2},
				Runners:      []Runner{r1, r2},
				Deregistered: map[string]time.Time{"ended": ago(time.Minute)}},
			want: Decision{Wanted: 2, Serving: 2, Present: 3, Spares: Spares{Count: 2, Reason: SparesRecent,
				LastActive: ago(time.Minute), Until: now.Add(29 * time.Minute)}},
		},
		{
			name: "once quiet for the idle timeout, the idle spares go, oldest first, guarded",
			obs: Observation{Now: now, Demand: new(0), LastActive: ago(30 * time.Minute),
				Machines: []Machine{s1, s2}, Runners: []Runner{r1, r2}},
			want: Decision{Present: 2, Spares: Spares{Reason: SparesQuiet, LastActive: ago(30 * time.Minute)},
				Deletions: []Deletion{{Machine: "s1", Reason: Surplus, RunnerID: 1, Guarded: true},
					{Machine: "s2", Reason: Surplus, RunnerID: 2, Guarded: true}}},
		},
		{
			name: "a burst larger than the spares: the rest are created on demand",
			obs: Observation{Now: now, Demand: new(5), LastActive: ago(time.Minute),
				Machines: []Machine{s1, s2}, Runners: []Runner{r1, r2}},
			limits: Limits{Maximum: 10, Spares: 2, RegistrationTimeout: idle.RegistrationTimeout,
				IdleTimeout: idle.IdleTimeout},
			want: Decision{Create: 5, Wanted: 7, Serving: 2, Present: 2,
				Spares: Spares{Count: 2, Reason: SparesBusy, LastActive: now}},
		},
		{
			name: "spares count toward the maximum",
			obs: Observation{Now: now, Demand: new(3), Runners: []Runner{},
				Machines: []Machine{machine("done", Stopped, time.Hour)}},
			want: Decision{Create: 3, Wanted: 4, Present: 1,
				Spares:    Spares{Count: 2, Reason: SparesBusy, LastActive: now},
				Deletions: []Deletion{{Machine: "done", Reason: Finished}}},
		},
		{
			name: "a spare past its maximum lifetime is replaced while the window is open",
			obs: Observation{Now: now, Demand: new(0), LastActive: ago(5 * time.Minute),
				Machines: []Machine{expired, s2}, Runners: []Runner{rExpired, r2}},
			want: Decision{Create: 1, Wanted: 2, Serving: 1, Present: 2,
				Spares: Spares{Count: 2, Reason: SparesRecent, LastActive: ago(5 * time.Minute),
					Until: now.Add(25 * time.Minute)},
				Deletions: []Deletion{{Machine: "old", Reason: Expired, RunnerID: 1}}},
		},
		{
			name: "a spare past its maximum lifetime is not replaced once the pool is quiet",
			obs: Observation{Now: now, Demand: new(0), LastActive: ago(time.Hour),
				Machines: []Machine{expired, s2}, Runners: []Runner{rExpired, r2}},
			want: Decision{Present: 2, Spares: Spares{Reason: SparesQuiet, LastActive: ago(time.Hour)},
				Deletions: []Deletion{{Machine: "old", Reason: Expired, RunnerID: 1},
					{Machine: "s2", Reason: Surplus, RunnerID: 2, Guarded: true}}},
		},
		{
			name: "an evicted Spot spare is replaced while the window is open",
			obs: Observation{Now: now, Demand: new(0), LastActive: ago(5 * time.Minute),
				Machines: []Machine{machine("s1", Stopped, 20*time.Minute), s2}, Runners: []Runner{r2}},
			want: Decision{Create: 1, Wanted: 2, Serving: 1, Present: 2,
				Spares: Spares{Count: 2, Reason: SparesRecent, LastActive: ago(5 * time.Minute),
					Until: now.Add(25 * time.Minute)},
				Deletions: []Deletion{{Machine: "s1", Reason: Finished}}},
		},
		{
			name: "an evicted Spot spare the cloud already deleted is replaced while the window is open",
			obs: Observation{Now: now, Demand: new(0), LastActive: ago(5 * time.Minute),
				Machines: []Machine{s2}, Runners: []Runner{r2}},
			want: Decision{Create: 1, Wanted: 2, Serving: 1, Present: 1,
				Spares: Spares{Count: 2, Reason: SparesRecent, LastActive: ago(5 * time.Minute),
					Until: now.Add(25 * time.Minute)}},
		},
		{
			name: "a spare that never registers is replaced while the window is open",
			obs: Observation{Now: now, Demand: new(0), LastActive: ago(5 * time.Minute),
				Machines: []Machine{machine("s1", Running, 11*time.Minute), s2},
				Runners:  []Runner{{ID: 3, Name: "s1", CreatedAt: ago(11 * time.Minute)}, r2}},
			want: Decision{Create: 1, Wanted: 2, Serving: 1, Present: 2,
				Spares: Spares{Count: 2, Reason: SparesRecent, LastActive: ago(5 * time.Minute),
					Until: now.Add(25 * time.Minute)},
				Deletions: []Deletion{{Machine: "s1", Reason: Unregistered, RunnerID: 3}}},
		},
		{
			name: "after a restart, the newest machine made before it stands for the last job",
			obs: Observation{Now: now, Demand: new(0), Started: ago(time.Minute),
				Machines: []Machine{s1, s2}, Runners: []Runner{r1, r2}},
			want: Decision{Wanted: 2, Serving: 2, Present: 2, Spares: Spares{Count: 2, Reason: SparesRecent,
				LastActive: ago(10 * time.Minute), Until: now.Add(20 * time.Minute)}},
		},
		{
			name: "after a restart, spares older than the idle timeout go",
			obs: Observation{Now: now, Demand: new(0), Started: ago(time.Minute),
				Machines: []Machine{machine("s1", Running, 40*time.Minute)},
				Runners:  []Runner{runner(1, "s1", 40*time.Minute, false)}},
			want: Decision{Present: 1, Spares: Spares{Reason: SparesQuiet, LastActive: ago(40 * time.Minute)},
				Deletions: []Deletion{{Machine: "s1", Reason: Surplus, RunnerID: 1, Guarded: true}}},
		},
		{
			name: "a machine made since the start is no evidence: a replacement does not extend the window",
			obs: Observation{Now: now, Demand: new(0), Started: ago(time.Hour), LastActive: ago(31 * time.Minute),
				Machines: []Machine{machine("s1", Running, 5*time.Minute)},
				Runners:  []Runner{runner(1, "s1", 5*time.Minute, false)}},
			want: Decision{Present: 1, Spares: Spares{Reason: SparesQuiet, LastActive: ago(31 * time.Minute)},
				Deletions: []Deletion{{Machine: "s1", Reason: Surplus, RunnerID: 1, Guarded: true}}},
		},
		{
			name: "demand unknown (no session, such as a second copy): nothing created, nothing scaled down",
			obs:  Observation{Now: now, LastActive: ago(time.Hour), Machines: []Machine{s1}, Runners: []Runner{r1}},
			want: Decision{Serving: 1, Present: 1, Spares: Spares{Reason: SparesQuiet, LastActive: ago(time.Hour)}},
		},
		{
			name: "demand unknown in an open window: nothing created",
			obs:  Observation{Now: now, LastActive: ago(time.Minute), Runners: []Runner{}},
			want: Decision{Spares: Spares{Count: 2, Reason: SparesRecent, LastActive: ago(time.Minute),
				Until: now.Add(29 * time.Minute)}},
		},
		{
			name: "draining keeps no spares",
			obs: Observation{Now: now, Demand: new(1), Draining: true, LastActive: now,
				Machines: []Machine{s1}, Runners: []Runner{r1}},
			want: Decision{Present: 1, Spares: Spares{Reason: SparesDraining},
				Deletions: []Deletion{{Machine: "s1", Reason: Surplus, RunnerID: 1, Guarded: true}}},
		},
		{
			name: "a clock set back keeps the spares the idle timeout past the last job it recorded",
			obs:  Observation{Now: now, Demand: new(0), LastActive: now.Add(time.Hour), Runners: []Runner{}},
			want: Decision{Create: 2, Wanted: 2, Spares: Spares{Count: 2, Reason: SparesRecent,
				LastActive: now.Add(time.Hour), Until: now.Add(90 * time.Minute)}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := tt.limits
			if l == (Limits{}) {
				l = idle
			}
			got := Decide(tt.obs, l)
			if tt.want.Deletions == nil {
				tt.want.Deletions = []Deletion{}
			}
			if tt.want.Removals == nil {
				tt.want.Removals = []Removal{}
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got  %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

// TestNoIdleTimeoutIgnoresHistory checks that a pool without an idle timeout decides the same whatever its process
// saw or when it started: it keeps its spares, demand plus spares within the maximum.
func TestNoIdleTimeoutIgnoresHistory(t *testing.T) {
	states := []MachineState{Pending, Running, Stopped, Failed, Deleting}
	fixed := Limits{Maximum: 4, Spares: 2, RegistrationTimeout: Duration(10 * time.Minute)}
	histories := []struct{ lastActive, started time.Time }{
		{}, {ago(time.Hour), ago(time.Minute)}, {ago(time.Minute), ago(2 * time.Hour)}, {now.Add(time.Hour), now},
	}
	for n := range 5 {
		for demand := range 6 {
			for s := range len(states) {
				var machines []Machine
				var runners []Runner
				for i := range n {
					name := string(rune('a' + i))
					machines = append(machines, machine(name, states[(s+i)%len(states)], time.Duration(i)*time.Hour))
					runners = append(runners, runner(int64(i+1), name, time.Duration(i)*time.Hour, i%2 == 0))
				}
				base := Observation{Now: now, Machines: machines, Runners: runners, Demand: new(demand)}
				want := Decide(base, fixed)
				if want.Spares != (Spares{Count: 2, Reason: SparesAlways}) {
					t.Fatalf("spares %+v, want always 2", want.Spares)
				}
				if want.Wanted != min(demand+2, 4) {
					t.Fatalf("demand %d: wanted %d, want %d", demand, want.Wanted, min(demand+2, 4))
				}
				for _, h := range histories {
					o := base
					o.LastActive, o.Started = h.lastActive, h.started
					if got := Decide(o, fixed); !reflect.DeepEqual(got, want) {
						t.Fatalf("history %+v changed the decision:\ngot  %+v\nwant %+v", h, got, want)
					}
				}
			}
		}
	}
}
