# Operations

## Installing

Each release publishes:

| What | Where |
|---|---|
| `matchstick` and `matchstick-adapter` for Linux and macOS, amd64 and arm64: one `.tar.gz` per platform, each with an SPDX SBOM (`.sbom.json`); `checksums.txt` over all of them, signed (`checksums.txt.sigstore.json`) | the GitHub release |
| the container image, for linux/amd64 and linux/arm64: `matchstick` (`/ko-app/matchstick`, on `PATH`) on [distroless](https://github.com/GoogleContainerTools/distroless) static (CA certificates, time zones, no shell), user 65532, with an SPDX SBOM beside it in the registry | `ghcr.io/fakta-ai/matchstick:<version>` |
| the Helm chart, its version and `appVersion` the release's | `oci://ghcr.io/fakta-ai/charts/matchstick:<version>` |

`matchstick-adapter` is in the archives only, not in the image.

The release notes give the image's and the chart's digests. The image, the chart and `checksums.txt` are signed with
Sigstore, keyless: the signing certificate names the release workflow on `main`,
`https://github.com/fakta-ai/matchstick/.github/workflows/release.yml@refs/heads/main`, and records the commit it
built. Releases up to 0.3.1 were signed by the workflow at the version's tag instead,
`…/release.yml@refs/tags/v<version>`. Check them before you use them:

```sh
identity=https://github.com/fakta-ai/matchstick/.github/workflows/release.yml@refs/heads/main
cosign verify ghcr.io/fakta-ai/matchstick@sha256:<digest> \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com --certificate-identity "$identity"
cosign verify ghcr.io/fakta-ai/charts/matchstick@sha256:<digest> \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com --certificate-identity "$identity"
# the downloaded archives, through checksums.txt
cosign verify-blob checksums.txt --bundle checksums.txt.sigstore.json \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com --certificate-identity "$identity"
sha256sum --check --ignore-missing checksums.txt
```

Each release's image and chart are built once, into `ghcr.io/fakta-ai/matchstick-staging` and
`ghcr.io/fakta-ai/charts-staging/matchstick`, and copied from there by digest, with their signatures, SBOMs and
attestations: the public names hold exactly the bytes that were staged and checked. Use the public names.

A release published from the public repository also has GitHub's build provenance attestations (SLSA provenance) for
its archives, `checksums.txt`, the image and the chart, naming the commit and the workflow run that built them:

```sh
gh attestation verify matchstick_<version>_linux_amd64.tar.gz --repo fakta-ai/matchstick
gh attestation verify oci://ghcr.io/fakta-ai/matchstick@sha256:<digest> --repo fakta-ai/matchstick
gh attestation verify oci://ghcr.io/fakta-ai/charts/matchstick@sha256:<digest> --repo fakta-ai/matchstick
```

Flux checks the chart itself when its `OCIRepository` has `verify.provider: cosign` with that issuer and identity
(`matchOIDCIdentity`). A released chart runs its release's image by digest (below), so a chart whose signature checks
out also fixes the image it runs.

### The end-to-end check before publication

Between staging and copying, the release workflow can wait for an end-to-end system to test the staged chart and
image. Three repository variables set it:

| Variable | Unset | Set |
|---|---|---|
| `RELEASE_E2E_CONTEXT` | no wait: the staged bytes are published at once | the commit status context the release waits for, on the release's commit |
| `RELEASE_E2E_TIMEOUT` | 30 minutes | how long, in minutes (1 to 350), the release waits for `success` |
| `RELEASE_E2E_ACK_TIMEOUT` | one wait, up to `RELEASE_E2E_TIMEOUT`, whatever the end-to-end system does | how long, in minutes (1 to 350), the end-to-end system has to acknowledge the build |

Only a status of that context whose description names the staged chart's digest counts; one naming another digest is
about an earlier build of the same commit and is ignored. With `RELEASE_E2E_ACK_TIMEOUT` set, the first such status,
in any state, acknowledges the build: usually `pending`, set as the test starts, but a result arriving first counts
too.

| What the release sees | What it does |
|---|---|
| an acknowledgement, then `success` | copies the staged bytes to the public names and publishes |
| an acknowledgement, then `failure` or `error` | fails with the status's description |
| an acknowledgement, then nothing more | fails at `RELEASE_E2E_TIMEOUT` |
| no status naming this build's chart (none, or only another build's) | fails at `RELEASE_E2E_ACK_TIMEOUT`, "no end-to-end system acknowledged this build"; without it set, at `RELEASE_E2E_TIMEOUT` |

