# Configuration

One YAML (or JSON) file, by default `matchstick.yaml`; `--config` names another. Unknown fields are errors. The file
holds no secrets. Examples: [examples/azure.yaml](../examples/azure.yaml), [examples/fake.yaml](../examples/fake.yaml).

## Top level

| Field | Default | Meaning |
|---|---|---|
| `instance` | required | names this installation: lower-case letters and digits, starting with a letter, at most 10. It starts every machine, runner and scale-set name and marks every machine; installations sharing a GitHub organisation or a resource group need different ones |
| `interval` | `30s` | the longest time between two rounds of a pool |
| `listen.metrics` | `:9090` | serves `/metrics` and `/healthz` |
| `listen.admin` | `127.0.0.1:9091` | serves the drain endpoint; keep it on a loopback address |
| `listen.claim` | `:8443` | serves the claim endpoint over TLS, when `claimURL` is set; the machines' subnet must reach it |
| `claimURL` | required with `azure` | where machines claim their runner configuration: `https://<host>[:<port>][/<path>]`, an address of `listen.claim` as the machines reach it, such as `https://10.20.0.4:8443/claim` or `https://matchstick.runners.internal/claim`. It goes into every machine's user data, with the endpoint's certificate |
| `github` | | where the scale sets live; required unless `fake` is set |
| `azure` | | where the machines go; required unless `fake` is set |
| `fake` | | made-up pools instead of GitHub and Azure, to try the planner |
| `pools` | required | the pools |

### The claim endpoint

matchstick makes the endpoint's TLS key when it starts and keeps it in memory only, with a self-signed certificate for
`claimURL`'s host; there is no key or certificate to configure. Every machine gets that certificate in its user data
and trusts nothing else there, so the endpoint must be reached directly: a TCP load balancer or a Kubernetes Service
passes it through, but nothing may terminate TLS in front of it. A restart makes a new key; machines created before it
cannot claim and are replaced.

Checking a machine's proof on Azure, matchstick downloads the certificates of Microsoft's intermediate certificate
authorities the first time it meets each, over HTTP from `cacerts.digicert.com`, `cacerts.digicert.cn`,
`cacerts.geotrust.com`, `caissuers.microsoft.com` or `www.microsoft.com`; allow those in its outbound network.

## `github`

