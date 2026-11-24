// Package plan decides what one pool should do now, from what was observed. It is pure: no calls, no clock, no
// randomness; the same observation always gives the same decision.
package plan

import (
	"cmp"
	"slices"
	"time"
)

// MachineState is a machine's state as its cloud reports it.
type MachineState string

const (
	// Pending means accepted by the cloud, not yet running.
	Pending MachineState = "pending"
	// Running means powered on.
	Running MachineState = "running"
	// Stopped means powered off. A runner image powers its machine off after its one job.
	Stopped MachineState = "stopped"
	// Failed means the cloud could not create or run it, or only parts of it remain.
	Failed MachineState = "failed"
	// Deleting means its deletion is under way.
	Deleting MachineState = "deleting"
)

// Machine is one of the pool's machines.
type Machine struct {
	Name      string       `json:"name"`
	State     MachineState `json:"state"`
	CreatedAt time.Time    `json:"createdAt"`
	ExpiresAt time.Time    `json:"expiresAt"`
}

// Runner is one of the pool's runner registrations with GitHub.
type Runner struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"` // from its name
	Online    bool      `json:"online"`
	Busy      bool      `json:"busy"`
}

// Limits are the pool's settings that the decision depends on.
type Limits struct {
	// Maximum is the most machines the pool may have at once, in any state.
	Maximum int `json:"maximum"`
	// Spares is how many idle machines to keep ready beyond demand.
	Spares int `json:"spares"`
	// RegistrationTimeout is how long a machine may run without its runner registered, and how long a registration
	// may exist without its machine.
	RegistrationTimeout Duration `json:"registrationTimeout"`
	// IdleTimeout, when set, keeps the spares only while the pool has a job, and for this long after its last job
	// ended; zero keeps them always.
	IdleTimeout Duration `json:"idleTimeout,omitempty"`
}

// Observation is what one round saw.
type Observation struct {
	Now      time.Time `json:"now"`
	Machines []Machine `json:"machines"`
	// Runners is nil when GitHub's runner list could not be read; the rules that need it are skipped.
	Runners []Runner `json:"runners"`
	// Demand is the number of jobs assigned to the pool, waiting or running; nil until the listener's first report.
	Demand *int `json:"demand"`
	// Draining pools create nothing and give up their idle runners.
	Draining bool `json:"draining,omitempty"`
	// CreationPaused is set while the pool backs off after capacity errors or machines that never register.
	CreationPaused bool `json:"creationPaused,omitempty"`
	// LastActive is when the pool's process last saw a job assigned to the pool, waiting or running, or saw the last
	// of them end; zero if it has seen none.
	LastActive time.Time `json:"lastActive,omitzero"`
	// Started is when the pool's process started. A machine created before it stands for a job the process did not
	// see: the pool had a job, or kept its spares, when that machine was created.
	Started time.Time `json:"started,omitzero"`
	// Deregistered is when the pool's process first saw each running machine without its runner's registration.
	// GitHub removes a just-in-time runner's registration when its job ends, so such a machine has run its job and is
	// on its way to power off. A running machine without its registration that is missing here counts from its
	// creation.
	Deregistered map[string]time.Time `json:"deregistered,omitempty"`
	// LastOnline is when the pool's process last saw each machine's runner online or busy, or GitHub refuse to remove
	// its registration because the runner is running a job. GitHub's runner list can show a runner offline and idle
	// while it runs a job, so a runner seen online once is never taken for one that never came online. A machine
	// missing here has not been seen so by this process.
	LastOnline map[string]time.Time `json:"lastOnline,omitempty"`
}

// SparesReason says why the pool keeps the spares it keeps now.
type SparesReason string

const (
	// SparesAlways means the pool has no idle timeout: it keeps its spares at all times.
	SparesAlways SparesReason = "always"
	// SparesBusy means the pool has a job assigned, waiting or running.
	SparesBusy SparesReason = "busy"
	// SparesRecent means the pool's last job ended less than the idle timeout ago.
	SparesRecent SparesReason = "recent"
	// SparesQuiet means the pool has had no job for the idle timeout: it keeps no spares.
	SparesQuiet SparesReason = "quiet"
	// SparesDraining means the pool drains: it keeps no spares.
	SparesDraining SparesReason = "draining"
)

// Spares is how many idle machines the pool keeps ready now, and why.
type Spares struct {
	Count  int          `json:"count"`
	Reason SparesReason `json:"reason"`
	// LastActive is the latest time the observation shows the pool with a job: now while it has one, else the later
	// of the time its process saw the last job end and the creation of its newest machine made before the process
	// started. Zero without an idle timeout, or when nothing shows a job.
	LastActive time.Time `json:"lastActive,omitzero"`
	// Until is when the spares go if no job comes first; only with reason recent.
	Until time.Time `json:"until,omitzero"`
}

