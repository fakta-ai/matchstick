# Changelog

All notable changes are listed here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and
versions follow [Semantic Versioning](https://semver.org/).

## [Unreleased]

## [0.4.1] - 2026-11-17

### Fixed

- The chart installs when its version carries build metadata, as Flux gives it when it installs the chart from an OCI
  artifact (`0.4.0+457a3409ec79`): the `helm.sh/chart` label had the `+` in it, which Kubernetes refuses in a label,
  so every object of the release was rejected. The `+` is written `_`, as Helm's own chart scaffold does.

### Security

- Built with Go 1.27.4, which fixes GO-2026-6859: a race in `http.MaxBytesReader` could crash an HTTP/1 server. The
  claim endpoint and the admin endpoint read their requests through it.

## [0.4.0] - 2026-11-06

### Added

- After publishing, the release workflow verifies the public image, chart and files: digests, signatures,
  `checksums.txt`, provenance once the repository is public, and that the image runs and reports its version.

### Changed

- Releases happen when a pull request that moves `CHANGELOG.md`'s entries under a new version merges: the CI run on
  `main` tags the commit itself once its checks pass. Nobody pushes version tags.
- A release builds once: the image and the chart go first to `ghcr.io/fakta-ai/matchstick-staging` and
  `ghcr.io/fakta-ai/charts-staging/matchstick`, then their exact digests, with signatures, SBOMs and attestations, are
  copied to the public names before the GitHub release is published. When the repository variable
  `RELEASE_E2E_CONTEXT` is set, promotion waits for that commit status to succeed (`RELEASE_E2E_TIMEOUT` minutes, 30
  by default).
- Signatures name the release workflow on `main`:
  `--certificate-identity https://github.com/fakta-ai/matchstick/.github/workflows/release.yml@refs/heads/main`
  (releases up to 0.3.1: `…/release.yml@refs/tags/v<version>`).

### Removed

- The flake's package and checks, and `nix/`: the flake is the dev shell only; CI and releases build with Go and
  GoReleaser.

## [0.3.1] - 2026-11-06

### Fixed

- The release workflow signs the Helm chart: cosign now logs in to the registry the chart is pushed to. 0.3.0's
  chart was pushed but not signed, so 0.3.0 was not published.

### Changed

- Pull requests run the Go checks only when what Go builds or tests changes. Every commit on `main` also builds every
  release target, so a release builds from a warm cache.

## [0.3.0] - 2026-11-06

### Added

- Releases publish an SPDX SBOM for each archive and for the image, and sign `checksums.txt` with Sigstore, keyless
  (`checksums.txt.sigstore.json`).
- Releases from the public repository carry GitHub's build provenance attestations for the archives,
  `checksums.txt`, the image and the chart (`gh attestation verify`, `docs/operations.md`).

### Changed

- The container image is built with ko, for linux/amd64 and linux/arm64, on distroless static (CA certificates, time
  zones, no shell, user 65532) pinned by digest. It holds `matchstick` only, at `/ko-app/matchstick` and on `PATH`,
  with no default arguments; `matchstick-adapter` is in the release archives only. The chart runs it unchanged.
- A version tag publishes only a commit on `main` whose CI run there passed; the release waits for that run while it
  is in progress. The GitHub release stays a draft until the image and the chart are published.
- CI runs its checks as parallel jobs; pull requests build the Nix package only when its inputs change.

### Removed

- The flake's `image` output; the Nix package and the dev shell remain.

## [0.2.0] - 2026-11-06

### Added

- The chart's `values.schema.json`: Helm refuses unknown values, wrong types and values outside their allowed set,
  `config` included.
- The chart's readiness probe, `GET /metrics`: the pod is ready once matchstick serves, whether or not GitHub and the
  cloud are reachable.
- The chart's `files` value: files the configuration names, such as `azure.customData`, mounted beside it at
  `/etc/matchstick/<name>`.
- Dependabot also updates the Go modules and the workflows' actions, weekly, each at least 14 days old.
- `matchstick github-app create`: registers the GitHub App through GitHub's App manifest flow, for an organisation or
  a repository, on github.com or GitHub Enterprise Server. A page on a loopback address sends the manifest; the App is
  private, has no webhook, and has only the permissions matchstick needs. Its private key goes to a new file with mode
  0600; the output gives the App's ID, client ID, slug, page and install address.
- `matchstick github-app check`: from the configuration and secret mount `run` uses, checks that GitHub accepts the
  key, that the App is installed on the configured owner (reporting the installation ID), that the installation has
  the permissions matchstick needs, and that the runner group exists for it; one entry per check, exit 1 on a failure.
  Over `matchstick-adapter`, a failed check's report comes with the error (`data.result` in JSON-RPC, structured
  content in MCP).
