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
			obs: Observation{Now: now, Demand: new(4), Runners: []Runner{},
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
			name: "unregistered: a machine past the registration timeout without a registration",
			obs: Observation{Now: now, Demand: new(2), Runners: []Runner{},
				Machines: []Machine{machine("a", Running, 11*time.Minute), machine("b", Pending, 11*time.Minute),
					machine("c", Running, 9*time.Minute)}},
			want: Decision{Create: 1, Wanted: 2, Serving: 1, Present: 3, Deletions: []Deletion{
				{Machine: "a", Reason: Unregistered}, {Machine: "b", Reason: Unregistered}}},
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
	want := `{"deletions":[],"removals":[],"create":1,"wanted":1,"serving":0,"present":0}`
	if string(b) != want {
		t.Errorf("got %s, want %s", b, want)
	}
}