| Field | Default | Meaning |
|---|---|---|
| `url` | required | the organisation, repository or enterprise, such as `https://github.com/acme`, `https://github.com/acme/app` or `https://github.com/enterprises/acme`; GitHub Enterprise Server URLs work the same way |
| `runnerGroup` | `default` | the runner group of the scale sets of pools that name none ([runner groups](#runner-groups)) |
| `app.clientID`, `app.installationID` | | the GitHub App to sign in as, with its private key in the secret mount; without `app`, a token from the secret mount |

The App needs, on an organisation: **Self-hosted runners: read and write** (organisation permission). On a repository:
**Administration: read and write**. On both: **Metadata: read**. `matchstick github-app create` registers an App with
exactly these; `matchstick github-app check` checks an installed one ([setup.md](setup.md#1-create-the-github-app)).
GitHub Apps cannot register an enterprise's runners: for an enterprise `url`, leave out `app` and give a token (a
classic personal access token with `manage_runners:enterprise`).

### Runner groups

Each pool's scale set is in one runner group: the pool's `runnerGroup`, or `github.runnerGroup` if it names none. A
runner group decides which repositories may use its runners, so a pool whose machines hold an identity
(`azure.identity`) can sit in a group that admits only the repositories that should get it, while the other pools
sit in a group that admits more. Pools may share a group. A scale set is named `<instance>-<pool>` whatever its
group, and matchstick keeps one scale set of that name across all of the target's runner groups.

- The group must exist beforehand (on an organisation, **Settings → Actions → Runner groups**); matchstick neither
  creates nor changes groups. `matchstick github-app check` checks each group the configuration names.
- A pool whose group is missing, or that the credential cannot use, creates no machines: its listener logs `the pool
  cannot scale until its scale set is in its runner group` with the reason, and tries again after 5 seconds, doubling
  up to 2 minutes; its `matchstick_listening` stays 0 meanwhile. Its rounds still delete finished, expired and
  unregistered machines; the other pools run as usual.
- When a pool's group changes, `run` moves the pool's scale set from the group it is in to the new one. It stays the
  same scale set, with the same ID, so the runners registered in it and the jobs GitHub assigned to it stay with it,
  and no job is left on a scale set nobody listens to; from then on the new group's repository access applies.
  `status` shows the group the scale set is in until `run` moves it. A scale set of the pool's name in two other
  groups keeps the pool from scaling, with both named, until one of them is deleted. A repository has only its
  default group.

### The credential

The credential is not in this file. matchstick reads it once, when it starts, from the secret mount, by default
`/var/run/secrets/matchstick` (`--secrets-dir` names another): the file `github-app-private-key`, the App's private key
in PEM, or `github-token`, a token; exactly one of them. `run --secrets -` reads it from standard input instead, as a
JSON object with `githubAppPrivateKey` or `githubToken`.

On Kubernetes the chart mounts a Secret there (`githubSecret.name`). Create it yourself, or have it filled from your
vault by the External Secrets Operator or the Secrets Store CSI driver's Secret sync;
[setup.md](setup.md#3-store-the-key) has an example. After replacing the key, restart matchstick.

## `azure`

| Field | Meaning |
|---|---|
| `subscriptionID`, `resourceGroup`, `location` | where VMs and network interfaces are created |
| `subnetID` | the subnet's resource ID; machines get a private address there and no public one |

matchstick's identity needs, on the resource group: create, read and delete VMs, their OS disks and network
interfaces; on the subnet: join (`Microsoft.Network/virtualNetworks/subnets/join/action`); on the image: read; on each
pool's identity: assign (`Microsoft.ManagedIdentity/userAssignedIdentities/assign/action`). Reading a VM is also how a
claim is matched to it. [setup.md](setup.md#role) lists the exact actions as a custom role. The built-in roles
Virtual Machine Contributor and Network Contributor on the resource group also cover the group's part.

## `pools[]`

| Field | Default | Meaning |
|---|---|---|
| `name` | required | lower-case letters, digits and inner hyphens, starting with a letter, at most 20 |
| `labels` | the name | the scale set's labels, which workflows ask for in `runs-on` |
| `runnerGroup` | `github.runnerGroup` | the runner group of the pool's scale set ([runner groups](#runner-groups)); only with `github` |
| `maximum` | required | the most machines at once, in any state; 0 keeps the pool empty |
| `spares` | 0 | idle machines kept ready beyond demand, at most `maximum` ([spares](#spares-and-the-idle-timeout)) |
| `idleTimeout` | none | keep the spares only while the pool has a job and for this long after its last job ended, such as `30m`; without it, the spares are kept at all times. Needs `spares` |
| `maxLifetime` | `6h` | a machine older than this is deleted, job or not |
| `registrationTimeout` | `10m` | a machine whose runner is not online by then is deleted; a registration without a machine for this long is removed |
| `azure.size` | required | the VM size, such as `Standard_D4ads_v5` |
| `azure.image` | required | a Compute Gallery image version's resource ID, or an image definition's for its latest version |
| `azure.identity` | none | a user-assigned managed identity's resource ID for the VMs |
| `azure.ephemeralOSDisk` | managed disk | `ResourceDisk`, `CacheDisk` or `NvmeDisk`: the OS disk on the VM's local storage; the size must have room for the image |
| `azure.osDiskSizeGB` | the image's | the OS disk's size |
| `azure.spot` | false | Spot priority (evicted VMs are deleted), regular priority for 30 minutes after Spot has no capacity |
| `azure.acceleratedNetworking` | false | accelerated networking on the network interface |
| `azure.customData` | none | a file whose contents become the VM's custom data, such as a cloud-init configuration |

### Spares and the idle timeout

A job that finds no idle runner waits while its VM is created and boots. `spares` keeps that many machines created,
registered and idle beyond the jobs the pool has, so a job starts on one at once. A spare is a fresh machine that has
run nothing; when a job takes it, it serves that one job and is deleted, and the pool creates the next spare. Spares
count toward `maximum`, and a burst of more jobs than spares gets the rest of its machines created as usual.

Without `idleTimeout`, the pool keeps its spares at all times. With it, the pool keeps them only around its jobs:

- while a job is assigned to the pool, waiting or running (GitHub reports assigned jobs, or one of the pool's runners
  is busy), the pool keeps its spares;
- `idleTimeout` counts from the end of the pool's last job, the moment GitHub reports no job assigned to it. A job
  that comes before it passes starts on a spare and starts the count again once it ends;
- once `idleTimeout` passes with no job, the pool deletes its idle spares (one that takes a job meanwhile is kept for
  that job) and holds no machine until the next job.

**The first job after a quiet period waits for its machine**, as in a pool without spares: the pool creates the
machine for it and the spares with it, and the jobs that follow start on spares. A pool whose every job must start at
once keeps its spares without `idleTimeout`, and pays for them at all times. matchstick has no schedules (spares only
during set hours).

`status` shows each pool's `spares`: `count`, the spares it keeps now; `reason`, why: `always` (no `idleTimeout`),
`busy` (a job is assigned), `recent` (the last job ended less than `idleTimeout` ago), `quiet` or `draining`;
`lastActive`, the last time the pool had a job as far as matchstick can tell; and `until`, when the spares go if no job
comes first. `matchstick_spares` reports the count.

**Cost.** A spare bills like any VM from its creation to its deletion. A pool never holds more than `spares` idle
spares, all within `maximum`. With `idleTimeout`, the spares bill for at most `spares` × (the time the pool has jobs +
`idleTimeout` after each quiet period begins) a day, never more than the same spares without it, `spares` × 24 hours.
Each quiet period also ends with up to `spares` machines deleted that ran nothing: one creation each that the jobs did
not need.

Example: a pool with `spares: 2` and `idleTimeout: 30m` on a VM size that costs, say, $0.20 an hour. Its jobs come
in three bursts a day, each about an hour long, a few minutes apart within a burst. The spares bill for at most
2 × 3 × (1 h + 0.5 h) = 9 VM-hours a day, $1.80; without `idleTimeout`, 48 VM-hours, $9.60. The VMs that run the jobs
bill the same either way. In each burst only the first job waits for a VM.

To choose `idleTimeout`, take the longest gap between jobs that should still find a spare, such as between the jobs of
one workflow or between a push and the next; longer keeps spares through more gaps, at `spares` VMs for each minute
of it after each burst. To choose `spares`, take how many jobs start at once on the pool; the rest of a burst waits
for VMs.

Limits:

- matchstick keeps no records, so after a restart it has not seen the last job end. It takes the creation of the
  newest machine made before the restart as the pool's last activity: such a machine was made for a job or as a spare.
  The window can then end earlier than the old process would have ended it, by up to the last job's run time, or
  once later, by up to `idleTimeout`, when that machine replaced a spare. A pool that holds no machine after a restart
  is quiet until its next job. `status` and `plan`, which run as their own processes, show the window the same way.
- A spare is deleted at `maxLifetime` like any machine, even mid-job, and replaced while the pool keeps spares. A
  spare that waited long for its job leaves that job less of `maxLifetime`.
- An evicted Spot spare, or one whose runner does not register within `registrationTimeout`, is deleted and replaced
  while the pool keeps spares.
- `idleTimeout` is measured with the clock of the host matchstick runs on; a step of that clock moves the end of the
  window by the step.

## `fake`

`fake.pools.<name>` gives a pool's `demand`, `machines` (`name`, `state`, `createdAt`, `expiresAt`) and `runners`
(`id`, `name`, `createdAt`, `online`, `busy`). `plan` and `status` show them; `run` acts on them in memory.