// SparesNow returns how many spares the pool keeps now. Without an idle timeout that is always the pool's spares.
// With one, the pool keeps them while it has a job and for the idle timeout after the last job, and none after.
//
// The pool has a job while its demand is above zero or one of its runners is busy. The end of its last job is what
// its process saw; after a restart the process has seen nothing, and the newest machine made before it started
// stands in: such a machine was created for a job or as a spare, so the pool was active then.
func SparesNow(o Observation, l Limits) Spares {
	switch {
	case o.Draining:
		return Spares{Reason: SparesDraining}
	case l.IdleTimeout <= 0:
		return Spares{Count: l.Spares, Reason: SparesAlways}
	case o.Demand != nil && *o.Demand > 0 || slices.ContainsFunc(o.Runners, func(r Runner) bool { return r.Busy }):
		return Spares{Count: l.Spares, Reason: SparesBusy, LastActive: o.Now}
	}
	last := o.LastActive
	for _, m := range o.Machines {
		if m.CreatedAt.Before(o.Started) && m.CreatedAt.After(last) {
			last = m.CreatedAt
		}
	}
	if last.IsZero() {
		return Spares{Reason: SparesQuiet}
	}
	until := last.Add(time.Duration(l.IdleTimeout))
	if o.Now.Before(until) {
		return Spares{Count: l.Spares, Reason: SparesRecent, LastActive: last, Until: until}
	}
	return Spares{Reason: SparesQuiet, LastActive: last}
}

// Reason says why an action was decided.
type Reason string

const (
	// Finished means the machine stopped or failed, or its runner's job ended (GitHub removed its registration) and
	// the machine has not stopped within the registration timeout since.
	Finished Reason = "finished"
	// Expired means the machine outlived the pool's maximum lifetime.
	Expired Reason = "expired"
	// Unregistered means the machine's runner did not come online within the registration timeout, as far as the
	// pool's process saw: the machine is still pending, or its registration is offline and idle and was never seen
	// online or busy.
	Unregistered Reason = "unregistered"
	// Offline means the machine's runner came online and has been offline and idle for longer than the registration
	// timeout since the pool's process last saw it online or busy.
	Offline Reason = "offline"
	// Orphan means the registration has no machine.
	Orphan Reason = "orphan"
	// Surplus means the pool has more idle machines than it needs.
	Surplus Reason = "surplus"
)

// Deletion is a machine to delete.
type Deletion struct {
	Machine string `json:"machine"`
	Reason  Reason `json:"reason"`
	// RunnerID is the machine's registration, removed before the machine; 0 if it has none.
	RunnerID int64 `json:"runnerId,omitempty"`
	// Guarded deletions go ahead only once the registration is removed. GitHub refuses to remove the registration of
	// a runner running a job, so a guarded deletion never cuts a job short. Unguarded ones delete the machine whatever
	// GitHub answers.
	Guarded bool `json:"guarded"`
}

// Removal is a registration to remove that has no machine.
type Removal struct {
	RunnerID int64  `json:"runnerId"`
	Name     string `json:"name"`
	Reason   Reason `json:"reason"`
}

// Decision is what the pool should do now.
type Decision struct {
	Deletions []Deletion `json:"deletions"`
	Removals  []Removal  `json:"removals"`
	Create    int        `json:"create"`
	// Wanted is how many machines the pool should be serving: demand and spares, within the maximum.
	Wanted int `json:"wanted"`
	// Spares is how many idle machines the pool keeps ready now, and why; part of Wanted.
	Spares Spares `json:"spares"`
	// Serving counts machines pending or running that this decision keeps and that can take a job: a running machine
	// whose job ended (its registration gone) is left to power off and counts only in Present.
	Serving int `json:"serving"`
	// Present counts every machine listed, which all count against the maximum until they are gone.
	Present int `json:"present"`
}

