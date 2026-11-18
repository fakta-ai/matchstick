# Contributing

Thank you for helping.

## Before you start

- For anything larger than a fix, open an issue first and describe the problem, so we can agree on the approach
  before you write code.
- Read [docs/architecture.md](docs/architecture.md). A change that adds remembered state, a dependency or an
  extension point is discussed in an issue first.
- Security problems: see [SECURITY.md](SECURITY.md), not an issue.

## Setting up

With [Nix](https://nixos.org/), `nix develop` gives a shell with Go, golangci-lint, govulncheck, Helm, cosign, jq and
yq. Without Nix, you need Go (the version in `go.mod`) and [golangci-lint](https://golangci-lint.run/) v2.

```sh
go vet ./...
go test -race ./...
golangci-lint run ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
helm lint deploy/chart/matchstick
helm lint deploy/chart/matchstick --values deploy/chart/matchstick/ci/azure-values.yaml
bash .github/tests/codex.sh
bash .github/tests/release.sh
# what a release builds, published nowhere; needs syft (https://github.com/anchore/syft), and Docker for the image
go run github.com/goreleaser/goreleaser/v2@v2.19.0 release --snapshot --clean --skip=sign
```

CI runs these on every pull request as parallel jobs: the Go checks only when Go files, `cmd/`, `internal/`,
`examples/`, `go.mod`, `go.sum`, `.golangci.yml` or the CI workflow change (and on every commit on `main`), the
GoReleaser snapshot only when `.goreleaser.yaml` or the release workflow change. Nothing needs a cloud account or a
GitHub App: the tests use in-memory fakes, and `matchstick plan --config examples/fake.yaml` runs the planner against
a fake pool.

## How the code is organised

- The decisions are pure functions in `internal/plan`: they take what was observed and return what to do. Put new
  rules there, with table tests.
- Everything that talks to the outside (cloud, GitHub, files, HTTP, the terminal) is a thin layer around them.
- Interfaces exist only where there are two implementations: `provider.Provider` (Azure and the in-memory fake) and
  `github.Runners` (GitHub and its fake). Supporting another cloud means implementing `provider.Provider`: create,
  list and delete one pool's machines. Please don't add options or hooks nobody uses yet.
- Every operation is declared once with JSON Schemas for its input and output (`internal/op`); the command line and
  the `rpc`/`mcp` adapters are built from those declarations. A new operation or field starts there. Output fields are
  only ever added, never renamed or removed.
- Secrets are read only from standard input or the platform's secret mount, and never logged or printed.

## Tests

Test behaviour: given these machines and runners, the pool deletes this and creates that; after a restart halfway, it
converges. Use the fakes in `internal/provider/fake` and `internal/github/fake`. A bug fix comes with a test that
fails without it.

## Pull requests

- Titles and commits follow [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/).
- Update `docs/` and the `Unreleased` section of `CHANGELOG.md` when behaviour changes.
- Keep each pull request to one concern. Pull requests are merged with a merge commit once CI passes and review
  threads are resolved.
- Dependabot opens weekly pull requests moving `flake.lock`'s inputs, the Go modules and the workflows' actions, each
  to a release or commit at least 14 days old. One that changes only `flake.lock` needs no review and merges by itself
  once CI passes.

## Releases

A release is a pull request that moves the `Unreleased` entries of `CHANGELOG.md` under a new version heading,
`## [x.y.z] - YYYY-MM-DD`, above the previous one. Nobody pushes tags. When it merges and CI passes on `main`, the
same CI run (`.github/workflows/release.yml`):

1. tags the commit `vx.y.z`, if that version has no tag yet;
2. builds everything once (`.goreleaser.yaml`): the binaries, their SBOMs and the signed `checksums.txt` into a draft
   release, and the image and the chart into staging repositories only, `ghcr.io/fakta-ai/matchstick-staging` and
   `ghcr.io/fakta-ai/charts-staging/matchstick`, each tagged with the version, signed, with SBOMs, and with build
   provenance once the repository is public. The chart's default image is the staged image's digest under its public
   name, `ghcr.io/fakta-ai/matchstick@sha256:…`;
3. when the repository variable `RELEASE_E2E_CONTEXT` is set, waits for the commit status with that context on the
   release's commit to be `success` with a description that names the staged chart's digest, for at most
   `RELEASE_E2E_TIMEOUT` minutes (30 by default). An end-to-end system installs the staged chart with
   `image.repository` set to `ghcr.io/fakta-ai/matchstick-staging`, runs a real job on a machine it makes, and sets
   that status, naming the chart's digest (the digests are in the run's summary). A status naming another digest is
   about an earlier build of the same commit and doesn't count. A failure or timeout ends the run, with the draft and
   the staged images left as they are;
4. copies the staged image and chart, by digest, with their signatures, SBOMs and attestations, to
   `ghcr.io/fakta-ai/matchstick:<version>` and `ghcr.io/fakta-ai/charts/matchstick:<version>`;
5. publishes the draft release;
6. verifies, from the public names, the signatures, the digests, that the chart runs the image by its digest, the
   files against `checksums.txt`, the provenance once public, and that the image runs and reports the version. A
   failed check fails the run; nothing is undone.

A version whose run failed after tagging is not built again by later commits: the next release takes the next
version. Re-running the failed jobs of that run continues it.

By contributing you agree that your contribution is licensed under the [MIT License](LICENSE).
