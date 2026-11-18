# Working on matchstick

For anyone changing this repository, people and coding agents alike. [CONTRIBUTING.md](CONTRIBUTING.md) has the same
rules for people; this file is the short form. How it is built: [docs/architecture.md](docs/architecture.md).

## Build and check

```sh
nix develop                    # Go, golangci-lint, govulncheck, Helm, cosign, jq, yq
go vet ./...
go test -race ./...
golangci-lint run ./...        # configuration in .golangci.yml
govulncheck ./...
helm lint deploy/chart/matchstick
helm lint deploy/chart/matchstick --values deploy/chart/matchstick/ci/azure-values.yaml
bash .github/tests/codex.sh    # the review gate: Dependabot's flake.lock-only pull requests pass without a review
bash .github/tests/release.sh  # the release's decisions: which version, the tag, the chart's image, the end-to-end wait
# what a release builds, published nowhere (needs syft, and Docker for the image)
go run github.com/goreleaser/goreleaser/v2@v2.19.0 release --snapshot --clean --skip=sign
```

The flake is the dev shell only; nothing builds with Nix. CI (`.github/workflows/ci.yml`) runs these as parallel jobs
with Go's caches: Go (vet, race tests, golangci-lint, govulncheck), the chart (lint and render, default and example
values) and the workflows' tests. A pull request runs the Go job only when Go files, `cmd/`, `internal/`,
`examples/`, `go.mod`, `go.sum`, `.golangci.yml` or `ci.yml` change, and the GoReleaser snapshot only when
`.goreleaser.yaml` or the release workflow change. Every commit on `main` runs the Go job and builds every release
target to keep the cache the release restores (`release-build`). Job `check` passes only when every job the run
needs passed. `main` accepts changes only through pull requests whose `check` passed. Dependabot opens weekly pull
requests for `flake.lock`, the Go modules and the workflows' actions (`.github/dependabot.yml`); one that changes
only `flake.lock` merges by itself once `check` passes.

## Design rules

- **Functional core, imperative shell.** Decisions are pure functions over data (`internal/plan`): state in, decision
  out, no clock, no network, no randomness. Everything that talks to the cloud, GitHub, files, the clock or a
  terminal lives at the edges and stays thin.
- **No records of its own.** matchstick never writes state anywhere. What the cloud and GitHub report is the truth,
  read again every round. A change that needs remembered state needs a design change first, in
  `docs/architecture.md`.
- **Interfaces only where a second implementation exists** (the provider: Azure and the fake; the GitHub side: real and
  fake). No speculative options, hooks or abstractions.
- **Every operation is declared once**, with a JSON Schema for its input and its output and its annotations (read-only,
  destructive, idempotent, open world), and checked against both on every call. The command line, JSON-RPC and MCP
  are adapters over those declarations.
- **Command line:** `-h`/`--help` with an example, `--version`; flags are the input schema's names in kebab-case;
  `--input <file|->` takes the whole input as JSON; `--json` prints one JSON value on stdout and later versions only
  add fields; `--help --json` prints the operations as MCP tool definitions; exit 0 on success, 2 on invalid input, 1
  otherwise; with `--json`, errors go to stderr as `{"error": {"code": "...", "message": "..."}}`; nothing prompts.
- **Secrets** come only from standard input or the platform's secret mount: never from flags, environment variables,
  the configuration file, logs, errors or output. The process that holds them keeps third-party code to the minimum,
  pinned; anything else (`matchstick-adapter`'s `rpc` and `mcp` modes) runs as a separate executable without secrets.
- **Tests test behaviour**: what a pool does given what it observes, against the in-memory fakes, including restarts
  and failures halfway. Not which function called which.
- **Docs say what is true now**, for the reader: what it does, how to use it, its limits. That holds for the README,
  `docs/` and code comments alike. No roadmap, plans or direction; no comparisons or positioning; no design history
  or the reasons behind decisions. A limit is stated as a fact ("Azure only"). The README tells a user what
  matchstick does, what it supports and how to use it. A change to behaviour updates `docs/` and `CHANGELOG.md` in
  the same pull request.

## Go

- The `go` version in `go.mod`; standard library first (`slices`, `maps`, `math/rand/v2`, `log/slog`); `new(expr)`
  rather than pointer helpers; `for i := range n`; `b.Loop()` in benchmarks; `net.JoinHostPort` for addresses;
  `runtime.AddCleanup` rather than finalizers; no `io/ioutil`.
- Errors wrap with `%w` and say what failed; sentinel errors are checked with `errors.Is`.
- A new dependency needs a reason in its pull request: what it gives that the standard library doesn't.

## Commits and pull requests

- [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/): `feat:`, `fix:`, `docs:`, `chore:`, `test:`,
  `refactor:`, `ci:`.
- Branches `feat/<slug>`, `fix/<slug>`, `chore/<slug>`, `docs/<slug>`; merged with a merge commit.
- One concern per pull request, small enough to review.
- A release is a pull request that moves `CHANGELOG.md`'s `Unreleased` entries under a new `## [x.y.z] - YYYY-MM-DD`
  heading. Nobody tags: once it merges and `check` passes, the same CI run (`.github/workflows/release.yml`) tags the
  commit `vx.y.z`, builds everything once with GoReleaser (`.goreleaser.yaml`) into a draft release and staging
  registries (`ghcr.io/fakta-ai/matchstick-staging`, `ghcr.io/fakta-ai/charts-staging/matchstick`), signed and with
  SBOMs, the chart's default image the staged image's digest; waits, when `RELEASE_E2E_CONTEXT` is set, for an
  end-to-end status on the commit whose description names the staged chart's digest, and, when
  `RELEASE_E2E_ACK_TIMEOUT` is set too, fails early unless a status naming that digest (`pending` or a result)
  acknowledges the build within that many minutes; copies the staged digests to the public names, publishes the
  draft, and verifies what it published. Signatures are Sigstore's, keyless, by `release.yml@refs/heads/main`; build
  provenance attestations are added once the repository is public. The image's base is pinned by digest in
  `.goreleaser.yaml` and moved by hand.
