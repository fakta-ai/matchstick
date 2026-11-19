# Architecture

matchstick gives every GitHub Actions job a fresh cloud virtual machine and deletes the machine when the job is done.
It keeps no records of its own: what exists in the cloud and what GitHub reports are the whole picture, and the two
things it holds besides, each waiting machine's runner configuration and when it last saw each pool with a job, live
only in its memory. This document describes the code as it is, for people changing it.

## Packages

```
cmd/matchstick/            the program: run, and the operations decide, plan, status, drain, github-app create and
                           github-app check; holds the secrets
cmd/matchstick-adapter/    the operations over JSON-RPC (rpc) and MCP (mcp); holds no secrets
internal/adapter/          runs the matchstick command line for each JSON-RPC or MCP call
internal/op/               operations: names, JSON Schemas in and out, annotations, checks on every call
internal/cli/              the command-line adapter: flags, --input, --json, help, errors, exit codes
internal/config/           the configuration file: types, defaults, validation
internal/naming/           machine and registration names: building them, reading owner, pool and age back
internal/plan/             the decisions: a pure function from what one pool's round observed to what to do
internal/pool/             one pool's round: gather, plan, act; backoff; drain; the runner configurations it holds
internal/provider/         the Provider interface: create, list and delete one pool's machines; check a machine's proof
internal/provider/fake/    Provider in memory
internal/provider/azure/   Provider on Azure virtual machines; the attested-document check
internal/claim/            the claim endpoint: machines fetch their runner configuration over TLS
internal/github/           the Runners interface, and its GitHub implementation: scale sets, JIT configurations,
                           the runner list, removal, the listener
internal/github/fake/      Runners in memory
internal/secret/           the GitHub credential, from standard input or the secret mount
internal/githubapp/        the GitHub App: its manifest and GitHub's answer to it, the manifest flow's loopback page,
                           and the checks of an installed App
internal/metrics/          Prometheus collectors
internal/server/           HTTP: /metrics and /healthz; the admin endpoint (drain) on a loopback address
deploy/chart/matchstick/   Helm chart: one replica, Recreate, Azure workload identity
.goreleaser.yaml           the release: archives, checksums, SBOMs, signatures, the container image (ko)
examples/                  configurations and a cloud-init runner image example
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
	// nil if proof shows its sender is the pool's machine name; ErrNotProven if it does not
	Identify(ctx context.Context, name string, proof json.RawMessage) error
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

- Each pool has one runner scale set, named `<instance>-<pool>`, in the pool's runner group (`pools[].runnerGroup`,
  else `github.runnerGroup`). The scale set is looked up by name in that group; if it is not there, in every other
  runner group of the target, by the IDs the REST API lists them with (`/orgs/{org}/actions/runner-groups` or
  `/enterprises/{enterprise}/actions/runner-groups`; a repository has only its default group). `plan` and `status`
  only look it up, and report the group it is in. `run`'s listener ensures it before it opens a session: it creates
  the scale set in the pool's group when no group holds it, moves it there (`PATCH` of the scale set with the
  group's ID) when one other group holds it, and updates its labels. A moved scale set keeps its ID, its
  runners and its assigned jobs; creating a new one instead would leave the old one's jobs with no listener. When
  it cannot tell where the scale set is (the pool's group is missing or refused, the group list cannot be read, or
  two other groups hold one of that name), it changes nothing on GitHub and tries again after the listener's
  backoff; until then the pool knows no demand and creates nothing.
- `JITConfig` calls the scale-set client's `GenerateJitRunnerConfig`; `Remove` calls `RemoveRunner`, mapping "job still
  running" to `ErrJobRunning` and "not found" to success.
- `List` reads GitHub's REST runner list (`/orgs/{org}/actions/runners`, `/repos/{owner}/{repo}/actions/runners` or
  `/enterprises/{enterprise}/actions/runners`, 100 per page), because the scale-set client has no list, and keeps the
  names the pool owns. With a GitHub App it signs in with an installation token, renewed five minutes before it
  expires.
- The listener (`ScaleSet.Listen`) holds the scale set's message session through the client's `listener` package.
  For each message it records `TotalAssignedJobs` as the pool's demand and acquires the jobs offered, unless the pool
  drains (then it also tells GitHub its capacity is 0). For each job-started message that carries both times, it
  reports to the pool how long the job waited between GitHub assigning it to the scale set and to a runner
  (`matchstick_job_startup_seconds`). It never calls the cloud. A failed session, or a scale set not
  yet ensured, is tried again after a backoff from 5 seconds to 2 minutes. The pool's `matchstick_listening` is 1 from
  a session's opening until it ends, 0 otherwise.
- If GitHub refuses a second message session for the same scale set (another copy holds it), that copy keeps retrying.
  Until it has a session it knows no demand, so it creates and scales down nothing; its rounds still apply the cleanup
  rules, repeating the other copy's deletions, which are idempotent.

## Azure

Per machine: a network interface `<name>-nic` in the configured subnet with a private address only, then a VM
`<name>` from the pool's Compute Gallery image, with the machine's bootstrap (see [Claims](#claims)) as user data
(base64), the OS disk and network interface set to be deleted with the VM, boot diagnostics on, and a throwaway SSH key
(Azure requires a login; its private half is discarded). Both carry the tags `matchstick-instance`, `matchstick-pool`,
`matchstick-created-at`, `matchstick-expires-at` and `matchstick-priority`; the instance and pool tags decide
ownership. Nothing secret goes into any of the VM's settings.

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
- `Identify` checks an attested document (see [Claims](#claims)).

## Claims

A VM's settings (user data, custom data, tags, extensions, OS profile) are readable by anyone allowed to read the VM,
so matchstick puts no secret in them. A machine gets public bootstrap information only, and fetches its runner's JIT
configuration from matchstick's claim endpoint, proving which machine it is with a document its cloud signs and only it
can obtain.

```mermaid
sequenceDiagram
    participant P as matchstick (pool)
    participant G as GitHub
    participant A as Azure
    participant V as VM (runner image)
    participant I as IMDS (inside the VM)
    P->>G: register runner <name>
    G-->>P: JIT configuration, held in memory
    P->>A: create VM <name>, user data = bootstrap
    V->>I: user data, attested document
    I-->>V: bootstrap; document signed by Microsoft naming the VM's vmId
    V->>P: POST claimURL {machine, proof}, trusting only the bootstrap's certificate
    P->>P: is a configuration held for <name>?
    P->>P: check the document: signature, signer, times, subscription, chain
    P->>A: GET VM <name>: the pool's tags, the document's vmId?
    P->>P: take the configuration out of memory
    P-->>V: {jitConfig}
    V->>G: run.sh --jitconfig: registers, runs one job
