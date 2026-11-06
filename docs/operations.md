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
Sigstore, keyless: the signing certificate names the release workflow at the version's tag. Check them before you use
them:

```sh
cosign verify ghcr.io/fakta-ai/matchstick@sha256:<digest> \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity https://github.com/fakta-ai/matchstick/.github/workflows/release.yml@refs/tags/v<version>
cosign verify ghcr.io/fakta-ai/charts/matchstick@sha256:<digest> \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity https://github.com/fakta-ai/matchstick/.github/workflows/release.yml@refs/tags/v<version>
# the downloaded archives, through checksums.txt
cosign verify-blob checksums.txt --bundle checksums.txt.sigstore.json \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity https://github.com/fakta-ai/matchstick/.github/workflows/release.yml@refs/tags/v<version>
sha256sum --check --ignore-missing checksums.txt
```

A release published from the public repository also has GitHub's build provenance attestations (SLSA provenance) for
its archives, `checksums.txt`, the image and the chart, naming the commit and the workflow run that built them:

```sh
gh attestation verify matchstick_<version>_linux_amd64.tar.gz --repo fakta-ai/matchstick
gh attestation verify oci://ghcr.io/fakta-ai/matchstick@sha256:<digest> --repo fakta-ai/matchstick
gh attestation verify oci://ghcr.io/fakta-ai/charts/matchstick@sha256:<digest> --repo fakta-ai/matchstick
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