A failed wait leaves the draft release and the staged bytes as they are. Re-running the release's failed jobs once
the end-to-end system has reported picks its status up.

## Running

```sh
matchstick run --config matchstick.yaml
```

Run one copy. Replace it rather than run two at once (on Kubernetes: one replica, `Recreate`). A restart is safe at any
time: the next start finds every machine and registration again. Machines still booting when it restarts cannot claim
their runner configuration, which only the old process held: they power off, and the new process deletes them and
creates replacements as jobs need them.

Two installations can share a GitHub organisation, a cloud account and a GitHub App, such as one that takes a new
version first: each has its own `instance` and its own claim endpoint, and each label belongs to one installation's
pools only, or a job asking for it may land on either.

On Kubernetes, the chart in `deploy/chart/matchstick` runs it that way. A released chart's image is
`<image.repository>@<image.digest>`, its `image.digest` defaulting to the manifest digest of the image released with
it: the pod runs exactly that image, and `image.tag` is ignored. To run another image with it, set `image.digest` to
that image's digest, or to `""` to run `<image.repository>:<image.tag>`, the tag defaulting to the chart's
`appVersion`. The chart in this repository has no digest of its own; give it one:

```sh
helm upgrade --install matchstick deploy/chart/matchstick -f values.yaml \
  --set image.digest=sha256:<digest>
```

The chart's `values.schema.json` describes every value, `config` included: Helm refuses to install or upgrade with a
value it doesn't know, of the wrong type, or outside its allowed values (a pool name with capitals, an
`ephemeralOSDisk` that isn't `ResourceDisk`, `CacheDisk` or `NvmeDisk`, `azure` without `claimURL`), and names it. The
chart sets `config.listen` itself, from `metrics.port` and `claim.port`.

Files the configuration names, such as a pool's `azure.customData`, go in `files`, one entry per file, and are mounted
beside it at `/etc/matchstick/<name>`:

```sh
helm upgrade --install matchstick deploy/chart/matchstick -f values.yaml \
  --set-file 'files.cloud-init\.yaml=cloud-init.yaml'   # and in values.yaml: customData: /etc/matchstick/cloud-init.yaml
```

The claim Service is a `LoadBalancer` with the annotations in `claim.service.annotations`; the default asks Azure for
an internal load balancer. On another cloud, give its own internal load balancer's annotations and set
`service.beta.kubernetes.io/azure-load-balancer-internal` to `null`.

| Probe | Checks | Fails when |
|---|---|---|
| readiness | `GET /metrics` on `metrics.port`, every 10 seconds | the process is not yet serving, or has stopped; it never depends on GitHub or the cloud, so the claim endpoint stays in service while either is down |
| liveness | `GET /healthz` on `metrics.port`, every 30 seconds, from 30 seconds after start | four checks in a row fail: a pool has gathered no machines for three intervals plus five minutes, such as during a cloud outage. Kubernetes then restarts the process |

## Watching

