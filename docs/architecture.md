# Architecture

matchstick gives every GitHub Actions job a fresh cloud virtual machine and deletes the machine when the job is done.
It keeps no records of its own: what exists in the cloud and what GitHub reports are the whole picture. This document
describes the code as it is, for people changing it.

## Packages

```
internal/naming/   machine and registration names: building them, reading owner, pool and age back
internal/plan/     the decisions: a pure function from what one pool's round observed to what to do
```

`plan` imports only the standard library: no network, no clock, no randomness.

## Names

A machine and its runner registration share one name: `<instance>-<pool>-<created><random>`. The instance names the
installation (lower-case letters and digits, no hyphen); the pool may have inner hyphens; `created` is the creation
time in seconds since the Unix epoch in base 36 (7 characters) and `random` 4 base-36 characters. A name splits one way
only, so it tells who owns a machine or registration, which pool it serves, and how old it is.

## The planner

`plan.Decide(observation, limits)` takes what a round saw, with the pool's limits, and returns a decision: machines to
delete and registrations to remove, each with a reason, and how many machines to create.

The observation holds the current time, the pool's machines (name, state, creation time, expiry), its runner
registrations (id, name, creation time from the name, online, busy) or `null` when GitHub's list could not be read,
its demand (jobs assigned, waiting or running) or `null` when unknown, whether the pool drains, and whether creation is
paused. The limits are the maximum number of machines, the number of idle spares, and the registration timeout.

| Rule | Condition | Action |
|---|---|---|
| finished | machine stopped or failed | remove its registration if any, delete the machine |
| expired | now is past the machine's expiry | remove its registration if any, delete the machine, even mid-job |
| unregistered | machine older than the registration timeout, and its runner has no registration or an offline, idle one | remove the registration if any, delete the machine |
| orphan | registration older than the registration timeout with no machine of its name | remove the registration |
| surplus | more serving machines than wanted | for idle, online registrations, oldest first: remove the registration, then delete the machine (guarded) |
| needed | fewer serving machines than wanted | create machines |

Counting:

- `wanted = min(demand + spares, maximum)`; 0 while draining.
- `serving` = machines pending or running that no rule deletes.
- `present` = every machine listed, in any state, including those being deleted.
- creates = `min(wanted − serving, maximum − present)`, at least 0; none while demand is unknown or creation is paused.
- surplus deletions happen only when demand is known.
- machines being deleted are left alone but counted in `present`.
- with the registration list unknown, the unregistered, orphan and surplus rules are skipped.

A guarded deletion is carried out only once the registration is removed. GitHub refuses to remove the registration of
a runner that is running a job, so a guarded deletion never cuts a job short. The other deletions delete the machine
whatever GitHub answers.

## Limits

- Each machine serves one job.
- The only bounds are the maximum machines per pool and the maximum lifetime per machine.