- `docs/setup.md`: setting up from nothing to a first job, including the Azure custom role's exact actions and storing
  the key with the External Secrets Operator.
- An operation's `--help` lists its output's fields.

### Changed

- A version tag publishes only after the checks CI runs pass on the tagged commit.

### Fixed

- The documentation no longer says a GitHub App can register an enterprise's runners; an enterprise needs a token.

## [0.1.0] - 2026-11-06

### Added

- The architecture, in `docs/architecture.md`.
- The planner: given a pool's machines, runner registrations and demand, decides which machines to delete (finished,
  expired, never registered, surplus), which registrations to remove (orphans) and how many machines to create,
  within the pool's maximum.
- One pool's loop: gathers the pool's machines and registrations, decides, and acts: removes registrations before
  deleting their machines, creates a registration and then its machine, removes the registration of a machine the
  cloud refused, pauses creation after capacity errors and after machines that never register.
- The `decide`, `plan` and `status` commands, with JSON Schemas for input and output, `--json`, `--input`, and
  `--help --json` listing them as MCP tools.
- A fake cloud and GitHub, configured in the configuration file: `matchstick plan --config examples/fake.yaml`.
- Prometheus metrics for each pool.
- `matchstick run`: serves every configured pool with GitHub runner scale sets and Azure virtual machines; metrics and
  health on `listen.metrics`, drain on `listen.admin`.
- `matchstick drain`: drains a pool of the running process, or stops draining it.
- The Azure provider: private network interface, Compute Gallery image, identity per pool or none, ephemeral OS disk,
  Spot with regular fallback, quota and allocation failures reported as retry-later.
- The GitHub credential is read from standard input or the secret mount.
- `docs/configuration.md`, `docs/operations.md`, `examples/azure.yaml`.
- `matchstick-adapter rpc` and `matchstick-adapter mcp`: the operations over JSON-RPC 2.0 and MCP on standard input and
  output, running the `matchstick` command line for each call.
- `docs/image-contract.md` and `examples/cloud-init.yaml`: what a runner image does with the JIT configuration.
- A Helm chart, `deploy/chart/matchstick`: one replica, `Recreate`, Azure workload identity, the GitHub credential
  from a Secret.
- A Nix flake: the package, a dev shell, checks, and the container image (no shell, non-root).
- The release workflow: a version tag publishes binaries for Linux and macOS and `ghcr.io/fakta-ai/matchstick`.
- The claim endpoint (`claimURL`, `listen.claim`): a machine fetches its runner's JIT configuration over TLS, proving
  which VM it is with Azure's attested document; matchstick releases each configuration once, only to its own VM,
  and holds it in memory only. Its TLS key is made at start and never stored; machines trust only its certificate.
- `matchstick_claims_total{pool,result}`.
- The chart's `image.digest` pins the image by digest.
- `docs/threat-model.md`: what matchstick protects, whom it trusts, each threat with its mitigation and what remains,
  and what the operator must provide.
- Releases also publish the Helm chart, `oci://ghcr.io/fakta-ai/charts/matchstick`, and sign the image and the chart
  with Sigstore (keyless, for the release workflow at the version's tag); the release notes give both digests.

### Security

- The JIT configuration is no longer put in the VM's user data, which anyone allowed to read the VM could read and
  use to register as its runner. User data now holds public bootstrap information only (the claim endpoint's address
  and certificate); runner images claim the configuration as `docs/image-contract.md` describes, and
  `examples/cloud-init.yaml` does so.