- `/healthz` on `listen.metrics`: 200 while every pool has gathered its machines within three intervals plus five
  minutes (a round's actions take at most five minutes).
- `/metrics`: `matchstick_demand`, `matchstick_machines{state}`, `matchstick_deletion_wait_seconds`,
  `matchstick_runners{status}`, `matchstick_creations_total`, `matchstick_creation_failures_total{kind}`,
  `matchstick_deletions_total{reason}`, `matchstick_registration_removals_total{reason}`,
  `matchstick_rounds_total{result}`, `matchstick_round_duration_seconds`, `matchstick_last_success_timestamp_seconds`,
  `matchstick_creation_paused`, `matchstick_draining`, `matchstick_listening`, `matchstick_claims_total{result}`
  (`released`, `refused`, `failed`), `matchstick_spares`, `matchstick_job_startup_seconds`, each with a `pool` label;
  `matchstick_claim_probes_total`, connections to the claim endpoint that closed before sending a byte, such as the
  claim Service's load balancer health probe.
- A machine whose job has ended and that is powering off counts in `matchstick_machines{state="running"}` until the
  cloud reports it stopped, and in no status of `matchstick_runners`: GitHub has removed its registration. So it is
  never an idle runner, and a gap between running machines and registrations of up to the machines that just
  finished their jobs is normal.
- `matchstick_listening` is 1 while the pool's listener holds a message session for its scale set, and 0 from the
  start until the first session opens and whenever one has ended: while its scale set is not in its runner group,
  while GitHub refuses or drops the session. While it is 0 the pool learns of no new jobs, so they queue on GitHub
  with no machine made for them. A pool of a `fake` configuration has no listener and no such series.
- `matchstick_spares` is how many idle machines the pool keeps ready now beyond its jobs: its `spares`, or 0 while it
  is quiet past its `idleTimeout` or drains ([spares](configuration.md#spares-and-the-idle-timeout)).
- `matchstick_deletion_wait_seconds` is how long the pool's longest-waiting machine has been stopped, failed or being
  deleted while the cloud still lists it, counted from the first round of this process that saw it so; 0 when there is
  none. A machine that powers off after its job is deleted at the next round and gone within a minute or two, so a
  wait of many minutes is a machine that is not being deleted: the cloud refuses its deletion, or never finishes it,
  and it keeps billing. `matchstick_machines{state="stopped"}` cannot tell that apart from a busy pool, where some
  machine has just stopped at almost every round. A restart starts the count again.
- `matchstick_job_startup_seconds` is a histogram of how long each job waited, from GitHub assigning it to the pool's
  scale set to GitHub assigning it to one of the pool's runners, both times as GitHub's job-started message gives
  them. A job that started on a spare shows seconds; one that waited for its VM to be created shows that wait. The
  median over a day: `histogram_quantile(0.5, sum by (pool, le) (increase(matchstick_job_startup_seconds_bucket[1d])))`.
  It counts the jobs whose job-started message the pool's listener receives.
- Logs: JSON lines on stdout, one per action, with `pool`, `machine` and `reason`; a refused claim says why. A failed
  TLS handshake on the claim endpoint is logged as `claim endpoint` with the error, unless the connection closed
  before sending a byte (counted in `matchstick_claim_probes_total` instead). A pool whose scale set cannot be put in
  its runner group logs `the pool cannot scale until its scale set is in its runner group`, with the error and
  `runnerGroup`, at each try; it creates nothing meanwhile.

Suggested alerts: `time() - matchstick_last_success_timestamp_seconds > 300`; `matchstick_creation_paused == 1` for
more than 30 minutes; `increase(matchstick_deletions_total{reason="unregistered"}[1h]) > 3`;
`matchstick_deletion_wait_seconds > 900`, a machine stopped, failed or being deleted for 15 minutes and still there;
`max_over_time(matchstick_listening[10m]) == 0`, a pool that has not listened for 10 minutes (a session that ends is
reopened after a backoff that starts at 5 seconds, so a brief 0 does not fire it);
`min_over_time(matchstick_runners{status="idle"}[30m]) > ignoring(status) max_over_time(matchstick_spares[30m])`, a
pool that has held more idle runners than it keeps spares for 30 minutes, such as idle machines it cannot delete
(`ignoring(status)` because `matchstick_spares` has no `status` label; without it the comparison matches nothing).

## Looking

```sh
matchstick status --config matchstick.yaml --json   # machines, registrations, demand, runner group, spares, per pool
matchstick plan --config matchstick.yaml --json     # what a round would delete and create now
```

Both read the cloud and GitHub with the same configuration and secret mount as `run`, and change nothing. A pool's
`spares` in either says how many idle machines it keeps now, why, and until when; they see only the cloud and
GitHub, so after the last job ended they show the window as a restarted process would
([limits](configuration.md#spares-and-the-idle-timeout)).

## Draining a pool

```sh
matchstick drain --config matchstick.yaml --pool build          # stop taking jobs, give up idle runners
matchstick drain --config matchstick.yaml --pool build --stop   # back to normal
```

`drain` talks to the running process on `listen.admin`, a loopback address, so it runs where `run` runs. On Kubernetes:

```sh
kubectl exec deploy/matchstick -- matchstick drain --config /etc/matchstick/matchstick.yaml --pool build
```

`status` shows the pool empty once its busy runners have finished. A drain lasts until the process restarts; to keep a
pool empty, set its `maximum` to 0.

## Cost

A machine that powered itself off still bills until matchstick deletes it, which it does on the next round. While
matchstick is stopped, finished machines keep billing, within each pool's maximum, until it starts again.

Idle machines bill too: the spares, and machines still booting. A pool holds at most `spares` idle spares, within its
`maximum`; without `idleTimeout` they bill at all times, with it only around the pool's jobs
([spares](configuration.md#spares-and-the-idle-timeout), with a worked example). The VM-hours of idle runners over
the last day, per pool: `avg_over_time(matchstick_runners{status="idle"}[1d]) * 24`; times the VM size's hourly price,
their cost.
