# Architecture

matchstick gives every GitHub Actions job a fresh cloud virtual machine and deletes the machine when the job is done.
It keeps no records of its own: what exists in the cloud and what GitHub reports are the whole picture. This document
describes the code as it is, for people changing it.

## Packages

```
cmd/matchstick/            the command line: decide, plan, status
internal/op/               operations: names, JSON Schemas in and out, annotations, checks on every call
internal/cli/              the command-line adapter: flags, --input, --json, help, errors, exit codes
internal/config/           the configuration file: types, defaults, validation
internal/naming/           machine and registration names: building them, reading owner, pool and age back
internal/plan/             the decisions: a pure function from what one pool's round observed to what to do
internal/pool/             one pool's round: gather, plan, act; backoff; drain
internal/provider/         the Provider interface: create, list and delete one pool's machines
internal/provider/fake/    Provider in memory
internal/github/           the Runners interface: register, list and remove one pool's runners
internal/github/fake/      Runners in memory
internal/metrics/          Prometheus collectors
```

`plan` imports only the standard library: no network, no clock, no randomness. `pool` depends on `plan` and the two
interfaces; only `cmd/matchstick` chooses implementations.

## Interfaces

```go
// provider: one pool's machines in one cloud.
type Provider interface {
	Create(ctx context.Context, spec CreateSpec) error    // returns once accepted; ErrCapacity: retry later
	List(ctx context.Context) ([]plan.Machine, error)     // every machine of the pool, half-made ones included
	Delete(ctx context.Context, name string) error        // idempotent
}

// github: one pool's runner registrations.
type Runners interface {
	JITConfig(ctx context.Context, name string) (JIT, error) // registers a runner; its single-use configuration
	List(ctx context.Context) ([]plan.Runner, error)         // registrations whose names the pool owns
	Remove(ctx context.Context, id int64) error              // idempotent; ErrJobRunning while a job runs
}
```

Implementations: the in-memory fakes, used by the tests and by a configuration with a `fake` section, which serves
each pool's machines, registrations and demand from the file itself.

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

## A round

`pool.Pool.Round` gathers, decides and acts once.

1. **Gather**: list the pool's machines and registrations; demand is the last value given to `SetDemand`. If the
   machines cannot be listed, the round does nothing. If the registrations cannot be listed, the round goes on with
   them unknown.
2. **Decide**: `plan.Decide`.
3. **Act**, at most four calls at once: registration removals and deletions, then creations. A deletion with a
   registration removes it first; a guarded one stops if GitHub refuses. A creation registers a runner under a new
   name, then creates the machine with its JIT configuration and an expiry of now plus the pool's maximum lifetime; if
   the cloud refuses, the registration is removed at once (the orphan rule removes it later if that fails too).

Nothing is retried within a round; the next round observes again and plans again. `Pool.Run` runs a round at once,
then on each `Wake` (from `SetDemand`, `SetDraining`) and each interval.

Creation pauses, in memory:

- after `ErrCapacity`: 2 minutes, doubling up to 30, reset by a successful creation;
- after three machines in a row deleted as unregistered: 10 minutes; a machine whose runner registers resets the count.

A new `Pool` over the same cloud and GitHub starts with demand unknown: its first round cleans up (finished, expired,
unregistered, orphan) and creates nothing until demand is set.

## Operations

Each operation is declared once in `cmd/matchstick` with `op.Define`: name, description, example, annotations, and Go
input and output types from which the JSON Schemas are derived. `op.Operation.Call` checks the input against its
schema, runs the operation and checks the output against its schema.

| Operation | Does | Read-only | Destructive | Idempotent | Open world |
|---|---|---|---|---|---|
| `decide` | the planner: an observation and limits in, a decision out | yes | no | yes | no |
| `plan` | gathers each configured pool and returns what a round would do; changes nothing | yes | no | yes | yes |
| `status` | each pool's machines, registrations and demand | yes | no | yes | yes |

The command line (`internal/cli`):

- `matchstick <operation> [flags]`; `-h`/`--help` with an example; `--version`.
- Flags are the input schema's properties in kebab-case; `--input <file|->` gives the whole input as a JSON object,
  and flags override its fields.
- `--json` prints one JSON value on stdout; without it, the same value indented.
- `--help --json` prints the operations as MCP tool definitions.
- Exit 0 on success, 2 on invalid input or configuration, 1 otherwise; with `--json`, errors go to stderr as
  `{"error": {"code": "...", "message": "..."}}`.
- Nothing prompts; `--no-input` is accepted.

## Configuration

YAML (or JSON), unknown fields rejected. `instance` (letters and digits, at most 10), `interval` (default 30s),
`listen`, and `pools`, each with `name` (at most 20), `labels`, `maximum`, `spares`, `maxLifetime` (default 6h) and
`registrationTimeout` (default 10m). `examples/fake.yaml` is a complete example with a `fake` section.

## Metrics

Per pool: demand, machines by state, registrations by status, creations, creation failures by kind, deletions and
removals by reason, rounds by result, round duration, time of the last successful round, creation paused, draining.

## Limits

- Each machine serves one job.
- The only bounds are the maximum machines per pool and the maximum lifetime per machine.