```

The bootstrap, as JSON:

| Field | Value |
|---|---|
| `machine` | the machine's name |
| `claimURL` | the claim endpoint, the configured `claimURL` |
| `claimCertificate` | the claim endpoint's certificate (PEM), the only one the machine trusts for `claimURL` |

The endpoint (`internal/claim`) serves `POST` on `claimURL`'s path, over TLS with a key generated at start and held
only in memory; its self-signed certificate names `claimURL`'s host. The request is `{"machine": "<name>", "proof":
<the cloud's document>}` (at most 64 KiB); the answer is `{"jitConfig": "..."}`, or `{"error": {"code": "...",
"message": "..."}}` with 400 `invalid_input`, 403 `refused` (final: nothing will be released to this machine) or 503
`unavailable` (try again). Neither the proof nor the configuration is logged.

### Single claim

A pool holds a machine's configuration from just before it asks the cloud for the machine until the first of: a claim
releases it, the creation fails, a round deletes the machine, or the registration timeout passes since the machine's
creation. A claim is checked in this order, cheapest first, and refused at the first failure:

1. the name belongs to a pool, and that pool holds a configuration for it;
2. the provider's `Identify` accepts the proof for that name;
3. the configuration is taken out of memory (under the pool's lock); a claim that finds it already gone is refused.

Releasing removes the only copy, so nothing is released twice: not to a retry after success, a replayed document, a
second claim racing the first, or a job running on the machine fetching its own document. Nothing is written
anywhere; after a restart the new process holds nothing, so every machine still waiting for its configuration is
refused (and no longer trusts the endpoint, whose key is new).

### Azure's check

The proof is the attested document as IMDS returns it (`/metadata/attested/document`): `{"encoding": "pkcs7",
"signature": "<base64>"}`. `Identify` refuses it unless:

- it is a PKCS#7 SignedData (parsed with `github.com/smallstep/pkcs7`) with exactly one signer, whose signature over
  the content verifies;
- the signer's certificate names `metadata.azure.com` or a subdomain of it among its DNS names;
- the content's `timeStamp.createdOn` is at most 5 minutes in the future and its `timeStamp.expiresOn` at most 5
  minutes in the past (Azure signs documents valid for 6 hours);
- its `subscriptionId` is the configured subscription;
- the signer's certificate chains, at the current time, to one of the root certificate authorities Microsoft lists for
  Azure (embedded in `internal/provider/azure/roots.pem`). Intermediates the document does not carry are downloaded
  from the certificate's Authority Information Access URL, only from the hosts Microsoft lists for it
  (`cacerts.digicert.com`, `cacerts.digicert.cn`, `cacerts.geotrust.com`, `caissuers.microsoft.com`,
  `www.microsoft.com`), at most 64 KiB and 10 seconds each, and kept in memory;
- the VM `<name>` exists, carries the pool's instance and pool tags, and its `vmId` is the document's.

matchstick sends no nonce. A failed download or VM lookup is `unavailable`; everything else is `refused`.

### When things go wrong

| What happens | Outcome |
|---|---|
| matchstick restarts while machines boot | their claims fail TLS verification or are refused; the image powers the machine off; the next round deletes it (finished), removes its registration, and creates a replacement if demand needs one |
| a claim fails midway (network, Azure API, certificate download) | `unavailable`; the configuration stays held; the image tries again |
| two claims for the same machine at once | one gets the configuration, the other is refused |
| a machine never claims | its configuration is dropped after the registration timeout; the unregistered rule deletes the machine and its registration |
| the cloud refuses the machine | its configuration is dropped and its registration removed |
| clocks disagree | documents are accepted from 5 minutes before their creation to 5 minutes after their expiry; certificates are checked at matchstick's time |
| a document for another VM, a VM outside the pool, another subscription; expired, altered, signed by a certificate not for `metadata.azure.com` or not chaining to an Azure root | refused |

### Security notes

The whole picture, threat by threat: [threat-model.md](threat-model.md).

- The bootstrap is public: the claim endpoint's address and certificate.
- Only processes inside a VM can obtain its attested document. Before the runner has claimed, the only code running
  there is the image's own; after, nothing is left to release, so a job cannot register a second runner.
- A machine cannot claim another's configuration: its document names its own vmId.
- A host posing as the endpoint gets nothing from the runner and can give it nothing: the runner trusts only the
  certificate in its bootstrap, whose private key never leaves matchstick's memory.
- The fake provider's proof is the machine's made-up id; it is for tests and trying the planner only.

## Secrets

The GitHub credential (an App's private key or a token) comes from standard input (`run --secrets -`, a JSON object
`{"githubAppPrivateKey": "..."}` or `{"githubToken": "..."}`), otherwise from the secret mount: the files
`github-app-private-key` or `github-token` in `/var/run/secrets/matchstick` (`--secrets-dir` for `run`, `secretsDir`
for the operations). Never from flags, environment variables or the configuration file; the secret type prints as
redacted. The claim endpoint's TLS key is generated at start and never leaves memory.

## run

`matchstick run --config <file>` builds every pool, then runs each pool's loop and listener, and serves:

- on `listen.metrics` (default `:9090`): `/metrics`, and `/healthz`, which fails once a pool has gathered nothing for
  three intervals plus five minutes (counted from the start before the first round);
- on `listen.admin` (default `127.0.0.1:9091`): `GET /pools`, and `POST /pools/{pool}/drain` with
  `{"draining": true|false}`;
- on `listen.claim` (default `:8443`), when `claimURL` is set: the claim endpoint, over TLS. Its server's errors go
  to the log, failed TLS handshakes included, except a handshake that ended in EOF on a connection that read no byte:
  a load balancer's TCP health probe opens a connection and closes it without sending anything. Those are counted in
  `matchstick_claim_probes_total` instead. The listener counts what each connection reads, which tells them apart.

Logs are JSON lines on stdout. SIGINT or SIGTERM stops it; machines and registrations stay as they are for the next
start, and machines still waiting for their configuration power off and are replaced.

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
its demand (jobs assigned, waiting or running) or `null` when unknown, whether the pool drains, whether creation is
paused, when the process last saw the pool with a job (`lastActive`), when the process started (`started`), and when
it first saw each running machine without its runner's registration (`deregistered`). The limits are the maximum
number of machines, the number of idle spares, the registration timeout, and the idle timeout.

GitHub removes a just-in-time runner's registration when its job ends, so a running machine without its registration
has run its job, however long that took, and is on its way to power off.

| Rule | Condition | Action |
|---|---|---|
| finished | machine stopped or failed; or running without its registration for longer than the registration timeout since `deregistered` (its creation if the observation has no time for it): its job ended and it did not power off | remove its registration if any, delete the machine |
| expired | now is past the machine's expiry | remove its registration if any, delete the machine, even mid-job |
| unregistered | machine older than the registration timeout whose runner never came online: still pending, or its registration offline and idle | remove the registration if any, delete the machine |
| orphan | registration older than the registration timeout with no machine of its name | remove the registration |
| surplus | more serving machines than wanted | for idle, online registrations, oldest first: remove the registration, then delete the machine (guarded) |
| needed | fewer serving machines than wanted | create machines |

Counting:

- `wanted = min(demand + spares now, maximum)`; 0 while draining.
- spares now (`plan.SparesNow`, in the decision as `spares` with its reason): 0 while draining; without an idle
  timeout, the pool's spares (`always`); with one, the pool's spares while demand is above 0 or a runner is busy
  (`busy`), and until the idle timeout after the pool's last activity (`recent`), 0 after (`quiet`). The last activity
  is the later of `lastActive` and the creation of the newest machine created before `started`: after a restart, such
  a machine shows that the pool had a job or kept spares when it was made. Machines created since the start are no
  such evidence, so a spare replaced in the window does not extend it.
- `serving` = machines pending or running that no rule deletes and that can take a job. A running machine whose job
  ended (its registration gone) is left to power off and is not serving: it meets no demand, is no spare and is never
  deleted as surplus, so a job queued meanwhile gets a new machine within the maximum.
- `present` = every machine listed, in any state, including those being deleted and those powering off after their
  job.
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
2. **Decide**: `plan.Decide`. The pool drops the configurations it holds for machines the decision deletes and for
   machines older than the registration timeout.
3. **Act**, at most four calls at once, for at most five minutes: registration removals and deletions, then
   creations. A deletion with a registration removes it first; a guarded one stops if GitHub refuses. A creation
   registers a runner under a new name, holds its JIT configuration, then creates the machine with its bootstrap and an
   expiry of now plus the pool's maximum lifetime; if the cloud refuses, the configuration is dropped and the
   registration removed at once (the orphan rule removes it later if that fails too).

A drain or pause that begins during a round stops the creations it has not started. Nothing is retried within a
round; the next round observes again and plans again. `Pool.Run` runs a round at once,
then on each `Wake` (from `SetDemand`, `SetDraining`) and each interval.

Creation pauses, in memory:

- after `ErrCapacity`: 2 minutes, doubling up to 30, reset by a successful creation;
- after three machines in a row deleted as unregistered: 10 minutes; a machine whose runner is online resets the count.

The pool's last activity, in memory: `SetDemand` records the time when demand is above 0 and when it falls to 0 (the
end of the last job), and each round keeps the later of that and the decision's `spares.lastActive`.

When the pool first saw each running machine without its runner's registration, in memory (`plan.Deregistered`, kept
by each round): a machine that has its registration again, stops or goes is dropped from it.

A new `Pool` over the same cloud and GitHub starts with demand unknown, no configurations, no last activity and no
deregistration times (a running machine without its registration then has the registration timeout from the first
round): its first round cleans up (finished, expired, unregistered, orphan) and creates nothing until demand is set.

## Operations

Each operation is declared once in `cmd/matchstick` with `op.Define`: name, description, example, annotations, and Go
input and output types from which the JSON Schemas are derived. `op.Operation.Call` checks the input against its
schema, runs the operation and checks the output against its schema.

| Operation | Does | Read-only | Destructive | Idempotent | Open world |
|---|---|---|---|---|---|
| `decide` | the planner: an observation and limits in, a decision out | yes | no | yes | no |
| `plan` | gathers each configured pool and returns what a round would do; changes nothing | yes | no | yes | yes |
| `status` | each pool's machines, registrations, demand and the spares it keeps now | yes | no | yes | yes |
| `drain` | asks the running process, at `listen.admin`, to drain a pool or stop draining it | no | yes | yes | yes |
| `github-app create` | registers a GitHub App through GitHub's App manifest flow and writes its private key to a new file | no | no | no | yes |
| `github-app check` | checks the configured App: its key, its installation, the installation's permissions, each runner group the configuration names | yes | no | yes | yes |

`plan` and `status` read demand from the scale set's statistics, as GitHub reports them; `null` when it reports none.
A drain lasts until the process restarts; to keep a pool empty, set its `maximum` to 0.

`github-app create` serves a page on `127.0.0.1` (a random port), at a random path, answering only requests addressed
to that address; another local user who cannot see the printed address cannot read the page. The page posts the
manifest to GitHub with a random `state`, and GitHub's redirect back is accepted only with that `state`. The App
GitHub returns must be owned by the organisation or repository owner asked for (`githubapp.CheckOwner`), or its key is
not written. The private key goes to a file created with mode 0600 before the flow starts, removed if the flow fails.
The manifest (`githubapp.NewManifest`), GitHub's answer (`githubapp.ParseConversion`, `githubapp.CheckOwner`) and the
checks' verdicts (`githubapp.Evaluate`, over what `githubapp.Gather` read) are pure functions.

The command line (`internal/cli`):

- `matchstick <operation> [flags]`; an operation named by two words, such as `github-app check`, takes both;
  `-h`/`--help` with the input flags, the output's fields and an example; `--version`.
- Flags are the input schema's properties in kebab-case; `--input <file|->` gives the whole input as a JSON object,
  and flags override its fields.
- `--json` prints one JSON value on stdout; without it, the same value indented.
- `--help --json` prints the operations as MCP tool definitions; a two-word operation's tool name joins the words
  with `_` (`github-app_check`).
- Exit 0 on success, 2 on invalid input or configuration, 1 otherwise; with `--json`, errors go to stderr as
  `{"error": {"code": "...", "message": "..."}}`. A check that ran and failed (`github-app check`) prints its report
  on stdout and fails with the code `check_failed`.
- Nothing prompts; `--no-input` is accepted.

## rpc and mcp

`matchstick-adapter rpc` (JSON-RPC 2.0, one message per line) and `matchstick-adapter mcp` (MCP over stdio, with the
official Go SDK) are a separate executable that holds no secrets and links none of the GitHub or Azure code. It reads
the tool definitions from `matchstick --help --json`, and for each call runs `matchstick <operation> --json --input -`
with the parameters (without `_meta`) on standard input.

- JSON-RPC: the method is the operation, the result its output. Errors carry matchstick's error object in
  `data` (`code`, `message`), and in `data.result` the output a failed call printed (a failed `github-app check`'s
  report); invalid input is -32602, other failures -32000, an unknown method -32601, a line that is not JSON -32700,
  after which the loop goes on. Requests run concurrently; `notifications/cancelled` with a `requestId` stops that
  call.
- MCP: each operation is a tool with its schemas and annotations; the output is the tool's structured content, an
  error the tool's error result with the error object as text, plus a failed check's report as structured content
  and text.

`--matchstick <path>` names the executable to run; by default `matchstick` on `PATH`. No operation reports progress.

## Configuration

YAML (or JSON), unknown fields rejected. `instance` (letters and digits, at most 10), `interval` (default 30s),
`listen`, `claimURL`, and `pools`, each with `name` (at most 20), `labels`, `runnerGroup`, `maximum`, `spares`,
`idleTimeout` (none by default), `maxLifetime` (default 6h) and `registrationTimeout` (default 10m); then either
`github` and `azure` sections with each pool's `azure` shape, or a `fake` section. [configuration.md](configuration.md)
lists every field.

## Metrics

Per pool: demand, machines by state, registrations by status, creations, creation failures by kind, deletions and
removals by reason, rounds by result, round duration, time of the last successful round, creation paused, draining,
listening (holding a message session), claims by result, spares kept now, and a histogram of how long jobs waited
for a runner. For the claim endpoint: connections closed before their first byte.

## Limits

- Azure only.
- One replica. A restart makes the machines still waiting for their runner configuration unable to claim it: they
  power off and are replaced.
- Each machine serves one job.
- Two installations must not share an instance name in the same GitHub organisation or resource group.
- The network, identities, images and role assignments must exist beforehand.
- Scale sets are created, updated or moved between runner groups, never deleted. Runner groups are never created or
  changed.
- The only bounds are the maximum machines per pool and the maximum lifetime per machine.
