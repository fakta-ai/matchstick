# Architecture

matchstick gives every GitHub Actions job a fresh cloud virtual machine and deletes the machine when the job is done.
It keeps no records of its own: what exists in the cloud and what GitHub reports are the whole picture. This document
describes the code as it is, for people changing it.

## Packages

```
cmd/matchstick/            the program: run, and the operations decide, plan, status, drain; holds the secrets
cmd/matchstick-adapter/    the operations over JSON-RPC (rpc) and MCP (mcp); holds no secrets
internal/adapter/          runs the matchstick command line for each JSON-RPC or MCP call
internal/op/               operations: names, JSON Schemas in and out, annotations, checks on every call
internal/cli/              the command-line adapter: flags, --input, --json, help, errors, exit codes
internal/config/           the configuration file: types, defaults, validation
internal/naming/           machine and registration names: building them, reading owner, pool and age back
internal/plan/             the decisions: a pure function from what one pool's round observed to what to do
internal/pool/             one pool's round: gather, plan, act; backoff; drain
internal/provider/         the Provider interface: create, list and delete one pool's machines
internal/provider/fake/    Provider in memory
internal/provider/azure/   Provider on Azure virtual machines
internal/github/           the Runners interface, and its GitHub implementation: scale sets, JIT configurations,
                           the runner list, removal, the listener
internal/github/fake/      Runners in memory
internal/secret/           the GitHub credential, from standard input or the secret mount
internal/metrics/          Prometheus collectors
internal/server/           HTTP: /metrics and /healthz; the admin endpoint (drain) on a loopback address
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

Implementations: Azure (`provider/azure`) and GitHub (`github.ScaleSet`), and the in-memory fakes, used by the tests
and by a configuration with a `fake` section, which serves each pool's machines, registrations and demand from the
file itself.

## GitHub

- Each pool has one runner scale set, named `<instance>-<pool>`, in the configured runner group. `run` creates it or
  updates its labels; `plan` and `status` only look it up.
- `JITConfig` calls the scale-set client's `GenerateJitRunnerConfig`; `Remove` calls `RemoveRunner`, mapping "job still
  running" to `ErrJobRunning` and "not found" to success.
- `List` reads GitHub's REST runner list (`/orgs/{org}/actions/runners`, `/repos/{owner}/{repo}/actions/runners` or
  `/enterprises/{enterprise}/actions/runners`, 100 per page), because the scale-set client has no list, and keeps the
  names the pool owns. With a GitHub App it signs in with an installation token, renewed five minutes before it
  expires.
- The listener (`ScaleSet.Listen`) holds the scale set's message session through the client's `listener` package.
  For each message it records `TotalAssignedJobs` as the pool's demand and acquires the jobs offered, unless the pool
  drains (then it also tells GitHub its capacity is 0). It never calls the cloud. A failed session is reopened after a
  backoff from 5 seconds to 2 minutes.
- If GitHub refuses a second message session for the same scale set (another copy holds it), that copy keeps retrying.
  Until it has a session it knows no demand, so it creates and scales down nothing; its rounds still apply the cleanup
  rules, repeating the other copy's deletions, which are idempotent.

## Azure

Per machine: a network interface `<name>-nic` in the configured subnet with a private address only, then a VM
`<name>` from the pool's Compute Gallery image, with the JIT configuration as user data (base64), the OS disk and
network interface set to be deleted with the VM, boot diagnostics on, and a throwaway SSH key (Azure requires a login;
its private half is discarded). Both carry the tags `matchstick-instance`, `matchstick-pool`, `matchstick-created-at`,
`matchstick-expires-at` and `matchstick-priority`; the instance and pool tags decide ownership.

- Pool options: VM size, image, a user-assigned identity or none, an ephemeral OS disk (`ResourceDisk`, `CacheDisk`
  or `NvmeDisk`), OS disk size, Spot, accelerated networking, a custom-data file (such as cloud-init).
- `Create` waits up to a minute for the VM's allocation. Quota and allocation failures (`QuotaExceeded`,
  `SkuNotAvailable`, `AllocationFailed`, `ZonalAllocationFailed`, `Overconstrained*`, and `OperationNotAllowed` naming a
  quota) are `ErrCapacity`. A Spot VM without capacity is reported as a plain failure and the pool uses regular
  priority for 30 minutes. A VM Azure refused outright takes its network interface with it.
- `List` reads the group's VMs and, for those provisioned, their power state: running is running; stopping, stopped,
  deallocating and deallocated are stopped; provisioning failed is failed; deleting is deleting; anything else is
  pending. A network interface without its VM is pending for two minutes, then failed.
- `Delete` starts the VM's deletion, or the network interface's if no VM is left, and returns once Azure accepts it.
- Sign-in: workload identity, then a managed identity, then the Azure CLI. No client secret.

## Secrets

The GitHub credential (an App's private key or a token) comes from standard input (`run --secrets -`, a JSON object
`{"githubAppPrivateKey": "..."}` or `{"githubToken": "..."}`), otherwise from the secret mount: the files
`github-app-private-key` or `github-token` in `/var/run/secrets/matchstick` (`--secrets-dir` for `run`, `secretsDir`
for the operations). Never from flags, environment variables or the configuration file; the secret type prints as
redacted.

## run

`matchstick run --config <file>` builds every pool, then runs each pool's loop and listener, and serves:

- on `listen.metrics` (default `:9090`): `/metrics`, and `/healthz`, which fails once a pool has gathered nothing for
  three intervals plus five minutes (counted from the start before the first round);
- on `listen.admin` (default `127.0.0.1:9091`): `GET /pools`, and `POST /pools/{pool}/drain` with
  `{"draining": true|false}`.

Logs are JSON lines on stdout. SIGINT or SIGTERM stops it; machines and registrations stay as they are for the next
start.

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
- surplus deletions happen only when demand is known or the pool drains.
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
3. **Act**, at most four calls at once, for at most five minutes: registration removals and deletions, then creations. A deletion with a
   registration removes it first; a guarded one stops if GitHub refuses. A creation registers a runner under a new
   name, then creates the machine with its JIT configuration and an expiry of now plus the pool's maximum lifetime; if
   the cloud refuses, the registration is removed at once (the orphan rule removes it later if that fails too).

A drain or pause that begins during a round stops the creations it has not started. Nothing is retried within a
round; the next round observes again and plans again. `Pool.Run` runs a round at once,
then on each `Wake` (from `SetDemand`, `SetDraining`) and each interval.

Creation pauses, in memory:

- after `ErrCapacity`: 2 minutes, doubling up to 30, reset by a successful creation;
- after three machines in a row deleted as unregistered: 10 minutes; a machine whose runner is online resets the count.

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
| `drain` | asks the running process, at `listen.admin`, to drain a pool or stop draining it | no | yes | yes | yes |

`plan` and `status` read demand from the scale set's statistics, as GitHub reports them; `null` when it reports none.
A drain lasts until the process restarts; to keep a pool empty, set its `maximum` to 0.

The command line (`internal/cli`):

- `matchstick <operation> [flags]`; `-h`/`--help` with an example; `--version`.
- Flags are the input schema's properties in kebab-case; `--input <file|->` gives the whole input as a JSON object,
  and flags override its fields.
- `--json` prints one JSON value on stdout; without it, the same value indented.
- `--help --json` prints the operations as MCP tool definitions.
- Exit 0 on success, 2 on invalid input or configuration, 1 otherwise; with `--json`, errors go to stderr as
  `{"error": {"code": "...", "message": "..."}}`.
- Nothing prompts; `--no-input` is accepted.

## rpc and mcp

`matchstick-adapter rpc` (JSON-RPC 2.0, one message per line) and `matchstick-adapter mcp` (MCP over stdio, with the
official Go SDK) are a separate executable that holds no secrets and links none of the GitHub or Azure code. It reads
the tool definitions from `matchstick --help --json`, and for each call runs `matchstick <operation> --json --input -`
with the parameters (without `_meta`) on standard input.

- JSON-RPC: the method is the operation, the result its output. Errors carry matchstick's error object in
  `data` (`code`, `message`); invalid input is -32602, other failures -32000, an unknown method -32601, a line that is
  not JSON -32700, after which the loop goes on. Requests run concurrently; `notifications/cancelled` with a
  `requestId` stops that call.
- MCP: each operation is a tool with its schemas and annotations; the output is the tool's structured content, an
  error the tool's error result with the error object as text.

`--matchstick <path>` names the executable to run; by default `matchstick` on `PATH`. No operation reports progress.

## Configuration

YAML (or JSON), unknown fields rejected. `instance` (letters and digits, at most 10), `interval` (default 30s),
`listen`, and `pools`, each with `name` (at most 20), `labels`, `maximum`, `spares`, `maxLifetime` (default 6h) and
`registrationTimeout` (default 10m); then either `github` and `azure` sections with each pool's `azure` shape, or a
`fake` section. [configuration.md](configuration.md) lists every field.

## Metrics

Per pool: demand, machines by state, registrations by status, creations, creation failures by kind, deletions and
removals by reason, rounds by result, round duration, time of the last successful round, creation paused, draining.

## Limits

- Azure only.
- One replica.
- Each machine serves one job.
- Two installations must not share an instance name in the same GitHub organisation or resource group.
- The network, identities, images and role assignments must exist beforehand.
- Scale sets are created or updated, never deleted.
- The only bounds are the maximum machines per pool and the maximum lifetime per machine.