// Decide returns what the pool should do now.
func Decide(o Observation, l Limits) Decision {
	d := Decision{Deletions: []Deletion{}, Removals: []Removal{}, Present: len(o.Machines), Spares: SparesNow(o, l)}
	timeout := time.Duration(l.RegistrationTimeout)

	runners := make(map[string]Runner, len(o.Runners))
	for _, r := range o.Runners {
		runners[r.Name] = r
	}
	machines := make(map[string]bool, len(o.Machines))
	var serving []Machine
	for _, m := range o.Machines {
		machines[m.Name] = true
		r, registered := runners[m.Name]
		deletion := Deletion{Machine: m.Name, RunnerID: r.ID}
		switch {
		case m.State == Deleting:
			continue
		case m.State == Stopped || m.State == Failed:
			deletion.Reason = Finished
		case !m.ExpiresAt.IsZero() && !o.Now.Before(m.ExpiresAt):
			deletion.Reason = Expired
		case o.Runners != nil && m.State == Running && !registered:
			// Its job ended: wait for the image to power the machine off, for at most the registration timeout. It
			// counts toward the maximum (Present) but serves nothing: no job, no spare, no surplus.
			if o.Now.Sub(deregisteredAt(o, m)) <= timeout {
				continue
			}
			deletion.Reason = Finished
		case o.Runners != nil && (!registered || !r.Online && !r.Busy) && o.Now.Sub(lastOnline(o, m)) > timeout:
			// Not online for the registration timeout: since its creation if never seen online, unregistered, else
			// since it was last seen so. GitHub's runner list can show a runner offline and idle while it runs a
			// job, so the deletion is guarded: GitHub refuses to remove the registration of a runner running a job.
			deletion.Reason, deletion.Guarded = Unregistered, true
			if _, seen := o.LastOnline[m.Name]; seen {
				deletion.Reason = Offline
			}
		default:
			serving = append(serving, m)
			continue
		}
		d.Deletions = append(d.Deletions, deletion)
	}

	if o.Runners != nil {
		for _, r := range o.Runners {
			if !machines[r.Name] && o.Now.Sub(r.CreatedAt) > timeout {
				d.Removals = append(d.Removals, Removal{RunnerID: r.ID, Name: r.Name, Reason: Orphan})
			}
		}
	}

	if o.Demand == nil && !o.Draining {
		d.Serving = len(serving)
		return d
	}
	if !o.Draining {
		d.Wanted = min(*o.Demand+d.Spares.Count, l.Maximum)
	}

	if surplus := len(serving) - d.Wanted; surplus > 0 {
		var idle []Machine
		for _, m := range serving {
			if r, ok := runners[m.Name]; ok && r.Online && !r.Busy {
				idle = append(idle, m)
			}
		}
		slices.SortFunc(idle, func(a, b Machine) int {
			return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.Name, b.Name))
		})
		for _, m := range idle[:min(surplus, len(idle))] {
			d.Deletions = append(d.Deletions, Deletion{
				Machine: m.Name, Reason: Surplus, RunnerID: runners[m.Name].ID, Guarded: true,
			})
		}
		serving = slices.DeleteFunc(serving, func(m Machine) bool {
			return slices.ContainsFunc(d.Deletions, func(x Deletion) bool { return x.Machine == m.Name })
		})
	}
	d.Serving = len(serving)

	if !o.CreationPaused {
		d.Create = max(0, min(d.Wanted-d.Serving, l.Maximum-d.Present))
	}
	return d
}

// deregisteredAt is when the pool first saw the running machine m without its registration, or m's creation if the
// observation doesn't say.
func deregisteredAt(o Observation, m Machine) time.Time {
	if at, ok := o.Deregistered[m.Name]; ok {
		return at
	}
	return m.CreatedAt
}

// Deregistered returns when each running machine the observation lists without its runner's registration was first
// seen so: its time in since, or now for one seen so for the first time. Machines with their registration, not
// running, or no longer listed are left out. Without the runner list it returns since unchanged, as nothing can be
// told.
func Deregistered(since map[string]time.Time, o Observation) map[string]time.Time {
	if o.Runners == nil {
		return since
	}
	registered := make(map[string]bool, len(o.Runners))
	for _, r := range o.Runners {
		registered[r.Name] = true
	}
	deregistered := map[string]time.Time{}
	for _, m := range o.Machines {
		if m.State != Running || registered[m.Name] {
			continue
		}
		first, seen := since[m.Name]
		if !seen {
			first = o.Now
		}
		deregistered[m.Name] = first
	}
	return deregistered
}

// lastOnline is when the pool last saw m's runner online or busy, or m's creation if it has not.
func lastOnline(o Observation, m Machine) time.Time {
	if at, ok := o.LastOnline[m.Name]; ok {
		return at
	}
	return m.CreatedAt
}

// LastOnline returns when each machine the observation lists was last seen with its runner online or busy: now for
// one whose runner is online or busy in the observation, else its time in since. Machines never seen so, or no longer
// listed, are left out. Without the runner list it returns since unchanged, as nothing can be told.
func LastOnline(since map[string]time.Time, o Observation) map[string]time.Time {
	if o.Runners == nil {
		return since
	}
	online := make(map[string]bool, len(o.Runners))
	for _, r := range o.Runners {
		online[r.Name] = r.Online || r.Busy
	}
	last := map[string]time.Time{}
	for _, m := range o.Machines {
		if online[m.Name] {
			last[m.Name] = o.Now
		} else if at, seen := since[m.Name]; seen {
			last[m.Name] = at
		}
	}
	return last
}

// Leaving returns when each machine the observation lists stopped, failed or being deleted was first seen so (its time
// in since, or now for one seen so for the first time), and how long the longest-waiting of them has waited; zero when
// there is none. Machines no longer listed, or listed pending or running, are left out.
func Leaving(since map[string]time.Time, o Observation) (map[string]time.Time, time.Duration) {
	leaving := map[string]time.Time{}
	var longest time.Duration
	for _, m := range o.Machines {
		if m.State != Stopped && m.State != Failed && m.State != Deleting {
			continue
		}
		first, seen := since[m.Name]
		if !seen {
			first = o.Now
		}
		leaving[m.Name] = first
		longest = max(longest, o.Now.Sub(first))
	}
	return leaving, longest
}
