# Operations

## Installing

Each release publishes:

| What | Where |
|---|---|
| binaries for Linux and macOS, amd64 and arm64, with `checksums.txt` | the GitHub release |
| the container image (both executables, CA certificates, no shell, user 65532) | `ghcr.io/fakta-ai/matchstick:<version>` |
| the Helm chart, its version and `appVersion` the release's | `oci://ghcr.io/fakta-ai/charts/matchstick:<version>` |

The release notes give the image's and the chart's digests. Both are signed with Sigstore, keyless: the signing
certificate names the release workflow at the version's tag. Check one before you use it:

```sh
cosign verify ghcr.io/fakta-ai/matchstick@sha256:<digest> \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity https://github.com/fakta-ai/matchstick/.github/workflows/release.yml@refs/tags/v<version>
cosign verify ghcr.io/fakta-ai/charts/matchstick@sha256:<digest> \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity https://github.com/fakta-ai/matchstick/.github/workflows/release.yml@refs/tags/v<version>
```

Flux checks the chart itself when its `OCIRepository` has `verify.provider: cosign` with that issuer and identity
(`matchOIDCIdentity`); pin the image with the chart's `image.digest`.

## Running

```sh
matchstick run --config matchstick.yaml
```

Run one copy. Replace it rather than run two at once (on Kubernetes: one replica, `Recreate`). A restart is safe at any
time: the next start finds every machine and registration again. Machines still booting when it restarts cannot claim
their runner configuration, which only the old process held: they power off, and the new process deletes them and
creates replacements as jobs need them.

On Kubernetes, the chart in `deploy/chart/matchstick` runs it that way. Its image is
`<image.repository>:<image.tag>`, the tag defaulting to the chart's `appVersion`. To run exactly the image you
checked, pin it by digest: set `image.digest` to the image's manifest digest, `sha256:` followed by 64 hexadecimal
digits. The pod then runs `<image.repository>@<image.digest>`, and `image.tag` is ignored.

```sh
helm upgrade --install matchstick deploy/chart/matchstick -f values.yaml \
  --set image.digest=sha256:<digest>
```

## Watching

- `/healthz` on `listen.metrics`: 200 while every pool has gathered its machines within three intervals plus five
  minutes (a round's actions take at most five minutes).
- `/metrics`: `matchstick_demand`, `matchstick_machines{state}`, `matchstick_runners{status}`,
  `matchstick_creations_total`, `matchstick_creation_failures_total{kind}`, `matchstick_deletions_total{reason}`,
  `matchstick_registration_removals_total{reason}`, `matchstick_rounds_total{result}`,
  `matchstick_round_duration_seconds`, `matchstick_last_success_timestamp_seconds`, `matchstick_creation_paused`,
  `matchstick_draining`, `matchstick_claims_total{result}` (`released`, `refused`, `failed`), each with a `pool`
  label.
- Logs: JSON lines on stdout, one per action, with `pool`, `machine` and `reason`; a refused claim says why.

Suggested alerts: `time() - matchstick_last_success_timestamp_seconds > 300`; `matchstick_creation_paused == 1` for
more than 30 minutes; `increase(matchstick_deletions_total{reason="unregistered"}[1h]) > 3`.

## Looking

```sh
matchstick status --config matchstick.yaml --json   # machines, registrations, demand, per pool
matchstick plan --config matchstick.yaml --json     # what a round would delete and create now
```

Both read the cloud and GitHub with the same configuration and secret mount as `run`, and change nothing.

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
