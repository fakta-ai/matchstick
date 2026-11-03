# Working on matchstick

For anyone changing this repository, people and coding agents alike. [CONTRIBUTING.md](CONTRIBUTING.md) has the same
rules for people; this file is the short form. How it is built: [docs/architecture.md](docs/architecture.md).

## Build and check

```sh
go vet ./...
go test -race ./...
golangci-lint run ./...        # configuration in .golangci.yml
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
```

CI runs exactly these on every pull request (`.github/workflows/ci.yml`, job `check`); `main` accepts changes only
through pull requests whose `check` passed.

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
- Releases are cut by pushing a `vX.Y.Z` tag; `CHANGELOG.md` gets the version's section first.
