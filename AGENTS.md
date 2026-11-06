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
nix build .#matchstick         # both executables; runs the tests; checks vendorHash
bash .github/tests/codex.sh    # the review gate: Dependabot's flake.lock-only pull requests pass without a review
# what a release builds, published nowhere (needs syft, and Docker for the image)
go run github.com/goreleaser/goreleaser/v2@v2.19.0 release --snapshot --clean --skip=sign
```

CI (`.github/workflows/ci.yml`) runs these as parallel jobs with Go's caches: Go (vet, race tests, golangci-lint,
govulncheck), the chart (lint and render, default and example values), the review gate's test, and `nix build`. A
pull request runs the Go job only when Go files, `cmd/`, `internal/`, `examples/`, `go.mod`, `go.sum`,
`.golangci.yml` or `ci.yml` change; `nix build` only when `flake.nix`, `flake.lock`, `nix/`, `go.mod` or `go.sum`
change; and the GoReleaser snapshot only when `.goreleaser.yaml` or the release workflow change. Every commit on `main`
runs the Go job and `nix build`, and builds every release target to keep the cache the release restores
(`release-build`). Job `check` passes only when every job the run needs passed. `main` accepts changes only through
pull requests whose `check` passed. A change to `go.sum` updates `vendorHash` in `nix/package.nix`. Dependabot opens
weekly pull requests for `flake.lock`, the Go modules and the workflows' actions (`.github/dependabot.yml`); one that
changes only `flake.lock` merges by itself once `check` passes.

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
- Releases are cut by pushing a `vX.Y.Z` tag on a commit of `main`; `CHANGELOG.md` gets the version's section first.
  The release workflow (`.github/workflows/release.yml`) waits for `ci.yml`'s run for that commit on `main` and
  refuses the tag if it failed or never ran; then GoReleaser (`.goreleaser.yaml`) publishes the binaries, SBOMs,
  signed checksums and the ko-built image, a parallel job publishes the chart, and the draft release is published
  once both are. Signatures are Sigstore's, keyless; build provenance attestations are added once the repository is
  public. The image's base is pinned by digest in `.goreleaser.yaml` and moved by hand.
